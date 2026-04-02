package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// --- V2 formulas still load ---

func TestMigration_V2FormulasStillLoad(t *testing.T) {
	v2Names := []struct {
		name   string
		phases []string
	}{
		{"spire-agent-work", []string{"plan", "implement", "review", "merge"}},
		{"spire-bugfix", []string{"plan", "implement", "review", "merge"}},
		{"spire-epic", []string{"design", "plan", "implement", "review", "merge"}},
	}

	for _, tt := range v2Names {
		t.Run(tt.name, func(t *testing.T) {
			f, err := formula.LoadFormulaByName(tt.name)
			if err != nil {
				t.Fatalf("LoadFormulaByName(%q): %v", tt.name, err)
			}
			if f.Name != tt.name {
				t.Errorf("name = %q, want %q", f.Name, tt.name)
			}
			if f.Version != 2 {
				t.Errorf("version = %d, want 2", f.Version)
			}

			// Verify expected phases.
			enabled := f.EnabledPhases()
			if len(enabled) != len(tt.phases) {
				t.Fatalf("phases = %v, want %v", enabled, tt.phases)
			}
			for i, p := range tt.phases {
				if enabled[i] != p {
					t.Errorf("phase[%d] = %q, want %q", i, enabled[i], p)
				}
			}
		})
	}
}

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
		if (step.Condition != "") != expect.hasCond {
			t.Errorf("step %q: has condition = %v, want %v", name, step.Condition != "", expect.hasCond)
		}
	}

	// Specific condition checks.
	if g.Steps["fix"].Condition == "" {
		t.Error("fix should have a condition")
	}
	if g.Steps["merge"].Condition == "" {
		t.Error("merge should have a condition")
	}
}

// --- ParseFormulaAny dispatches correctly ---

func TestMigration_V2AndV3Coexist(t *testing.T) {
	// Load raw v2 formula data from embedded.
	v2Data, err := formula.LoadFormulaByName("spire-agent-work")
	if err != nil {
		t.Fatalf("load v2: %v", err)
	}
	if v2Data.Version != 2 {
		t.Errorf("v2 version = %d", v2Data.Version)
	}

	// Load raw v3 formula data from embedded.
	v3Data, err := formula.LoadEmbeddedStepGraph("spire-agent-work-v3")
	if err != nil {
		t.Fatalf("load v3: %v", err)
	}
	if v3Data.Version != 3 {
		t.Errorf("v3 version = %d", v3Data.Version)
	}

	// Verify they have the same conceptual structure (plan, implement, review, merge).
	v2Phases := v2Data.EnabledPhases()
	v3Steps := make(map[string]bool)
	for name := range v3Data.Steps {
		v3Steps[name] = true
	}

	// The v2 phases (plan, implement, review, merge) should all be represented
	// as steps in the v3 formula.
	for _, phase := range v2Phases {
		if !v3Steps[phase] {
			t.Errorf("v2 phase %q not represented in v3 steps", phase)
		}
	}
}

// --- ResolveAny correctly dispatches based on labels ---

func TestMigration_V3FormulaResolution(t *testing.T) {
	tests := []struct {
		name        string
		bead        formula.BeadInfo
		wantVersion int
	}{
		{
			name:        "default task resolves to v2",
			bead:        formula.BeadInfo{ID: "spi-test", Type: "task"},
			wantVersion: 2,
		},
		{
			name:        "formula-version:3 label resolves to v3",
			bead:        formula.BeadInfo{ID: "spi-test", Type: "task", Labels: []string{"formula-version:3"}},
			wantVersion: 3,
		},
		{
			name:        "explicit v3 formula label resolves to v3",
			bead:        formula.BeadInfo{ID: "spi-test", Type: "task", Labels: []string{"formula:spire-agent-work-v3"}},
			wantVersion: 3,
		},
		{
			name:        "explicit v2 formula label resolves to v2",
			bead:        formula.BeadInfo{ID: "spi-test", Type: "task", Labels: []string{"formula:spire-agent-work"}},
			wantVersion: 2,
		},
		{
			name:        "bug type resolves to v2 by default",
			bead:        formula.BeadInfo{ID: "spi-test", Type: "bug"},
			wantVersion: 2,
		},
		{
			name:        "bug type with v3 label resolves to v3",
			bead:        formula.BeadInfo{ID: "spi-test", Type: "bug", Labels: []string{"formula-version:3"}},
			wantVersion: 3,
		},
		{
			name:        "epic type resolves to v2 by default",
			bead:        formula.BeadInfo{ID: "spi-test", Type: "epic"},
			wantVersion: 2,
		},
		{
			name:        "epic type with v3 label resolves to v3",
			bead:        formula.BeadInfo{ID: "spi-test", Type: "epic", Labels: []string{"formula-version:3"}},
			wantVersion: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, v, err := formula.ResolveAny(tt.bead)
			if err != nil {
				t.Fatalf("ResolveAny: %v", err)
			}
			if v != tt.wantVersion {
				t.Errorf("version = %d, want %d", v, tt.wantVersion)
			}
			if f == nil {
				t.Fatal("expected non-nil formula")
			}

			// Type-check the returned formula.
			switch tt.wantVersion {
			case 2:
				if _, ok := f.(*formula.FormulaV2); !ok {
					t.Errorf("expected *FormulaV2, got %T", f)
				}
			case 3:
				if _, ok := f.(*formula.FormulaStepGraph); !ok {
					t.Errorf("expected *FormulaStepGraph, got %T", f)
				}
			}
		})
	}
}
