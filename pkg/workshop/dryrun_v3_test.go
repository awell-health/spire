package workshop

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

func TestDryRunStepGraph_V3Fields(t *testing.T) {
	g := &formula.FormulaStepGraph{
		Name:    "epic-test",
		Version: 3,
		Workspaces: map[string]formula.WorkspaceDecl{
			"staging": {
				Kind:      formula.WorkspaceKindStaging,
				Branch:    "staging/test",
				Scope:     formula.WorkspaceScopeRun,
				Ownership: "owned",
				Cleanup:   formula.WorkspaceCleanupTerminal,
			},
		},
		Vars: map[string]formula.FormulaVar{
			"bead_id": {Type: formula.VarTypeBeadID, Required: true},
		},
		Steps: map[string]formula.StepConfig{
			"plan": {
				Kind:      formula.StepKindOp,
				Action:    formula.OpcodeWizardRun,
				Flow:      "epic-plan",
				Workspace: "staging",
				Produces:  []string{"plan"},
			},
			"finish": {
				Kind:     formula.StepKindOp,
				Action:   formula.OpcodeBeadFinish,
				Needs:    []string{"plan"},
				Terminal: true,
				With:     map[string]string{"status": "closed"},
			},
		},
	}

	sim, err := DryRunStepGraph(g)
	if err != nil {
		t.Fatal(err)
	}

	if sim.Entry != "plan" {
		t.Fatalf("expected entry=plan, got %s", sim.Entry)
	}

	// Check workspace simulations
	if len(sim.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(sim.Workspaces))
	}
	ws := sim.Workspaces[0]
	if ws.Name != "staging" {
		t.Fatalf("expected workspace name staging, got %s", ws.Name)
	}
	if ws.Kind != "staging" {
		t.Fatalf("expected workspace kind staging, got %s", ws.Kind)
	}

	// Check var types
	if sim.VarTypes["bead_id"] != "bead_id" {
		t.Fatalf("expected var type bead_id, got %s", sim.VarTypes["bead_id"])
	}

	// Check step simulation v3 fields
	for _, step := range sim.Steps {
		switch step.Name {
		case "plan":
			if step.Kind != "op" {
				t.Fatalf("expected kind=op for plan, got %s", step.Kind)
			}
			if step.Action != "wizard.run" {
				t.Fatalf("expected action=wizard.run for plan, got %s", step.Action)
			}
			if step.Flow != "epic-plan" {
				t.Fatalf("expected flow=epic-plan, got %s", step.Flow)
			}
			if step.Workspace != "staging" {
				t.Fatalf("expected workspace=staging, got %s", step.Workspace)
			}
			if len(step.Produces) != 1 || step.Produces[0] != "plan" {
				t.Fatalf("unexpected produces: %v", step.Produces)
			}
			if step.Description == "" {
				t.Fatal("expected non-empty description for plan step")
			}
		case "finish":
			if step.With["status"] != "closed" {
				t.Fatalf("expected with.status=closed, got %s", step.With["status"])
			}
			if step.Description == "" {
				t.Fatal("expected non-empty description for finish step")
			}
		}
	}
}

func TestDescribeStep_Actions(t *testing.T) {
	tests := []struct {
		sim  StepSimulation
		want string
	}{
		{
			sim:  StepSimulation{Action: "wizard.run", Flow: "epic-plan", Workspace: "staging"},
			want: "Run wizard epic-plan flow in staging",
		},
		{
			sim:  StepSimulation{Action: "dispatch.children", With: map[string]string{"strategy": "dependency-wave"}},
			want: "Dispatch children via dependency-wave strategy",
		},
		{
			sim:  StepSimulation{Action: "graph.run", Graph: "review-default"},
			want: "Execute nested graph review-default",
		},
		{
			sim:  StepSimulation{Action: "git.merge_to_main", Workspace: "staging"},
			want: "Merge staging to main",
		},
		{
			sim:  StepSimulation{Action: "bead.finish", With: map[string]string{"status": "closed"}},
			want: "Finalize bead (status=closed)",
		},
		{
			sim:  StepSimulation{Action: "verify.run", Workspace: "work"},
			want: "Run verification in work",
		},
		{
			sim:  StepSimulation{Action: "check.design-linked"},
			want: "Verify linked design bead",
		},
		{
			sim:  StepSimulation{Action: "beads.materialize_plan"},
			want: "Materialize plan into child beads",
		},
		{
			sim:  StepSimulation{Action: "custom.action", Kind: "op"},
			want: "Execute custom.action (op)",
		},
		{
			sim:  StepSimulation{Role: "sage", Title: "Review code"},
			want: "Sage reviews: Review code",
		},
		{
			sim:  StepSimulation{Role: "apprentice"},
			want: "Apprentice implements in worktree",
		},
		{
			sim:  StepSimulation{Name: "mystery"},
			want: `Step "mystery"`,
		},
	}

	for _, tt := range tests {
		got := describeStep(tt.sim)
		if got != tt.want {
			t.Errorf("describeStep(%+v) = %q, want %q", tt.sim, got, tt.want)
		}
	}
}

func TestDryRunStepGraph_WithWhenCondition(t *testing.T) {
	g := &formula.FormulaStepGraph{
		Name:    "when-test",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"start": {
				Kind:   formula.StepKindOp,
				Action: formula.OpcodeCheckDesignLinked,
			},
			"revise": {
				Kind:   formula.StepKindOp,
				Action: formula.OpcodeWizardRun,
				Flow:   "build-fix",
				Needs:  []string{"start"},
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "verdict", Op: "eq", Right: "request_changes"},
					},
				},
				Terminal: true,
			},
		},
	}

	sim, err := DryRunStepGraph(g)
	if err != nil {
		t.Fatal(err)
	}

	for _, step := range sim.Steps {
		if step.Name == "revise" {
			if step.When == "" {
				t.Fatal("expected non-empty when for revise step")
			}
			if step.When != "verdict == request_changes" {
				t.Fatalf("unexpected when: %s", step.When)
			}
		}
	}
}
