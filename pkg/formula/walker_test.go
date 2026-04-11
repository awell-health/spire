package formula

import (
	"strings"
	"testing"
)

// reviewGraph builds a test fixture mirroring subgraph-review.formula.toml.
func reviewGraph() *FormulaStepGraph {
	return &FormulaStepGraph{
		Name:    "subgraph-review",
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

func TestTopoSort_LinearChain(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"plan":      {},
			"implement": {Needs: []string{"plan"}},
			"review":    {Needs: []string{"implement"}},
			"merge":     {Needs: []string{"review"}},
			"close":     {Needs: []string{"merge"}, Terminal: true},
		},
	}
	got := TopoSort(g)
	want := []string{"plan", "implement", "review", "merge", "close"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: expected %s, got %s (full: %v)", i, want[i], got[i], got)
		}
	}
}

func TestTopoSort_Diamond(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {},
			"b": {Needs: []string{"a"}},
			"c": {Needs: []string{"a"}},
			"d": {Needs: []string{"b", "c"}, Terminal: true},
		},
	}
	got := TopoSort(g)
	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: expected %s, got %s (full: %v)", i, want[i], got[i], got)
		}
	}
}

func TestTopoSort_BranchingTerminals(t *testing.T) {
	// Mirrors task-default: review→{merge,discard}, merge→close
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"plan":      {},
			"implement": {Needs: []string{"plan"}},
			"review":    {Needs: []string{"implement"}},
			"merge":     {Needs: []string{"review"}},
			"discard":   {Needs: []string{"review"}, Terminal: true},
			"close":     {Needs: []string{"merge"}, Terminal: true},
		},
	}
	got := TopoSort(g)
	// Level 0: plan, Level 1: implement, Level 2: review,
	// Level 3: discard, merge (alphabetical tiebreak), Level 4: close
	want := []string{"plan", "implement", "review", "discard", "merge", "close"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: expected %s, got %s (full: %v)", i, want[i], got[i], got)
		}
	}
}

func TestTopoSort_NilGraph(t *testing.T) {
	if got := TopoSort(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestTopoSort_EmptyGraph(t *testing.T) {
	g := &FormulaStepGraph{Steps: map[string]StepConfig{}}
	if got := TopoSort(g); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestTopoSort_EmbeddedTaskDefault(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("task-default")
	if err != nil {
		t.Fatalf("failed to load task-default: %v", err)
	}
	got := TopoSort(g)
	// plan(0) → implement(1) → review(2) → discard(3),merge(3) → close(4)
	want := []string{"plan", "implement", "review", "discard", "merge", "close"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: expected %s, got %s (full: %v)", i, want[i], got[i], got)
		}
	}
}

func TestStepOrderMap_Basic(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {},
			"b": {Needs: []string{"a"}},
			"c": {Needs: []string{"b"}, Terminal: true},
		},
	}
	m := StepOrderMap(g)
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	if m["a"] != 0 || m["b"] != 1 || m["c"] != 2 {
		t.Fatalf("expected a=0, b=1, c=2, got %v", m)
	}
}

