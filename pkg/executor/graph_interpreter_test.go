package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// testGraphDeps returns mock deps suitable for graph interpreter tests.
// The returned actionLog captures dispatched action calls.
func testGraphDeps(t *testing.T) (*Deps, *[]string) {
	t.Helper()
	dir := t.TempDir()
	actionLog := &[]string{}

	stepBeadCounter := 0
	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		CreateStepBead: func(parentID, stepName string) (string, error) {
			stepBeadCounter++
			return fmt.Sprintf("step-%d", stepBeadCounter), nil
		},
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		HookStepBead:     func(stepID string) error { return nil },
		UnhookStepBead:   func(stepID string) error { return nil },
		UpdateBead:       func(id string, updates map[string]interface{}) error { return nil },
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			return "attempt-1", nil
		},
		CloseAttemptBead: func(attemptID, result string) error { return nil },
		GetActiveAttempt: func(parentID string) (*Bead, error) { return nil, nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return ".", "", "main", nil
		},
		ResolveBranch: func(beadID string) string { return "feat/" + beadID },
		RegistryAdd:    func(entry agent.Entry) error { return nil },
		RegistryRemove: func(name string) error { return nil },
		RegisterSelf:   func(name, beadID, phase string) func() { return func() {} },
		HasLabel: func(b Bead, prefix string) string { return "" },
		ContainsLabel: func(b Bead, label string) bool { return false },
		AddLabel:    func(id, label string) error { return nil },
		RemoveLabel: func(id, label string) error { return nil },
		CloseBead:   func(id string) error { return nil },
		CreateBead:  func(opts CreateOpts) (string, error) { return "alert-1", nil },
		AddComment:  func(id, text string) error { return nil },
		AddDepTyped: func(from, to, depType string) error { return nil },
	}

	return deps, actionLog
}

// --- Test: Linear graph (a -> b -> c, c is terminal) ---

func TestRunGraph_Linear(t *testing.T) {
	deps, _ := testGraphDeps(t)

	// Track which actions were dispatched in order.
	var dispatched []string

	// Register a test action that just records the call.
	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-linear",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop"},
			"b": {Action: "test.noop", Needs: []string{"a"}},
			"c": {Action: "test.noop", Needs: []string{"b"}, Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph returned error: %v", err)
	}

	if !exec.terminated {
		t.Error("expected executor to be terminated")
	}

	// Verify all steps completed in order.
	if len(dispatched) != 3 {
		t.Fatalf("expected 3 dispatched steps, got %d: %v", len(dispatched), dispatched)
	}
	if dispatched[0] != "a" || dispatched[1] != "b" || dispatched[2] != "c" {
		t.Errorf("expected dispatch order [a, b, c], got %v", dispatched)
	}

	// Verify final state.
	for _, name := range []string{"a", "b", "c"} {
		ss := exec.graphState.Steps[name]
		if ss.Status != "completed" {
			t.Errorf("step %s: expected completed, got %s", name, ss.Status)
		}
	}
}

// --- Test: Branching graph with conditions ---

func TestRunGraph_BranchingCondition(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var dispatched []string

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.produce"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"choice": "left"}}
	}

	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-branch",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Action: "test.produce"},
			"left": {
				Action:    "test.noop",
				Needs:     []string{"entry"},
				Condition: "steps.entry.outputs.choice == left",
				Terminal:  true,
			},
			"right": {
				Action:    "test.noop",
				Needs:     []string{"entry"},
				Condition: "steps.entry.outputs.choice == right",
				Terminal:  true,
			},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph returned error: %v", err)
	}

	if !exec.terminated {
		t.Error("expected executor to be terminated")
	}

	// Only left branch should have been taken.
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatched steps, got %d: %v", len(dispatched), dispatched)
	}
	if dispatched[0] != "entry" || dispatched[1] != "left" {
		t.Errorf("expected [entry, left], got %v", dispatched)
	}

	// Left should be completed, right should still be pending.
	if exec.graphState.Steps["left"].Status != "completed" {
		t.Errorf("left: expected completed, got %s", exec.graphState.Steps["left"].Status)
	}
	if exec.graphState.Steps["right"].Status != "pending" {
		t.Errorf("right: expected pending, got %s", exec.graphState.Steps["right"].Status)
	}
}

// --- Test: Terminal detection (merge vs discard) ---

func TestRunGraph_TerminalDetection(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var dispatched []string

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"decision": "discard"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-terminal",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"decide": {Action: "test.noop"},
			"merge": {
				Action:    "test.noop",
				Needs:     []string{"decide"},
				Condition: "steps.decide.outputs.decision == merge",
				Terminal:  true,
			},
			"discard": {
				Action:    "test.noop",
				Needs:     []string{"decide"},
				Condition: "steps.decide.outputs.decision == discard",
				Terminal:  true,
			},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph returned error: %v", err)
	}

	// Should have taken the discard path.
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatched, got %d: %v", len(dispatched), dispatched)
	}
	if dispatched[1] != "discard" {
		t.Errorf("expected discard terminal, got %s", dispatched[1])
	}

	if exec.graphState.Steps["merge"].Status != "pending" {
		t.Errorf("merge: expected pending, got %s", exec.graphState.Steps["merge"].Status)
	}
}

// --- Test: Resume from persisted state ---

func TestRunGraph_Resume(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var dispatched []string

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-resume",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop"},
			"b": {Action: "test.noop", Needs: []string{"a"}},
			"c": {Action: "test.noop", Needs: []string{"b"}, Terminal: true},
		},
	}

	// Pre-populate state with "a" already completed.
	state := NewGraphState(graph, "spi-test", "wizard-test")
	state.Steps["a"] = StepState{
		Status:  "completed",
		Outputs: map[string]string{"done": "true"},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, state, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph returned error: %v", err)
	}

	// Step "a" should NOT have been re-dispatched.
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatched (b, c), got %d: %v", len(dispatched), dispatched)
	}
	if dispatched[0] != "b" || dispatched[1] != "c" {
		t.Errorf("expected [b, c], got %v", dispatched)
	}
}

// --- Test: Stuck detection ---

