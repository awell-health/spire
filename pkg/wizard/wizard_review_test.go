package wizard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWizardWriteResult_ApproveVerdict verifies that WizardWriteResult
// writes a result.json with the "approve" result field. This is the
// contract the executor relies on: when the sage writes "approve", the
// executor's readAgentResult returns it, and verdict promotion in
// actionWizardRun sets outputs["verdict"] = "approve" so the review
// graph can route to the merge terminal.
func TestWizardWriteResult_ApproveVerdict(t *testing.T) {
	dir := t.TempDir()
	deps := &Deps{
		DoltGlobalDir: func() string { return dir },
	}
	noop := func(string, ...interface{}) {}

	WizardWriteResult("test-sage", "spi-test", "approve", "", "",
		5*time.Second, ClaudeMetrics{}, deps, noop)

	data, err := os.ReadFile(filepath.Join(dir, "wizards", "test-sage", "result.json"))
	if err != nil {
		t.Fatalf("read result.json: %s", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parse result.json: %s", err)
	}

	got, ok := result["result"].(string)
	if !ok {
		t.Fatalf("result.json missing 'result' field or not a string")
	}
	if got != "approve" {
		t.Errorf("result = %q, want %q", got, "approve")
	}

	// Verify branch and commit are empty (no-diff path).
	if branch, _ := result["branch"].(string); branch != "" {
		t.Errorf("branch = %q, want empty", branch)
	}
	if commit, _ := result["commit"].(string); commit != "" {
		t.Errorf("commit = %q, want empty", commit)
	}
}

// TestParseReviewOutput_Approve verifies that ParseReviewOutput correctly
// parses an "approve" verdict from structured JSON.
func TestParseReviewOutput_Approve(t *testing.T) {
	input := `{"verdict": "approve", "summary": "LGTM", "issues": []}`
	review, err := ParseReviewOutput(input)
	if err != nil {
		t.Fatalf("ParseReviewOutput: %v", err)
	}
	if review.Verdict != "approve" {
		t.Errorf("verdict = %q, want %q", review.Verdict, "approve")
	}
}

// TestCmdWizardReview_MissingBaseBranchOverride documents a bug where the sage
// review resolves baseBranch from ResolveRepo (repo default) but does NOT apply
// the bead-level base-branch: label override. The wizard (implement/fix) path
// applies this override via findBaseBranchInParentChain, so the sage diffs
// against a different base than the wizard worked on.
//
// Symptom: when a bead has base-branch:hello-world-iteration-one, the wizard
// implements on that branch, but the sage diffs against origin/main. The review
// includes changes from the base branch the wizard didn't write, the fix wizard
// gets nonsensical feedback, and exits with zero changes.
//
// Bug: spi-ybcpe
func TestCmdWizardReview_MissingBaseBranchOverride(t *testing.T) {
	// Simulate the Deps the sage and wizard both use.
	testBead := Bead{
		ID:     "oo-1z2",
		Status: "in_progress",
		Title:  "Consolidate type system",
		Labels: []string{"base-branch:hello-world-iteration-one", "feat-branch:feat/oo-1z2"},
	}

	deps := &Deps{
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return "/tmp/open-orchestration", "git@github.com:example/oo.git", "main", nil
		},
		GetBead: func(id string) (Bead, error) {
			if id == testBead.ID {
				return testBead, nil
			}
			return Bead{}, nil
		},
		HasLabel: func(b Bead, prefix string) string {
			for _, l := range b.Labels {
				if len(l) > len(prefix) && l[:len(prefix)] == prefix {
					return l[len(prefix):]
				}
			}
			return ""
		},
	}

	// --- Wizard path: applies the override ---
	_, _, wizardBase, _ := deps.ResolveRepo(testBead.ID)
	if bb := findBaseBranchInParentChain(testBead.ID, deps); bb != "" {
		wizardBase = bb
	}

	if wizardBase != "hello-world-iteration-one" {
		t.Fatalf("wizard baseBranch = %q, want %q", wizardBase, "hello-world-iteration-one")
	}

	// --- Sage path: must apply the same override after ResolveRepo ---
	_, _, sageBase, _ := deps.ResolveRepo(testBead.ID)
	if bb := findBaseBranchInParentChain(testBead.ID, deps); bb != "" {
		sageBase = bb
	}

	if sageBase != "hello-world-iteration-one" {
		t.Errorf("sage baseBranch = %q, want %q (bead label override)",
			sageBase, "hello-world-iteration-one")
	}
}

// TestCmdWizardMerge_MissingBaseBranchOverride documents the same bug in the
// merge entry point. CmdWizardMerge (line 784) resolves baseBranch from
// ResolveRepo without applying the base-branch: label override, so it would
// attempt to merge into the wrong branch.
func TestCmdWizardMerge_MissingBaseBranchOverride(t *testing.T) {
	testBead := Bead{
		ID:     "oo-abc",
		Status: "in_progress",
		Labels: []string{"base-branch:develop", "feat-branch:feat/oo-abc", "review-approved"},
	}

	deps := &Deps{
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return "/tmp/repo", "", "main", nil
		},
		GetBead: func(id string) (Bead, error) {
			return testBead, nil
		},
		HasLabel: func(b Bead, prefix string) string {
			for _, l := range b.Labels {
				if len(l) > len(prefix) && l[:len(prefix)] == prefix {
					return l[len(prefix):]
				}
			}
			return ""
		},
	}

	_, _, mergeBase, _ := deps.ResolveRepo(testBead.ID)
	if bb := findBaseBranchInParentChain(testBead.ID, deps); bb != "" {
		mergeBase = bb
	}

	if mergeBase != "develop" {
		t.Errorf("merge baseBranch = %q, want %q (bead label override)",
			mergeBase, "develop")
	}
}

// TestParseReviewOutput_RequestChanges verifies request_changes parsing.
func TestParseReviewOutput_RequestChanges(t *testing.T) {
	input := `{"verdict": "request_changes", "summary": "Missing tests", "issues": [{"file": "main.go", "line": 42, "severity": "error", "message": "no tests"}]}`
	review, err := ParseReviewOutput(input)
	if err != nil {
		t.Fatalf("ParseReviewOutput: %v", err)
	}
	if review.Verdict != "request_changes" {
		t.Errorf("verdict = %q, want %q", review.Verdict, "request_changes")
	}
	if len(review.Issues) != 1 {
		t.Errorf("issues count = %d, want 1", len(review.Issues))
	}
}
