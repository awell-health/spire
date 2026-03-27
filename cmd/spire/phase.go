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
// When phase is "review", annotates with the round count from review child beads.
func getBoardBeadPhase(b BoardBead) string {
	// Primary: check for active step bead.
	if step, err := storeGetActiveStep(b.ID); err == nil && step != nil {
		if name := stepBeadPhaseName(*step); name != "" {
			if name == "review" {
				return reviewPhaseLabel(b.ID)
			}
			return name
		}
	}
	// Fallback: phase: label.
	phase := ""
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "phase:") {
			phase = l[len("phase:"):]
		}
	}
	if phase == "review" {
		return reviewPhaseLabel(b.ID)
	}
	return phase
}

// reviewPhaseLabel returns "review rN" if N review child beads exist, else "review".
func reviewPhaseLabel(id string) string {
	reviews, err := storeGetReviewBeads(id)
	if err != nil || len(reviews) == 0 {
		return "review"
	}
	n := 0
	for _, r := range reviews {
		if rn := reviewRoundNumber(r); rn > n {
			n = rn
		}
	}
	if n == 0 {
		n = len(reviews)
	}
	return fmt.Sprintf("review r%d", n)
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
