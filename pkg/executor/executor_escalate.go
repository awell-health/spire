package executor

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/alerts"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// depsBeadOps adapts *Deps to alerts.BeadOps so Raise can be called with
// the executor's existing dependency surface.
type depsBeadOps struct {
	deps *Deps
}

func (a depsBeadOps) CreateBead(opts store.CreateOpts) (string, error) {
	return a.deps.CreateBead(opts)
}

func (a depsBeadOps) AddDepTyped(from, to, depType string) error {
	if a.deps.AddDepTyped == nil {
		return nil
	}
	return a.deps.AddDepTyped(from, to, depType)
}

// isRecoveryBead returns true if the bead is itself a recovery bead,
// used as a circuit breaker to prevent cascading escalations.
func isRecoveryBead(beadID string, deps *Deps) bool {
	if deps.GetBead == nil {
		return false
	}
	b, err := deps.GetBead(beadID)
	if err != nil {
		return false
	}
	if b.Type == "recovery" {
		return true
	}
	for _, l := range b.Labels {
		if l == "recovery-bead" {
			return true
		}
	}
	return false
}

// DefaultClericRetryCap is the default upper bound on consecutive
// cleric escalation failures a single recovery bead may suppress
// before the bead is closed and labeled `needs-human`. Without a
// bound, a broken cleric (e.g. one whose stdout fails to parse on
// every run) keeps the recovery bead open forever; the steward then
// dispatches a fresh cleric on every tick — burning an agent slot
// indefinitely. spi-9eopwy / spi-1u84ec.
//
// The cap is intentionally high so genuinely-recoverable failures
// (transient model flakes, network blips) get plenty of cleric
// attempts before flipping to needs-human. At a 10s steward poll
// cadence, 25 retries = ~4 minutes of recovery budget — long enough
// for real recovery to land, short enough that pathological loops
// self-cap.
//
// Operators can override per-tower via TowerConfig.ClericRetryCap or
// per-process via SPIRE_CLERIC_RETRY_CAP.
const DefaultClericRetryCap = 25

// LabelClericRetry is the label prefix the recovery bead carries to
// track suppressed escalations across cleric restarts. Persisted on
// the bead because in-memory counters reset every steward restart.
const LabelClericRetry = "cleric-retry:"

// effectiveClericRetryCap returns deps.ClericRetryCap when set to a
// positive value, falling back to DefaultClericRetryCap. 0 (zero
// value of the JSON field) and negative values both resolve to the
// default — operators tune the cap by setting a positive integer,
// not by clearing the field.
func effectiveClericRetryCap(deps *Deps) int {
	if deps != nil && deps.ClericRetryCap > 0 {
		return deps.ClericRetryCap
	}
	return DefaultClericRetryCap
}

// suppressRecoveryEscalation handles the recovery-bead branch of an
// escalation: it adds the cascade-prevention comment, and persistently
// counts the suppression. Once the count reaches the effective cap
// (DefaultClericRetryCap, or deps.ClericRetryCap when set), the bead
// is labeled `needs-human` and closed so the steward stops dispatching
// against it. Returns true so the caller short-circuits the rest of
// its escalation logic (createOrUpdateRecoveryBead, etc.) the same way
// the original guard did.
func suppressRecoveryEscalation(beadID, failureType, message string, deps *Deps) bool {
	if !isRecoveryBead(beadID, deps) {
		return false
	}
	if deps.AddComment != nil {
		deps.AddComment(beadID, fmt.Sprintf(
			"Failure on recovery bead (%s): %s — escalation suppressed to prevent cascade.",
			failureType, message))
	}

	// Read the existing retry count from the bead's labels. Missing label
	// = first failure (count was 0 before this one).
	count := 0
	prevLabel := ""
	if deps.GetBead != nil {
		if b, err := deps.GetBead(beadID); err == nil {
			if val := store.HasLabel(b, LabelClericRetry); val != "" {
				fmt.Sscanf(val, "%d", &count)
				prevLabel = LabelClericRetry + val
			}
		}
	}
	count++

	newLabel := fmt.Sprintf("%s%d", LabelClericRetry, count)
	if prevLabel != "" && deps.RemoveLabel != nil {
		// Best-effort: stale label is harmless because HasLabel returns
		// the first match; the new one will sort first if added second
		// only if labels are alphabetical, so remove explicitly.
		_ = deps.RemoveLabel(beadID, prevLabel)
	}
	if deps.AddLabel != nil {
		_ = deps.AddLabel(beadID, newLabel)
	}

	cap := effectiveClericRetryCap(deps)
	if count >= cap {
		if deps.AddLabel != nil {
			_ = deps.AddLabel(beadID, "needs-human")
		}
		if deps.AddComment != nil {
			deps.AddComment(beadID, fmt.Sprintf(
				"cleric-retry budget exhausted (%d/%d failures): closing recovery bead and labeling `needs-human`. "+
					"The repeated failure was: %s — %s",
				count, cap, failureType, message))
		}
		if deps.CloseBead != nil {
			if err := deps.CloseBead(beadID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: close exhausted recovery bead %s: %s\n", beadID, err)
			}
		}
	}
	return true
}