func TestRunGraph_StuckDetection(t *testing.T) {
	deps, _ := testGraphDeps(t)

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Outputs: map[string]string{"result": "bad"}}
	}

	// Graph where after entry, both branches have unsatisfiable conditions.
	graph := &formula.FormulaStepGraph{
		Name:    "test-stuck",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Action: "test.noop"},
			"path-a": {
				Action:    "test.noop",
				Needs:     []string{"entry"},
				Condition: "steps.entry.outputs.result == good",
				Terminal:  true,
			},
			"path-b": {
				Action:    "test.noop",
				Needs:     []string{"entry"},
				Condition: "steps.entry.outputs.result == great",
				Terminal:  true,
			},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err == nil {
		t.Fatal("expected error for stuck graph, got nil")
	}

	if exec.terminated {
		t.Error("should not be terminated on stuck graph")
	}

	// Verify the error mentions "stuck".
	errStr := err.Error()
	if !(contains(errStr, "stuck") || contains(errStr, "no ready steps")) {
		t.Errorf("expected stuck error, got: %s", errStr)
	}
}

// --- Test: Action dispatch ---

func TestDispatchAction_RoutesToCorrectHandler(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var called string
	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.alpha"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		called = "alpha"
		return ActionResult{Outputs: map[string]string{"handler": "alpha"}}
	}
	actionRegistry["test.beta"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		called = "beta"
		return ActionResult{Outputs: map[string]string{"handler": "beta"}}
	}

	exec := NewGraphForTest("spi-test", "wizard-test", nil, nil, deps)

	// Dispatch alpha.
	result := exec.dispatchAction("s1", StepConfig{Action: "test.alpha"}, &GraphState{})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if called != "alpha" {
		t.Errorf("expected alpha handler, got %s", called)
	}
	if result.Outputs["handler"] != "alpha" {
		t.Errorf("expected alpha output, got %v", result.Outputs)
	}

	// Dispatch beta.
	result = exec.dispatchAction("s2", StepConfig{Action: "test.beta"}, &GraphState{})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if called != "beta" {
		t.Errorf("expected beta handler, got %s", called)
	}

	// Dispatch unknown.
	result = exec.dispatchAction("s3", StepConfig{Action: "test.unknown"}, &GraphState{})
	if result.Error == nil {
		t.Error("expected error for unknown action")
	}
}

// --- Test: buildConditionContext ---

func TestBuildConditionContext(t *testing.T) {
	deps, _ := testGraphDeps(t)
	exec := NewGraphForTest("spi-test", "wizard-test", nil, nil, deps)

	state := &GraphState{
		Steps: map[string]StepState{
			"review": {
				Status:  "completed",
				Outputs: map[string]string{"verdict": "approve", "score": "90"},
			},
			"build": {
				Status:  "completed",
				Outputs: map[string]string{"result": "passed"},
			},
		},
		Counters: map[string]int{
			"round":      2,
			"max_rounds": 3,
		},
		Vars: map[string]string{
			"target_branch": "main",
		},
	}

	ctx := exec.buildConditionContext(state)

	// Check step outputs.
	if ctx["steps.review.outputs.verdict"] != "approve" {
		t.Errorf("expected approve, got %s", ctx["steps.review.outputs.verdict"])
	}
	if ctx["steps.build.outputs.result"] != "passed" {
		t.Errorf("expected passed, got %s", ctx["steps.build.outputs.result"])
	}

	// Check counters (both forms).
	if ctx["state.counters.round"] != "2" {
		t.Errorf("expected 2, got %s", ctx["state.counters.round"])
	}
	if ctx["round"] != "2" {
		t.Errorf("expected short-form round=2, got %s", ctx["round"])
	}
	if ctx["max_rounds"] != "3" {
		t.Errorf("expected short-form max_rounds=3, got %s", ctx["max_rounds"])
	}

	// Check vars (both forms).
	if ctx["vars.target_branch"] != "main" {
		t.Errorf("expected main, got %s", ctx["vars.target_branch"])
	}
	if ctx["target_branch"] != "main" {
		t.Errorf("expected short-form target_branch=main, got %s", ctx["target_branch"])
	}

	// Check step status.
	if ctx["steps.review.status"] != "completed" {
		t.Errorf("expected completed, got %s", ctx["steps.review.status"])
	}
}

// --- Test: GraphState Save/Load round-trip ---

func TestGraphState_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	graph := &formula.FormulaStepGraph{
		Name:    "test-roundtrip",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop"},
			"b": {Action: "test.noop", Needs: []string{"a"}, Terminal: true},
		},
	}

	state := NewGraphState(graph, "spi-rt", "wizard-rt")
	state.Steps["a"] = StepState{
		Status:  "completed",
		Outputs: map[string]string{"key": "value"},
	}
	state.Counters["round"] = 5
	state.Vars["foo"] = "bar"

	if err := state.Save("wizard-rt", configDirFn); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	loaded, err := LoadGraphState("wizard-rt", configDirFn)
	if err != nil {
		t.Fatalf("LoadGraphState error: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil state after save")
	}

	if loaded.BeadID != "spi-rt" {
		t.Errorf("BeadID = %q, want %q", loaded.BeadID, "spi-rt")
	}
	if loaded.Steps["a"].Status != "completed" {
		t.Errorf("step a status = %q, want completed", loaded.Steps["a"].Status)
	}
	if loaded.Steps["a"].Outputs["key"] != "value" {
		t.Errorf("step a output key = %q, want value", loaded.Steps["a"].Outputs["key"])
	}
	if loaded.Counters["round"] != 5 {
		t.Errorf("counter round = %d, want 5", loaded.Counters["round"])
	}
	if loaded.Vars["foo"] != "bar" {
		t.Errorf("var foo = %q, want bar", loaded.Vars["foo"])
	}
}

// --- Test: RunNestedGraph (no cleanup side effects) ---

func TestRunNestedGraph_Linear(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var dispatched []string

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	subGraph := &formula.FormulaStepGraph{
		Name:    "test-nested",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"x": {Action: "test.noop"},
			"y": {Action: "test.noop", Needs: []string{"x"}, Terminal: true},
		},
	}

	subState := NewGraphState(subGraph, "spi-test", "wizard-test-nested")
	subState.RepoPath = "."
	subState.BaseBranch = "main"

	// Create parent executor.
	exec := NewGraphForTest("spi-test", "wizard-test", nil, nil, deps)

	// RunNestedGraph should NOT modify exec.terminated.
	exec.terminated = false
	err := exec.RunNestedGraph(subGraph, subState)
	if err != nil {
		t.Fatalf("RunNestedGraph returned error: %v", err)
	}

	// Parent executor should NOT be terminated.
	if exec.terminated {
		t.Error("RunNestedGraph should not set parent executor to terminated")
	}

	// Sub-graph steps should be completed.
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatched, got %d: %v", len(dispatched), dispatched)
	}
	if dispatched[0] != "x" || dispatched[1] != "y" {
		t.Errorf("expected [x, y], got %v", dispatched)
	}

	if subState.Steps["y"].Status != "completed" {
		t.Errorf("sub-graph terminal step y: expected completed, got %s", subState.Steps["y"].Status)
	}
}

