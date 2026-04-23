package intent

// Phase classifier tests (spi-12rno4).
//
// Pins the routing seam between steward and operator: every phase
// string the system uses must be classified into exactly one of
// bead-level / step-level / review-level. Misclassification means the
// operator builds the wrong pod shape.

import "testing"

func TestPhaseClassification(t *testing.T) {
	cases := []struct {
		phase string
		bead  bool
		step  bool
		review bool
	}{
		// Bead-level
		{PhaseWizard, true, false, false},
		{"task", true, false, false},
		{"bug", true, false, false},
		{"epic", true, false, false},
		{"feature", true, false, false},
		{"chore", true, false, false},

		// Step-level
		{PhaseImplement, false, true, false},
		{PhaseFix, false, true, false},

		// Review-level
		{PhaseReview, false, false, true},
		{PhaseArbiter, false, false, true},

		// Unknown / unclassified
		{"", false, false, false},
		{"unknown-phase", false, false, false},
		{"merge", false, false, false},
		{"plan", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			if got := IsBeadLevelPhase(tc.phase); got != tc.bead {
				t.Errorf("IsBeadLevelPhase(%q) = %v, want %v", tc.phase, got, tc.bead)
			}
			if got := IsStepLevelPhase(tc.phase); got != tc.step {
				t.Errorf("IsStepLevelPhase(%q) = %v, want %v", tc.phase, got, tc.step)
			}
			if got := IsReviewLevelPhase(tc.phase); got != tc.review {
				t.Errorf("IsReviewLevelPhase(%q) = %v, want %v", tc.phase, got, tc.review)
			}
		})
	}
}

// TestPhaseClassification_Disjoint asserts that no phase falls into
// more than one classifier. The operator's routing switch depends on
// disjointness — overlap would mean two pod builders compete for one
// intent.
func TestPhaseClassification_Disjoint(t *testing.T) {
	all := []string{
		PhaseWizard, "task", "bug", "epic", "feature", "chore",
		PhaseImplement, PhaseFix,
		PhaseReview, PhaseArbiter,
	}
	for _, p := range all {
		count := 0
		if IsBeadLevelPhase(p) {
			count++
		}
		if IsStepLevelPhase(p) {
			count++
		}
		if IsReviewLevelPhase(p) {
			count++
		}
		if count != 1 {
			t.Errorf("phase %q classifies into %d categories, want exactly 1", p, count)
		}
	}
}
