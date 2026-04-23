package steward

// Bead-level phase resolution test (spi-12rno4).
//
// Pins beadDispatchPhase: the steward emits a bead-level FormulaPhase
// (bead type or "wizard") for cluster-native dispatch so the operator
// routes to a wizard pod rather than directly to an apprentice. The
// override leg keeps existing test fixtures that pin specific phase
// strings working.

import (
	"testing"

	"github.com/awell-health/spire/pkg/steward/intent"
)

func TestBeadDispatchPhase(t *testing.T) {
	cases := []struct {
		name     string
		override string
		beadType string
		want     string
	}{
		{"override wins", "implement", "task", "implement"},
		{"override-only", "review", "", "review"},
		{"bead type task", "", "task", "task"},
		{"bead type bug", "", "bug", "bug"},
		{"bead type epic", "", "epic", "epic"},
		{"empty falls back to wizard", "", "", intent.PhaseWizard},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := beadDispatchPhase(tc.override, tc.beadType)
			if got != tc.want {
				t.Errorf("beadDispatchPhase(%q, %q) = %q, want %q",
					tc.override, tc.beadType, got, tc.want)
			}
		})
	}
}

// TestBeadDispatchPhase_AllBeadTypesAreBeadLevel ensures every type
// the steward might stamp survives the operator's bead-level classifier.
// If someone adds a new bead type that's not registered as bead-level
// in pkg/steward/intent, this test fires.
func TestBeadDispatchPhase_AllBeadTypesAreBeadLevel(t *testing.T) {
	for _, beadType := range []string{"task", "bug", "epic", "feature", "chore"} {
		got := beadDispatchPhase("", beadType)
		if !intent.IsBeadLevelPhase(got) {
			t.Errorf("phase %q (from bead type %q) is not classified as bead-level — operator would route it to the wrong pod builder",
				got, beadType)
		}
	}
}
