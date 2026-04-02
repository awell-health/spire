package executor

import (
	"fmt"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
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
		HasLabel: func(b Bead, prefix string) string { return "" },
		ContainsLabel: func(b Bead, label string) bool { return false },
		AddLabel:    func(id, label string) error { return nil },
		RemoveLabel: func(id, label string) error { return nil },
		CloseBead:   func(id string) error { return nil },
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

	// The review-phase graph uses role-based steps (no action field for most).
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
