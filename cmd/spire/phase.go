package main

import (
	"fmt"
	"strings"
)

// validPhases lists the 5 universal phases in order.
var validPhases = []string{"design", "plan", "implement", "review", "merge"}

// getPhase returns the current phase of a bead.
// Checks for an active step bead first (primary), then falls back to phase:X label.
// Returns "" if neither source indicates a phase (treated as READY by callers).
func getPhase(b Bead) string {
	// Primary: check for active step bead.
	if step, err := storeGetActiveStep(b.ID); err == nil && step != nil {
		if name := stepBeadPhaseName(*step); name != "" {
			return name
		}
	}
	// Fallback: phase: label.
	return hasLabel(b, "phase:")
}

// getBoardBeadPhase returns the current phase of a BoardBead.
// Checks for an active step bead first (primary), then falls back to phase:X label.
// When phase is "review" and a review-round:N label is present, returns "review rN".
func getBoardBeadPhase(b BoardBead) string {
	// Primary: check for active step bead.
	if step, err := storeGetActiveStep(b.ID); err == nil && step != nil {
		if name := stepBeadPhaseName(*step); name != "" {
			phase := name
			// Preserve review round annotation.
			if phase == "review" {
				for _, l := range b.Labels {
					if strings.HasPrefix(l, "review-round:") {
						return "review r" + l[len("review-round:"):]
					}
				}
			}
			return phase
		}
	}
	// Fallback: phase: label.
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
