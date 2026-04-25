package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// fakeRunner returns a ClaudeCLIRunner that serves a pre-scripted sequence
// of (stdout, err) pairs — one per invocation. The call index increments
// each time the returned function is invoked so tests can assert per-call
// env and args through envCaptures / argsCaptures.
type fakeRunner struct {
	responses   []fakeResponse
	invocations []fakeInvocation
}

type fakeResponse struct {
	stdout []byte
	err    error
}

type fakeInvocation struct {
	args []string
	dir  string
	env  []string
}

func (f *fakeRunner) run() ClaudeCLIRunner {
	return func(_ context.Context, args []string, dir string, env []string, stdout, stderr io.Writer) ([]byte, error) {
		idx := len(f.invocations)
		// Snapshot slices so the caller can't mutate them post-call.
		argsCopy := append([]string(nil), args...)
		envCopy := append([]string(nil), env...)
		f.invocations = append(f.invocations, fakeInvocation{args: argsCopy, dir: dir, env: envCopy})
		if idx >= len(f.responses) {
			return nil, errors.New("fakeRunner: no response scripted for call")
		}
		r := f.responses[idx]
		if stdout != nil && len(r.stdout) > 0 {
			_, _ = stdout.Write(r.stdout)
		}
		return r.stdout, r.err
	}
}

func newAuth(active, apiKey *config.AuthCredential, autoPromote, ephemeral bool) *config.AuthContext {
	return &config.AuthContext{
		Active:           active,
		APIKey:           apiKey,
		AutoPromoteOn429: autoPromote,
		Ephemeral:        ephemeral,
	}
}

func subCred(secret string) *config.AuthCredential {
	return &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: secret}
}

func apiCred(secret string) *config.AuthCredential {
	return &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: secret}
}

// stream-json fragment that encodes a 429 from the Anthropic API. We match
// on `"rate_limit_error"`, so include that phrase.
var body429 = []byte(`{"type":"assistant","message":{"error":{"type":"rate_limit_error","message":"rate limit exceeded"}}}
{"type":"result","is_error":true,"subtype":"error_during_execution"}
`)

var bodyOK = []byte(`{"type":"result","result":"done","subtype":"success","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}
`)

// --- ShouldAutoPromote -----------------------------------------------------

