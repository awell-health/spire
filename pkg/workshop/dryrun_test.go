package workshop

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

func TestDryRunStepGraph(t *testing.T) {
	g, err := formula.LoadReviewPhaseFormula()
	if err != nil {
		t.Fatalf("load review-phase formula: %v", err)
	}

	result, err := DryRunStepGraph(g)
	if err != nil {
		t.Fatalf("DryRunStepGraph: %v", err)
	}

	if result.Formula != "review-phase" {
		t.Errorf("formula: got %q, want %q", result.Formula, "review-phase")
	}
	if result.Version != 3 {
		t.Errorf("version: got %d, want 3", result.Version)
	}
	if result.Entry != "sage-review" {
		t.Errorf("entry: got %q, want %q", result.Entry, "sage-review")
	}

	// Check step count: sage-review, fix, arbiter, merge, discard = 5
	if len(result.Steps) != 5 {
		t.Errorf("steps: got %d, want 5", len(result.Steps))
	}

	// Count terminal steps (merge and discard)
	terminalCount := 0
	for _, step := range result.Steps {
		if step.Terminal {
			terminalCount++
		}
	}
	if terminalCount != 2 {
		t.Errorf("terminal steps: got %d, want 2", terminalCount)
	}

	// Verify paths exist
	if len(result.Paths) == 0 {
		t.Fatal("no paths found")
	}

	// Check that we have paths ending at merge and discard
	var hasMergePath, hasDiscardPath bool
	for _, path := range result.Paths {
		if len(path) == 0 {
			continue
		}
		last := path[len(path)-1]
		if last == "merge" {
			hasMergePath = true
		}
		if last == "discard" {
			hasDiscardPath = true
		}
	}
	if !hasMergePath {
		t.Error("no path ending at merge")
	}
	if !hasDiscardPath {
		t.Error("no path ending at discard")
	}

	// Verify all paths start with sage-review
	for i, path := range result.Paths {
		if len(path) > 0 && path[0] != "sage-review" {
			t.Errorf("path[%d] starts with %q, expected sage-review", i, path[0])
		}
	}

	if len(result.Errors) > 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}
}

func TestDryRunStepGraphNil(t *testing.T) {
	_, err := DryRunStepGraph(nil)
	if err == nil {
		t.Fatal("expected error for nil step graph")
	}
}

func TestDryRunStepGraph_EmbeddedV3Formulas(t *testing.T) {
	tests := []struct {
		name          string
		minSteps      int
		expectEntry   bool
		expectTerminal bool
	}{
		{
			name:           "spire-agent-work-v3",
			minSteps:       3,
			expectEntry:    true,
			expectTerminal: true,
		},
		{
			name:           "spire-bugfix-v3",
			minSteps:       3,
			expectEntry:    true,
			expectTerminal: true,
		},
		{
			name:           "spire-epic-v3",
			minSteps:       4,
			expectEntry:    true,
			expectTerminal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := formula.LoadEmbeddedStepGraph(tt.name)
			if err != nil {
				t.Fatalf("load embedded step graph %q: %v", tt.name, err)
			}

			result, err := DryRunStepGraph(g)
			if err != nil {
				t.Fatalf("DryRunStepGraph: %v", err)
			}

			if result.Formula != tt.name {
				t.Errorf("formula name: got %q, want %q", result.Formula, tt.name)
			}
			if result.Version != 3 {
				t.Errorf("version: got %d, want 3", result.Version)
			}
			if len(result.Steps) < tt.minSteps {
				t.Errorf("steps: got %d, want at least %d", len(result.Steps), tt.minSteps)
			}

			if tt.expectEntry && result.Entry == "" {
				t.Error("expected non-empty entry step")
			}

			if tt.expectTerminal {
				hasTerminal := false
				for _, step := range result.Steps {
					if step.Terminal {
						hasTerminal = true
						break
					}
				}
				if !hasTerminal {
					t.Error("expected at least one terminal step")
				}
			}

			// Every step should have a description
			for _, step := range result.Steps {
				if step.Description == "" {
					t.Errorf("step %q has empty description", step.Name)
				}
			}

			// Should have at least one path
			if len(result.Paths) == 0 {
				t.Error("no execution paths found")
			}

			if len(result.Errors) > 0 {
				t.Errorf("unexpected errors: %v", result.Errors)
			}
		})
	}
}