// --- Test: actionGraphRun dispatches nested graph ---

func TestActionGraphRun_NestedGraph(t *testing.T) {
	deps, _ := testGraphDeps(t)

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	// The subgraph-review graph uses role-based steps (no action field for most).
	// For this test, register test handlers and use a custom graph that
	// graph.run can load. Instead, we test actionGraphRun with a manually
	// constructed graph by temporarily registering it.

	// Override actionGraphRun to use a test graph instead of loading from embedded.
	testSubGraph := &formula.FormulaStepGraph{
		Name:    "test-sub",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Action: "test.produce-merge"},
			"merge": {
				Action:    "test.noop",
				Needs:     []string{"entry"},
				Condition: "steps.entry.outputs.verdict == approve",
				Terminal:  true,
			},
			"discard": {
				Action:    "test.noop",
				Needs:     []string{"entry"},
				Condition: "steps.entry.outputs.verdict == reject",
				Terminal:  true,
			},
		},
	}

	actionRegistry["test.produce-merge"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Outputs: map[string]string{"verdict": "approve"}}
	}
	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	// Build a parent graph that calls graph.run.
	parentGraph := &formula.FormulaStepGraph{
		Name:    "test-parent",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"start": {Action: "test.noop"},
			"review": {
				Kind:   "call",
				Action: "graph.run",
				Needs:  []string{"start"},
				Graph:  "test-sub", // will be loaded by actionGraphRun
			},
			"done": {
				Action:   "test.noop",
				Needs:    []string{"review"},
				Terminal: true,
			},
		},
	}

	// We can't easily mock LoadStepGraphByName, so instead test RunNestedGraph
	// directly to verify the plumbing works.
	exec := NewGraphForTest("spi-test", "wizard-test", parentGraph, nil, deps)

	// Test RunNestedGraph directly with the test sub-graph.
	subState := NewGraphState(testSubGraph, "spi-test", "wizard-test-review")
	subState.RepoPath = "."
	subState.BaseBranch = "main"

	err := exec.RunNestedGraph(testSubGraph, subState)
	if err != nil {
		t.Fatalf("RunNestedGraph error: %v", err)
	}

	// Check that merge terminal step fired (verdict=approve → merge path).
	if subState.Steps["merge"].Status != "completed" {
		t.Errorf("merge step: expected completed, got %s", subState.Steps["merge"].Status)
	}
	if subState.Steps["discard"].Status != "pending" {
		t.Errorf("discard step: expected pending, got %s", subState.Steps["discard"].Status)
	}

	// Verify the parent executor is not terminated.
	if exec.terminated {
		t.Error("parent executor should not be terminated after RunNestedGraph")
	}
}

// --- Test: actionMaterializePlan ---

func TestActionMaterializePlan_NoChildren(t *testing.T) {
	deps, _ := testGraphDeps(t)
	deps.IsAttemptBead = func(b Bead) bool { return false }
	deps.IsStepBead = func(b Bead) bool { return false }
	deps.IsReviewRoundBead = func(b Bead) bool { return false }

	exec := NewGraphForTest("spi-test", "wizard-test", nil, nil, deps)
	state := &GraphState{Vars: map[string]string{}}

	result := actionMaterializePlan(exec, "materialize", StepConfig{}, state)
	if result.Error == nil {
		t.Fatal("expected error when no children exist")
	}
	if !containsSubstr(result.Error.Error(), "no subtask beads found") {
		t.Errorf("expected 'no subtask beads found' error, got: %s", result.Error)
	}
}

func TestActionMaterializePlan_WithChildren(t *testing.T) {
	deps, _ := testGraphDeps(t)
	deps.GetChildren = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-test.1", Type: "task", Title: "Subtask 1"},
			{ID: "spi-test.2", Type: "task", Title: "Subtask 2"},
			{ID: "step-1", Type: "task", Title: "Step bead"},     // internal
			{ID: "attempt-1", Type: "task", Title: "Attempt bead"}, // internal
		}, nil
	}
	deps.IsAttemptBead = func(b Bead) bool { return b.ID == "attempt-1" }
	deps.IsStepBead = func(b Bead) bool { return b.ID == "step-1" }
	deps.IsReviewRoundBead = func(b Bead) bool { return false }

	exec := NewGraphForTest("spi-test", "wizard-test", nil, nil, deps)
	state := &GraphState{Vars: map[string]string{}}

	result := actionMaterializePlan(exec, "materialize", StepConfig{}, state)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Outputs["child_count"] != "2" {
		t.Errorf("expected child_count=2, got %s", result.Outputs["child_count"])
	}
	if result.Outputs["status"] != "pass" {
		t.Errorf("expected status=pass, got %s", result.Outputs["status"])
	}
}

// --- Test: actionWizardRun routes task-plan flow ---

func TestActionWizardRun_TaskPlan(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var claudeCalledWith []string
	var commentAdded string

	deps.GetComments = func(id string) ([]*beads.Comment, error) {
		return nil, nil
	}
	deps.GetDepsWithMeta = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil
	}
	deps.ClaudeRunner = func(args []string, dir string) ([]byte, error) {
		claudeCalledWith = args
		return []byte("**Approach:** Do the thing\n\n**Steps:**\n1. Change foo.go"), nil
	}
	deps.AddComment = func(id, text string) error {
		commentAdded = text
		return nil
	}

	exec := NewGraphForTest("spi-test", "wizard-test", nil, nil, deps)
	// Set graphState so effectiveRepoPath() returns something sensible.
	exec.graphState = &GraphState{RepoPath: "/tmp/test-repo"}

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "task-plan",
		Model:  "test-model",
	}
	state := &GraphState{RepoPath: "/tmp/test-repo"}

	result := actionWizardRun(exec, "plan", step, state)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Outputs["status"] != "planned" {
		t.Errorf("expected status=planned, got %s", result.Outputs["status"])
	}

	// Verify Claude was invoked (not a subprocess spawn).
	if len(claudeCalledWith) == 0 {
		t.Fatal("expected ClaudeRunner to be called")
	}

	// Verify a plan comment was posted.
	if commentAdded == "" {
		t.Error("expected a comment to be added with the plan")
	}
	if !containsSubstr(commentAdded, "Implementation plan:") {
		t.Errorf("expected comment to start with 'Implementation plan:', got: %s", commentAdded)
	}
}

