package main

import (
	"fmt"
	"strings"
)

// validPhases lists the 5 universal phases in order.
var validPhases = []string{"design", "plan", "implement", "review", "merge"}

// getPhase returns the current phase of a bead by reading its phase:X label.
// Returns "" if the bead has no phase label (treated as READY by callers).
func getPhase(b Bead) string {
	return hasLabel(b, "phase:")
}

// getBoardBeadPhase returns the current phase of a BoardBead by reading its phase:X label.
// When phase is "review" and a review-round:N label is present, returns "review rN".
func getBoardBeadPhase(b BoardBead) string {
	phase := ""
	round := ""
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "phase:") {
			phase = l[len("phase:"):]
		}
		if strings.HasPrefix(l, "review-round:") {
			round = l[len("review-round:"):]
		}
	}
	if phase == "review" && round != "" {
		return "review r" + round
	}
	return phase
}

// setPhase atomically transitions a bead to a new phase.
// Removes the old phase: label (if any) and adds the new one.
// Idempotent: if the bead is already in the target phase, this is a no-op.
func setPhase(beadID, phase string) error {
	// Validate phase
	if !isValidPhase(phase) {
		return fmt.Errorf("invalid phase %q (valid: %v)", phase, validPhases)
	}

	// Get current bead to find existing phase label
	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("setPhase %s: %w", beadID, err)
	}

	currentPhase := getPhase(bead)
	if currentPhase == phase {
		return nil // already in target phase
	}

	// Remove old phase label if present
	if currentPhase != "" {
		if err := storeRemoveLabel(beadID, "phase:"+currentPhase); err != nil {
			return fmt.Errorf("setPhase remove old: %w", err)
		}
	}

	// Add new phase label
	if err := storeAddLabel(beadID, "phase:"+phase); err != nil {
		return fmt.Errorf("setPhase add new: %w", err)
	}

	return nil
}

// isValidPhase checks if a phase name is one of the 5 universal phases.
func isValidPhase(phase string) bool {
	for _, p := range validPhases {
		if p == phase {
			return true
		}
	}
	return false
}