// executorBeadOps adapts executor.Deps to recovery.BeadOps.
type executorBeadOps struct {
	deps *Deps
}

func (o executorBeadOps) GetDependentsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	if o.deps.GetDependentsWithMeta == nil {
		return nil, nil
	}
	return o.deps.GetDependentsWithMeta(id)
}

func (o executorBeadOps) AddComment(id, text string) error {
	if o.deps.AddComment == nil {
		return nil
	}
	return o.deps.AddComment(id, text)
}

func (o executorBeadOps) CloseBead(id string) error {
	if o.deps.CloseBead == nil {
		return nil
	}
	return o.deps.CloseBead(id)
}

// MessageArchmage sends a spire message to the archmage referencing the given bead.
// Errors are logged but do not block the caller.
func MessageArchmage(from, beadID, message string, deps *Deps) {
	if _, err := alerts.Raise(depsBeadOps{deps}, beadID, alerts.ClassArchmageMsg, message,
		alerts.WithFrom(from)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: message archmage: %s\n", err)
	}
}

// EscalateEmptyImplement handles the case where an apprentice completes the
// implement phase but produces no code changes. Instead of advancing to
// review (which would review nothing), it escalates immediately.
//
// Actions:
//  1. Creates an alert bead linked via a "caused-by" dep (not ref: label)
//  2. Adds a comment explaining what happened
//  3. Messages the archmage
//
// The bead stays at the implement phase so it can be resummon'd after the user
// provides better context (design bead, improved description, etc.).
func EscalateEmptyImplement(beadID, agentName string, deps *Deps) {
	// Circuit breaker: don't cascade if this is already a recovery bead.
	// Bounded retry: after the cleric-retry cap (DefaultClericRetryCap or
	// the operator's tower override), the recovery bead is closed and
	// labeled `needs-human` so the steward stops re-dispatching against
	// a stuck loop.
	if suppressRecoveryEscalation(beadID, "empty-implement", "apprentice produced no code changes", deps) {
		return
	}

	alertTitle := fmt.Sprintf("[empty-implement] %s: apprentice produced no code changes", beadID)
	if _, err := alerts.Raise(depsBeadOps{deps}, beadID, alerts.ClassAlert, alertTitle,
		alerts.WithSubclass("empty-implement")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: escalate empty-implement alert: %s\n", err)
	}

	deps.AddComment(beadID, fmt.Sprintf(
		"Apprentice produced no code changes during implement phase.\n"+
			"Bead left at implement for retry. Add a design bead, improve the description, or provide more context, then resummon.",
	))

	MessageArchmage(agentName, beadID,
		fmt.Sprintf("Empty implement on %s: apprentice produced no code changes — needs human guidance", beadID),
		deps)

	createOrUpdateRecoveryBead(beadID, agentName, "empty-implement", "apprentice produced no code changes", "", deps)
}

