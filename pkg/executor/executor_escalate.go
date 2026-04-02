package executor

import (
	"fmt"
	"os"

	"github.com/steveyegge/beads"
)

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
//  2. Creates an alert bead linked via a "related" dep (not ref: label)
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

	// Link alert to bead via related dep (not ref: label).
	if alertID != "" && deps.AddDepTyped != nil {
		if derr := deps.AddDepTyped(alertID, beadID, "related"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: add related dep %s→%s: %s\n", alertID, beadID, derr)
		}
	}

	deps.AddComment(beadID, fmt.Sprintf(
		"Apprentice produced no code changes during implement phase.\n"+
			"Bead left at implement for retry. Add a design bead, improve the description, or provide more context, then resummon.",
	))

	MessageArchmage(agentName, beadID,
		fmt.Sprintf("Empty implement on %s: apprentice produced no code changes — needs human guidance", beadID),
		deps)
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
}
