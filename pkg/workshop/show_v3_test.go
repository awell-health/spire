package workshop

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

func TestRenderV3_FullEpicFormula(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Name:        "epic-lifecycle",
		Description: "Full epic lifecycle",
		Version:     3,
		Workspaces: map[string]formula.WorkspaceDecl{
			"staging": {
				Kind:      formula.WorkspaceKindStaging,
				Branch:    "staging/{vars.bead_id}",
				Base:      "main",
				Scope:     formula.WorkspaceScopeRun,
				Ownership: "owned",
				Cleanup:   formula.WorkspaceCleanupTerminal,
			},
		},
		Vars: map[string]formula.FormulaVar{
			"bead_id": {
				Type:        formula.VarTypeBeadID,
				Description: "Target bead",
				Required:    true,
			},
			"max_rounds": {
				Type:        formula.VarTypeInt,
				Description: "Max review rounds",
				Default:     "3",
			},
		},
		Steps: map[string]formula.StepConfig{
			"check-design": {
				Kind:   formula.StepKindOp,
				Action: formula.OpcodeCheckDesignLinked,
			},
			"plan": {
				Kind:      formula.StepKindOp,
				Action:    formula.OpcodeWizardRun,
				Flow:      "epic-plan",
				Workspace: "staging",
				Needs:     []string{"check-design"},
				Produces:  []string{"plan"},
			},
			"dispatch": {
				Kind:   formula.StepKindDispatch,
				Action: formula.OpcodeDispatchChildren,
				Needs:  []string{"plan"},
				With:   map[string]string{"children": "subtasks", "strategy": "dependency-wave"},
			},
			"review": {
				Kind:  formula.StepKindCall,
				Graph: "review-default",
				Needs: []string{"dispatch"},
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "steps.dispatch.outputs.status", Op: "eq", Right: "complete"},
					},
				},
			},
			"merge": {
				Kind:      formula.StepKindOp,
				Action:    formula.OpcodeGitMergeToMain,
				Workspace: "staging",
				Needs:     []string{"review"},
			},
			"finish": {
				Kind:     formula.StepKindOp,
				Action:   formula.OpcodeBeadFinish,
				Needs:    []string{"merge"},
				Terminal: true,
				With:     map[string]string{"status": "closed"},
			},
		},
	}

	output := renderV3(f, "test")

	// Check header
	if !strings.Contains(output, "epic-lifecycle (v3)") {
		t.Fatal("missing header")
	}
	if !strings.Contains(output, "Full epic lifecycle") {
		t.Fatal("missing description")
	}

	// Check workspaces section
	if !strings.Contains(output, "Workspaces:") {
		t.Fatal("missing Workspaces section")
	}
	if !strings.Contains(output, "staging") {
		t.Fatal("missing staging workspace")
	}
	if !strings.Contains(output, "kind:      staging") {
		t.Fatal("missing workspace kind")
	}

	// Check step actions
	if !strings.Contains(output, "action:    check.design-linked") {
		t.Fatal("missing check.design-linked action")
	}
	if !strings.Contains(output, "action:    wizard.run") {
		t.Fatal("missing wizard.run action")
	}
	if !strings.Contains(output, "flow:      epic-plan") {
		t.Fatal("missing flow")
	}

	// Check structured conditions rendered
	if !strings.Contains(output, "when:") {
		t.Fatal("missing when rendering")
	}
	if !strings.Contains(output, "steps.dispatch.outputs.status == complete") {
		t.Fatal("missing when predicate rendering")
	}

	// Check produces
	if !strings.Contains(output, "produces:  plan") {
		t.Fatal("missing produces")
	}

	// Check variables with type
	if !strings.Contains(output, "bead_id [bead_id] (required)") {
		t.Fatal("missing typed variable")
	}
	if !strings.Contains(output, "max_rounds [int]") {
		t.Fatal("missing int variable")
	}

	// Check graph field
	if !strings.Contains(output, "graph:     review-default") {
		t.Fatal("missing graph field")
	}
}

func TestRenderWhenPredicate_AllAndAny(t *testing.T) {
	when := &formula.StructuredCondition{
		All: []formula.Predicate{
			{Left: "verdict", Op: "eq", Right: "request_changes"},
			{Left: "round", Op: "lt", Right: "vars.max_rounds"},
		},
		Any: []formula.Predicate{
			{Left: "status", Op: "eq", Right: "open"},
			{Left: "status", Op: "eq", Right: "in_progress"},
		},
	}

	result := renderWhenPredicate(when)

	if !strings.Contains(result, "verdict == request_changes") {
		t.Fatalf("missing first all predicate, got: %s", result)
	}
	if !strings.Contains(result, "round < vars.max_rounds") {
		t.Fatalf("missing second all predicate, got: %s", result)
	}
	if !strings.Contains(result, "AND") {
		t.Fatalf("missing AND connector, got: %s", result)
	}
	if !strings.Contains(result, "OR") {
		t.Fatalf("missing OR in any group, got: %s", result)
	}
}

func TestRenderWhenPredicate_AnyOnly(t *testing.T) {
	when := &formula.StructuredCondition{
		Any: []formula.Predicate{
			{Left: "verdict", Op: "eq", Right: "approve"},
			{Left: "verdict", Op: "eq", Right: "merge"},
		},
	}

	result := renderWhenPredicate(when)
	if !strings.Contains(result, "verdict == approve OR verdict == merge") {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestOpSymbol(t *testing.T) {
	tests := map[string]string{
		"eq": "==", "ne": "!=", "lt": "<", "gt": ">", "le": "<=", "ge": ">=",
	}
	for op, want := range tests {
		if got := opSymbol(op); got != want {
			t.Errorf("opSymbol(%q) = %q, want %q", op, got, want)
		}
	}
}
