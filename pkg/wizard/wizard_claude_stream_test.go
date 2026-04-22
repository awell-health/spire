package wizard

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWizardRunClaude_StdoutNotTeedToStderr is a regression guard for
// spi-4ytwi: the streaming Claude variant (WizardRunClaude) used to tee
// stdout to os.Stderr, which caused stream-json events to leak into the
// wizard orchestrator log (captured via wizards/<wizard>.log). The tee
// was redundant because the per-invocation transcript file under
// <agentResultDir>/claude/<label>-<ts>.jsonl already captures the stream.
//
// This test runs the function against a fake claude binary that emits
// stream-json to stdout, captures the process's os.Stderr via a pipe,
// and asserts the captured stderr contains no '{"type":"' substring.
// It also asserts the per-invocation transcript file DID capture the
// stream-json (so we know the fix hasn't also accidentally suppressed
// the intended sink).
func TestWizardRunClaude_StdoutNotTeedToStderr(t *testing.T) {
	// Fake claude binary that emits a handful of stream-json events to
	// stdout, closely matching the real stream-json format.
	binDir := t.TempDir()
	claudePath := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
cat <<'EOF'
{"type":"system","subtype":"init","session_id":"s1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"done","session_id":"s1","total_cost_usd":0.01,"usage":{"input_tokens":10,"output_tokens":5}}
EOF
`
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	worktreeDir := t.TempDir()
	promptPath := filepath.Join(worktreeDir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("test prompt"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	agentResultDir := t.TempDir()

	// Swap os.Stderr for a pipe so we can capture everything the function
	// writes (or tees) to stderr during this invocation.
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w

	var stderrBuf bytes.Buffer
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderrBuf, r)
		close(drained)
	}()

	_, runErr := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, agentResultDir, "test-label")

	_ = w.Close()
	os.Stderr = oldStderr
	<-drained
	_ = r.Close()

	if runErr != nil {
		t.Fatalf("WizardRunClaude: %v", runErr)
	}

	captured := stderrBuf.String()
	if strings.Contains(captured, `{"type":"`) {
		t.Errorf("wizard os.Stderr contains Claude stream-json — the stdout tee was not removed.\ncaptured stderr:\n%s", captured)
	}

	// Sanity check: the per-invocation transcript file should still have
	// the stream-json. If this assertion fails, the fix has over-reached
	// and broken the intended sink.
	claudeDir := filepath.Join(agentResultDir, "claude")
	entries, err := os.ReadDir(claudeDir)
	if err != nil {
		t.Fatalf("read claude transcript dir %s: %v", claudeDir, err)
	}
	var jsonl string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			jsonl = filepath.Join(claudeDir, e.Name())
			break
		}
	}
	if jsonl == "" {
		t.Fatalf("no .jsonl transcript found under %s", claudeDir)
	}
	content, err := os.ReadFile(jsonl)
	if err != nil {
		t.Fatalf("read transcript %s: %v", jsonl, err)
	}
	if !strings.Contains(string(content), `{"type":"result"`) {
		t.Errorf("per-invocation transcript %s missing stream-json result event; got:\n%s", jsonl, content)
	}
}