// EscalateHumanFailure handles a terminal step failure in the review DAG.
// It performs three actions:
//  1. Creates an alert bead (surfaces in ALERTS on spire board)
//  2. Adds a comment and messages the archmage
//  3. Leaves the bead at its current phase
//
// Failure types: "merge-failure", "build-failure", "repo-resolution", "arbiter-failure", "review-fix-merge-conflict"
func EscalateHumanFailure(beadID, agentName, failureType, message string, deps *Deps) {
	// Circuit breaker: don't cascade if this is already a recovery bead.
	// Bounded retry: after the cleric-retry cap (DefaultClericRetryCap or
	// the operator's tower override), the recovery bead is closed and
	// labeled `needs-human` so the steward stops re-dispatching against
	// a stuck loop.
	if suppressRecoveryEscalation(beadID, failureType, message, deps) {
		return
	}

	// Create an alert bead that surfaces at the top of the board.
	alertTitle := fmt.Sprintf("[%s] %s: %s", failureType, beadID, message)
	if _, err := alerts.Raise(depsBeadOps{deps}, beadID, alerts.ClassAlert, alertTitle,
		alerts.WithSubclass(failureType)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: escalate alert: %s\n", err)
	}

	// Leave a comment on the bead so the history is clear.
	deps.AddComment(beadID, fmt.Sprintf(
		"Escalated to archmage: %s — %s\nBranch and bead left intact for diagnosis.",
		failureType, message,
	))

	// Direct message to archmage.
	MessageArchmage(agentName, beadID,
		fmt.Sprintf("Terminal failure on %s (%s): %s", beadID, failureType, message),
		deps)

	createOrUpdateRecoveryBead(beadID, agentName, failureType, message, "", deps)
}

// EscalateGraphStepFailure is the v3-aware variant of EscalateHumanFailure.
// It includes step-scoped metadata (step name, action, flow, workspace) in
// the interruption label, alert title, comment, and message.
func EscalateGraphStepFailure(beadID, agentName, failureType, message string, stepName, action, flow, workspace string, deps *Deps) {
	// Circuit breaker: don't cascade if this is already a recovery bead.
	// Bounded retry: after the cleric-retry cap (DefaultClericRetryCap or
	// the operator's tower override), the recovery bead is closed and
	// labeled `needs-human` so the steward stops re-dispatching against
	// a stuck loop (spi-9eopwy / spi-1u84ec).
	if suppressRecoveryEscalation(beadID, failureType, message, deps) {
		return
	}

	// Build node-scoped context string.
	var ctx []string
	if stepName != "" {
		ctx = append(ctx, "step="+stepName)
	}
	if action != "" {
		ctx = append(ctx, "action="+action)
	}
	if flow != "" {
		ctx = append(ctx, "flow="+flow)
	}
	if workspace != "" {
		ctx = append(ctx, "workspace="+workspace)
	}
	stepCtx := strings.Join(ctx, " ")

	// Alert title includes node context.
	alertTitle := fmt.Sprintf("[%s] %s: %s (%s)", failureType, beadID, message, stepCtx)
	if _, err := alerts.Raise(depsBeadOps{deps}, beadID, alerts.ClassAlert, alertTitle,
		alerts.WithSubclass(failureType)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: escalate alert: %s\n", err)
	}

	// Comment uses node-scoped wording.
	deps.AddComment(beadID, fmt.Sprintf(
		"Escalated to archmage: %s — %s\nNode context: %s\nBranch and bead left intact for diagnosis.",
		failureType, message, stepCtx,
	))

	MessageArchmage(agentName, beadID,
		fmt.Sprintf("Terminal failure on %s (%s) at %s: %s", beadID, failureType, stepCtx, message),
		deps)

	createOrUpdateRecoveryBead(beadID, agentName, failureType, message, stepCtx, deps)
}

