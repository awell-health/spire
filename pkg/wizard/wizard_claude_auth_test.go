package wizard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
)

// Scripted CLI runner that returns a sequence of pre-built responses and
// records the env used on each invocation so tests can assert the auth
// env flips after a 429-driven swap.
type scriptedClaudeRunner struct {
	responses []scriptedResponse
	calls     int
	envs      [][]string
}

type scriptedResponse struct {
	stdout []byte
	err    error
}

func (r *scriptedClaudeRunner) fn() agent.ClaudeCLIRunner {
	return func(_ context.Context, _ []string, _ string, env []string, stdout, _ io.Writer) ([]byte, error) {
		idx := r.calls
		r.calls++
		r.envs = append(r.envs, append([]string(nil), env...))
		if idx >= len(r.responses) {
			return nil, errors.New("scriptedClaudeRunner: no response scripted")
		}
		resp := r.responses[idx]
		if stdout != nil && len(resp.stdout) > 0 {
			_, _ = stdout.Write(resp.stdout)
		}
		return resp.stdout, resp.err
	}
}

// setupWizardRun writes a fake prompt and agentResultDir and returns their
// paths, plus a cleanup to restore state (the wizard's auth cache + CLI
// runner).
func setupWizardRun(t *testing.T, auth *config.AuthContext) (worktreeDir, promptPath, agentResultDir string) {
	t.Helper()
	worktreeDir = t.TempDir()
	promptPath = filepath.Join(worktreeDir, ".prompt")
	if err := os.WriteFile(promptPath, []byte("prompt"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	agentResultDir = t.TempDir()
	InitWizardAuth(auth)
	t.Cleanup(func() { resetWizardAuthStateForTest() })
	return
}

// stream-json fragments reused by multiple tests.
var (
	streamRateLimit = []byte(`{"type":"assistant","message":{"error":{"type":"rate_limit_error","message":"quota"}}}
{"type":"result","is_error":true,"subtype":"error_during_execution"}
`)
	streamOK = []byte(`{"type":"result","subtype":"success","is_error":false,"result":"done","num_turns":1,"total_cost_usd":0.01,"usage":{"input_tokens":10,"output_tokens":5}}
`)
)

// TestWizardRunClaude_Promote_StickyAcrossCalls drives two consecutive
// WizardRunClaude calls on the same auth pointer. Call 1 hits a 429 on
// subscription and promotes; call 2 must start on api-key (sticky) and
// succeed without re-entering the 429 path. Also asserts
// AuthProfileFinal is surfaced on both calls' metrics once the promote
// fired.
func TestWizardRunClaude_Promote_StickyAcrossCalls(t *testing.T) {
	auth := &config.AuthContext{
		Active:           &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "sub-tok"},
		APIKey:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "api-key-xyz"},
		AutoPromoteOn429: true,
	}
	worktreeDir, promptPath, resultDir := setupWizardRun(t, auth)

	script := &scriptedClaudeRunner{
		responses: []scriptedResponse{
			{stdout: streamRateLimit, err: errors.New("exit 1")}, // call 1 first attempt: 429
			{stdout: streamOK, err: nil},                         // call 1 retry: success
			{stdout: streamOK, err: nil},                         // call 2: success
		},
	}
	origRunner := wizardClaudeCLIRunner
	wizardClaudeCLIRunner = script.fn()
	t.Cleanup(func() { wizardClaudeCLIRunner = origRunner })

	// Call 1 — triggers promote.
	metrics1, err1 := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err1 != nil {
		t.Fatalf("call 1 unexpected error: %v", err1)
	}
	if metrics1.AuthProfileFinal != config.AuthSlotAPIKey {
		t.Errorf("call 1 metrics.AuthProfileFinal = %q, want %q", metrics1.AuthProfileFinal, config.AuthSlotAPIKey)
	}
	if auth.SlotName() != config.AuthSlotAPIKey {
		t.Errorf("auth.Active.Slot = %q after call 1, want api-key", auth.SlotName())
	}
	if wizardPromotionSlot() != config.AuthSlotAPIKey {
		t.Errorf("wizardPromotionSlot() = %q, want api-key", wizardPromotionSlot())
	}

	// Call 2 — must NOT hit 429 again (auth already api-key).
	metrics2, err2 := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err2 != nil {
		t.Fatalf("call 2 unexpected error: %v", err2)
	}
	if metrics2.AuthProfileFinal != config.AuthSlotAPIKey {
		t.Errorf("call 2 metrics.AuthProfileFinal = %q, want %q (sticky)", metrics2.AuthProfileFinal, config.AuthSlotAPIKey)
	}
	// 3 total invocations: call 1 first + call 1 retry + call 2.
	if script.calls != 3 {
		t.Errorf("runner invocations = %d, want 3", script.calls)
	}
	// Call 2's env must carry api-key, NOT subscription.
	env2 := strings.Join(script.envs[2], ",")
	if !strings.Contains(env2, config.EnvAnthropicAPIKey+"=api-key-xyz") {
		t.Errorf("call 2 env missing ANTHROPIC_API_KEY: %v", script.envs[2])
	}
}

