package cleric

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// StatusInProgress / StatusAwaitingReview / StatusClosed are the recovery-bead
// statuses cleric handlers transition through. Mirrored as constants so the
// package doesn't pull in pkg/store's full status surface.
const (
	StatusInProgress     = "in_progress"
	StatusAwaitingReview = "awaiting_review"
	StatusClosed         = "closed"
)

// gateOutput keys the wait_for_gate step's outputs. The gateway sets
// these externally when the human reviewer acts on the proposal.
const (
	GateApprove  = "approve"
	GateReject   = "reject"
	GateTakeover = "takeover"
)

// Deps groups the pkg/store reads/writes the cleric handlers need.
// Decoupled from pkg/executor.Deps so pkg/cleric stays testable in
// isolation and so the executor's wiring is the only place where the
// two surfaces need to be reconciled. All function fields are required
// — handlers nil-check and return a clear error rather than panic when
// a seam is unwired, which keeps tests honest about what they're
// exercising.
type Deps struct {
	// GetBead loads a bead by ID.
	GetBead func(id string) (store.Bead, error)

	// SetBeadMetadata merges metadata kv pairs into a bead.
	SetBeadMetadata func(id string, meta map[string]string) error

	// UpdateBead applies field updates (status, etc.).
	UpdateBead func(id string, updates map[string]interface{}) error

	// AddLabel adds a label to a bead.
	AddLabel func(id, label string) error

	// AddComment writes an audit comment.
	AddComment func(id, text string) error

	// CloseBead transitions a bead to closed.
	CloseBead func(id string) error

	// GetDepsWithMeta returns dependency edges with metadata; handlers
	// use this to find the source bead via caused-by.
	GetDepsWithMeta func(id string) ([]*beads.IssueWithDependencyMetadata, error)

	// Gateway is the client used by Execute to invoke action endpoints.
	// Production callers wire a real client; tests pass stubs.
	Gateway GatewayClient

	// Learning is the promotion/demotion policy the auto-approve
	// fast-path consults during cleric.publish. Production wiring uses
	// StoreLearning{}; tests inject a fake. Nil disables the fast-path.
	Learning LearningStore

	// RecordOutcome persists a per-round outcome row for the
	// promotion/demotion learning loop. Production wires this to
	// store.RecordClericOutcomeAuto. Nil disables outcome recording —
	// handlers do not error in that case so legacy tests don't have to
	// wire it up.
	RecordOutcome func(o store.ClericOutcome) error

	// Now returns the current wall-clock time. Tests inject a fixed
	// clock; production callers leave it nil and the package falls back
	// to time.Now.
	Now func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

// HandlerResult is the return value from each cleric handler. It maps
// 1:1 onto the executor's ActionResult fields when the executor adapts
// it. The struct keeps pkg/cleric independent of pkg/executor.
type HandlerResult struct {
	// Outputs flow into the formula step's outputs map.
	Outputs map[string]string

	// Err, when non-nil, means the step's action failed. The formula
	// engine treats this as a step error per the step's on_error
	// directive (default: park the step / hook the bead).
	Err error
}

// Publish parses the cleric Claude agent's stdout (passed in as
// decideStdout — the "result" output of the decide step), validates it
// against the ProposedAction schema, persists it on the recovery bead's
// metadata, and transitions the bead to awaiting_review.
//
// Auto-approve fast-path (spi-kl8x5y): if deps.Learning reports the
// (failure_class, action) pair is promoted, Publish skips
// awaiting_review entirely. The recovery bead is labeled
// auto-approved:promoted and the result outputs carry an
// auto_approved="true" flag plus the gate values the formula's
// wait_for_gate step would otherwise wait for; the executor adapter
// pre-writes those into wait_for_gate's outputs so the formula advances
// to cleric.execute synchronously.
//
// Returns Outputs.status="awaiting_review" on the normal path,
// "auto_approved" on the fast path. On parse / validation failure,
// returns Err so the formula engine parks the step (the recovery bead
// becomes its own failure case; per the design, recursive recoveries
// are out of scope for v1).
func Publish(recoveryBeadID, decideStdout string, deps Deps) HandlerResult {
	if err := requireSeams(deps, "Publish",
		deps.GetBead, deps.SetBeadMetadata, deps.UpdateBead, deps.AddComment); err != nil {
		return HandlerResult{Err: err}
	}
	if strings.TrimSpace(recoveryBeadID) == "" {
		return HandlerResult{Err: fmt.Errorf("cleric.publish: recovery bead ID is empty")}
	}

	pa, err := ParseProposedAction([]byte(decideStdout))
	if err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.publish: parse cleric stdout: %w", err)}
	}

	encoded, err := pa.Marshal()
	if err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.publish: marshal proposal: %w", err)}
	}
	now := deps.now().Format(time.RFC3339)

	if err := deps.SetBeadMetadata(recoveryBeadID, map[string]string{
		MetadataKeyProposal:            string(encoded),
		"cleric_proposal_published_at":  now,
		"cleric_proposal_verb":          pa.Verb,
		"cleric_proposal_failure_class": pa.FailureClass,
	}); err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.publish: persist proposal: %w", err)}
	}

	autoApproved := IsPromoted(deps.Learning, pa.FailureClass, pa.Verb)
	if autoApproved {
		// Fast-path: transition straight to closed-pending-execute. The
		// formula's wait_for_gate step will see gate=approve in its
		// outputs (pre-written by the executor adapter) and advance
		// without parking.
		if deps.AddLabel != nil {
			_ = deps.AddLabel(recoveryBeadID, LabelAutoApproved)
		}
		summary := fmt.Sprintf(
			"cleric proposes: %s — %s [auto-approved: %s/%s promoted on prior outcomes]",
			pa.Verb, truncate(pa.Reasoning, 200), pa.FailureClass, pa.Verb)
		if cerr := deps.AddComment(recoveryBeadID, summary); cerr != nil {
			// Non-fatal: persistence already succeeded.
			return HandlerResult{
				Outputs: map[string]string{
					"status":            "auto_approved",
					"verb":              pa.Verb,
					"auto_approved":     "true",
					"gate":              GateApprove,
					"rejection_comment": "auto-approved",
					"comment_warning":   cerr.Error(),
				},
			}
		}
		return HandlerResult{
			Outputs: map[string]string{
				"status":            "auto_approved",
				"verb":              pa.Verb,
				"auto_approved":     "true",
				"gate":              GateApprove,
				"rejection_comment": "auto-approved",
			},
		}
	}

	// Normal path: transition recovery bead to awaiting_review.
	if err := deps.UpdateBead(recoveryBeadID, map[string]interface{}{
		"status": StatusAwaitingReview,
	}); err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.publish: transition to awaiting_review: %w", err)}
	}

	// Audit comment so humans see the proposal in the bead's stream.
	summary := fmt.Sprintf("cleric proposes: %s — %s", pa.Verb, truncate(pa.Reasoning, 240))
	if cerr := deps.AddComment(recoveryBeadID, summary); cerr != nil {
		// Non-fatal: persistence already succeeded. Surface in outputs.
		return HandlerResult{
			Outputs: map[string]string{"status": "awaiting_review", "verb": pa.Verb, "comment_warning": cerr.Error()},
		}
	}

	return HandlerResult{
		Outputs: map[string]string{
			"status": "awaiting_review",
			"verb":   pa.Verb,
		},
	}
}

