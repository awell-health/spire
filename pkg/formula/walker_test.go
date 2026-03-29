package formula

import "testing"

// reviewGraph builds a test fixture mirroring review-phase.formula.toml.
func reviewGraph() *FormulaStepGraph {
	return &FormulaStepGraph{
		Name:    "review-phase",
		Version: 3,
		Steps: map[string]StepConfig{
			"sage-review": {Role: "sage", Title: "Sage review"},
			"fix": {
				Role:      "apprentice",
				Title:     "Fix: address sage review feedback",
				Needs:     []string{"sage-review"},
				Condition: "verdict == request_changes && round < max_rounds",
			},
			"arbiter": {
				Role:      "arbiter",
				Title:     "Arbiter: break review deadlock",
				Needs:     []string{"sage-review"},
				Condition: "verdict == request_changes && round >= max_rounds",
			},
			"merge": {
				Role:      "executor",
				Title:     "Merge to main",
				Needs:     []string{"sage-review", "arbiter"},
				Condition: "verdict == approve || arbiter_decision == merge || arbiter_decision == split",
				Terminal:  true,
			},
			"discard": {
				Role:      "executor",
				Title:     "Discard branch",
				Needs:     []string{"arbiter"},
				Condition: "arbiter_decision == discard",
				Terminal:  true,
			},
		},
		Vars: map[string]FormulaVar{
			"max_rounds": {Default: "3"},
		},
	}
}

func TestNextSteps_EntryPoint(t *testing.T) {
	g := reviewGraph()
	next, err := NextSteps(g, map[string]bool{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0] != "sage-review" {
		t.Fatalf("expected [sage-review], got %v", next)
	}
}

func TestNextSteps_Approve(t *testing.T) {
	g := reviewGraph()
	completed := map[string]bool{"sage-review": true}
	ctx := map[string]string{"verdict": "approve"}
	next, err := NextSteps(g, completed, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0] != "merge" {
		t.Fatalf("expected [merge], got %v", next)
	}
}

func TestNextSteps_RequestChanges(t *testing.T) {
	g := reviewGraph()
	completed := map[string]bool{"sage-review": true}
	ctx := map[string]string{"verdict": "request_changes", "round": "1", "max_rounds": "3"}
	next, err := NextSteps(g, completed, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0] != "fix" {
		t.Fatalf("expected [fix], got %v", next)
	}
}

func TestNextSteps_MaxRounds(t *testing.T) {
	g := reviewGraph()
	completed := map[string]bool{"sage-review": true}
	ctx := map[string]string{"verdict": "request_changes", "round": "3", "max_rounds": "3"}
	next, err := NextSteps(g, completed, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0] != "arbiter" {
		t.Fatalf("expected [arbiter], got %v", next)
	}
}

func TestNextSteps_ArbiterMerge(t *testing.T) {
	g := reviewGraph()
	completed := map[string]bool{"sage-review": true, "arbiter": true}
	ctx := map[string]string{"verdict": "request_changes", "round": "3", "max_rounds": "3", "arbiter_decision": "merge"}
	next, err := NextSteps(g, completed, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0] != "merge" {
		t.Fatalf("expected [merge], got %v", next)
	}
}

func TestNextSteps_ArbiterDiscard(t *testing.T) {
	g := reviewGraph()
	completed := map[string]bool{"sage-review": true, "arbiter": true}
	ctx := map[string]string{"verdict": "request_changes", "round": "3", "max_rounds": "3", "arbiter_decision": "discard"}
	next, err := NextSteps(g, completed, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0] != "discard" {
		t.Fatalf("expected [discard], got %v", next)
	}
}

func TestEntryStep(t *testing.T) {
	g := reviewGraph()
	if got := EntryStep(g); got != "sage-review" {
		t.Fatalf("expected sage-review, got %s", got)
	}
}

func TestIsTerminal(t *testing.T) {
	g := reviewGraph()
	if !IsTerminal(g, "merge") {
		t.Fatal("merge should be terminal")
	}
	if !IsTerminal(g, "discard") {
		t.Fatal("discard should be terminal")
	}
	if IsTerminal(g, "sage-review") {
		t.Fatal("sage-review should not be terminal")
	}
	if IsTerminal(g, "fix") {
		t.Fatal("fix should not be terminal")
	}
	if IsTerminal(g, "arbiter") {
		t.Fatal("arbiter should not be terminal")
	}
}

func TestValidateGraph_Valid(t *testing.T) {
	g := reviewGraph()
	if err := ValidateGraph(g); err != nil {
		t.Fatalf("expected valid graph, got: %s", err)
	}
}

func TestValidateGraph_DanglingNeed(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {},
			"b": {Needs: []string{"nonexistent"}, Terminal: true},
		},
	}
	if err := ValidateGraph(g); err == nil {
		t.Fatal("expected error for dangling need")
	}
}

func TestValidateGraph_NoEntryPoint(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {Needs: []string{"b"}, Terminal: true},
			"b": {Needs: []string{"a"}},
		},
	}
	if err := ValidateGraph(g); err == nil {
		t.Fatal("expected error for no entry point")
	}
}

func TestValidateGraph_SelfReference(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {},
			"b": {Needs: []string{"b"}, Terminal: true},
		},
	}
	if err := ValidateGraph(g); err == nil {
		t.Fatal("expected error for self-reference")
	}
}

func TestValidateGraph_NoTerminal(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {},
			"b": {Needs: []string{"a"}},
		},
	}
	if err := ValidateGraph(g); err == nil {
		t.Fatal("expected error for no terminal steps")
	}
}