// --- Test: actionWizardRun routes epic-plan flow ---

func TestActionWizardRun_EpicPlan(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var claudeCalledWith []string
	var createdBeads []string
	var commentAdded string

	deps.GetComments = func(id string) ([]*beads.Comment, error) {
		return nil, nil
	}
	deps.GetDepsWithMeta = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil
	}
	deps.IsAttemptBead = func(b Bead) bool { return false }
	deps.IsStepBead = func(b Bead) bool { return false }
	deps.IsReviewRoundBead = func(b Bead) bool { return false }
	deps.ClaudeRunner = func(args []string, dir string) ([]byte, error) {
		claudeCalledWith = args
		// Return two subtask JSON lines.
		return []byte(`{"title": "Task A", "description": "Do A", "deps": [], "shared_files": [], "do_not_touch": []}
{"title": "Task B", "description": "Do B", "deps": ["Task A"], "shared_files": [], "do_not_touch": []}`), nil
	}
	beadCounter := 0
	deps.CreateBead = func(opts CreateOpts) (string, error) {
		beadCounter++
		createdBeads = append(createdBeads, opts.Title)
		return fmt.Sprintf("spi-test.%d", beadCounter), nil
	}
	deps.AddDep = func(issueID, dependsOnID string) error { return nil }
	deps.AddComment = func(id, text string) error {
		if id == "spi-test" {
			commentAdded = text
		}
		return nil
	}
	deps.ParseIssueType = func(s string) beads.IssueType {
		return beads.IssueType(s)
	}

	exec := NewGraphForTest("spi-test", "wizard-test", nil, nil, deps)
	exec.graphState = &GraphState{RepoPath: "/tmp/test-repo"}

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "epic-plan",
		Model:  "test-model",
	}
	state := &GraphState{RepoPath: "/tmp/test-repo"}

	result := actionWizardRun(exec, "plan", step, state)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Outputs["status"] != "planned" {
		t.Errorf("expected status=planned, got %s", result.Outputs["status"])
	}

	// Verify Claude was invoked inline.
	if len(claudeCalledWith) == 0 {
		t.Fatal("expected ClaudeRunner to be called")
	}

	// Verify subtask beads were created.
	if len(createdBeads) != 2 {
		t.Fatalf("expected 2 subtasks created, got %d: %v", len(createdBeads), createdBeads)
	}
	if createdBeads[0] != "Task A" || createdBeads[1] != "Task B" {
		t.Errorf("expected [Task A, Task B], got %v", createdBeads)
	}

	// Verify plan summary was posted.
	if commentAdded == "" {
		t.Error("expected a plan summary comment")
	}
	if !containsSubstr(commentAdded, "Wizard plan:") {
		t.Errorf("expected 'Wizard plan:' in comment, got: %s", commentAdded)
	}
}

// --- Test: effectiveRepoPath ---

func TestEffectiveRepoPath_GraphState(t *testing.T) {
	deps, _ := testGraphDeps(t)
	exec := NewGraphForTest("spi-test", "wizard-test", nil, nil, deps)
	exec.graphState = &GraphState{RepoPath: "/path/from/graph"}

	if got := exec.effectiveRepoPath(); got != "/path/from/graph" {
		t.Errorf("effectiveRepoPath() = %q, want /path/from/graph", got)
	}
}

func TestEffectiveRepoPath_FallbackToDot(t *testing.T) {
	deps, _ := testGraphDeps(t)
	exec := NewGraphForTest("spi-test", "wizard-test", nil, nil, deps)
	// Neither state nor graphState is set (graphState is nil from no graph).

	if got := exec.effectiveRepoPath(); got != "." {
		t.Errorf("effectiveRepoPath() = %q, want \".\"", got)
	}
}

// --- Test: Vars initialized from formula defaults ---

func TestRunGraph_VarsInitializedFromFormula(t *testing.T) {
	deps, _ := testGraphDeps(t)

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	var capturedVars map[string]string
	actionRegistry["test.capture-vars"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		capturedVars = make(map[string]string)
		for k, v := range state.Vars {
			capturedVars[k] = v
		}
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-vars-init",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Action: "test.capture-vars", Terminal: true},
		},
		Vars: map[string]formula.FormulaVar{
			"max_review_rounds": {Default: "3", Type: "int", Description: "max review rounds"},
			"target_branch":     {Default: "main", Type: "string", Description: "target branch"},
			"no_default":        {Type: "string", Description: "no default value", Required: true},
		},
	}

	exec := NewGraphForTest("spi-vars", "wizard-vars", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph returned error: %v", err)
	}

	// Verify vars were initialized from defaults.
	if capturedVars["max_review_rounds"] != "3" {
		t.Errorf("max_review_rounds = %q, want %q", capturedVars["max_review_rounds"], "3")
	}
	if capturedVars["target_branch"] != "main" {
		t.Errorf("target_branch = %q, want %q", capturedVars["target_branch"], "main")
	}
	// Vars with no default should not be set.
	if _, ok := capturedVars["no_default"]; ok {
		t.Errorf("no_default should not be set, got %q", capturedVars["no_default"])
	}
	// bead_id should always be set.
	if capturedVars["bead_id"] != "spi-vars" {
		t.Errorf("bead_id = %q, want %q", capturedVars["bead_id"], "spi-vars")
	}
}

// --- Test: Workspaces initialized from formula declarations ---

