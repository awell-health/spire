package main

import (
	"fmt"
	"os"

	"github.com/steveyegge/beads"
)

// wizardMessageArchmage sends a spire message to the archmage referencing the given bead.
// Errors are logged but do not block the caller.
func wizardMessageArchmage(from, beadID, message string) {
	labels := []string{"msg", "to:archmage", "from:" + from, "ref:" + beadID}
	if _, err := storeCreateBead(createOpts{
		Title:    message,
		Priority: 1,
		Type:     beads.TypeTask,
		Prefix:   "spi",
		Labels:   labels,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: message archmage: %s\n", err)
	}
}

// escalateHumanFailure handles a terminal step failure in the review DAG.
// It performs three actions:
//  1. Creates an alert bead (surfaces in ALERTS on spire board) with --ref and --type
//  2. Labels the bead needs-human so spire board surfaces it
//  3. Leaves the bead at its current phase — does NOT close it or delete branches
//
// Failure types: "merge-failure", "build-failure", "repo-resolution", "arbiter-failure"
func escalateHumanFailure(beadID, agentName, failureType, message string) {
	// Label needs-human so the board surfaces it in ALERTS.
	storeAddLabel(beadID, "needs-human")

	// Create an alert bead that surfaces at the top of the board.
	alertTitle := fmt.Sprintf("[%s] %s: %s", failureType, beadID, message)
	if len(alertTitle) > 200 {
		alertTitle = alertTitle[:200]
	}
	alertLabels := []string{"alert:" + failureType, "ref:" + beadID}
	if _, err := storeCreateBead(createOpts{
		Title:    alertTitle,
		Priority: 0,
		Type:     beads.TypeTask,
		Labels:   alertLabels,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: escalate alert: %s\n", err)
	}

	// Leave a comment on the bead so the history is clear.
	storeAddComment(beadID, fmt.Sprintf(
		"Escalated to archmage: %s — %s\nBranch and bead left intact for diagnosis.",
		failureType, message,
	))

	// Direct message to archmage.
	wizardMessageArchmage(agentName, beadID,
		fmt.Sprintf("Terminal failure on %s (%s): %s", beadID, failureType, message))
}