func TestStepOrderMap_Nil(t *testing.T) {
	if m := StepOrderMap(nil); m != nil {
		t.Fatalf("expected nil, got %v", m)
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

func TestEntryStep_ExplicitEntry(t *testing.T) {
	g := &FormulaStepGraph{
		Entry: "start",
		Steps: map[string]StepConfig{
			"start":  {Terminal: false},
			"finish": {Needs: []string{"start"}, Terminal: true},
		},
	}
	if got := EntryStep(g); got != "start" {
		t.Fatalf("expected start, got %s", got)
	}
}

func TestEntryStep_ExplicitEntryMissing(t *testing.T) {
	g := &FormulaStepGraph{
		Entry: "nonexistent",
		Steps: map[string]StepConfig{
			"a": {Terminal: true},
		},
	}
	if got := EntryStep(g); got != "" {
		t.Fatalf("expected empty string for missing explicit entry, got %s", got)
	}
}

func TestValidateGraph_ExplicitEntry(t *testing.T) {
	// With explicit Entry, multiple needless steps are allowed.
	g := &FormulaStepGraph{
		Entry: "start",
		Steps: map[string]StepConfig{
			"start":  {},
			"alt":    {},
			"finish": {Needs: []string{"start"}, Terminal: true},
		},
	}
	if err := ValidateGraph(g); err != nil {
		t.Fatalf("expected valid graph with explicit entry, got: %s", err)
	}
}

func TestValidateGraph_ExplicitEntryNotInSteps(t *testing.T) {
	g := &FormulaStepGraph{
		Entry: "nonexistent",
		Steps: map[string]StepConfig{
			"a": {Terminal: true},
		},
	}
	err := ValidateGraph(g)
	if err == nil {
		t.Fatal("expected error for explicit entry not in steps")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGraph_InvalidWorkspaceRef(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {Workspace: "missing", Terminal: true},
		},
	}
	err := ValidateGraph(g)
	if err == nil {
		t.Fatal("expected error for workspace ref with no workspaces declared")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGraph_InvalidWorkspaceRefWithDecls(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {Workspace: "missing", Terminal: true},
		},
		Workspaces: map[string]WorkspaceDecl{
			"main": {Kind: WorkspaceKindRepo},
		},
	}
	err := ValidateGraph(g)
	if err == nil {
		t.Fatal("expected error for undeclared workspace ref")
	}
	if !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGraph_InvalidOpcode(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {Action: "nonexistent.opcode", Terminal: true},
		},
	}
	err := ValidateGraph(g)
	if err == nil {
		t.Fatal("expected error for invalid opcode")
	}
	if !strings.Contains(err.Error(), "invalid action") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGraph_InvalidStepKind(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {Kind: "invalid", Terminal: true},
		},
	}
	err := ValidateGraph(g)
	if err == nil {
		t.Fatal("expected error for invalid step kind")
	}
	if !strings.Contains(err.Error(), "invalid kind") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGraph_WhenAndConditionCollision(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {},
			"b": {
				Needs:     []string{"a"},
				Condition: "verdict == approve",
				When:      &StructuredCondition{All: []Predicate{{Left: "verdict", Op: "eq", Right: "approve"}}},
				Terminal:  true,
			},
		},
	}
	err := ValidateGraph(g)
	if err == nil {
		t.Fatal("expected error for when+condition collision")
	}
	if !strings.Contains(err.Error(), "both when and condition") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGraph_InvalidVarType(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {Terminal: true},
		},
		Vars: map[string]FormulaVar{
			"x": {Type: "float64"},
		},
	}
	err := ValidateGraph(g)
	if err == nil {
		t.Fatal("expected error for invalid var type")
	}
	if !strings.Contains(err.Error(), "invalid type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGraph_InvalidPredicateOp(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {},
			"b": {
				Needs: []string{"a"},
				When: &StructuredCondition{
					All: []Predicate{{Left: "x", Op: "like", Right: "y"}},
				},
				Terminal: true,
			},
		},
	}
	err := ValidateGraph(g)
	if err == nil {
		t.Fatal("expected error for invalid predicate op")
	}
	if !strings.Contains(err.Error(), "invalid op") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGraph_ValidV3Fields(t *testing.T) {
	g := &FormulaStepGraph{
		Entry: "check",
		Steps: map[string]StepConfig{
			"check": {
				Kind:   StepKindOp,
				Action: OpcodeCheckDesignLinked,
			},
			"plan": {
				Kind:      StepKindCall,
				Action:    OpcodeWizardRun,
				Flow:      "plan",
				Needs:     []string{"check"},
				Workspace: "impl",
				Produces:  []string{"plan_bead_id"},
			},
			"finish": {
				Kind:     StepKindOp,
				Action:   OpcodeBeadFinish,
				Needs:    []string{"plan"},
				Terminal: true,
			},
		},
		Vars: map[string]FormulaVar{
			"bead_id":     {Type: VarTypeBeadID, Required: true},
			"max_rounds":  {Type: VarTypeInt, Default: "3"},
			"auto_merge":  {Type: VarTypeBool},
			"description": {Type: VarTypeString},
			"untyped":     {Required: false},
		},
		Workspaces: map[string]WorkspaceDecl{
			"impl": {Kind: WorkspaceKindOwnedWorktree, Scope: WorkspaceScopeRun, Cleanup: WorkspaceCleanupTerminal},
		},
	}
	if err := ValidateGraph(g); err != nil {
		t.Fatalf("expected valid v3 graph, got: %s", err)
	}
}

func TestValidateGraph_ReviewGraphBackwardCompat(t *testing.T) {
	// The existing review graph must still validate unchanged.
	g := reviewGraph()
	if err := ValidateGraph(g); err != nil {
		t.Fatalf("review graph should still validate: %s", err)
	}
}

func TestValidateGraph_InvalidWorkspaceKind(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {Terminal: true},
		},
		Workspaces: map[string]WorkspaceDecl{
			"bad": {Kind: "container"},
		},
	}
	err := ValidateGraph(g)
	if err == nil {
		t.Fatal("expected error for invalid workspace kind")
	}
	if !strings.Contains(err.Error(), "invalid kind") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGraph_WorkspaceKindRequired(t *testing.T) {
	g := &FormulaStepGraph{
		Steps: map[string]StepConfig{
			"a": {Terminal: true},
		},
		Workspaces: map[string]WorkspaceDecl{
			"bad": {Scope: WorkspaceScopeStep},
		},
	}
	err := ValidateGraph(g)
	if err == nil {
		t.Fatal("expected error for workspace with no kind")
	}
	if !strings.Contains(err.Error(), "invalid kind") {
		t.Fatalf("unexpected error: %v", err)
	}
}