func TestRunGraph_WorkspacesInitializedFromFormula(t *testing.T) {
	deps, _ := testGraphDeps(t)

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	var capturedWorkspaces map[string]WorkspaceState
	actionRegistry["test.capture-ws"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		capturedWorkspaces = make(map[string]WorkspaceState)
		for k, v := range state.Workspaces {
			capturedWorkspaces[k] = v
		}
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-ws-init",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Action: "test.capture-ws", Terminal: true},
		},
		Workspaces: map[string]formula.WorkspaceDecl{
			"staging": {
				Kind:   "staging",
				Branch: "staging/{vars.bead_id}",
				Base:   "{vars.base_branch}",
			},
			"repo": {
				Kind: "repo",
			},
		},
	}

	exec := NewGraphForTest("spi-ws", "wizard-ws", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph returned error: %v", err)
	}

	// Verify staging workspace was initialized with defaults applied.
	staging, ok := capturedWorkspaces["staging"]
	if !ok {
		t.Fatal("staging workspace not found in state")
	}
	if staging.Kind != "staging" {
		t.Errorf("staging.Kind = %q, want %q", staging.Kind, "staging")
	}
	if staging.Branch != "staging/{vars.bead_id}" {
		t.Errorf("staging.Branch = %q, want %q", staging.Branch, "staging/{vars.bead_id}")
	}
	if staging.Status != "pending" {
		t.Errorf("staging.Status = %q, want %q", staging.Status, "pending")
	}
	// Defaults should have been applied.
	if staging.Scope != "run" {
		t.Errorf("staging.Scope = %q, want %q (default)", staging.Scope, "run")
	}
	if staging.Ownership != "owned" {
		t.Errorf("staging.Ownership = %q, want %q (default)", staging.Ownership, "owned")
	}
	if staging.Cleanup != "terminal" {
		t.Errorf("staging.Cleanup = %q, want %q (default)", staging.Cleanup, "terminal")
	}

	// Verify repo workspace.
	repo, ok := capturedWorkspaces["repo"]
	if !ok {
		t.Fatal("repo workspace not found in state")
	}
	if repo.Kind != "repo" {
		t.Errorf("repo.Kind = %q, want %q", repo.Kind, "repo")
	}
	if repo.Status != "pending" {
		t.Errorf("repo.Status = %q, want %q", repo.Status, "pending")
	}
}

// --- Test: Resume preserves existing vars and workspaces ---

func TestRunGraph_ResumePreservesVarsAndWorkspaces(t *testing.T) {
	deps, _ := testGraphDeps(t)

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	var capturedVars map[string]string
	var capturedWorkspaces map[string]WorkspaceState
	actionRegistry["test.capture-state"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		capturedVars = make(map[string]string)
		for k, v := range state.Vars {
			capturedVars[k] = v
		}
		capturedWorkspaces = make(map[string]WorkspaceState)
		for k, v := range state.Workspaces {
			capturedWorkspaces[k] = v
		}
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-resume-state",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.capture-state"},
			"b": {Action: "test.capture-state", Needs: []string{"a"}, Terminal: true},
		},
		Vars: map[string]formula.FormulaVar{
			"max_rounds": {Default: "3", Type: "int"},
		},
		Workspaces: map[string]formula.WorkspaceDecl{
			"staging": {Kind: "staging", Branch: "staging/{vars.bead_id}"},
		},
	}

	// Pre-populate state as if resuming mid-execution.
	state := NewGraphState(graph, "spi-resume", "wizard-resume")
	state.Steps["a"] = StepState{
		Status:  "completed",
		Outputs: map[string]string{"done": "true"},
	}
	// Set vars as if they were initialized in a previous run.
	state.Vars["max_rounds"] = "5" // overridden from default of "3"
	state.Vars["bead_id"] = "spi-resume"
	state.Vars["custom"] = "value" // extra var set at runtime
	// Set workspace as if it was resolved in a previous run.
	state.Workspaces["staging"] = WorkspaceState{
		Name:   "staging",
		Kind:   "staging",
		Branch: "staging/spi-resume",
		Dir:    "/tmp/some-worktree",
		Status: "active",
		Scope:  "run",
	}

	exec := NewGraphForTest("spi-resume", "wizard-resume", graph, state, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph returned error: %v", err)
	}

	// Vars should be preserved (not re-initialized from defaults).
	if capturedVars["max_rounds"] != "5" {
		t.Errorf("max_rounds = %q, want %q (preserved, not reset to default)", capturedVars["max_rounds"], "5")
	}
	if capturedVars["bead_id"] != "spi-resume" {
		t.Errorf("bead_id = %q, want %q", capturedVars["bead_id"], "spi-resume")
	}
	if capturedVars["custom"] != "value" {
		t.Errorf("custom = %q, want %q (preserved)", capturedVars["custom"], "value")
	}

	// Workspaces should be preserved (not re-initialized).
	ws, ok := capturedWorkspaces["staging"]
	if !ok {
		t.Fatal("staging workspace not found after resume")
	}
	if ws.Dir != "/tmp/some-worktree" {
		t.Errorf("staging.Dir = %q, want %q (preserved)", ws.Dir, "/tmp/some-worktree")
	}
	if ws.Status != "active" {
		t.Errorf("staging.Status = %q, want %q (preserved)", ws.Status, "active")
	}
	if ws.Branch != "staging/spi-resume" {
		t.Errorf("staging.Branch = %q, want %q (preserved)", ws.Branch, "staging/spi-resume")
	}
}

