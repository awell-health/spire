package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// TestV3Resume_MidGraph runs the interpreter for 2 steps, saves state, loads
// from disk, creates a new executor from loaded state, and verifies it resumes
// at step 3.
func TestV3Resume_MidGraph(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps, _ := testGraphDeps(t)
	deps.ConfigDir = configDirFn

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
		Name:    "test-resume-mid",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop"},
			"b": {Action: "test.noop", Needs: []string{"a"}},
			"c": {Action: "test.noop", Needs: []string{"b"}, Terminal: true},
		},
	}

	// --- Phase 1: Run first 2 steps, then save state ---
	state := NewGraphState(graph, "spi-resume", "wizard-resume")
	state.Steps["a"] = StepState{
		Status:  "completed",
		Outputs: map[string]string{"done": "true"},
	}
	state.Steps["b"] = StepState{
		Status:  "completed",
		Outputs: map[string]string{"done": "true"},
	}
	if err := state.Save("wizard-resume", configDirFn); err != nil {
		t.Fatalf("save state: %v", err)
	}

	// --- Phase 2: Load state from disk and resume ---
	loaded, err := LoadGraphState("wizard-resume", configDirFn)
	if err != nil {
		t.Fatalf("LoadGraphState: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil state after load")
	}

	exec2 := NewGraphForTest("spi-resume", "wizard-resume", graph, loaded, deps)
	err = exec2.RunGraph(graph, exec2.graphState)
	if err != nil {
		t.Fatalf("RunGraph on resume: %v", err)
	}

	// Only step C should have been dispatched (A and B already completed).
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatched step (c), got %d: %v", len(dispatched), dispatched)
	}
	if dispatched[0] != "c" {
		t.Errorf("expected dispatched step c, got %s", dispatched[0])
	}

	if !exec2.terminated {
		t.Error("expected executor to be terminated after terminal step c")
	}
}

// TestV3Resume_CompletedStepsSkipped marks A,B completed in state, resumes,
// and verifies execution starts at C.
func TestV3Resume_CompletedStepsSkipped(t *testing.T) {
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
		Name:    "test-skip",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop"},
			"b": {Action: "test.noop", Needs: []string{"a"}},
			"c": {Action: "test.noop", Needs: []string{"b"}},
			"d": {Action: "test.noop", Needs: []string{"c"}, Terminal: true},
		},
	}

	state := NewGraphState(graph, "spi-skip", "wizard-skip")
	state.Steps["a"] = StepState{Status: "completed", Outputs: map[string]string{"done": "true"}}
	state.Steps["b"] = StepState{Status: "completed", Outputs: map[string]string{"done": "true"}}

	exec := NewGraphForTest("spi-skip", "wizard-skip", graph, state, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}

	// Only C and D should have been dispatched.
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatched (c, d), got %d: %v", len(dispatched), dispatched)
	}
	if dispatched[0] != "c" || dispatched[1] != "d" {
		t.Errorf("expected [c, d], got %v", dispatched)
	}
}

// TestV3Resume_StepBeadIDsPreserved verifies step bead IDs from the first run
// survive a save/load cycle.
func TestV3Resume_StepBeadIDsPreserved(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	graph := &formula.FormulaStepGraph{
		Name:    "test-beadids",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop"},
			"b": {Action: "test.noop", Needs: []string{"a"}, Terminal: true},
		},
	}

	state := NewGraphState(graph, "spi-ids", "wizard-ids")
	state.StepBeadIDs = map[string]string{
		"a": "step-bead-111",
		"b": "step-bead-222",
	}

	if err := state.Save("wizard-ids", configDirFn); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadGraphState("wizard-ids", configDirFn)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.StepBeadIDs["a"] != "step-bead-111" {
		t.Errorf("step bead a = %q, want step-bead-111", loaded.StepBeadIDs["a"])
	}
	if loaded.StepBeadIDs["b"] != "step-bead-222" {
		t.Errorf("step bead b = %q, want step-bead-222", loaded.StepBeadIDs["b"])
	}
}

// TestV3Resume_CountersSurvive verifies counters in state survive a save/load cycle.
func TestV3Resume_CountersSurvive(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	graph := &formula.FormulaStepGraph{
		Name:    "test-counters",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop", Terminal: true},
		},
	}

	state := NewGraphState(graph, "spi-cnt", "wizard-cnt")
	state.Counters["round"] = 3
	state.Counters["max_rounds"] = 5
	state.Counters["build_fix_attempts"] = 1

	if err := state.Save("wizard-cnt", configDirFn); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadGraphState("wizard-cnt", configDirFn)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.Counters["round"] != 3 {
		t.Errorf("counter round = %d, want 3", loaded.Counters["round"])
	}
	if loaded.Counters["max_rounds"] != 5 {
		t.Errorf("counter max_rounds = %d, want 5", loaded.Counters["max_rounds"])
	}
	if loaded.Counters["build_fix_attempts"] != 1 {
		t.Errorf("counter build_fix_attempts = %d, want 1", loaded.Counters["build_fix_attempts"])
	}
}
