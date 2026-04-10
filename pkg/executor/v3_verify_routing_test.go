package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// TestEpicImplementPhase_ValidatesCleanly verifies that the embedded
// subgraph-implement formula still parses and validates after edits.
func TestEpicImplementPhase_ValidatesCleanly(t *testing.T) {
	_, err := formula.LoadEmbeddedStepGraph("subgraph-implement")
	if err != nil {
		t.Fatalf("subgraph-implement should validate cleanly: %v", err)
	}
}

// TestEpicImplementPhase_NoVerifiedAfterFailedVerify proves that verified
// does NOT become ready when verify-build completes with status=fail, and
// that the build-failed terminal fires instead.
func TestEpicImplementPhase_NoVerifiedAfterFailedVerify(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("subgraph-implement")
	if err != nil {
		t.Fatalf("load subgraph-implement: %v", err)
	}

	// Simulate: dispatch-children completed, verify-build completed with status=fail
	completed := map[string]bool{
		"dispatch-children": true,
		"verify-build":      true,
	}
	ctx := map[string]string{
		"steps.verify-build.outputs.status": "fail",
	}

	ready, err := formula.NextSteps(graph, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}

	// verified should NOT be ready
	for _, s := range ready {
		if s == "verified" {
			t.Error("verified should not be ready after failed verify")
		}
	}

	// build-failed SHOULD be ready
	found := false
	for _, s := range ready {
		if s == "build-failed" {
			found = true
		}
	}
	if !found {
		t.Errorf("build-failed should be ready after failed verify, got: %v", ready)
	}
}

// TestEpicImplementPhase_VerifiedAfterPassedVerify proves that verified
// becomes ready when verify-build completes with status=pass, and that
// build-failed does NOT fire.
func TestEpicImplementPhase_VerifiedAfterPassedVerify(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("subgraph-implement")
	if err != nil {
		t.Fatalf("load subgraph-implement: %v", err)
	}

	completed := map[string]bool{
		"dispatch-children": true,
		"verify-build":      true,
	}
	ctx := map[string]string{
		"steps.verify-build.outputs.status": "pass",
	}

	ready, err := formula.NextSteps(graph, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}

	// verified SHOULD be ready
	found := false
	for _, s := range ready {
		if s == "verified" {
			found = true
		}
	}
	if !found {
		t.Errorf("verified should be ready after passed verify, got: %v", ready)
	}

	// build-failed should NOT be ready
	for _, s := range ready {
		if s == "build-failed" {
			t.Error("build-failed should not be ready after passed verify")
		}
	}
}

// TestEpicV3_ReviewBeforeMerge proves that epic review happens before
// merge-to-main in the v3 epic lifecycle. This is the regression test for
// spi-77g23: the nested subgraph-implement must NOT merge to main — only
// the parent epic-default merges, and only after review.
func TestEpicV3_ReviewBeforeMerge(t *testing.T) {
	// 1. Load both embedded formulas.
	implGraph, err := formula.LoadEmbeddedStepGraph("subgraph-implement")
	if err != nil {
		t.Fatalf("load subgraph-implement: %v", err)
	}
	epicGraph, err := formula.LoadEmbeddedStepGraph("epic-default")
	if err != nil {
		t.Fatalf("load epic-default: %v", err)
	}

	// 2. subgraph-implement must have NO step with action git.merge_to_main.
	for name, step := range implGraph.Steps {
		if step.Action == "git.merge_to_main" {
			t.Errorf("subgraph-implement step %q has action git.merge_to_main — nested graph must not merge", name)
		}
	}

	// 3. epic-default step ordering: implement needs materialize, review needs implement, merge needs review.
	implementStep, ok := epicGraph.Steps["implement"]
	if !ok {
		t.Fatal("epic-default missing 'implement' step")
	}
	if !containsStr(implementStep.Needs, "materialize") {
		t.Errorf("implement step should need 'materialize', got needs=%v", implementStep.Needs)
	}

	reviewStep, ok := epicGraph.Steps["review"]
	if !ok {
		t.Fatal("epic-default missing 'review' step")
	}
	if !containsStr(reviewStep.Needs, "implement") {
		t.Errorf("review step should need 'implement', got needs=%v", reviewStep.Needs)
	}

	mergeStep, ok := epicGraph.Steps["merge"]
	if !ok {
		t.Fatal("epic-default missing 'merge' step")
	}
	if !containsStr(mergeStep.Needs, "review") {
		t.Errorf("merge step should need 'review', got needs=%v", mergeStep.Needs)
	}

	// 4. epic-default has exactly one step with action git.merge_to_main, and it needs review.
	mergeCount := 0
	for name, step := range epicGraph.Steps {
		if step.Action == "git.merge_to_main" {
			mergeCount++
			if !containsStr(step.Needs, "review") {
				t.Errorf("git.merge_to_main step %q should need 'review', got needs=%v", name, step.Needs)
			}
		}
	}
	if mergeCount != 1 {
		t.Errorf("epic-default should have exactly 1 git.merge_to_main step, got %d", mergeCount)
	}

	// 5. Review step condition routes on steps.implement.outputs.outcome == "verified".
	if reviewStep.When == nil {
		t.Fatal("review step should have a 'when' condition")
	}
	foundVerifiedCondition := false
	for _, cond := range reviewStep.When.All {
		if cond.Left == "steps.implement.outputs.outcome" && cond.Op == "eq" && cond.Right == "verified" {
			foundVerifiedCondition = true
		}
	}
	if !foundVerifiedCondition {
		t.Errorf("review step 'when' should contain condition {steps.implement.outputs.outcome eq verified}, got: %+v", reviewStep.When.All)
	}
}

// containsStr checks if a slice contains a specific string.
func containsStr(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}