// Execute reads the persisted ProposedAction from the recovery bead's
// metadata and invokes the gateway endpoint for the proposal's verb.
// Records the outcome (success/error) on the bead so the human
// reviewer can audit what happened.
//
// Returns Outputs.status="executed" + Outputs.execute_success=("true"|"false")
// on success (gateway returned). Returns Err only if the proposal can't be
// loaded or if the gateway returned an unexpected error (other than
// ErrGatewayUnimplemented, which is recorded but not raised — the recovery
// bead lives on for human takeover).
func Execute(recoveryBeadID string, deps Deps) HandlerResult {
	if err := requireSeams(deps, "Execute",
		deps.GetBead, deps.SetBeadMetadata, deps.GetDepsWithMeta); err != nil {
		return HandlerResult{Err: err}
	}
	if deps.Gateway == nil {
		return HandlerResult{Err: fmt.Errorf("cleric.execute: gateway client unwired")}
	}

	bead, err := deps.GetBead(recoveryBeadID)
	if err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.execute: load recovery bead: %w", err)}
	}
	rawProposal := bead.Meta(MetadataKeyProposal)
	if rawProposal == "" {
		return HandlerResult{Err: fmt.Errorf("cleric.execute: recovery bead %s has no %s metadata", recoveryBeadID, MetadataKeyProposal)}
	}
	pa, err := ParseProposedAction([]byte(rawProposal))
	if err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.execute: re-parse stored proposal: %w", err)}
	}

	sourceID, err := SourceBeadID(recoveryBeadID, deps)
	if err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.execute: resolve source bead: %w", err)}
	}

	ctx := context.Background()
	res, gerr := deps.Gateway.Execute(ctx, ExecuteRequest{
		RecoveryBeadID: recoveryBeadID,
		SourceBeadID:   sourceID,
		Proposal:       pa,
	})

	now := deps.now().Format(time.RFC3339)
	resultMeta := map[string]string{
		"cleric_executed_at":   now,
		"cleric_executed_verb": pa.Verb,
		// Default: not successful. Only the success branch below
		// flips this to "true". cleric.finish and steward's
		// recoveryShouldResume key on this strict marker rather than
		// the broader cleric_outcome string.
		MetadataKeyExecuteSuccess: "false",
	}
	successFlag := "false"
	switch {
	case gerr == nil && res.Success:
		resultMeta[MetadataKeyExecuteResult] = res.Message
		resultMeta[MetadataKeyExecuteSuccess] = "true"
		successFlag = "true"
	case gerr == nil && !res.Success:
		resultMeta[MetadataKeyExecuteResult] = "gateway: " + res.Message
	case errors.Is(gerr, ErrGatewayUnimplemented):
		// Stub gateway — record so the human reviewer can take over.
		// NOT a success: the action never ran.
		resultMeta[MetadataKeyExecuteResult] = "unimplemented: " + res.Message
	default:
		resultMeta[MetadataKeyExecuteResult] = "error: " + gerr.Error()
	}
	if err := deps.SetBeadMetadata(recoveryBeadID, resultMeta); err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.execute: persist outcome: %w", err)}
	}

	// Audit comment.
	if deps.AddComment != nil {
		_ = deps.AddComment(recoveryBeadID,
			fmt.Sprintf("cleric.execute: %s — %s", pa.Verb, resultMeta[MetadataKeyExecuteResult]))
	}

	out := map[string]string{
		"status":           "executed",
		"verb":             pa.Verb,
		"execute_success":  successFlag,
		"source_bead":      sourceID,
	}
	return HandlerResult{Outputs: out}
}

