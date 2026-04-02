package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// saveAndRestoreRegistry snapshots the action registry and returns a cleanup
// function that restores it. This is used by e2e tests that register custom
// test actions.
func saveAndRestoreRegistry(t *testing.T) func() {
	t.Helper()
	orig := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		orig[k] = v
	}
	return func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range orig {
			actionRegistry[k] = v
		}
	}
}

// TestV3E2E_TaskLifecycle runs a complete task graph (plan -> implement ->
// review -> merge -> close) using test.noop actions. Verifies all steps
// dispatch in order, terminal step fires, executor terminates, and bead
// closes.
func TestV3E2E_TaskLifecycle(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var dispatched []string
	beadClosed := false
	deps.CloseBead = func(id string) error {
		beadClosed = true
		return nil
	}
	deps.IsAttemptBead = func(b Bead) bool { return false }
	deps.IsStepBead = func(b Bead) bool { return false }
	deps.IsReviewRoundBead = func(b Bead) bool { return false }

	restore := saveAndRestoreRegistry(t)
	defer restore()

	// Register test actions for each opcode used in the graph.
	actionRegistry["test.plan"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"result": "success"}}
	}
	actionRegistry["test.implement"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"result": "success"}}
	}
	actionRegistry["test.review"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"outcome": "merge"}}
	}
	actionRegistry["test.merge"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"merged": "true"}}
	}
	actionRegistry["test.close"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		e.terminated = true
		if err := e.deps.CloseBead(e.beadID); err != nil {
			return ActionResult{Error: err}
		}
		return ActionResult{Outputs: map[string]string{"status": "closed"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "task-e2e",
		Version: 3,
		Entry:   "plan",
		Steps: map[string]formula.StepConfig{
			"plan":      {Action: "test.plan"},
			"implement": {Action: "test.implement", Needs: []string{"plan"}},
			"review":    {Action: "test.review", Needs: []string{"implement"}},
			"merge": {
				Action: "test.merge",
				Needs:  []string{"review"},
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "steps.review.outputs.outcome", Op: "eq", Right: "merge"},
					},
				},
			},
			"close": {
				Action:   "test.close",
				Needs:    []string{"merge"},
				Terminal: true,
			},
			"discard": {
				Action:   "test.close",
				Needs:    []string{"review"},
				Terminal: true,
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "steps.review.outputs.outcome", Op: "eq", Right: "discard"},
					},
				},
			},
		},
	}

	exec := NewGraphForTest("spi-e2e", "wizard-e2e", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}

	// Verify dispatch order.
	expected := []string{"plan", "implement", "review", "merge", "close"}
	if len(dispatched) != len(expected) {
		t.Fatalf("dispatched = %v, want %v", dispatched, expected)
	}
	for i, name := range expected {
		if dispatched[i] != name {
			t.Errorf("dispatched[%d] = %q, want %q", i, dispatched[i], name)
		}
	}

	// Verify terminal state.
	if !exec.terminated {
		t.Error("expected executor to be terminated")
	}

	// Verify bead was closed.
	if !beadClosed {
		t.Error("expected bead to be closed")
	}

	// Verify all merge-path steps completed.
	for _, name := range []string{"plan", "implement", "review", "merge", "close"} {
		ss := exec.graphState.Steps[name]
		if ss.Status != "completed" {
			t.Errorf("step %s: expected completed, got %s", name, ss.Status)
		}
	}

	// Discard should still be pending (merge path was taken).
	if exec.graphState.Steps["discard"].Status != "pending" {
		t.Errorf("discard: expected pending, got %s", exec.graphState.Steps["discard"].Status)
	}
}

