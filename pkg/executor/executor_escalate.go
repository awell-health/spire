package executor

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/steveyegge/beads"
)

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
	labels := []string{"msg", "to:archmage", "from:" + from}
	msgID, err := deps.CreateBead(CreateOpts{
		Title:    message,
		Priority: 1,
		Type:     beads.TypeTask,
		Prefix:   "spi",
		Labels:   labels,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: message archmage: %s\n", err)
		return
	}

	// Link message to bead via related dep (not ref: label).
	if msgID != "" && deps.AddDepTyped != nil {
		if derr := deps.AddDepTyped(msgID, beadID, "related"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: add related dep %s→%s: %s\n", msgID, beadID, derr)
		}
	}
}

// EscalateEmptyImplement handles the case where an apprentice completes the
// implement phase but produces no code changes. Instead of advancing to
// review (which would review nothing), it escalates immediately.
//
// Actions:
//  1. Labels the bead needs-human
//  2. Creates an alert bead linked via a "caused-by" dep (not ref: label)
//  3. Adds a comment explaining what happened
//  4. Messages the archmage
//
// The bead stays at the implement phase so it can be resummon'd after the user
// provides better context (design bead, improved description, etc.).
func EscalateEmptyImplement(beadID, agentName string, deps *Deps) {
	deps.AddLabel(beadID, "needs-human")
	deps.AddLabel(beadID, "interrupted:empty-implement")

	alertTitle := fmt.Sprintf("[empty-implement] %s: apprentice produced no code changes", beadID)
	if len(alertTitle) > 200 {
		alertTitle = alertTitle[:200]
	}
	alertLabels := []string{"alert:empty-implement"}
	alertID, err := deps.CreateBead(CreateOpts{
		Title:    alertTitle,
		Priority: 0,
		Type:     beads.TypeTask,
		Labels:   alertLabels,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: escalate empty-implement alert: %s\n", err)
	}

	// Link alert to source bead via caused-by dep so closing the source
	// bead can cascade-close this alert automatically.
	if alertID != "" && deps.AddDepTyped != nil {
		if derr := deps.AddDepTyped(alertID, beadID, "caused-by"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: add caused-by dep %s→%s: %s\n", alertID, beadID, derr)
		}
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
//  2. Labels the bead needs-human so spire board surfaces it
//  3. Leaves the bead at its current phase
//
// Failure types: "merge-failure", "build-failure", "repo-resolution", "arbiter-failure", "review-fix-merge-conflict"
func EscalateHumanFailure(beadID, agentName, failureType, message string, deps *Deps) {
	// Label needs-human so the board surfaces it in ALERTS.
	deps.AddLabel(beadID, "needs-human")
	// Explicit interrupted signal with failure type for board consumption.
	// This is separate from needs-human so design approval gates (which use
	// needs-human alone) are not confused with interrupted/error states.
	deps.AddLabel(beadID, "interrupted:"+failureType)

	// Create an alert bead that surfaces at the top of the board.
	alertTitle := fmt.Sprintf("[%s] %s: %s", failureType, beadID, message)
	if len(alertTitle) > 200 {
		alertTitle = alertTitle[:200]
	}
	alertLabels := []string{"alert:" + failureType}
	alertID, err := deps.CreateBead(CreateOpts{
		Title:    alertTitle,
		Priority: 0,
		Type:     beads.TypeTask,
		Labels:   alertLabels,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: escalate alert: %s\n", err)
	}

	// Link alert to source bead via caused-by dep so closing the source
	// bead can cascade-close this alert automatically.
	if alertID != "" && deps.AddDepTyped != nil {
		if derr := deps.AddDepTyped(alertID, beadID, "caused-by"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: add caused-by dep %s→%s: %s\n", alertID, beadID, derr)
		}
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
	// Labels: same as EscalateHumanFailure — interrupt type is still the classification key.
	deps.AddLabel(beadID, "needs-human")
	deps.AddLabel(beadID, "interrupted:"+failureType)

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
	if len(alertTitle) > 200 {
		alertTitle = alertTitle[:200]
	}
	alertLabels := []string{"alert:" + failureType}
	alertID, err := deps.CreateBead(CreateOpts{
		Title:    alertTitle,
		Priority: 0,
		Type:     beads.TypeTask,
		Labels:   alertLabels,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: escalate alert: %s\n", err)
	}

	if alertID != "" && deps.AddDepTyped != nil {
		if derr := deps.AddDepTyped(alertID, beadID, "caused-by"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: add caused-by dep %s→%s: %s\n", alertID, beadID, derr)
		}
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
	ops := executorBeadOps{deps}

	// Failure-class-scoped dedupe.
	existingID, found, err := recovery.DedupeRecoveryBead(ops, parentID, failureType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: dedupe recovery bead for %s: %s\n", parentID, err)
	}
	if found {
		// Append full context comment to existing recovery bead.
		ctx := buildRecoveryComment(parentID, agentName, failureType, message, nodeCtx)
		deps.AddComment(existingID, ctx)
		return
	}

	// Build description with structured metadata block.
	stepName := ""
	if nodeCtx != "" {
		// Extract step name from nodeCtx ("step=foo action=bar ...").
		for _, part := range strings.Fields(nodeCtx) {
			if strings.HasPrefix(part, "step=") {
				stepName = strings.TrimPrefix(part, "step=")
				break
			}
		}
	}
	desc := fmt.Sprintf(
		"failure_class: %s\nsource_bead: %s\nsource_step: %s\n\n%s",
		failureType, parentID, stepName, message,
	)
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
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: create recovery bead for %s: %s\n", parentID, err)
		return
	}

	// Link via caused-by dep.
	if recoveryID != "" && deps.AddDepTyped != nil {
		if derr := deps.AddDepTyped(recoveryID, parentID, "caused-by"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: add caused-by dep %s→%s: %s\n", recoveryID, parentID, derr)
		}
	}

	// Seed with context comment.
	ctx := buildRecoveryComment(parentID, agentName, failureType, message, nodeCtx)
	deps.AddComment(recoveryID, ctx)
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