// Takeover applies the human-takeover semantics: source bead stays
// `hooked` (we do NOT touch its status) but gets the `needs-manual`
// label so the human and dashboards know the bead is awaiting manual
// repair. The recovery bead transitions to closed with an audit comment
// documenting the takeover.
//
// Records a takeover outcome row (spi-kl8x5y) — finalized=true since
// the wizard never resumes after a takeover, so there's nothing for the
// observer to fill. Takeover rows break BOTH the promotion and the
// demotion streak (they're neither approve+success nor reject).
func Takeover(recoveryBeadID string, deps Deps) HandlerResult {
	if err := requireSeams(deps, "Takeover",
		deps.GetBead, deps.GetDepsWithMeta, deps.AddLabel, deps.CloseBead); err != nil {
		return HandlerResult{Err: err}
	}

	sourceID, err := SourceBeadID(recoveryBeadID, deps)
	if err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.takeover: resolve source bead: %w", err)}
	}
	if sourceID == "" {
		return HandlerResult{Err: fmt.Errorf("cleric.takeover: recovery %s has no caused-by source", recoveryBeadID)}
	}

	if err := deps.AddLabel(sourceID, LabelNeedsManual); err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.takeover: label source %s: %w", sourceID, err)}
	}

	// Record outcome before closing the recovery bead so a downstream
	// learning consumer can find the row even if close-time hooks run
	// asynchronously.
	if deps.RecordOutcome != nil {
		bead, _ := deps.GetBead(recoveryBeadID)
		_ = deps.RecordOutcome(store.ClericOutcome{
			RecoveryBeadID: recoveryBeadID,
			SourceBeadID:   sourceID,
			FailureClass:   bead.Meta("cleric_proposal_failure_class"),
			Action:         bead.Meta("cleric_proposal_verb"),
			Gate:           GateTakeover,
			Finalized:      true,
			CreatedAt:      deps.now(),
		})
	}

	if deps.AddComment != nil {
		_ = deps.AddComment(sourceID,
			fmt.Sprintf("cleric: human took over recovery %s — bead labeled %s, status stays hooked", recoveryBeadID, LabelNeedsManual))
		_ = deps.AddComment(recoveryBeadID,
			fmt.Sprintf("cleric.takeover: closing recovery; source %s left hooked + %s for manual repair", sourceID, LabelNeedsManual))
	}

	if err := deps.CloseBead(recoveryBeadID); err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.takeover: close recovery %s: %w", recoveryBeadID, err)}
	}

	return HandlerResult{
		Outputs: map[string]string{
			"status":      "takeover",
			"source_bead": sourceID,
		},
	}
}

