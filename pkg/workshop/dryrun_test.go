package workshop

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

func TestDryRunEmbeddedFormulas(t *testing.T) {
	tests := []struct {
		name           string
		expectedPhases int
		phases         []string
	}{
		{
			name:           "spire-agent-work",
			expectedPhases: 4,
			phases:         []string{"plan", "implement", "review", "merge"},
		},
		{
			name:           "spire-bugfix",
			expectedPhases: 4,
			phases:         []string{"plan", "implement", "review", "merge"},
		},
		{
			name:           "spire-epic",
			expectedPhases: 5,
			phases:         []string{"design", "plan", "implement", "review", "merge"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := formula.LoadEmbeddedFormula(tt.name)
			if err != nil {
				t.Fatalf("load embedded formula %q: %v", tt.name, err)
			}

			result, err := DryRun(f, "", nil)
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}

			if result.Formula != tt.name {
				t.Errorf("formula name: got %q, want %q", result.Formula, tt.name)
			}
			if result.Version != 2 {
				t.Errorf("version: got %d, want 2", result.Version)
			}
			if len(result.EnabledPhases) != tt.expectedPhases {
				t.Errorf("enabled phases: got %d, want %d (%v)", len(result.EnabledPhases), tt.expectedPhases, result.EnabledPhases)
			}
			if len(result.Phases) != tt.expectedPhases {
				t.Fatalf("phase simulations: got %d, want %d", len(result.Phases), tt.expectedPhases)
			}

			for i, phase := range result.Phases {
				if phase.Name != tt.phases[i] {
					t.Errorf("phase[%d]: got %q, want %q", i, phase.Name, tt.phases[i])
				}
				if phase.Role == "" {
					t.Errorf("phase %q has empty role", phase.Name)
				}
				if phase.Description == "" {
					t.Errorf("phase %q has empty description", phase.Name)
				}
			}

			if len(result.Errors) > 0 {
				t.Errorf("unexpected errors: %v", result.Errors)
			}
		})
	}
}

func TestDryRunWithBeadContext(t *testing.T) {
	f, err := formula.LoadEmbeddedFormula("spire-epic")
	if err != nil {
		t.Fatalf("load formula: %v", err)
	}

	loadBead := func(id string) (BeadInfo, error) {
		return BeadInfo{
			ID:   id,
			Type: "epic",
			Title: "Test epic",
		}, nil
	}

	result, err := DryRun(f, "spi-abc", loadBead)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	// Check that staging branch was substituted for implement phase
	var implPhase *PhaseSimulation
	var mergePhase *PhaseSimulation
	for i := range result.Phases {
		if result.Phases[i].Name == "implement" {
			implPhase = &result.Phases[i]
		}
		if result.Phases[i].Name == "merge" {
			mergePhase = &result.Phases[i]
		}
	}

	if implPhase == nil {
		t.Fatal("implement phase not found")
	}
	if implPhase.StagingBranch != "epic/spi-abc" {
		t.Errorf("implement staging branch: got %q, want %q", implPhase.StagingBranch, "epic/spi-abc")
	}

	if mergePhase == nil {
		t.Fatal("merge phase not found")
	}
	if mergePhase.StagingBranch != "epic/spi-abc" {
		t.Errorf("merge staging branch: got %q, want %q", mergePhase.StagingBranch, "epic/spi-abc")
	}
}

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
