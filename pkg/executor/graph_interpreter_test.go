package executor

import (
	"fmt"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
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
