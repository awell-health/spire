package wizard

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
)

// streamCutPartial is the production death signature: claude was streaming
// an input_json_delta when the connection broke. No `result` event, no
// 429, no max_turns — just an incomplete non-terminal stream event.
var streamCutPartial = []byte(`{"type":"system","subtype":"init"}
{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"Edit"}}}
{"type":"stream_event","event":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/tmp/foo"}
`)

var streamMaxTurns = []byte(`{"type":"result","stop_reason":"max_turns","subtype":"error_max_turns","is_error":true}
`)

// installRetryTestRunner swaps the wizard's CLI runner for a scripted one
// and shrinks the retry backoff so tests don't sleep the production 5s/30s.
// Restoration is registered as a t.Cleanup.
func installRetryTestRunner(t *testing.T, responses []scriptedResponse) *scriptedClaudeRunner {
	t.Helper()
	script := &scriptedClaudeRunner{responses: responses}
	origRunner := wizardClaudeCLIRunner
	wizardClaudeCLIRunner = script.fn()
	origBackoffs := transientRetryBackoffs
	transientRetryBackoffs = []time.Duration{0, 0}
	t.Cleanup(func() {
		wizardClaudeCLIRunner = origRunner
		transientRetryBackoffs = origBackoffs
	})
	return script
}

func newImplementAuth() *config.AuthContext {
	return &config.AuthContext{
		Active: &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "api-key-xyz"},
		APIKey: &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "api-key-xyz"},
	}
}

// TestWizardRunClaude_RetryTransientThenSuccess: one transient cut, then
// success on the retry. WizardRunClaude must return nil error and have
// invoked claude exactly twice.
func TestWizardRunClaude_RetryTransientThenSuccess(t *testing.T) {
	worktreeDir, promptPath, resultDir := setupWizardRun(t, newImplementAuth())
	script := installRetryTestRunner(t, []scriptedResponse{
		{stdout: streamCutPartial, err: errors.New("exit 1")}, // attempt 1: transient
		{stdout: streamOK, err: nil},                          // attempt 2: success
	})

	_, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err != nil {
		t.Fatalf("WizardRunClaude err = %v, want nil after retry", err)
	}
	if script.calls != 2 {
		t.Errorf("invocations = %d, want 2 (one cut + one retry)", script.calls)
	}
}

// TestWizardRunClaude_RetryTwiceThenSuccess: two consecutive transient
// cuts followed by a success on the third attempt.
func TestWizardRunClaude_RetryTwiceThenSuccess(t *testing.T) {
	worktreeDir, promptPath, resultDir := setupWizardRun(t, newImplementAuth())
	script := installRetryTestRunner(t, []scriptedResponse{
		{stdout: streamCutPartial, err: errors.New("exit 1")},
		{stdout: streamCutPartial, err: errors.New("exit 1")},
		{stdout: streamOK, err: nil},
	})

	_, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err != nil {
		t.Fatalf("WizardRunClaude err = %v, want nil after two retries", err)
	}
	if script.calls != 3 {
		t.Errorf("invocations = %d, want 3 (two cuts + one retry success)", script.calls)
	}
}

// TestWizardRunClaude_RetryExhausted: three transient cuts in a row.
// We cap at maxTransientRetries (2) so the third attempt is the final
// one — its error surfaces.
func TestWizardRunClaude_RetryExhausted(t *testing.T) {
	worktreeDir, promptPath, resultDir := setupWizardRun(t, newImplementAuth())
	script := installRetryTestRunner(t, []scriptedResponse{
		{stdout: streamCutPartial, err: errors.New("exit 1")},
		{stdout: streamCutPartial, err: errors.New("exit 1")},
		{stdout: streamCutPartial, err: errors.New("exit 1")},
	})

	_, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err == nil {
		t.Fatal("WizardRunClaude err = nil, want non-nil after retries exhausted")
	}
	if got, want := script.calls, maxTransientRetries+1; got != want {
		t.Errorf("invocations = %d, want %d (initial + %d retries)", got, want, maxTransientRetries)
	}
}