// createOrUpdateRecoveryBead creates a first-class type=recovery bead for an
// interrupted parent bead. Dedupe is failure-class-scoped: if an open recovery
// bead already exists for parentID with the same failure_class label, the
// existing bead is updated with a new incident comment instead of creating a
// duplicate. New beads are linked via a caused-by dep (replacing the legacy
// recovery-for dep). Both new and legacy links are recognized during dedupe
// for backward compatibility.
func createOrUpdateRecoveryBead(parentID, agentName, failureType, message, nodeCtx string, deps *Deps) {
	// Check for relapse: if a prior "clean" recovery exists for this source +
	// failure class within 24h, mark it as relapsed before proceeding.
	checkAndMarkRelapse(parentID, failureType, deps)

	ops := executorBeadOps{deps}

	// Failure-class-scoped dedupe.
	existingID, found, err := recovery.DedupeRecoveryBead(ops, parentID, failureType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: dedupe recovery bead for %s: %s\n", parentID, err)
	}
	if found {
		// Re-seed structured metadata idempotently. Safe because
		// store.SetBeadMetadataMap merges into existing metadata (read-modify-write),
		// so fields set by later lifecycle phases (resolution_kind, verification_status, etc.)
		// are preserved.
		seedRecoveryMetadata(existingID, parentID, failureType, nodeCtx)
		// Append full context comment to existing recovery bead.
		ctx := buildRecoveryComment(parentID, agentName, failureType, message, nodeCtx)
		deps.AddComment(existingID, ctx)
		return
	}

	// Description is the human-readable narrative only. Structured fields
	// (failure_class, source_bead, source_step) are written to metadata by
	// seedRecoveryMetadata — no need to duplicate them here.
	desc := message
	if nodeCtx != "" {
		desc += "\nContext: " + nodeCtx
	}

	// Create type=recovery bead.
	title := fmt.Sprintf("[recovery] %s: %s", parentID, failureType)
	if len(title) > 200 {
		title = title[:200]
	}
	recoveryID, err := deps.CreateBead(CreateOpts{
		Title:       title,
		Priority:    1,
		Type:        beads.IssueType("recovery"),
		Labels:      []string{"recovery-bead", "failure_class:" + failureType},
		Description: desc,
		Prefix:      store.PrefixFromID(parentID),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: create recovery bead for %s: %s\n", parentID, err)
		return
	}

	// Link via caused-by dep to the source TOP-level bead. Cleric foundation
	// (spi-h2d7yn) constrains caused-by to point at the wizard's source bead,
	// not the failed step bead — clerics reason about the overall task, not
	// the step in isolation.
	if recoveryID != "" && deps.AddDepTyped != nil {
		if derr := deps.AddDepTyped(recoveryID, parentID, "caused-by"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: add caused-by dep %s→%s: %s\n", recoveryID, parentID, derr)
		}
	}

	// Link via related dep to the most-recent peer recovery bead on the same
	// source (if any). The cleric reads the chain through `related` to load
	// prior proposals, rejections, takeovers, and outcomes as context. We
	// link only to the most-recent peer; traversal across older peers is
	// linear via successive related edges.
	if recoveryID != "" && deps.AddDepTyped != nil {
		if peerID := mostRecentPeerRecovery(parentID, recoveryID); peerID != "" {
			if derr := deps.AddDepTyped(recoveryID, peerID, "related"); derr != nil {
				fmt.Fprintf(os.Stderr, "warning: add related dep %s→%s: %s\n", recoveryID, peerID, derr)
			}
		}
	}

	// Seed structured metadata (separate call — CreateOpts has no Metadata field).
	seedRecoveryMetadata(recoveryID, parentID, failureType, nodeCtx)

	// Seed with context comment.
	ctx := buildRecoveryComment(parentID, agentName, failureType, message, nodeCtx)
	deps.AddComment(recoveryID, ctx)
}

// executorRecoveryDeps adapts executor.Deps to recovery.RecoveryDeps for use
// by the document/finish/verify recovery lifecycle functions.
type executorRecoveryDeps struct {
	deps *Deps
}

func (d executorRecoveryDeps) GetBead(id string) (recovery.DepBead, error) {
	b, err := d.deps.GetBead(id)
	if err != nil {
		return recovery.DepBead{}, err
	}
	return recovery.DepBead{
		ID:     b.ID,
		Title:  b.Title,
		Status: b.Status,
		Labels: b.Labels,
		Parent: b.Parent,
	}, nil
}

func (d executorRecoveryDeps) GetDependentsWithMeta(id string) ([]recovery.DepDependent, error) {
	if d.deps.GetDependentsWithMeta == nil {
		return nil, nil
	}
	items, err := d.deps.GetDependentsWithMeta(id)
	if err != nil {
		return nil, err
	}
	result := make([]recovery.DepDependent, len(items))
	for i, item := range items {
		result[i] = recovery.DepDependent{
			ID:             item.ID,
			Title:          item.Title,
			Status:         string(item.Status),
			Labels:         item.Labels,
			DependencyType: string(item.DependencyType),
		}
	}
	return result, nil
}

func (d executorRecoveryDeps) UpdateBead(id string, meta map[string]interface{}) error {
	if d.deps.UpdateBead == nil {
		return nil
	}
	return d.deps.UpdateBead(id, meta)
}

func (d executorRecoveryDeps) AddComment(id, text string) error {
	if d.deps.AddComment == nil {
		return nil
	}
	return d.deps.AddComment(id, text)
}

func (d executorRecoveryDeps) CloseBead(id string) error {
	if d.deps.CloseBead == nil {
		return nil
	}
	return d.deps.CloseBead(id)
}

// recoveryDepsFromExecutor wraps executor Deps into a recovery.RecoveryDeps.
func recoveryDepsFromExecutor(deps *Deps) recovery.RecoveryDeps {
	return executorRecoveryDeps{deps: deps}
}

// DocumentRecovery is the executor-side entry point for writing durable
// recovery learning metadata and narrative onto a recovery bead.
// Called from formula phase dispatch at the document phase.
func DocumentRecovery(beadID string, learning recovery.RecoveryLearning, deps *Deps) error {
	rd := recoveryDepsFromExecutor(deps)
	return recovery.DocumentLearning(rd, beadID, learning)
}

// ExecutorFinishRecovery is the executor-side entry point for finalizing a
// recovery bead: documents the learning, adds a close comment, and closes
// the bead. Called from formula phase dispatch at the finish phase.
func ExecutorFinishRecovery(beadID string, learning recovery.RecoveryLearning, deps *Deps) error {
	rd := recoveryDepsFromExecutor(deps)
	return recovery.FinishRecovery(rd, beadID, learning)
}

// buildSeedMetadata constructs the RecoveryMetadata to seed on a recovery bead
// from the available escalation context. Pure logic — no side effects.
func buildSeedMetadata(parentID, failureType, nodeCtx string) recovery.RecoveryMetadata {
	stepName := ""
	flowName := ""
	if nodeCtx != "" {
		for _, part := range strings.Fields(nodeCtx) {
			if strings.HasPrefix(part, "step=") {
				stepName = strings.TrimPrefix(part, "step=")
			}
			if strings.HasPrefix(part, "flow=") {
				flowName = strings.TrimPrefix(part, "flow=")
			}
		}
	}

	// Build a stable failure signature from available context.
	sig := failureType
	if stepName != "" {
		sig = failureType + ":" + stepName
	}

	return recovery.RecoveryMetadata{
		FailureClass:     failureType,
		SourceBead:       parentID,
		SourceStep:       stepName,
		SourceFlow:       flowName,
		FailureSignature: sig,
		// TODO(source_formula): The Escalate* functions receive deps but not
		// the executor's formula reference. To populate SourceFormula, either
		// thread the formula name through the Escalate call chain or add a
		// FormulaName field to Deps. Until then, source_formula is seeded
		// empty. The lookup surface (GetRecoveryLearnings, FindMatchingLearning)
		// currently filters by source_bead + failure_class, not source_formula,
		// so this gap does not affect present queries.
	}
}

// seedRecoveryMetadata writes the structured source-context fields onto a
// recovery bead's issue metadata via store.SetBeadMetadataMap (which merges
// into existing metadata, preserving fields set by later lifecycle phases).
// Errors are logged but non-fatal — the bead is still useful even without
// all metadata.
func seedRecoveryMetadata(recoveryID, parentID, failureType, nodeCtx string) {
	if recoveryID == "" {
		return
	}
	meta := buildSeedMetadata(parentID, failureType, nodeCtx)
	if err := meta.Apply(recoveryID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: seed recovery metadata on %s: %s\n", recoveryID, err)
	}
}

// mostRecentPeerRecovery returns the ID of the most recently created recovery
// bead caused-by sourceBeadID, excluding excludeID (the bead we're currently
// creating, which would otherwise self-reference). Returns "" if no peer
// exists. Cleric foundation (spi-h2d7yn) uses this to link a fresh recovery
// to its predecessor via a `related` dep so the cleric can walk the history
// of prior proposals, rejections, takeovers, and outcomes.
func mostRecentPeerRecovery(sourceBeadID, excludeID string) string {
	peers, err := recovery.PeerRecoveries(sourceBeadID)
	if err != nil || len(peers) == 0 {
		return ""
	}
	// peers are sorted most-recent first by PeerRecoveries.
	for _, p := range peers {
		if p.ID == excludeID {
			continue
		}
		return p.ID
	}
	return ""
}

func buildRecoveryComment(parentID, agentName, failureType, message, nodeCtx string) string {
	s := fmt.Sprintf(
		"Recovery work surface for interrupted bead %s.\n"+
			"Failure: %s\nMessage: %s\nAgent: %s\nTime: %s",
		parentID, failureType, message, agentName,
		time.Now().UTC().Format(time.RFC3339),
	)
	if nodeCtx != "" {
		s += "\nContext: " + nodeCtx
	}
	s += fmt.Sprintf("\n\nOperate on %s (not this bead) for resummon/reset.", parentID)
	return s
}