// Reject records a reject outcome row (spi-kl8x5y) for the
// promotion/demotion learning loop. Replaces the legacy noop-action
// requeue_after_reject step in the cleric formula so reject outcomes
// are visible to the learning store. The reject is always finalized=true
// — there's no wizard observation downstream of a rejection.
//
// Reject does NOT close the recovery bead. The cleric formula's reset
// directive on requeue_after_reject sends decide / publish / wait_for_gate
// back to pending, so the next wizard run spawns a fresh cleric round
// against the rejection comment.
func Reject(recoveryBeadID string, deps Deps) HandlerResult {
	if err := requireSeams(deps, "Reject", deps.GetBead); err != nil {
		return HandlerResult{Err: err}
	}
	bead, err := deps.GetBead(recoveryBeadID)
	if err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.reject: load bead: %w", err)}
	}
	verb := bead.Meta("cleric_proposal_verb")
	failureClass := bead.Meta("cleric_proposal_failure_class")
	if deps.RecordOutcome != nil && verb != "" && failureClass != "" {
		sourceID, _ := SourceBeadID(recoveryBeadID, deps)
		_ = deps.RecordOutcome(store.ClericOutcome{
			RecoveryBeadID: recoveryBeadID,
			SourceBeadID:   sourceID,
			FailureClass:   failureClass,
			Action:         verb,
			Gate:           GateReject,
			Finalized:      true,
			CreatedAt:      deps.now(),
		})
	}
	if deps.AddComment != nil {
		_ = deps.AddComment(recoveryBeadID,
			fmt.Sprintf("cleric.reject: recording outcome verb=%s failure_class=%s for promotion/demotion tally", verb, failureClass))
	}
	return HandlerResult{
		Outputs: map[string]string{
			"status": "rejected",
			"verb":   verb,
		},
	}
}

// Finish records the final outcome of a recovery cycle for the
// promotion/demotion learning loop and closes the recovery bead.
//
// Two writes:
//   - Bead metadata (existing): a redundant cleric_outcome key that's
//     visible to anyone reading the recovery bead via the standard API.
//   - cleric_outcomes table (spi-kl8x5y): a queryable row keyed by
//     (failure_class, action) that drives IsPromoted / IsDemoted /
//     ListDemotedClericPairs. The row is inserted with finalized=false
//     because the wizard's post-action success isn't known yet — the
//     wizard observer fills that in when the source bead's next-step
//     transition fires.
//
// Strict success contract (spi-skfsia finding 2): cleric_outcome is
// only stamped as `approve+executed` when cleric.execute previously
// recorded MetadataKeyExecuteSuccess="true". A stub gateway, gateway
// error, or non-success result yields outcome=`approve+failed` instead;
// pkg/steward.recoveryShouldResume keys on the strict success marker so
// the source bead is NOT re-summoned in the failure path. The recovery
// bead still closes — the human reviewer is expected to take over from
// the failure metadata or file a fresh recovery.
func Finish(recoveryBeadID string, deps Deps) HandlerResult {
	if err := requireSeams(deps, "Finish",
		deps.GetBead, deps.SetBeadMetadata, deps.CloseBead); err != nil {
		return HandlerResult{Err: err}
	}

	bead, err := deps.GetBead(recoveryBeadID)
	if err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.finish: load bead: %w", err)}
	}

	verb := bead.Meta("cleric_proposal_verb")
	failureClass := bead.Meta("cleric_proposal_failure_class")
	executeResult := bead.Meta(MetadataKeyExecuteResult)
	executeSuccess := bead.Meta(MetadataKeyExecuteSuccess)
	now := deps.now().Format(time.RFC3339)

	outcomeValue := "approve+failed"
	if executeSuccess == "true" {
		outcomeValue = "approve+executed"
	}
	outcome := map[string]string{
		MetadataKeyOutcome:              outcomeValue,
		"cleric_outcome_verb":           verb,
		"cleric_outcome_failure_class":  failureClass,
		"cleric_outcome_execute_result": executeResult,
		"cleric_outcome_recorded_at":    now,
	}
	if err := deps.SetBeadMetadata(recoveryBeadID, outcome); err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.finish: persist outcome: %w", err)}
	}

	// Record a pending outcome row for the learning loop. The wizard
	// observer finalizes wizard_post_action_success when the source
	// bead's next step transitions. Only record on real success — a
	// failed action shouldn't seed a streak that the observer will then
	// have to refute.
	if executeSuccess == "true" && deps.RecordOutcome != nil && verb != "" && failureClass != "" {
		sourceID, _ := SourceBeadID(recoveryBeadID, deps)
		targetStep := proposalTargetStep(bead, verb)
		// Best-effort: don't fail the handler on outcome-write errors.
		_ = deps.RecordOutcome(store.ClericOutcome{
			RecoveryBeadID: recoveryBeadID,
			SourceBeadID:   sourceID,
			FailureClass:   failureClass,
			Action:         verb,
			Gate:           GateApprove,
			TargetStep:     targetStep,
			Finalized:      false,
			CreatedAt:      deps.now(),
		})
	}

	if deps.AddComment != nil {
		_ = deps.AddComment(recoveryBeadID,
			fmt.Sprintf("cleric.finish: outcome=%s verb=%s failure_class=%s", outcomeValue, verb, failureClass))
	}

	if err := deps.CloseBead(recoveryBeadID); err != nil {
		return HandlerResult{Err: fmt.Errorf("cleric.finish: close recovery: %w", err)}
	}

	return HandlerResult{
		Outputs: map[string]string{
			"status":          "finished",
			"verb":            verb,
			"failure_class":   failureClass,
			"execute_success": executeSuccess,
		},
	}
}

