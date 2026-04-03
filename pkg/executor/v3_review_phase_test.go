package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// TestReviewPhase_LoadsWithActions verifies that the embedded review-phase
// formula loads successfully and all steps have action fields defined.
func TestReviewPhase_LoadsWithActions(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("LoadEmbeddedStepGraph(review-phase): %v", err)
	}

	if graph.Version != 3 {
		t.Errorf("version = %d, want 3", graph.Version)
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
			t.Errorf("step %q not found", stepName)
			continue
		}
		if step.Action != wantAction {
			t.Errorf("step %q action = %q, want %q", stepName, step.Action, wantAction)
		}
	}

	expectedFlows := map[string]string{
		"sage-review": "sage-review",
		"fix":         "review-fix",
		"arbiter":     "arbiter",
	}
	for stepName, wantFlow := range expectedFlows {
		if graph.Steps[stepName].Flow != wantFlow {
			t.Errorf("step %q flow = %q, want %q", stepName, graph.Steps[stepName].Flow, wantFlow)
		}
	}

	for _, termName := range []string{"merge", "discard"} {
		if !graph.Steps[termName].Terminal {
			t.Errorf("step %q should be terminal", termName)
		}
	}
}

// TestReviewPhase_FixDeclaresResets verifies that the fix step declares
// resets for sage-review and fix, enabling the review loop.
func TestReviewPhase_FixDeclaresResets(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load review-phase: %v", err)
	}

	fix := graph.Steps["fix"]
	if len(fix.Resets) != 2 {
		t.Fatalf("fix.Resets = %v, want [sage-review, fix]", fix.Resets)
	}
	want := map[string]bool{"sage-review": true, "fix": true}
	for _, r := range fix.Resets {
		if !want[r] {
			t.Errorf("unexpected reset target: %q", r)
		}
	}
}

// TestReviewPhase_SageApprove_MergeReady verifies approve routes to merge.
func TestReviewPhase_SageApprove_MergeReady(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load review-phase: %v", err)
	}

	completed := map[string]bool{"sage-review": true}
	ctx := map[string]string{
		"steps.sage-review.outputs.verdict":  "approve",
		"steps.sage-review.completed_count":  "1",
		"vars.max_review_rounds":             "3",
	}

	next, err := formula.NextSteps(graph, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}

	found := false
	for _, s := range next {
		if s == "merge" {
			found = true
		}
		if s == "fix" || s == "arbiter" {
			t.Errorf("%s should not be ready when verdict=approve", s)
		}
	}
	if !found {
		t.Errorf("merge not in ready steps: %v", next)
	}
}

// TestReviewPhase_SageReject_FixReady verifies reject with round budget routes to fix.
func TestReviewPhase_SageReject_FixReady(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load review-phase: %v", err)
	}

	completed := map[string]bool{"sage-review": true}
	ctx := map[string]string{
		"steps.sage-review.outputs.verdict":  "request_changes",
		"steps.sage-review.completed_count":  "1",
		"vars.max_review_rounds":             "3",
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
			t.Error("arbiter should not be ready when completed_count < max_review_rounds")
		}
	}
	if !foundFix {
		t.Errorf("fix not in ready steps: %v", next)
	}
}

// TestReviewPhase_MaxRounds_ArbiterReady verifies exhausted round budget routes to arbiter.
func TestReviewPhase_MaxRounds_ArbiterReady(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load review-phase: %v", err)
	}

	completed := map[string]bool{"sage-review": true}
	ctx := map[string]string{
		"steps.sage-review.outputs.verdict":  "request_changes",
		"steps.sage-review.completed_count":  "3",
		"vars.max_review_rounds":             "3",
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
			t.Error("fix should not be ready when completed_count >= max_review_rounds")
		}
	}
	if !foundArbiter {
		t.Errorf("arbiter not in ready steps: %v", next)
	}
}

// TestReviewPhase_VarNameAlignment verifies max_review_rounds is the var name.
func TestReviewPhase_VarNameAlignment(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load review-phase: %v", err)
	}

	v, ok := graph.Vars["max_review_rounds"]
	if !ok {
		t.Fatal("var max_review_rounds not found")
	}
	if v.Default != "3" {
		t.Errorf("default = %q, want %q", v.Default, "3")
	}

	if _, exists := graph.Vars["max_rounds"]; exists {
		t.Error("var max_rounds should not exist")
	}

	// fix and arbiter should use structured when conditions (not bare condition string).
	for _, name := range []string{"fix", "arbiter"} {
		step := graph.Steps[name]
		if step.Condition != "" {
			t.Errorf("step %q uses bare condition (should use structured when): %s", name, step.Condition)
		}
		if step.When == nil {
			t.Errorf("step %q has no structured when condition", name)
		}
	}
}
