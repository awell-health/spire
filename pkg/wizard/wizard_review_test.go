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
