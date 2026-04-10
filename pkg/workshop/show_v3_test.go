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

func TestRenderV3_DAGSection(t *testing.T) {
	output := renderV3(&formula.FormulaStepGraph{
		Name:    "dag-test",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"plan": {
				Kind:   formula.StepKindOp,
				Action: formula.OpcodeWizardRun,
				Flow:   "task-plan",
			},
			"implement": {
				Kind:   formula.StepKindOp,
				Action: formula.OpcodeWizardRun,
				Flow:   "implement",
				Needs:  []string{"plan"},
			},
			"review": {
				Kind:  formula.StepKindCall,
				Graph: "subgraph-review",
				Needs: []string{"implement"},
			},
			"merge": {
				Kind:   formula.StepKindOp,
				Action: formula.OpcodeGitMergeToMain,
				Needs:  []string{"review"},
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "outcome", Op: "eq", Right: "merge"},
					},
				},
			},
			"close": {
				Kind:     formula.StepKindOp,
				Action:   formula.OpcodeBeadFinish,
				Needs:    []string{"merge"},
				Terminal: true,
			},
			"discard": {
				Kind:     formula.StepKindOp,
				Action:   formula.OpcodeBeadFinish,
				Needs:    []string{"review"},
				Terminal: true,
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "outcome", Op: "eq", Right: "discard"},
					},
				},
			},
		},
	}, "test")

	// Must have Graph: section
	if !strings.Contains(output, "Graph:") {
		t.Fatalf("missing Graph section in output:\n%s", output)
	}

	// Must contain tree connectors
	if !strings.Contains(output, "\u2514\u2500") { // └─
		t.Fatalf("missing tree connector in output:\n%s", output)
	}

	// Must mark entry and terminal
	if !strings.Contains(output, "[entry]") {
		t.Fatalf("missing [entry] marker in output:\n%s", output)
	}
	if !strings.Contains(output, "[terminal") {
		t.Fatalf("missing [terminal] marker in output:\n%s", output)
	}

	// Must show nested graph reference
	if !strings.Contains(output, "(-> subgraph-review)") {
		t.Fatalf("missing graph reference in output:\n%s", output)
	}

	// Must show when condition
	if !strings.Contains(output, "when: outcome == merge") {
		t.Fatalf("missing when condition in output:\n%s", output)
	}
}

func TestRenderDAG_WithResetCycle(t *testing.T) {
	// Build a graph with a cycle: sage-review -> fix -> sage-review (back-edge)
	f := &formula.FormulaStepGraph{
		Name:    "cycle-test",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"sage-review": {
				Kind:   formula.StepKindOp,
				Action: formula.OpcodeWizardRun,
				Flow:   "sage-review",
			},
			"fix": {
				Kind:  formula.StepKindOp,
				Needs: []string{"sage-review"},
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "verdict", Op: "eq", Right: "request_changes"},
					},
				},
			},
			"fix-review": {
				Kind:  formula.StepKindOp,
				Needs: []string{"fix"},
			},
			"merge": {
				Kind:     formula.StepKindOp,
				Needs:    []string{"sage-review", "fix-review"},
				Terminal: true,
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "verdict", Op: "eq", Right: "approve"},
					},
				},
			},
		},
	}

	var b strings.Builder
	renderDAG(&b, f)
	output := b.String()

	// Must contain the graph
	if !strings.Contains(output, "Graph:") {
		t.Fatalf("missing Graph section in output:\n%s", output)
	}

	// Must show all step names
	if !strings.Contains(output, "sage-review") {
		t.Fatalf("missing sage-review in output:\n%s", output)
	}
	if !strings.Contains(output, "fix") {
		t.Fatalf("missing fix in output:\n%s", output)
	}
	if !strings.Contains(output, "merge") {
		t.Fatalf("missing merge in output:\n%s", output)
	}
}

func TestRenderDAG_EmbeddedReviewPhase(t *testing.T) {
	g, err := formula.LoadReviewPhaseFormula()
	if err != nil {
		t.Fatalf("load subgraph-review: %v", err)
	}

	var b strings.Builder
	renderDAG(&b, g)
	output := b.String()

	if !strings.Contains(output, "Graph:") {
		t.Fatalf("missing Graph section:\n%s", output)
	}
	if !strings.Contains(output, "sage-review") {
		t.Fatalf("missing sage-review:\n%s", output)
	}
	if !strings.Contains(output, "[entry]") {
		t.Fatalf("missing entry marker:\n%s", output)
	}
	if !strings.Contains(output, "[terminal") {
		t.Fatalf("missing terminal marker:\n%s", output)
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
