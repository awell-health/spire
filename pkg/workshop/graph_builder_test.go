package workshop

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

func TestGraphBuilder_BasicBuild(t *testing.T) {
	gb := NewGraphBuilder("test-formula")
	gb.SetDescription("A test formula")

	if err := gb.AddStep("start", formula.StepConfig{
		Kind:   formula.StepKindOp,
		Action: formula.OpcodeCheckDesignLinked,
	}); err != nil {
		t.Fatal(err)
	}
	if err := gb.AddStep("finish", formula.StepConfig{
		Kind:     formula.StepKindOp,
		Action:   formula.OpcodeBeadFinish,
		Needs:    []string{"start"},
		Terminal: true,
	}); err != nil {
		t.Fatal(err)
	}

	g, err := gb.Build()
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "test-formula" {
		t.Fatalf("expected name test-formula, got %s", g.Name)
	}
	if g.Version != 3 {
		t.Fatalf("expected version 3, got %d", g.Version)
	}
	if len(g.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(g.Steps))
	}
}

func TestGraphBuilder_AddWorkspace(t *testing.T) {
	gb := NewGraphBuilder("ws-test")

	err := gb.AddWorkspace("main-repo", formula.WorkspaceDecl{
		Kind: formula.WorkspaceKindRepo,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = gb.AddWorkspace("staging", formula.WorkspaceDecl{
		Kind:   formula.WorkspaceKindStaging,
		Branch: "staging/{vars.bead_id}",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Add steps referencing workspace
	gb.AddStep("work", formula.StepConfig{
		Kind:      formula.StepKindOp,
		Action:    formula.OpcodeWizardRun,
		Flow:      "implement",
		Workspace: "staging",
	})
	gb.AddStep("done", formula.StepConfig{
		Kind:     formula.StepKindOp,
		Action:   formula.OpcodeBeadFinish,
		Needs:    []string{"work"},
		Terminal: true,
	})

	g, err := gb.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Workspaces) != 2 {
		t.Fatalf("expected 2 workspaces, got %d", len(g.Workspaces))
	}
}

func TestGraphBuilder_AddTypedVar(t *testing.T) {
	gb := NewGraphBuilder("var-test")
	gb.AddVar("bead_id", formula.FormulaVar{
		Type:        formula.VarTypeBeadID,
		Description: "Target bead",
		Required:    true,
	})
	gb.AddVar("max_rounds", formula.FormulaVar{
		Type:        formula.VarTypeInt,
		Description: "Max review rounds",
		Default:     "3",
	})

	gb.AddStep("start", formula.StepConfig{Terminal: true})

	g, err := gb.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Vars) != 2 {
		t.Fatalf("expected 2 vars, got %d", len(g.Vars))
	}
	if g.Vars["bead_id"].Type != "bead_id" {
		t.Fatalf("expected bead_id type, got %s", g.Vars["bead_id"].Type)
	}
}

func TestGraphBuilder_DuplicateStep(t *testing.T) {
	gb := NewGraphBuilder("dup-test")
	gb.AddStep("a", formula.StepConfig{Terminal: true})
	err := gb.AddStep("a", formula.StepConfig{Terminal: true})
	if err == nil {
		t.Fatal("expected error for duplicate step")
	}
}

func TestGraphBuilder_InvalidKind(t *testing.T) {
	gb := NewGraphBuilder("kind-test")
	err := gb.AddStep("a", formula.StepConfig{Kind: "invalid"})
	if err == nil {
		t.Fatal("expected error for invalid kind")
	}
}

func TestGraphBuilder_NoSteps(t *testing.T) {
	gb := NewGraphBuilder("empty")
	_, err := gb.Build()
	if err == nil {
		t.Fatal("expected error for no steps")
	}
}

func TestGraphBuilder_MarshalTOML(t *testing.T) {
	gb := NewGraphBuilder("marshal-test")
	gb.SetDescription("Test marshal")

	gb.AddVar("bead_id", formula.FormulaVar{
		Type:     formula.VarTypeBeadID,
		Required: true,
	})

	gb.AddWorkspace("repo", formula.WorkspaceDecl{
		Kind: formula.WorkspaceKindRepo,
	})

	gb.AddStep("start", formula.StepConfig{
		Kind:   formula.StepKindOp,
		Action: formula.OpcodeCheckDesignLinked,
	})
	gb.AddStep("finish", formula.StepConfig{
		Kind:     formula.StepKindOp,
		Action:   formula.OpcodeBeadFinish,
		Needs:    []string{"start"},
		Terminal: true,
	})

	data, err := gb.MarshalTOML()
	if err != nil {
		t.Fatal(err)
	}

	tomlStr := string(data)

	// Verify key sections are present
	if !strings.Contains(tomlStr, `name = "marshal-test"`) {
		t.Fatal("missing name in TOML")
	}
	if !strings.Contains(tomlStr, "version = 3") {
		t.Fatal("missing version in TOML")
	}
	if !strings.Contains(tomlStr, "[vars.bead_id]") {
		t.Fatal("missing vars section")
	}
	if !strings.Contains(tomlStr, "[workspaces.repo]") {
		t.Fatal("missing workspaces section")
	}
	if !strings.Contains(tomlStr, "[steps.start]") {
		t.Fatal("missing start step")
	}
	if !strings.Contains(tomlStr, "[steps.finish]") {
		t.Fatal("missing finish step")
	}
}

func TestGraphBuilder_RoundTrip(t *testing.T) {
	gb := NewGraphBuilder("roundtrip")
	gb.SetDescription("Round-trip test")

	gb.AddWorkspace("staging", formula.WorkspaceDecl{
		Kind:   formula.WorkspaceKindStaging,
		Branch: "staging/test",
	})

	gb.AddStep("plan", formula.StepConfig{
		Kind:      formula.StepKindOp,
		Action:    formula.OpcodeWizardRun,
		Flow:      "epic-plan",
		Workspace: "staging",
	})
	gb.AddStep("done", formula.StepConfig{
		Kind:     formula.StepKindOp,
		Action:   formula.OpcodeBeadFinish,
		Needs:    []string{"plan"},
		Terminal: true,
		With:     map[string]string{"status": "closed"},
	})

	data, err := gb.MarshalTOML()
	if err != nil {
		t.Fatal(err)
	}

	// Parse the TOML back
	g, err := formula.ParseFormulaStepGraph(data)
	if err != nil {
		t.Fatalf("round-trip parse failed: %v\nTOML:\n%s", err, data)
	}

	if g.Name != "roundtrip" {
		t.Fatalf("expected name roundtrip, got %s", g.Name)
	}
	if len(g.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(g.Steps))
	}
	if g.Steps["plan"].Flow != "epic-plan" {
		t.Fatalf("expected flow epic-plan, got %s", g.Steps["plan"].Flow)
	}
	if g.Steps["done"].With["status"] != "closed" {
		t.Fatalf("expected with.status=closed, got %s", g.Steps["done"].With["status"])
	}
}
