package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// TestClericRetry_RetryDeclaresResets verifies that the retry step declares
// resets for decide, execute, and verify, enabling the recovery retry loop.
func TestClericRetry_RetryDeclaresResets(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("cleric-default")
	if err != nil {
		t.Fatalf("LoadEmbeddedStepGraph(cleric-default): %v", err)
	}

	retry, ok := graph.Steps["retry"]
	if !ok {
		t.Fatal("step retry not found")
	}

	if len(retry.Resets) != 3 {
		t.Fatalf("retry.Resets = %v, want [decide, execute, verify]", retry.Resets)
	}
	want := map[string]bool{"decide": true, "execute": true, "verify": true}
	for _, r := range retry.Resets {
		if !want[r] {
			t.Errorf("unexpected reset target: %q", r)
		}
	}
}

// TestClericRetry_VerifyPass_LearnReady verifies that when verification passes,
// the learn step is ready and retry is not.
func TestClericRetry_VerifyPass_LearnReady(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("cleric-default")
	if err != nil {
		t.Fatalf("load cleric-default: %v", err)
	}

	completed := map[string]bool{
		"collect_context": true,
		"decide":          true,
		"execute":         true,
		"verify":          true,
	}
	ctx := map[string]string{
		"steps.verify.outputs.verification_status": "pass",
		"steps.decide.completed_count":             "1",
		"vars.max_retries":                         "4",
	}

	next, err := formula.NextSteps(graph, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}

	foundLearn := false
	for _, s := range next {
		if s == "learn" {
			foundLearn = true
		}
		if s == "retry" {
			t.Error("retry should not be ready when verification_status=pass")
		}
	}
	if !foundLearn {
		t.Errorf("learn not in ready steps: %v", next)
	}
}

// TestClericRetry_VerifyFail_RetryReady verifies that when verification fails
// and retry budget remains, the retry step is ready and learn is not.
func TestClericRetry_VerifyFail_RetryReady(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("cleric-default")
	if err != nil {
		t.Fatalf("load cleric-default: %v", err)
	}

	completed := map[string]bool{
		"collect_context": true,
		"decide":          true,
		"execute":         true,
		"verify":          true,
	}
	ctx := map[string]string{
		"steps.verify.outputs.verification_status": "fail",
		"steps.decide.completed_count":             "1",
		"vars.max_retries":                         "4",
	}

	next, err := formula.NextSteps(graph, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}

	foundRetry := false
	for _, s := range next {
		if s == "retry" {
			foundRetry = true
		}
		if s == "learn" {
			t.Error("learn should not be ready when verification fails and retries remain")
		}
	}
	if !foundRetry {
		t.Errorf("retry not in ready steps: %v", next)
	}
}

// TestClericRetry_Exhausted_LearnReady verifies that when verification fails
// but retry budget is exhausted, learn fires (via completed_count >= max_retries)
// and retry does not.
func TestClericRetry_Exhausted_LearnReady(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("cleric-default")
	if err != nil {
		t.Fatalf("load cleric-default: %v", err)
	}

	completed := map[string]bool{
		"collect_context": true,
		"decide":          true,
		"execute":         true,
		"verify":          true,
	}
	ctx := map[string]string{
		"steps.verify.outputs.verification_status": "fail",
		"steps.decide.completed_count":             "4",
		"vars.max_retries":                         "4",
	}

	next, err := formula.NextSteps(graph, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}

	foundLearn := false
	for _, s := range next {
		if s == "learn" {
			foundLearn = true
		}
		if s == "retry" {
			t.Error("retry should not be ready when completed_count >= max_retries")
		}
	}
	if !foundLearn {
		t.Errorf("learn not in ready steps: %v", next)
	}
}