func TestRunNestedGraph_InitializesMissingDeclaredWorkspaces(t *testing.T) {
	deps, _ := testGraphDeps(t)

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	var capturedWorkspaces map[string]WorkspaceState
	actionRegistry["test.capture-ws"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		capturedWorkspaces = make(map[string]WorkspaceState)
		for k, v := range state.Workspaces {
			capturedWorkspaces[k] = v
		}
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-nested-missing-workspaces",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Action: "test.capture-ws", Terminal: true},
		},
		Workspaces: map[string]formula.WorkspaceDecl{
			"staging": {
				Kind:   formula.WorkspaceKindStaging,
				Branch: "epic/{vars.bead_id}",
				Base:   "{vars.base_branch}",
			},
			"scratch": {
				Kind:   formula.WorkspaceKindOwnedWorktree,
				Branch: "scratch/{vars.bead_id}",
				Base:   "{vars.base_branch}",
			},
		},
	}

	state := NewGraphState(graph, "spi-nested", "wizard-nested")
	state.RepoPath = "."
	state.BaseBranch = "main"
	state.StagingBranch = "epic/spi-nested"
	state.Vars["bead_id"] = "spi-nested"
	state.Workspaces["staging"] = WorkspaceState{
		Name:       "staging",
		Kind:       formula.WorkspaceKindStaging,
		Dir:        "/tmp/spi-nested-staging",
		Branch:     "epic/spi-nested",
		BaseBranch: "main",
		Status:     "active",
		Scope:      formula.WorkspaceScopeRun,
		Ownership:  "owned",
		Cleanup:    formula.WorkspaceCleanupTerminal,
	}

	exec := NewGraphForTest("spi-nested", "wizard-nested", graph, state, deps)
	if err := exec.RunNestedGraph(graph, state); err != nil {
		t.Fatalf("RunNestedGraph returned error: %v", err)
	}

	staging, ok := capturedWorkspaces["staging"]
	if !ok {
		t.Fatal("missing propagated staging workspace")
	}
	if staging.Dir != "/tmp/spi-nested-staging" {
		t.Errorf("staging.Dir = %q, want %q", staging.Dir, "/tmp/spi-nested-staging")
	}

	scratch, ok := capturedWorkspaces["scratch"]
	if !ok {
		t.Fatal("missing initialized scratch workspace")
	}
	if scratch.Kind != formula.WorkspaceKindOwnedWorktree {
		t.Errorf("scratch.Kind = %q, want %q", scratch.Kind, formula.WorkspaceKindOwnedWorktree)
	}
	if scratch.Status != "pending" {
		t.Errorf("scratch.Status = %q, want %q", scratch.Status, "pending")
	}
	if scratch.Branch != "scratch/{vars.bead_id}" {
		t.Errorf("scratch.Branch = %q, want %q", scratch.Branch, "scratch/{vars.bead_id}")
	}
}

// --- Test: Step failure emits node-scoped result with metadata ---

func TestRunGraph_StepFailure_NodeScopedResult(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var capturedResult string
	deps.CloseAttemptBead = func(attemptID, result string) error {
		capturedResult = result
		return nil
	}

	// Track escalation: labels added and alert beads created.
	var addedLabels []string
	deps.AddLabel = func(id, label string) error {
		addedLabels = append(addedLabels, label)
		return nil
	}
	var createdAlerts []string
	deps.CreateBead = func(opts CreateOpts) (string, error) {
		createdAlerts = append(createdAlerts, opts.Title)
		return "alert-1", nil
	}
	var addedComments []string
	deps.AddComment = func(id, text string) error {
		addedComments = append(addedComments, text)
		return nil
	}
	deps.AddDepTyped = func(from, to, depType string) error { return nil }

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.ok"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Outputs: map[string]string{"status": "done"}}
	}
	actionRegistry["test.fail"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Error: fmt.Errorf("subprocess exited with code 1")}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-node-scoped",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"prepare": {Action: "test.ok"},
			"implement": {
				Action:    "test.fail",
				Flow:      "implement",
				Workspace: "feature",
				Needs:     []string{"prepare"},
			},
			"done": {
				Action:   "noop",
				Needs:    []string{"implement"},
				Terminal: true,
			},
		},
	}

	exec := NewGraphForTest("test-bead", "test-agent", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("expected nil error (hooked park), got: %v", err)
	}

	// Verify the step is hooked (not failed) after escalation.
	if ss, ok := exec.graphState.Steps["implement"]; !ok || ss.Status != "hooked" {
		status := "missing"
		if ok {
			status = ss.Status
		}
		t.Errorf("expected implement step status=hooked, got %s", status)
	}

	// Failed steps are now hooked (parked), so the attempt result is the
	// park message rather than node-scoped metadata. The node-scoped info
	// goes through EscalateGraphStepFailure (labels/alerts), not the attempt result.
	if !strings.Contains(capturedResult, "parked") {
		t.Errorf("expected result to contain 'parked', got: %s", capturedResult)
	}

	// Verify escalation: parent bead gets needs-human + interrupted:step-failure.
	hasNeedsHuman := false
	hasInterrupted := false
	for _, l := range addedLabels {
		if l == "needs-human" {
			hasNeedsHuman = true
		}
		if l == "interrupted:step-failure" {
			hasInterrupted = true
		}
	}
	if !hasNeedsHuman {
		t.Errorf("expected needs-human label on parent bead, got labels: %v", addedLabels)
	}
	if !hasInterrupted {
		t.Errorf("expected interrupted:step-failure label on parent bead, got labels: %v", addedLabels)
	}

	// Verify alert bead was created with node-scoped context.
	if len(createdAlerts) == 0 {
		t.Fatal("expected alert bead to be created")
	}
	alertTitle := createdAlerts[0]
	if !strings.Contains(alertTitle, "step=implement") {
		t.Errorf("expected alert title to contain 'step=implement', got: %s", alertTitle)
	}
	if !strings.Contains(alertTitle, "action=test.fail") {
		t.Errorf("expected alert title to contain 'action=test.fail', got: %s", alertTitle)
	}

	// Verify comment includes node context.
	if len(addedComments) == 0 {
		t.Fatal("expected comment on parent bead")
	}
	if !strings.Contains(addedComments[0], "Node context:") {
		t.Errorf("expected comment to contain 'Node context:', got: %s", addedComments[0])
	}
}

// --- Test: Terminal success reconciles sibling step beads ---