// TestWizardRunClaude_NoSwapWhenAutoPromoteDisabled asserts that when
// AutoPromoteOn429 is false, a 429 surfaces as-is and no metrics.AuthProfileFinal
// is written.
func TestWizardRunClaude_NoSwapWhenAutoPromoteDisabled(t *testing.T) {
	auth := &config.AuthContext{
		Active:           &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "sub-tok"},
		APIKey:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "api-key-xyz"},
		AutoPromoteOn429: false,
	}
	worktreeDir, promptPath, resultDir := setupWizardRun(t, auth)

	script := &scriptedClaudeRunner{
		responses: []scriptedResponse{
			{stdout: streamRateLimit, err: errors.New("exit 1")},
		},
	}
	origRunner := wizardClaudeCLIRunner
	wizardClaudeCLIRunner = script.fn()
	t.Cleanup(func() { wizardClaudeCLIRunner = origRunner })

	metrics, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err == nil {
		t.Error("expected 429 error, got nil")
	}
	if metrics.AuthProfileFinal != "" {
		t.Errorf("AuthProfileFinal = %q, want empty", metrics.AuthProfileFinal)
	}
	if auth.SlotName() != config.AuthSlotSubscription {
		t.Errorf("auth slot = %q, want subscription (no swap)", auth.SlotName())
	}
	if script.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry)", script.calls)
	}
}

// TestWizardRunClaude_NoSwapEphemeral asserts an ephemeral -H-header
// context is never swapped even when the other preconditions line up.
func TestWizardRunClaude_NoSwapEphemeral(t *testing.T) {
	auth := &config.AuthContext{
		Active:           &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "header-tok"},
		APIKey:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "api-key-xyz"},
		AutoPromoteOn429: true,
		Ephemeral:        true,
	}
	worktreeDir, promptPath, resultDir := setupWizardRun(t, auth)

	script := &scriptedClaudeRunner{
		responses: []scriptedResponse{
			{stdout: streamRateLimit, err: errors.New("exit 1")},
		},
	}
	origRunner := wizardClaudeCLIRunner
	wizardClaudeCLIRunner = script.fn()
	t.Cleanup(func() { wizardClaudeCLIRunner = origRunner })

	metrics, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err == nil {
		t.Error("expected 429 error, got nil")
	}
	if metrics.AuthProfileFinal != "" {
		t.Errorf("AuthProfileFinal = %q, want empty for ephemeral", metrics.AuthProfileFinal)
	}
	if auth.Ephemeral == false {
		t.Error("auth.Ephemeral flipped unexpectedly")
	}
	if auth.SlotName() != config.AuthSlotSubscription {
		t.Errorf("auth slot = %q, want subscription (ephemeral must not swap)", auth.SlotName())
	}
}

// TestWizardRunClaude_NoSwapAPIKeyUnconfigured asserts a 429 surfaces
// normally when the api-key slot was never populated.
func TestWizardRunClaude_NoSwapAPIKeyUnconfigured(t *testing.T) {
	auth := &config.AuthContext{
		Active:           &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "sub-tok"},
		APIKey:           nil,
		AutoPromoteOn429: true,
	}
	worktreeDir, promptPath, resultDir := setupWizardRun(t, auth)

	script := &scriptedClaudeRunner{
		responses: []scriptedResponse{
			{stdout: streamRateLimit, err: errors.New("exit 1")},
		},
	}
	origRunner := wizardClaudeCLIRunner
	wizardClaudeCLIRunner = script.fn()
	t.Cleanup(func() { wizardClaudeCLIRunner = origRunner })

	metrics, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if err == nil {
		t.Error("expected 429 error, got nil")
	}
	if metrics.AuthProfileFinal != "" {
		t.Errorf("AuthProfileFinal = %q, want empty (api-key unconfigured)", metrics.AuthProfileFinal)
	}
	if script.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry)", script.calls)
	}
}