// TestV3E2E_TaskLifecycle_Discard is the same as TaskLifecycle but review
// produces outcome=discard, so the discard terminal fires instead of merge.
func TestV3E2E_TaskLifecycle_Discard(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var dispatched []string
	deps.IsAttemptBead = func(b Bead) bool { return false }
	deps.IsStepBead = func(b Bead) bool { return false }
	deps.IsReviewRoundBead = func(b Bead) bool { return false }

	restore := saveAndRestoreRegistry(t)
	defer restore()

	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"result": "success"}}
	}
	actionRegistry["test.review-discard"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"outcome": "discard"}}
	}
	actionRegistry["test.discard"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		e.terminated = true
		return ActionResult{Outputs: map[string]string{"status": "discarded"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "task-e2e-discard",
		Version: 3,
		Entry:   "plan",
		Steps: map[string]formula.StepConfig{
			"plan":      {Action: "test.noop"},
			"implement": {Action: "test.noop", Needs: []string{"plan"}},
			"review":    {Action: "test.review-discard", Needs: []string{"implement"}},
			"merge": {
				Action: "test.noop",
				Needs:  []string{"review"},
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "steps.review.outputs.outcome", Op: "eq", Right: "merge"},
					},
				},
				Terminal: true,
			},
			"discard": {
				Action: "test.discard",
				Needs:  []string{"review"},
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "steps.review.outputs.outcome", Op: "eq", Right: "discard"},
					},
				},
				Terminal: true,
			},
		},
	}

	exec := NewGraphForTest("spi-e2e-d", "wizard-e2e-d", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}

	// Verify dispatch order: plan -> implement -> review -> discard.
	expected := []string{"plan", "implement", "review", "discard"}
	if len(dispatched) != len(expected) {
		t.Fatalf("dispatched = %v, want %v", dispatched, expected)
	}
	for i, name := range expected {
		if dispatched[i] != name {
			t.Errorf("dispatched[%d] = %q, want %q", i, dispatched[i], name)
		}
	}

	if !exec.terminated {
		t.Error("expected executor to be terminated")
	}

	// Merge should still be pending.
	if exec.graphState.Steps["merge"].Status != "pending" {
		t.Errorf("merge: expected pending, got %s", exec.graphState.Steps["merge"].Status)
	}
	if exec.graphState.Steps["discard"].Status != "completed" {
		t.Errorf("discard: expected completed, got %s", exec.graphState.Steps["discard"].Status)
	}
}

// TestV3E2E_EpicLifecycle builds a full epic graph with all step types
// (design-check, plan, materialize, implement, review, merge/close, discard)
// and runs it end-to-end.
func TestV3E2E_EpicLifecycle(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var dispatched []string
	beadClosed := false
	deps.CloseBead = func(id string) error {
		beadClosed = true
		return nil
	}
	deps.IsAttemptBead = func(b Bead) bool { return false }
	deps.IsStepBead = func(b Bead) bool { return false }
	deps.IsReviewRoundBead = func(b Bead) bool { return false }
	deps.GetChildren = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-epic.1", Type: "task", Title: "Subtask 1"},
			{ID: "spi-epic.2", Type: "task", Title: "Subtask 2"},
		}, nil
	}

	restore := saveAndRestoreRegistry(t)
	defer restore()

	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"result": "success"}}
	}
	actionRegistry["test.design-check"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"design_ref": "spi-design"}}
	}
	actionRegistry["test.materialize"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"status": "pass", "child_count": "2"}}
	}
	actionRegistry["test.review-merge"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		return ActionResult{Outputs: map[string]string{"outcome": "merge"}}
	}
	actionRegistry["test.close"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		dispatched = append(dispatched, stepName)
		e.terminated = true
		e.deps.CloseBead(e.beadID)
		return ActionResult{Outputs: map[string]string{"status": "closed"}}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "epic-e2e",
		Version: 3,
		Entry:   "design-check",
		Steps: map[string]formula.StepConfig{
			"design-check": {Action: "test.design-check"},
			"plan":         {Action: "test.noop", Needs: []string{"design-check"}},
			"materialize":  {Action: "test.materialize", Needs: []string{"plan"}},
			"implement":    {Action: "test.noop", Needs: []string{"materialize"}},
			"review":       {Action: "test.review-merge", Needs: []string{"implement"}},
			"merge": {
				Action: "test.noop",
				Needs:  []string{"review"},
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "steps.review.outputs.outcome", Op: "eq", Right: "merge"},
					},
				},
			},
			"close": {
				Action:   "test.close",
				Needs:    []string{"merge"},
				Terminal: true,
			},
			"discard": {
				Action: "test.close",
				Needs:  []string{"review"},
				When: &formula.StructuredCondition{
					All: []formula.Predicate{
						{Left: "steps.review.outputs.outcome", Op: "eq", Right: "discard"},
					},
				},
				Terminal: true,
			},
		},
	}

	exec := NewGraphForTest("spi-epic", "wizard-epic", graph, nil, deps)
	err := exec.RunGraph(graph, exec.graphState)
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}

	// Verify dispatch order for the merge path.
	expected := []string{"design-check", "plan", "materialize", "implement", "review", "merge", "close"}
	if len(dispatched) != len(expected) {
		t.Fatalf("dispatched = %v, want %v", dispatched, expected)
	}
	for i, name := range expected {
		if dispatched[i] != name {
			t.Errorf("dispatched[%d] = %q, want %q", i, dispatched[i], name)
		}
	}

	if !exec.terminated {
		t.Error("expected executor to be terminated")
	}
	if !beadClosed {
		t.Error("expected bead to be closed")
	}

	// Discard should be pending (merge path was taken).
	if exec.graphState.Steps["discard"].Status != "pending" {
		t.Errorf("discard: expected pending, got %s", exec.graphState.Steps["discard"].Status)
	}
}