// TestWizardRunClaude_NoRetryOn429: a 429 must NOT trigger the outer
// transient retry — the existing 429 auto-promote inside InvokeClaudeWithAuth
// already retried once on the same spawn. Auth is configured to NOT promote
// (already on api-key), so a 429 surfaces as a final error after exactly one
// outer attempt.
func TestWizardRunClaude_NoRetryOn429(t *testing.T) {
	worktreeDir, promptPath, resultDir := setupWizardRun(t, newImplementAuth())
	script := installRetryTestRunner(t, []scriptedResponse{
		{stdout: streamRateLimit, err: errors.New("exit 1")},
	})

	_, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err == nil {
		t.Fatal("WizardRunClaude err = nil, want non-nil for 429")
	}
	if script.calls != 1 {
		t.Errorf("invocations = %d, want 1 (no outer retry on 429)", script.calls)
	}
}

// TestWizardRunClaude_NoRetryOnMaxTurns: a max_turns terminal stream is a
// real budget exhaustion, not a transient cut — no retry.
func TestWizardRunClaude_NoRetryOnMaxTurns(t *testing.T) {
	worktreeDir, promptPath, resultDir := setupWizardRun(t, newImplementAuth())
	script := installRetryTestRunner(t, []scriptedResponse{
		{stdout: streamMaxTurns, err: errors.New("exit 1")},
	})

	_, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err == nil {
		t.Fatal("WizardRunClaude err = nil, want non-nil for max_turns")
	}
	if script.calls != 1 {
		t.Errorf("invocations = %d, want 1 (no retry on max_turns)", script.calls)
	}
}

// TestWizardRunClaude_NoRetryOnSuccess: a clean success on the first try
// should not trigger a retry and should not consume a retry slot.
func TestWizardRunClaude_NoRetryOnSuccess(t *testing.T) {
	worktreeDir, promptPath, resultDir := setupWizardRun(t, newImplementAuth())
	script := installRetryTestRunner(t, []scriptedResponse{
		{stdout: streamOK, err: nil},
	})

	_, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err != nil {
		t.Fatalf("WizardRunClaude err = %v, want nil", err)
	}
	if script.calls != 1 {
		t.Errorf("invocations = %d, want 1", script.calls)
	}
}

// TestWizardRunClaude_NoRetryOnEmptyOutput: when the subprocess dies before
// emitting any JSONL line, TransientStreamCutoff returns false and the
// failure surfaces immediately. This guards against retrying binary-launch
// failures that would never recover.
func TestWizardRunClaude_NoRetryOnEmptyOutput(t *testing.T) {
	worktreeDir, promptPath, resultDir := setupWizardRun(t, newImplementAuth())
	script := installRetryTestRunner(t, []scriptedResponse{
		{stdout: nil, err: errors.New("exit 127")},
	})

	_, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err == nil {
		t.Fatal("WizardRunClaude err = nil, want non-nil for empty-output failure")
	}
	if script.calls != 1 {
		t.Errorf("invocations = %d, want 1 (empty output is not a transient signal)", script.calls)
	}
}

// TestWizardRunClaude_RetryWritesDistinctTranscripts confirms that each
// retry attempt opens its own JSONL file so prior post-mortem trails are
// preserved.
func TestWizardRunClaude_RetryWritesDistinctTranscripts(t *testing.T) {
	worktreeDir, promptPath, resultDir := setupWizardRun(t, newImplementAuth())
	installRetryTestRunner(t, []scriptedResponse{
		{stdout: streamCutPartial, err: errors.New("exit 1")},
		{stdout: streamOK, err: nil},
	})

	if _, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement"); err != nil {
		t.Fatalf("WizardRunClaude err = %v, want nil", err)
	}

	matches, _ := filepath.Glob(filepath.Join(resultDir, "claude", "implement-*.jsonl"))
	if len(matches) < 2 {
		t.Fatalf("expected ≥2 per-attempt JSONL transcripts, got %d: %v", len(matches), matches)
	}
}

