package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// --- V3 formulas load cleanly ---

func TestMigration_V3FormulasLoadCleanly(t *testing.T) {
	v3Names := []string{
		"review-phase",
		"epic-implement-phase",
		"spire-agent-work-v3",
		"spire-bugfix-v3",
		"spire-epic-v3",
	}

	for _, name := range v3Names {
		t.Run(name, func(t *testing.T) {
			g, err := formula.LoadEmbeddedStepGraph(name)
			if err != nil {
				t.Fatalf("LoadEmbeddedStepGraph(%q): %v", name, err)
			}
			if g.Version != 3 {
				t.Errorf("version = %d, want 3", g.Version)
			}
			if g.Name != name {
				t.Errorf("name = %q, want %q", g.Name, name)
			}
			// Validate explicitly (even though LoadEmbeddedStepGraph does it).
			if err := formula.ValidateGraph(g); err != nil {
				t.Fatalf("ValidateGraph(%q): %v", name, err)
			}
		})
	}
}

// --- Review phase structure preserved ---

func TestMigration_ReviewPhaseUnchanged(t *testing.T) {
	g, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Expected steps with specific properties.
	expectedSteps := map[string]struct {
		isTerminal bool
		hasNeeds   bool
		hasCond    bool
	}{
		"sage-review": {false, false, false},
		"fix":         {false, true, true},
		"arbiter":     {false, true, true},
		"merge":       {true, true, true},
		"discard":     {true, true, true},
	}

	if len(g.Steps) != len(expectedSteps) {
		t.Fatalf("step count = %d, want %d", len(g.Steps), len(expectedSteps))
	}

	for name, expect := range expectedSteps {
		step, ok := g.Steps[name]
		if !ok {
			t.Errorf("missing step %q", name)
			continue
		}
		if step.Terminal != expect.isTerminal {
			t.Errorf("step %q: terminal = %v, want %v", name, step.Terminal, expect.isTerminal)
		}
		if (len(step.Needs) > 0) != expect.hasNeeds {
			t.Errorf("step %q: has needs = %v, want %v", name, len(step.Needs) > 0, expect.hasNeeds)
		}
		hasCondition := step.Condition != "" || step.When != nil
		if hasCondition != expect.hasCond {
			t.Errorf("step %q: has condition = %v, want %v", name, hasCondition, expect.hasCond)
		}
	}

	// Specific condition checks: fix and merge must have routing conditions.
	if g.Steps["fix"].Condition == "" && g.Steps["fix"].When == nil {
		t.Error("fix should have a condition (string or structured when)")
	}
	if g.Steps["merge"].Condition == "" && g.Steps["merge"].When == nil {
		t.Error("merge should have a condition (string or structured when)")
	}
}

// --- ResolveV3 always resolves to v3 ---

func TestMigration_V3FormulaResolution(t *testing.T) {
	tests := []struct {
		name string
		bead formula.BeadInfo
	}{
		{
			name: "default task resolves to v3",
			bead: formula.BeadInfo{ID: "spi-test", Type: "task"},
		},
		{
			name: "v2 label ignored — resolves to v3",
			bead: formula.BeadInfo{ID: "spi-test", Type: "task", Labels: []string{"formula-version:2"}},
		},
		{
			name: "explicit v3 formula label resolves to v3",
			bead: formula.BeadInfo{ID: "spi-test", Type: "task", Labels: []string{"formula:spire-agent-work-v3"}},
		},
		{
			name: "bug type resolves to v3",
			bead: formula.BeadInfo{ID: "spi-test", Type: "bug"},
		},
		{
			name: "epic type resolves to v3",
			bead: formula.BeadInfo{ID: "spi-test", Type: "epic"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := formula.ResolveV3(tt.bead)
			if err != nil {
				t.Fatalf("ResolveV3: %v", err)
			}
			if g == nil {
				t.Fatal("expected non-nil formula")
			}
			if g.Version != 3 {
				t.Errorf("version = %d, want 3", g.Version)
			}
		})
	}
}
