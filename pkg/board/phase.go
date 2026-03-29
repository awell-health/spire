package board

import (
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/store"
)

// GetPhase returns the current phase of a bead.
// Checks for an active step bead first (primary), then falls back to phase:X label.
// Returns "" if neither source indicates a phase (treated as READY by callers).
func GetPhase(b Bead) string {
	if step, err := store.GetActiveStep(b.ID); err == nil && step != nil {
		if name := store.StepBeadPhaseName(*step); name != "" {
			return name
		}
	}
	return store.HasLabel(b, "phase:")
}

// GetBoardBeadPhase returns the current phase of a BoardBead.
// Checks for an active step bead first (primary), then falls back to phase:X label.
// When phase is "review", annotates with the round count from review child beads.
func GetBoardBeadPhase(b BoardBead) string {
	if step, err := store.GetActiveStep(b.ID); err == nil && step != nil {
		if name := store.StepBeadPhaseName(*step); name != "" {
			if name == "review" {
				return reviewPhaseLabel(b.ID)
			}
			return name
		}
	}
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

// GetBoardBeadPhaseFromChildren returns the phase of a BoardBead using a pre-fetched
// children slice instead of making DB calls. Same logic as GetBoardBeadPhase.
func GetBoardBeadPhaseFromChildren(b BoardBead, children []store.Bead) string {
	// Find the active step bead from children (replicates store.GetActiveStep logic).
	var activeStep *store.Bead
	for i := range children {
		child := &children[i]
		if child.Status != "in_progress" {
			continue
		}
		if !store.IsStepBead(*child) {
			continue
		}
		activeStep = child
		break
	}

	if activeStep != nil {
		if name := store.StepBeadPhaseName(*activeStep); name != "" {
			if name == "review" {
				return reviewPhaseLabelFromChildren(children)
			}
			return name
		}
	}

	// Fall back to phase:X label on the bead itself.
	phase := ""
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "phase:") {
			phase = l[len("phase:"):]
		}
	}
	if phase == "review" {
		return reviewPhaseLabelFromChildren(children)
	}
	return phase
}

// reviewPhaseLabelFromChildren derives the review sub-label from a pre-fetched children
// slice. Same logic as reviewPhaseLabel but without DB calls.
func reviewPhaseLabelFromChildren(children []store.Bead) string {
	var reviews []store.Bead
	for _, child := range children {
		if store.IsReviewRoundBead(child) {
			reviews = append(reviews, child)
		}
	}
	if len(reviews) == 0 {
		return "review"
	}
	n := 0
	for _, r := range reviews {
		if rn := store.ReviewRoundNumber(r); rn > n {
			n = rn
		}
	}
	if n == 0 {
		n = len(reviews)
	}
	return fmt.Sprintf("review r%d", n)
}

// DerivePhaseMap computes phase strings for a batch of beads using pre-fetched children.
// Returns a map of beadID to phase string. Beads with no children entry get empty string.
func DerivePhaseMap(beads []BoardBead, childrenMap map[string][]store.Bead) map[string]string {
	m := make(map[string]string, len(beads))
	for _, b := range beads {
		children, ok := childrenMap[b.ID]
		if !ok {
			m[b.ID] = ""
			continue
		}
		m[b.ID] = GetBoardBeadPhaseFromChildren(b, children)
	}
	return m
}

// reviewPhaseLabel returns "review rN" if N review child beads exist, else "review".
func reviewPhaseLabel(id string) string {
	children, err := store.GetChildren(id)
	if err != nil {
		return "review"
	}
	var reviews []Bead
	for _, child := range children {
		if store.IsReviewRoundBead(child) {
			reviews = append(reviews, child)
		}
	}
	if len(reviews) == 0 {
		return "review"
	}
	n := 0
	for _, r := range reviews {
		if rn := store.ReviewRoundNumber(r); rn > n {
			n = rn
		}
	}
	if n == 0 {
		n = len(reviews)
	}
	return fmt.Sprintf("review r%d", n)
}