func TestShouldAutoPromote(t *testing.T) {
	cases := []struct {
		name string
		auth *config.AuthContext
		want bool
	}{
		{"nil", nil, false},
		{"nil Active", newAuth(nil, apiCred("k"), true, false), false},
		{"auto_promote=false", newAuth(subCred("t"), apiCred("k"), false, false), false},
		{"ephemeral", newAuth(subCred("t"), apiCred("k"), true, true), false},
		{"api-key unconfigured", newAuth(subCred("t"), nil, true, false), false},
		{"already api-key", newAuth(apiCred("k"), apiCred("k"), true, false), false},
		{"happy path", newAuth(subCred("t"), apiCred("k"), true, false), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldAutoPromote(tc.auth); got != tc.want {
				t.Fatalf("ShouldAutoPromote = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- Is429Response ---------------------------------------------------------

func TestIs429Response(t *testing.T) {
	cases := []struct {
		name   string
		output []byte
		err    error
		want   bool
	}{
		{"clean success", bodyOK, nil, false},
		{"rate_limit_error in stream", body429, errors.New("exit 1"), true},
		{"429 in error string", nil, errors.New("API returned status 429"), true},
		{"rate limit in error string", nil, errors.New("anthropic: rate limit exceeded"), true},
		{"too many requests in error string", nil, errors.New("too many requests"), true},
		{"status 429 in json", []byte(`{"error":{"status":429,"message":"slow down"}}`), nil, true},
		{"status_code 429 in json", []byte(`{"status_code":429}`), nil, true},
		{"code 429 in json", []byte(`{"code":429}`), nil, true},
		{"unrelated error", []byte(`{"type":"result","subtype":"success"}`), errors.New("spawn failed"), false},
		{"empty", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Is429Response(tc.output, tc.err); got != tc.want {
				t.Fatalf("Is429Response = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- MergeAuthEnv ----------------------------------------------------------

func TestMergeAuthEnv_InjectsActiveSlot(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/x"}
	auth := newAuth(apiCred("sk-ant-api03-xxx"), apiCred("sk-ant-api03-xxx"), true, false)
	got := MergeAuthEnv(base, auth)
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d: %v", len(got), got)
	}
	if got[2] != config.EnvAnthropicAPIKey+"=sk-ant-api03-xxx" {
		t.Errorf("last entry = %q, want ANTHROPIC_API_KEY=...", got[2])
	}
}

func TestMergeAuthEnv_StripsConflictingTokenOnSwap(t *testing.T) {
	// Simulate the post-swap scenario: base env still carries the
	// subscription token from the original spawn; after swap to api-key,
	// the subscription token must be stripped so `claude` picks the api-key
	// unambiguously.
	base := []string{
		"PATH=/usr/bin",
		config.EnvClaudeCodeOAuthToken + "=sk-ant-oat01-sub",
		config.EnvAnthropicAuthToken + "=legacy-token",
	}
	auth := newAuth(apiCred("sk-ant-api03-new"), apiCred("sk-ant-api03-new"), true, false)
	got := MergeAuthEnv(base, auth)
	// Expect: PATH + ANTHROPIC_API_KEY (the stripped vars are gone).
	joined := strings.Join(got, ",")
	if strings.Contains(joined, config.EnvClaudeCodeOAuthToken+"=") {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN not stripped: %v", got)
	}
	if strings.Contains(joined, config.EnvAnthropicAuthToken+"=") {
		t.Errorf("ANTHROPIC_AUTH_TOKEN not stripped: %v", got)
	}
	if !strings.Contains(joined, config.EnvAnthropicAPIKey+"=sk-ant-api03-new") {
		t.Errorf("ANTHROPIC_API_KEY not injected: %v", got)
	}
}

func TestMergeAuthEnv_SubscriptionStripsAPIKey(t *testing.T) {
	base := []string{"PATH=/usr/bin", config.EnvAnthropicAPIKey + "=stale"}
	auth := newAuth(subCred("sub-token"), nil, true, false)
	got := MergeAuthEnv(base, auth)
	joined := strings.Join(got, ",")
	if strings.Contains(joined, config.EnvAnthropicAPIKey+"=stale") {
		t.Errorf("stale ANTHROPIC_API_KEY not stripped: %v", got)
	}
	if !strings.Contains(joined, config.EnvClaudeCodeOAuthToken+"=sub-token") {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN not injected: %v", got)
	}
}

func TestMergeAuthEnv_NilAuth(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	got := MergeAuthEnv(base, nil)
	if len(got) != 1 || got[0] != "PATH=/usr/bin" {
		t.Errorf("MergeAuthEnv(nil) = %v, want unchanged base", got)
	}
}

// --- InvokeClaudeWithAuth --------------------------------------------------

// TestInvokeClaudeWithAuth_PromoteOnSubscription429 exercises the happy-path
// auto-promote: a subscription-configured run hits 429, swaps, retries, and
// the retry succeeds. Asserts (a) Promoted=true, (b) Auth is now on the
// api-key slot, (c) the second call's env carries ANTHROPIC_API_KEY, and
// (d) the INFO log fires with bead_id / step.
func TestInvokeClaudeWithAuth_PromoteOnSubscription429(t *testing.T) {
	runner := &fakeRunner{
		responses: []fakeResponse{
			{stdout: body429, err: errors.New("exit 1")}, // first: 429
			{stdout: bodyOK, err: nil},                   // retry: success
		},
	}
	auth := newAuth(subCred("sub-token"), apiCred("api-key-xyz"), true, false)
	var logLines []string
	res := InvokeClaudeWithAuth(ClaudeInvokeParams{
		Ctx:     context.Background(),
		Args:    []string{"-p", "prompt"},
		Dir:     "/tmp",
		BaseEnv: []string{config.EnvClaudeCodeOAuthToken + "=sub-token"},
		Auth:    auth,
		BeadID:  "spi-abc",
		Step:    "implement",
		Log:     func(format string, args ...interface{}) { logLines = append(logLines, format) },
		Runner:  runner.run(),
	})
	if !res.Promoted {
		t.Fatalf("Promoted = false, want true (result err=%v)", res.Err)
	}
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil after successful retry", res.Err)
	}
	if auth.SlotName() != config.AuthSlotAPIKey {
		t.Errorf("Auth.Active.Slot = %q, want api-key", auth.SlotName())
	}
	if len(runner.invocations) != 2 {
		t.Fatalf("invocations = %d, want 2 (initial + retry)", len(runner.invocations))
	}
	// Retry env must carry api-key, NOT subscription.
	retryEnv := strings.Join(runner.invocations[1].env, ",")
	if !strings.Contains(retryEnv, config.EnvAnthropicAPIKey+"=api-key-xyz") {
		t.Errorf("retry env missing ANTHROPIC_API_KEY: %v", runner.invocations[1].env)
	}
	if strings.Contains(retryEnv, config.EnvClaudeCodeOAuthToken+"=sub-token") {
		t.Errorf("retry env still carries subscription token: %v", runner.invocations[1].env)
	}
	if len(logLines) == 0 {
		t.Error("expected INFO log on promote, got none")
	}
}

// TestInvokeClaudeWithAuth_Sticky exercises the sticky invariant: once a
// promote happens, a second InvokeClaudeWithAuth call on the same auth
// pointer starts on api-key and doesn't re-run the 429 path.
func TestInvokeClaudeWithAuth_Sticky(t *testing.T) {
	runner := &fakeRunner{
		responses: []fakeResponse{
			{stdout: body429, err: errors.New("exit 1")}, // call 1: 429
			{stdout: bodyOK, err: nil},                   // call 1 retry: success
			{stdout: bodyOK, err: nil},                   // call 2: success (api-key from the start)
		},
	}
	auth := newAuth(subCred("sub-token"), apiCred("api-key-xyz"), true, false)
	run := runner.run()

	// Call 1 — triggers promote.
	res1 := InvokeClaudeWithAuth(ClaudeInvokeParams{
		Ctx:     context.Background(),
		Args:    []string{"-p", "one"},
		BaseEnv: []string{config.EnvClaudeCodeOAuthToken + "=sub-token"},
		Auth:    auth,
		Runner:  run,
	})
	if !res1.Promoted {
		t.Fatal("call 1: expected Promoted=true")
	}

	// Call 2 — auth already promoted; must NOT trigger another 429 path.
	res2 := InvokeClaudeWithAuth(ClaudeInvokeParams{
		Ctx:     context.Background(),
		Args:    []string{"-p", "two"},
		BaseEnv: []string{config.EnvClaudeCodeOAuthToken + "=sub-token"},
		Auth:    auth,
		Runner:  run,
	})
	if res2.Promoted {
		t.Error("call 2: Promoted=true, want false (auth was already api-key)")
	}
	if res2.Err != nil {
		t.Errorf("call 2: Err = %v, want nil", res2.Err)
	}
	// Total invocations: 2 (call 1: initial+retry) + 1 (call 2) = 3.
	if len(runner.invocations) != 3 {
		t.Fatalf("invocations = %d, want 3", len(runner.invocations))
	}
	// Call 2's env must be api-key, no subscription token.
	env := strings.Join(runner.invocations[2].env, ",")
	if !strings.Contains(env, config.EnvAnthropicAPIKey+"=api-key-xyz") {
		t.Errorf("call 2 env missing api-key: %v", runner.invocations[2].env)
	}
	if strings.Contains(env, config.EnvClaudeCodeOAuthToken+"=sub-token") {
		t.Errorf("call 2 env still carries subscription token: %v", runner.invocations[2].env)
	}
}

// TestInvokeClaudeWithAuth_NoSwapAutoPromoteFalse — when auto_promote is
// disabled, a 429 is returned as-is.
func TestInvokeClaudeWithAuth_NoSwapAutoPromoteFalse(t *testing.T) {
	runner := &fakeRunner{
		responses: []fakeResponse{
			{stdout: body429, err: errors.New("exit 1")},
		},
	}
	auth := newAuth(subCred("sub-token"), apiCred("api-key-xyz"), false /*auto_promote*/, false)
	res := InvokeClaudeWithAuth(ClaudeInvokeParams{
		Ctx:    context.Background(),
		Args:   []string{"-p", "x"},
		Auth:   auth,
		Runner: runner.run(),
	})
	if res.Promoted {
		t.Error("Promoted=true, want false when auto_promote_on_429 is false")
	}
	if res.Err == nil {
		t.Error("Err=nil, want original 429 error")
	}
	if auth.SlotName() != config.AuthSlotSubscription {
		t.Errorf("Auth slot = %q, want subscription (no swap)", auth.SlotName())
	}
	if len(runner.invocations) != 1 {
		t.Errorf("invocations = %d, want 1 (no retry)", len(runner.invocations))
	}
}

// TestInvokeClaudeWithAuth_NoSwapAPIKeyUnconfigured — when no api-key slot
// is configured, a 429 is returned as-is.
func TestInvokeClaudeWithAuth_NoSwapAPIKeyUnconfigured(t *testing.T) {
	runner := &fakeRunner{
		responses: []fakeResponse{
			{stdout: body429, err: errors.New("exit 1")},
		},
	}
	auth := newAuth(subCred("sub-token"), nil /*no api-key*/, true, false)
	res := InvokeClaudeWithAuth(ClaudeInvokeParams{
		Ctx:    context.Background(),
		Args:   []string{"-p", "x"},
		Auth:   auth,
		Runner: runner.run(),
	})
	if res.Promoted {
		t.Error("Promoted=true, want false when api-key slot is unconfigured")
	}
	if len(runner.invocations) != 1 {
		t.Errorf("invocations = %d, want 1 (no retry)", len(runner.invocations))
	}
}

// TestInvokeClaudeWithAuth_NoSwapAlreadyAPIKey — when the active slot is
// already api-key, 429 is surfaced as-is; no further swap is possible.
func TestInvokeClaudeWithAuth_NoSwapAlreadyAPIKey(t *testing.T) {
	runner := &fakeRunner{
		responses: []fakeResponse{
			{stdout: body429, err: errors.New("exit 1")},
		},
	}
	auth := newAuth(apiCred("api-key-xyz"), apiCred("api-key-xyz"), true, false)
	res := InvokeClaudeWithAuth(ClaudeInvokeParams{
		Ctx:    context.Background(),
		Args:   []string{"-p", "x"},
		Auth:   auth,
		Runner: runner.run(),
	})
	if res.Promoted {
		t.Error("Promoted=true, want false when already on api-key")
	}
	if len(runner.invocations) != 1 {
		t.Errorf("invocations = %d, want 1 (no retry)", len(runner.invocations))
	}
}

// TestInvokeClaudeWithAuth_NoSwapEphemeral — when the context was
// synthesized from an inline -H header (Ephemeral=true), a 429 must NOT
// trigger a swap; the inline credential is an explicit operator instruction.
func TestInvokeClaudeWithAuth_NoSwapEphemeral(t *testing.T) {
	runner := &fakeRunner{
		responses: []fakeResponse{
			{stdout: body429, err: errors.New("exit 1")},
		},
	}
	auth := newAuth(subCred("sub-header"), apiCred("api-key-xyz"), true, true /*ephemeral*/)
	res := InvokeClaudeWithAuth(ClaudeInvokeParams{
		Ctx:    context.Background(),
		Args:   []string{"-p", "x"},
		Auth:   auth,
		Runner: runner.run(),
	})
	if res.Promoted {
		t.Error("Promoted=true, want false for ephemeral context")
	}
	if auth.SlotName() != config.AuthSlotSubscription {
		t.Errorf("Auth slot = %q, want subscription (ephemeral must not swap)", auth.SlotName())
	}
	if len(runner.invocations) != 1 {
		t.Errorf("invocations = %d, want 1 (no retry)", len(runner.invocations))
	}
}

// TestInvokeClaudeWithAuth_NoSwapOnNon429Error — non-429 errors don't
// trigger the swap path.
func TestInvokeClaudeWithAuth_NoSwapOnNon429Error(t *testing.T) {
	runner := &fakeRunner{
		responses: []fakeResponse{
			{stdout: []byte(`{"type":"result","is_error":true,"subtype":"context_window_exceeded"}`), err: errors.New("exit 1")},
		},
	}
	auth := newAuth(subCred("sub-token"), apiCred("api-key-xyz"), true, false)
	res := InvokeClaudeWithAuth(ClaudeInvokeParams{
		Ctx:    context.Background(),
		Args:   []string{"-p", "x"},
		Auth:   auth,
		Runner: runner.run(),
	})
	if res.Promoted {
		t.Error("Promoted=true, want false for non-429 error")
	}
	if len(runner.invocations) != 1 {
		t.Errorf("invocations = %d, want 1 (no retry)", len(runner.invocations))
	}
}

// TestInvokeClaudeWithAuth_DefaultRunnerNilCtx — passing a nil context to
// DefaultClaudeCLIRunner is an error, not a panic. InvokeClaudeWithAuth
// itself normalizes nil ctx to context.Background, but this documents the
// direct-runner contract.
func TestDefaultClaudeCLIRunner_NilCtx(t *testing.T) {
	_, err := DefaultClaudeCLIRunner(nil, []string{"-v"}, "", nil, nil, nil)
	if err == nil {
		t.Fatal("DefaultClaudeCLIRunner(nil ctx) returned nil error, want error")
	}
}
