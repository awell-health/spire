package executor

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// TestReviewPhase_LoadsWithActions verifies that the embedded review-phase
// formula loads successfully and all steps have action fields defined,
// making it compatible with the v3 graph.run interpreter's dispatchAction.
func TestReviewPhase_LoadsWithActions(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("LoadEmbeddedStepGraph(review-phase): %v", err)
	}

	if graph.Version != 3 {
		t.Errorf("version = %d, want 3", graph.Version)
	}
	if graph.Name != "review-phase" {
		t.Errorf("name = %q, want %q", graph.Name, "review-phase")
	}

	expectedActions := map[string]string{
		"sage-review": "wizard.run",
		"fix":         "wizard.run",
		"arbiter":     "wizard.run",
		"merge":       "noop",
		"discard":     "noop",
	}

	for stepName, wantAction := range expectedActions {
		step, ok := graph.Steps[stepName]
		if !ok {
			t.Errorf("step %q not found in graph", stepName)
			continue
		}
		if step.Action == "" {
			t.Errorf("step %q has no action defined (v3 graph.run will fail)", stepName)
			continue
		}
		if step.Action != wantAction {
			t.Errorf("step %q action = %q, want %q", stepName, step.Action, wantAction)
		}
	}

	// Verify flow fields for wizard.run steps.
	expectedFlows := map[string]string{
		"sage-review": "sage-review",
		"fix":         "review-fix",
		"arbiter":     "arbiter",
	}
	for stepName, wantFlow := range expectedFlows {
		step := graph.Steps[stepName]
		if step.Flow != wantFlow {
			t.Errorf("step %q flow = %q, want %q", stepName, step.Flow, wantFlow)
		}
	}

	// Verify terminals use noop (parent graph handles real side effects).
	for _, termName := range []string{"merge", "discard"} {
		if !graph.Steps[termName].Terminal {
			t.Errorf("step %q should be terminal", termName)
		}
	}
}

// TestReviewPhase_SageApprove_MergeReady verifies that when sage-review
// completes with verdict=approve, the merge step becomes ready.
func TestReviewPhase_SageApprove_MergeReady(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load review-phase: %v", err)
	}

	completed := map[string]bool{
		"sage-review": true,
	}
	ctx := map[string]string{
		"verdict":           "approve",
		"review_round":      "0",
		"max_review_rounds": "3",
	}

	next, err := formula.NextSteps(graph, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}

	// merge should be ready (verdict == approve satisfies its condition).
	found := false
	for _, s := range next {
		if s == "merge" {
			found = true
		}
		// fix and arbiter should NOT be ready (verdict != request_changes).
		if s == "fix" {
			t.Error("fix should not be ready when verdict=approve")
		}
		if s == "arbiter" {
			t.Error("arbiter should not be ready when verdict=approve")
		}
	}
	if !found {
		t.Errorf("merge not in ready steps: %v", next)
	}
}

// TestReviewPhase_SageReject_FixReady verifies that when sage-review
// completes with verdict=request_changes and round < max, the fix step
// becomes ready (not arbiter).
func TestReviewPhase_SageReject_FixReady(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load review-phase: %v", err)
	}

	completed := map[string]bool{
		"sage-review": true,
	}
	ctx := map[string]string{
		"verdict":           "request_changes",
		"review_round":      "1",
		"max_review_rounds": "3",
	}

	next, err := formula.NextSteps(graph, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}

	foundFix := false
	for _, s := range next {
		if s == "fix" {
			foundFix = true
		}
		if s == "arbiter" {
			t.Error("arbiter should not be ready when review_round < max_review_rounds")
		}
		if s == "merge" {
			t.Error("merge should not be ready when verdict=request_changes")
		}
	}
	if !foundFix {
		t.Errorf("fix not in ready steps: %v", next)
	}
}

// TestReviewPhase_MaxRounds_ArbiterReady verifies that when review_round
// reaches max_review_rounds, the arbiter step fires instead of fix.
func TestReviewPhase_MaxRounds_ArbiterReady(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load review-phase: %v", err)
	}

	completed := map[string]bool{
		"sage-review": true,
	}
	ctx := map[string]string{
		"verdict":           "request_changes",
		"review_round":      "3",
		"max_review_rounds": "3",
	}

	next, err := formula.NextSteps(graph, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}

	foundArbiter := false
	for _, s := range next {
		if s == "arbiter" {
			foundArbiter = true
		}
		if s == "fix" {
			t.Error("fix should not be ready when review_round >= max_review_rounds")
		}
	}
	if !foundArbiter {
		t.Errorf("arbiter not in ready steps: %v", next)
	}
}

// TestReviewPhase_VarNameAlignment verifies that the review-phase formula
// declares max_review_rounds (not max_rounds), aligning with parent v3
// formulas that pass max_review_rounds as a var.
func TestReviewPhase_VarNameAlignment(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load review-phase: %v", err)
	}

	// max_review_rounds should exist.
	v, ok := graph.Vars["max_review_rounds"]
	if !ok {
		t.Fatal("var max_review_rounds not found in review-phase formula")
	}
	if v.Default != "3" {
		t.Errorf("max_review_rounds default = %q, want %q", v.Default, "3")
	}

	// max_rounds should NOT exist (old name).
	if _, exists := graph.Vars["max_rounds"]; exists {
		t.Error("var max_rounds still exists in review-phase formula; should be max_review_rounds")
	}

	// Verify conditions reference max_review_rounds, not max_rounds.
	fixStep := graph.Steps["fix"]
	if fixStep.Condition == "" {
		t.Fatal("fix step has no condition")
	}
	if strings.Contains(fixStep.Condition, "max_rounds") && !strings.Contains(fixStep.Condition, "max_review_rounds") {
		t.Errorf("fix condition uses max_rounds instead of max_review_rounds: %s", fixStep.Condition)
	}

	arbiterStep := graph.Steps["arbiter"]
	if arbiterStep.Condition == "" {
		t.Fatal("arbiter step has no condition")
	}
	if strings.Contains(arbiterStep.Condition, "max_rounds") && !strings.Contains(arbiterStep.Condition, "max_review_rounds") {
		t.Errorf("arbiter condition uses max_rounds instead of max_review_rounds: %s", arbiterStep.Condition)
	}
}