func TestRunGraph_TerminalReconcilesSiblingStepBeads(t *testing.T) {
	deps, _ := testGraphDeps(t)

	// Track which step beads were closed.
	var closedStepBeads []string
	deps.CloseStepBead = func(stepID string) error {
		closedStepBeads = append(closedStepBeads, stepID)
		return nil
	}

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Outputs: map[string]string{"decision": "merge"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-reconcile",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"decide": {Action: "test.noop"},
			"merge": {
				Action:    "test.noop",
				Needs:     []string{"decide"},
				Condition: "steps.decide.outputs.decision == merge",
				Terminal:  true,
			},
			"discard": {
				Action:    "test.noop",
				Needs:     []string{"decide"},
				Condition: "steps.decide.outputs.decision == discard",
				Terminal:  true,
			},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph returned error: %v", err)
	}

	if !exec.terminated {
		t.Error("expected executor to be terminated")
	}

	// The merge path was taken: decide + merge step beads were closed via normal flow.
	// The discard step bead should also have been closed by the reconcile loop.
	discardBeadID := exec.graphState.StepBeadIDs["discard"]
	if discardBeadID == "" {
		t.Fatal("expected discard step bead to have been created")
	}

	found := false
	for _, id := range closedStepBeads {
		if id == discardBeadID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected discard step bead %s to be closed by reconcile, closed beads: %v",
			discardBeadID, closedStepBeads)
	}

	// Verify merge step bead was also closed (via normal close, not reconcile).
	mergeBeadID := exec.graphState.StepBeadIDs["merge"]
	mergeFound := false
	for _, id := range closedStepBeads {
		if id == mergeBeadID {
			mergeFound = true
			break
		}
	}
	if !mergeFound {
		t.Errorf("expected merge step bead %s to be closed, closed beads: %v",
			mergeBeadID, closedStepBeads)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- Test: resolveGraphBranchState respects bead base-branch label ---

func TestResolveGraphBranchState_BeadLabelOverride(t *testing.T) {
	deps, _ := testGraphDeps(t)

	// ResolveRepo returns "main" as the default base branch.
	deps.ResolveRepo = func(beadID string) (string, string, string, error) {
		return "/tmp/repo", "", "main", nil
	}

	// Bead has a base-branch:develop label from spire file --branch.
	deps.HasLabel = func(b Bead, prefix string) string {
		if prefix == "base-branch:" {
			return "develop"
		}
		return ""
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-branch-override",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop", Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-bb", "wizard-bb", graph, nil, deps)
	err := exec.resolveGraphBranchState(graph, exec.graphState)
	if err != nil {
		t.Fatalf("resolveGraphBranchState: %v", err)
	}

	if exec.graphState.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want %q", exec.graphState.BaseBranch, "develop")
	}
}

func TestResolveGraphBranchState_NoLabelUsesRepoDefault(t *testing.T) {
	deps, _ := testGraphDeps(t)

	deps.ResolveRepo = func(beadID string) (string, string, string, error) {
		return "/tmp/repo", "", "main", nil
	}

	// No base-branch label on bead.
	deps.HasLabel = func(b Bead, prefix string) string { return "" }

	graph := &formula.FormulaStepGraph{
		Name:    "test-no-override",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop", Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-no", "wizard-no", graph, nil, deps)
	err := exec.resolveGraphBranchState(graph, exec.graphState)
	if err != nil {
		t.Fatalf("resolveGraphBranchState: %v", err)
	}

	if exec.graphState.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q", exec.graphState.BaseBranch, "main")
	}
}

func TestResolveGraphBranchState_ResumeSkipsOverride(t *testing.T) {
	deps, _ := testGraphDeps(t)

	// HasLabel should NOT be called on resume path.
	labelCalled := false
	deps.HasLabel = func(b Bead, prefix string) string {
		if prefix == "base-branch:" {
			labelCalled = true
			return "develop"
		}
		return ""
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-resume",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop", Terminal: true},
		},
	}

	// Pre-populate state to simulate resume.
	exec := NewGraphForTest("spi-res", "wizard-res", graph, nil, deps)
	exec.graphState.RepoPath = "/tmp/repo"
	exec.graphState.BaseBranch = "main"
	exec.graphState.StagingBranch = "staging/spi-res"

	err := exec.resolveGraphBranchState(graph, exec.graphState)
	if err != nil {
		t.Fatalf("resolveGraphBranchState: %v", err)
	}

	if labelCalled {
		t.Error("base-branch label check should be skipped on resume")
	}
	if exec.graphState.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q (resume should preserve)", exec.graphState.BaseBranch, "main")
	}
}

func TestResolveGraphBranchState_InheritsFromParent(t *testing.T) {
	deps, _ := testGraphDeps(t)

	deps.ResolveRepo = func(beadID string) (string, string, string, error) {
		return "/tmp/repo", "", "main", nil
	}

	// Child bead has no label, but its parent epic does.
	deps.GetBead = func(id string) (Bead, error) {
		switch id {
		case "spi-child":
			return Bead{ID: "spi-child", Status: "in_progress", Parent: "spi-epic"}, nil
		case "spi-epic":
			return Bead{ID: "spi-epic", Status: "in_progress", Labels: []string{"base-branch:develop"}}, nil
		default:
			return Bead{ID: id, Status: "in_progress"}, nil
		}
	}
	deps.HasLabel = func(b Bead, prefix string) string {
		return store.HasLabel(b, prefix)
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-parent-inherit",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop", Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-child", "wizard-child", graph, nil, deps)
	err := exec.resolveGraphBranchState(graph, exec.graphState)
	if err != nil {
		t.Fatalf("resolveGraphBranchState: %v", err)
	}

	if exec.graphState.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want %q (should inherit from parent)", exec.graphState.BaseBranch, "develop")
	}
}

func TestResolveGraphBranchState_NoLabelInChain(t *testing.T) {
	deps, _ := testGraphDeps(t)

	deps.ResolveRepo = func(beadID string) (string, string, string, error) {
		return "/tmp/repo", "", "main", nil
	}

	// Neither child nor parent has the label.
	deps.GetBead = func(id string) (Bead, error) {
		switch id {
		case "spi-child":
			return Bead{ID: "spi-child", Status: "in_progress", Parent: "spi-epic"}, nil
		case "spi-epic":
			return Bead{ID: "spi-epic", Status: "in_progress"}, nil
		default:
			return Bead{ID: id, Status: "in_progress"}, nil
		}
	}
	deps.HasLabel = func(b Bead, prefix string) string {
		return store.HasLabel(b, prefix)
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-no-chain",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop", Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-child", "wizard-child", graph, nil, deps)
	err := exec.resolveGraphBranchState(graph, exec.graphState)
	if err != nil {
		t.Fatalf("resolveGraphBranchState: %v", err)
	}

	if exec.graphState.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q (no label in chain)", exec.graphState.BaseBranch, "main")
	}
}

// --- Test: Hooked step parks graph safely ---

