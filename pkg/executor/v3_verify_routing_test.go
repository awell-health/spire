package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// TestEpicImplementPhase_ValidatesCleanly verifies that the embedded
// epic-implement-phase formula still parses and validates after edits.
func TestEpicImplementPhase_ValidatesCleanly(t *testing.T) {
	_, err := formula.LoadEmbeddedStepGraph("epic-implement-phase")
	if err != nil {
		t.Fatalf("epic-implement-phase should validate cleanly: %v", err)
	}
}

// TestEpicImplementPhase_NoMergeAfterFailedVerify proves that merge-to-main
// does NOT become ready when verify-build completes with status=fail, and
// that the build-failed terminal fires instead.
func TestEpicImplementPhase_NoMergeAfterFailedVerify(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("epic-implement-phase")
	if err != nil {
		t.Fatalf("load epic-implement-phase: %v", err)
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

	// merge-to-main should NOT be ready
	for _, s := range ready {
		if s == "merge-to-main" {
			t.Error("merge-to-main should not be ready after failed verify")
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

// TestEpicImplementPhase_MergeAfterPassedVerify proves that merge-to-main
// becomes ready when verify-build completes with status=pass, and that
// build-failed does NOT fire.
func TestEpicImplementPhase_MergeAfterPassedVerify(t *testing.T) {
	graph, err := formula.LoadEmbeddedStepGraph("epic-implement-phase")
	if err != nil {
		t.Fatalf("load epic-implement-phase: %v", err)
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

	// merge-to-main SHOULD be ready
	found := false
	for _, s := range ready {
		if s == "merge-to-main" {
			found = true
		}
	}
	if !found {
		t.Errorf("merge-to-main should be ready after passed verify, got: %v", ready)
	}

	// build-failed should NOT be ready
	for _, s := range ready {
		if s == "build-failed" {
			t.Error("build-failed should not be ready after passed verify")
		}
	}
}
