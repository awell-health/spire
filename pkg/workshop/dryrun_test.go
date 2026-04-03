package workshop

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

func TestDryRunNilFormula(t *testing.T) {
	_, err := DryRun(nil, "", nil)
	if err == nil {
		t.Fatal("expected error for nil formula")
	}
}

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

func TestDescribePhase(t *testing.T) {
	tests := []struct {
		name string
		sim  PhaseSimulation
		want string
	}{
		{
			name: "validate-design behavior",
			sim:  PhaseSimulation{Behavior: "validate-design"},
			want: "Wizard validates linked design bead is closed and substantive",
		},
		{
			name: "skip role",
			sim:  PhaseSimulation{Role: "skip"},
			want: "Phase skipped",
		},
		{
			name: "human role",
			sim:  PhaseSimulation{Role: "human"},
			want: "Blocks until human transitions phase",
		},
		{
			name: "apprentice wave",
			sim:  PhaseSimulation{Role: "apprentice", Dispatch: "wave"},
			want: "Parallel apprentice wave dispatch with staging branch merges",
		},
		{
			name: "apprentice direct",
			sim:  PhaseSimulation{Role: "apprentice", Dispatch: "direct"},
			want: "Single apprentice implements in worktree",
		},
		{
			name: "sage verdict only",
			sim:  PhaseSimulation{Role: "sage", VerdictOnly: true},
			want: "Sage reviews diff and returns verdict only (no edits)",
		},
		{
			name: "wizard",
			sim:  PhaseSimulation{Role: "wizard"},
			want: "Wizard invokes Claude for planning/validation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := describePhase(tt.sim)
			if got != tt.want {
				t.Errorf("describePhase: got %q, want %q", got, tt.want)
			}
		})
	}
}