// proposalTargetStep extracts the step name an action targets, when the
// action is a step-targeted verb. Used by the wizard observer to match
// a pending outcome to a specific source-bead step transition.
//
// Returns:
//   - For "reset --to <step>": the step arg if present in the proposal,
//     else empty.
//   - For "resummon": the source step the wizard last failed on, read
//     from the recovery bead's source_step / source_flow metadata when
//     present.
//   - For all other verbs: empty (the observer treats empty target_step
//     as "any next-step success counts").
func proposalTargetStep(bead store.Bead, verb string) string {
	switch verb {
	case "reset --to <step>":
		raw := bead.Meta(MetadataKeyProposal)
		if raw == "" {
			return ""
		}
		pa, err := ParseProposedAction([]byte(raw))
		if err != nil {
			return ""
		}
		return pa.Args["step"]
	case "resummon":
		return bead.Meta("source_step")
	}
	return ""
}

// SourceBeadID resolves the source (top-level) bead the recovery is
// recovering — the destination of the recovery bead's caused-by edge.
// Returns "" with no error when no caused-by edge exists, so callers
// can distinguish "no source" from a load failure.
func SourceBeadID(recoveryBeadID string, deps Deps) (string, error) {
	if deps.GetDepsWithMeta == nil {
		return "", fmt.Errorf("cleric: GetDepsWithMeta seam unwired")
	}
	depsList, err := deps.GetDepsWithMeta(recoveryBeadID)
	if err != nil {
		return "", err
	}
	for _, d := range depsList {
		if d == nil {
			continue
		}
		if string(d.DependencyType) == store.DepCausedBy {
			return d.ID, nil
		}
	}
	return "", nil
}

// HasOpenRecovery returns (true, recoveryID, nil) when sourceBeadID has
// a non-closed recovery bead linked via caused-by. Used by the wizard
// summon path to enforce the single-owner invariant — refuse to summon
// while a recovery is in flight.
//
// The query walks the source bead's dependents (reverse caused-by) and
// checks each one's status. A "non-closed" recovery is anything not in
// the closed status — covers in_progress, awaiting_review, hooked, and
// any future state the recovery lifecycle may grow.
func HasOpenRecovery(sourceBeadID string, getDependents func(id string) ([]*beads.IssueWithDependencyMetadata, error)) (bool, string, error) {
	if getDependents == nil {
		return false, "", fmt.Errorf("cleric: dependents seam unwired")
	}
	dependents, err := getDependents(sourceBeadID)
	if err != nil {
		return false, "", err
	}
	for _, d := range dependents {
		if d == nil {
			continue
		}
		if string(d.DependencyType) != store.DepCausedBy {
			continue
		}
		if string(d.IssueType) != "recovery" {
			// Non-recovery caused-by dependents (e.g. bug beads from the
			// post-merge sweep) don't gate wizard summon.
			continue
		}
		if string(d.Status) != StatusClosed {
			return true, d.ID, nil
		}
	}
	return false, "", nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// requireSeams returns a non-nil error when any of the required nil-able
// seams are nil. Funnels the per-handler nil-checks through one place so
// the error message identifies the handler and the missing seam isn't
// silently treated as "do nothing".
func requireSeams(deps Deps, handler string, seams ...interface{}) error {
	for _, s := range seams {
		if s == nil {
			return fmt.Errorf("cleric.%s: dependency seam unwired", handler)
		}
	}
	_ = deps // present for future per-field checks
	return nil
}