// TestWizardRunClaude_RetryLogsAttemptCounter ensures the operator-facing
// retry message is emitted with the bead id and attempt counter so log
// scrapers can spot transient cuts.
func TestWizardRunClaude_RetryLogsAttemptCounter(t *testing.T) {
	worktreeDir, promptPath, resultDir := setupWizardRun(t, newImplementAuth())
	installRetryTestRunner(t, []scriptedResponse{
		{stdout: streamCutPartial, err: errors.New("exit 1")},
		{stdout: streamOK, err: nil},
	})

	t.Setenv("SPIRE_BEAD_ID", "spi-test-retry")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	done := make(chan []byte)
	go func() {
		buf, _ := io.ReadAll(r)
		done <- buf
	}()

	_, runErr := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	w.Close()
	logged := <-done

	if runErr != nil {
		t.Fatalf("WizardRunClaude err = %v, want nil after retry", runErr)
	}
	loggedStr := string(logged)
	if !strings.Contains(loggedStr, "transient stream cut on bead spi-test-retry") {
		t.Errorf("retry log line missing transient cut marker:\n%s", loggedStr)
	}
	if !strings.Contains(loggedStr, "retrying (attempt 2/3)") {
		t.Errorf("retry log line missing attempt counter (2/3):\n%s", loggedStr)
	}
}

// TestWizardRunClaude_RetryAbortedByContextCancel: when the parent ctx is
// canceled mid-backoff, the loop should bail out cleanly with ctx.Err()
// instead of hanging or making more spawn attempts.
func TestWizardRunClaude_RetryAbortedByContextCancel(t *testing.T) {
	worktreeDir, promptPath, resultDir := setupWizardRun(t, newImplementAuth())

	// Use a long backoff so cancel beats time.After.
	origBackoffs := transientRetryBackoffs
	transientRetryBackoffs = []time.Duration{200 * time.Millisecond, 200 * time.Millisecond}
	t.Cleanup(func() { transientRetryBackoffs = origBackoffs })

	cancelled := false
	cancelOnceRunner := func(_ context.Context, _ []string, _ string, _ []string, stdout, _ io.Writer) ([]byte, error) {
		if !cancelled {
			cancelled = true
			if stdout != nil {
				_, _ = stdout.Write(streamCutPartial)
			}
			return streamCutPartial, errors.New("exit 1")
		}
		// Should never reach a second spawn — backoff should be cancelled
		// before then.
		t.Errorf("second spawn attempted after ctx cancel")
		return streamOK, nil
	}
	origRunner := wizardClaudeCLIRunner
	wizardClaudeCLIRunner = cancelOnceRunner
	t.Cleanup(func() { wizardClaudeCLIRunner = origRunner })

	// Use a tiny outer timeout so ctx.Done fires during the 200ms backoff.
	_, runErr := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "100ms", 0, resultDir, "implement")
	if runErr == nil {
		t.Fatal("WizardRunClaude err = nil, want context-related error after cancel during backoff")
	}
}

// Sanity guard: confirm the predicates and the in-loop classification are
// wired correctly by exercising agent.TransientStreamCutoff at the test
// boundary. Catches an accidental future change that swaps the predicate
// for a different one.
func TestTransientStreamCutoff_ProductionFixture(t *testing.T) {
	if !agent.TransientStreamCutoff(streamCutPartial, errors.New("exit 1")) {
		t.Fatal("streamCutPartial should be classified as a transient cutoff")
	}
	if agent.TransientStreamCutoff(streamOK, nil) {
		t.Fatal("clean stream must not be classified as transient")
	}
	if agent.TransientStreamCutoff(streamMaxTurns, errors.New("exit 1")) {
		t.Fatal("max_turns must not be classified as transient")
	}
	if agent.TransientStreamCutoff(streamRateLimit, errors.New("exit 1")) {
		t.Fatal("429 must not be classified as transient")
	}
}