func TestRunGraph_HookedParksSafely(t *testing.T) {
	deps, _ := testGraphDeps(t)

	// Track step bead closures and interruption labels to verify the defer
	// cleanup does NOT close step beads when the graph parks on a hooked step.
	var closedStepBeads []string
	var interruptLabels []string
	deps.CloseStepBead = func(stepID string) error {
		closedStepBeads = append(closedStepBeads, stepID)
		return nil
	}
	deps.AddLabel = func(id, label string) error {
		if label == "interrupted:executor-exit" {
			interruptLabels = append(interruptLabels, id)
		}
		return nil
	}

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.hook"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Hooked: true, Outputs: map[string]string{"design_ref": "spi-design1"}}
	}
	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-hooked",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.hook"},
			"b": {Action: "test.noop", Needs: []string{"a"}},
			"c": {Action: "test.noop", Needs: []string{"b"}, Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph returned error: %v", err)
	}

	// Step a should be hooked.
	if ss := exec.graphState.Steps["a"]; ss.Status != "hooked" {
		t.Errorf("step a: expected hooked, got %s", ss.Status)
	}
	// Steps b and c should still be pending.
	if ss := exec.graphState.Steps["b"]; ss.Status != "pending" {
		t.Errorf("step b: expected pending, got %s", ss.Status)
	}
	if ss := exec.graphState.Steps["c"]; ss.Status != "pending" {
		t.Errorf("step c: expected pending, got %s", ss.Status)
	}
	// Executor should NOT be terminated (graph is parked, not finished).
	if exec.terminated {
		t.Error("expected executor to NOT be terminated (graph is parked)")
	}
	// Step beads must NOT be closed when graph parks — board reads step bead
	// status for column placement (GetBoardBeadPhase via GetActiveStep).
	if len(closedStepBeads) > 0 {
		t.Errorf("expected no step beads closed on park, but got: %v", closedStepBeads)
	}
	if len(interruptLabels) > 0 {
		t.Errorf("expected no interrupted:executor-exit labels on park, but got: %v", interruptLabels)
	}
}

// --- Test: Step failure sets hooked status and exits gracefully ---

func TestRunGraph_FailedStepSetsHooked(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var dispatched []string
	var escalateCalls int

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.fail"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Error: fmt.Errorf("step %s failed", stepName)}
	}
	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	// Track escalation calls via deps.AddLabel (needs-human is added by escalation).
	deps.AddLabel = func(id, label string) error {
		if label == "needs-human" {
			escalateCalls++
		}
		return nil
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-fail-hooked",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"build": {Action: "test.fail"},
			"test":  {Action: "test.noop", Needs: []string{"build"}},
			"merge": {Action: "test.noop", Needs: []string{"test"}, Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)

	// Should exit gracefully (no error) — graph is parked, not stuck.
	if err != nil {
		t.Fatalf("expected nil error (parked graph), got: %v", err)
	}

	// The failing step should be hooked, not failed.
	buildStep := exec.graphState.Steps["build"]
	if buildStep.Status != "hooked" {
		t.Errorf("build step: expected hooked, got %s", buildStep.Status)
	}
	// CompletedAt should NOT be set for hooked steps.
	if buildStep.CompletedAt != "" {
		t.Errorf("build step: expected empty CompletedAt, got %s", buildStep.CompletedAt)
	}

	// Downstream steps should remain pending (never dispatched).
	if exec.graphState.Steps["test"].Status != "pending" {
		t.Errorf("test step: expected pending, got %s", exec.graphState.Steps["test"].Status)
	}
	if exec.graphState.Steps["merge"].Status != "pending" {
		t.Errorf("merge step: expected pending, got %s", exec.graphState.Steps["merge"].Status)
	}

	// Executor should NOT be terminated (graph is parked).
	if exec.terminated {
		t.Error("expected executor NOT to be terminated")
	}

	// The failing step should be dispatched exactly once (no infinite loop).
	buildCount := 0
	for _, name := range dispatched {
		if name == "build" {
			buildCount++
		}
	}
	if buildCount != 1 {
		t.Errorf("expected build dispatched once, got %d times (dispatched: %v)", buildCount, dispatched)
	}

	// Escalation should fire exactly once.
	if escalateCalls != 1 {
		t.Errorf("expected exactly 1 escalation call, got %d", escalateCalls)
	}
}

// --- Test: Multiple step failures, escalation fires once per step ---

func TestRunGraph_FailedStepNoInfiniteLoop(t *testing.T) {
	deps, _ := testGraphDeps(t)

	dispatchCount := 0

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	// The step always fails — if hooked logic is wrong, this will loop forever.
	actionRegistry["test.fail"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatchCount++
		if dispatchCount > 10 {
			t.Fatalf("dispatch count exceeded 10 — likely infinite loop")
		}
		return ActionResult{Error: fmt.Errorf("always fails")}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-no-loop",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.fail", Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("expected nil error (parked), got: %v", err)
	}

	// Should have been dispatched exactly once.
	if dispatchCount != 1 {
		t.Errorf("expected 1 dispatch, got %d", dispatchCount)
	}
	if exec.graphState.Steps["a"].Status != "hooked" {
		t.Errorf("expected step a to be hooked, got %s", exec.graphState.Steps["a"].Status)
	}
}

// --- Test: Hooked resume after external reset ---

func TestRunGraph_HookedResumeAfterReset(t *testing.T) {
	deps, _ := testGraphDeps(t)

	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()

	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-hooked-resume",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop"},
			"b": {Action: "test.noop", Needs: []string{"a"}, Terminal: true},
		},
	}

	// First run: pre-set step a as hooked (simulating a previously parked graph).
	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	exec.graphState.Steps["a"] = StepState{Status: "hooked", Outputs: map[string]string{"design_ref": "spi-d1"}}

	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("first RunGraph (parked) returned error: %v", err)
	}
	// Graph should park without escalation.
	if exec.terminated {
		t.Error("first run: expected executor to NOT be terminated (graph is parked)")
	}
	if ss := exec.graphState.Steps["a"]; ss.Status != "hooked" {
		t.Errorf("first run: step a expected hooked, got %s", ss.Status)
	}

	// Externally reset step a to pending (simulating steward sweep).
	ss := exec.graphState.Steps["a"]
	ss.Status = "pending"
	ss.Outputs = nil
	exec.graphState.Steps["a"] = ss

	// Second run: step a should now execute and complete.
	exec2 := NewGraphForTest("spi-test", "wizard-test", graph, exec.graphState, deps)
	err = exec2.RunGraph(graph, exec2.graphState)
	if err != nil {
		t.Fatalf("second RunGraph (resumed) returned error: %v", err)
	}

	if !exec2.terminated {
		t.Error("second run: expected executor to be terminated")
	}
	if ss := exec2.graphState.Steps["a"]; ss.Status != "completed" {
		t.Errorf("second run: step a expected completed, got %s", ss.Status)
	}
	if ss := exec2.graphState.Steps["b"]; ss.Status != "completed" {
		t.Errorf("second run: step b expected completed, got %s", ss.Status)
	}
}