// TestWizardRunClaude_NoSwapAlreadyAPIKey asserts 429 on an api-key
// active slot triggers no swap (no subscription to fall back from).
func TestWizardRunClaude_NoSwapAlreadyAPIKey(t *testing.T) {
	auth := &config.AuthContext{
		Active:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "api-key-xyz"},
		APIKey:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "api-key-xyz"},
		AutoPromoteOn429: true,
	}
	worktreeDir, promptPath, resultDir := setupWizardRun(t, auth)

	script := &scriptedClaudeRunner{
		responses: []scriptedResponse{
			{stdout: streamRateLimit, err: errors.New("exit 1")},
		},
	}
	origRunner := wizardClaudeCLIRunner
	wizardClaudeCLIRunner = script.fn()
	t.Cleanup(func() { wizardClaudeCLIRunner = origRunner })

	_, _ = WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0, resultDir, "implement")
	if wizardPromotionSlot() != "" {
		t.Errorf("promote slot = %q, want empty (already on api-key)", wizardPromotionSlot())
	}
	if script.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry)", script.calls)
	}
}

// TestWizardWriteResult_IncludesAuthProfileFinalOnPromote asserts the
// result.json written after a promoted wizard run carries auth_profile_final
// so the executor's recordAgentRun populates the agent_runs column.
func TestWizardWriteResult_IncludesAuthProfileFinalOnPromote(t *testing.T) {
	auth := &config.AuthContext{
		Active:           &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "sub-tok"},
		APIKey:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "api-key-xyz"},
		AutoPromoteOn429: true,
	}
	worktreeDir, promptPath, _ := setupWizardRun(t, auth)

	// Drive a promote by running a 429-then-OK sequence.
	script := &scriptedClaudeRunner{
		responses: []scriptedResponse{
			{stdout: streamRateLimit, err: errors.New("exit 1")},
			{stdout: streamOK, err: nil},
		},
	}
	origRunner := wizardClaudeCLIRunner
	wizardClaudeCLIRunner = script.fn()
	t.Cleanup(func() { wizardClaudeCLIRunner = origRunner })

	// Use a shared fake DoltGlobalDir so result.json lands somewhere we can read.
	dir := t.TempDir()
	deps := &Deps{DoltGlobalDir: func() string { return dir }}
	metrics, err := WizardRunClaude(worktreeDir, promptPath, "claude-sonnet-4-6", "30s", 0,
		filepath.Join(dir, "wizards", "test-wiz"), "implement")
	if err != nil {
		t.Fatalf("WizardRunClaude: %v", err)
	}

	WizardWriteResult("test-wiz", "spi-abc", "success", "feat/test", "sha-xyz", 1, metrics, deps, func(string, ...interface{}) {})

	raw, err := os.ReadFile(filepath.Join(dir, "wizards", "test-wiz", "result.json"))
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse result.json: %v", err)
	}
	v, ok := got["auth_profile_final"]
	if !ok {
		t.Fatalf("result.json missing auth_profile_final; contents=%s", raw)
	}
	if s, _ := v.(string); s != config.AuthSlotAPIKey {
		t.Errorf("auth_profile_final = %v, want %q", v, config.AuthSlotAPIKey)
	}
}

// TestClaudeMetrics_Add_AuthProfileFinalSticky asserts the Add reducer
// preserves AuthProfileFinal from either side so a single 429 swap in
// any phase of a run propagates to the top-level metrics.
func TestClaudeMetrics_Add_AuthProfileFinalSticky(t *testing.T) {
	cases := []struct {
		name           string
		a, b           ClaudeMetrics
		want           string
	}{
		{"both empty", ClaudeMetrics{}, ClaudeMetrics{}, ""},
		{"a set", ClaudeMetrics{AuthProfileFinal: "api-key"}, ClaudeMetrics{}, "api-key"},
		{"b set", ClaudeMetrics{}, ClaudeMetrics{AuthProfileFinal: "api-key"}, "api-key"},
		{"both set", ClaudeMetrics{AuthProfileFinal: "api-key"}, ClaudeMetrics{AuthProfileFinal: "api-key"}, "api-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.a.Add(tc.b)
			if got.AuthProfileFinal != tc.want {
				t.Errorf("Add.AuthProfileFinal = %q, want %q", got.AuthProfileFinal, tc.want)
			}
		})
	}
}
