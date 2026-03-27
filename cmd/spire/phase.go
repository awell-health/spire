package main

import (
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

// isValidPhase checks if a phase name is one of the 5 universal phases.
func isValidPhase(phase string) bool {
	for _, p := range validPhases {
		if p == phase {
			return true
		}
	}
	return false
}
