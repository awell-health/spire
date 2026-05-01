// Package agent — 429 auto-promote fallback (spi-mdxtww).
//
// This file wraps invocation of the `claude` CLI with detection of 429
// rate-limit responses and, when the active AuthContext meets the
// auto-promote preconditions, an in-memory swap from the subscription slot
// to the api-key slot followed by a one-shot retry.
//
// Scope (epic spi-gsmvr4):
//   - The swap is IN-MEMORY ONLY. Callers control stickiness by holding
//     onto the same *config.AuthContext pointer across calls; this file
//     never writes cooldown state to Dolt, local files, or ConfigMaps.
//   - The swap is one-way (subscription → api-key). There is no api-key →
//     subscription recovery path.
//   - Ephemeral contexts (synthesized from a summon `-H` header override)
//     are never swapped — an inline credential is an explicit one-shot
//     instruction from the operator and must not be silently replaced.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

// ClaudeCLIRunner is the injected function that actually spawns the
// `claude` binary. Extracted so tests can stub it without a real
// subprocess. Contract: stdout bytes returned, stderr written to the
// provided writer, error carries the process exit condition (including
// ctx cancellation, signals, non-zero exits).
type ClaudeCLIRunner func(ctx context.Context, args []string, dir string, env []string, stdout, stderr io.Writer) ([]byte, error)

// ClaudeInvokeParams captures one auth-aware claude invocation. See
// InvokeClaudeWithAuth for the retry semantics.
type ClaudeInvokeParams struct {
	Ctx     context.Context
	Args    []string
	Dir     string
	BaseEnv []string // typically os.Environ(); Auth env is merged in by MergeAuthEnv
	Auth    *config.AuthContext
	Stdout  io.Writer // optional tee destination for stdout
	Stderr  io.Writer // optional destination for stderr
	BeadID  string    // for structured INFO log on auto-promote
	Step    string    // for structured INFO log on auto-promote
	Log     func(format string, args ...interface{})
	Runner  ClaudeCLIRunner // defaults to DefaultClaudeCLIRunner
}

// ClaudeInvokeResult captures the outcome of InvokeClaudeWithAuth.
type ClaudeInvokeResult struct {
	// Stdout is the captured stdout from the final attempt (the retry after
	// a promote, if one happened; otherwise the first attempt).
	Stdout []byte
	// Err carries the process exit condition from the final attempt.
	Err error
	// Promoted is true when a 429 triggered a subscription→api-key swap on
	// Params.Auth. Callers that record auth_profile_final on the active
	// agent_runs row should observe this flag.
	Promoted bool
}

// InvokeClaudeWithAuth runs the `claude` CLI once. If the attempt returns
// a 429 (detected from stdout stream-json or the exit error) AND
// ShouldAutoPromote(p.Auth) holds, p.Auth is swapped to the api-key slot
// via (*config.AuthContext).SwapToAPIKey and the call is retried with the
// new env. A successful swap mutates p.Auth in place so callers holding
// onto that pointer see the promoted state on subsequent calls (stickiness).
// No disk persistence happens here — the epic locks the cooldown state to
// the wizard process's memory.
func InvokeClaudeWithAuth(p ClaudeInvokeParams) ClaudeInvokeResult {
	runner := p.Runner
	if runner == nil {
		runner = DefaultClaudeCLIRunner
	}
	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	env := MergeAuthEnv(p.BaseEnv, p.Auth)
	out, err := runner(ctx, p.Args, p.Dir, env, p.Stdout, p.Stderr)
	res := ClaudeInvokeResult{Stdout: out, Err: err}

	if !Is429Response(out, err) {
		return res
	}
	if !ShouldAutoPromote(p.Auth) {
		return res
	}

	if p.Log != nil {
		p.Log("auth: 429 on subscription, promoting to api-key (cooldown=in-memory) bead_id=%s step=%s", p.BeadID, p.Step)
	}
	if swapErr := p.Auth.SwapToAPIKey(); swapErr != nil {
		// SwapToAPIKey failed despite ShouldAutoPromote returning true —
		// either a race on ephemeral/APIKey or a nil ctx. Fall through with
		// the original error; do not retry.
		return res
	}
	res.Promoted = true

	env = MergeAuthEnv(p.BaseEnv, p.Auth)
	out, err = runner(ctx, p.Args, p.Dir, env, p.Stdout, p.Stderr)
	res.Stdout = out
	res.Err = err
	return res
}

// ShouldAutoPromote reports whether a 429 on auth should trigger a
// subscription→api-key swap. ALL preconditions must hold:
//   - Active.Slot == "subscription"
//   - APIKey != nil (api-key is configured)
//   - AutoPromoteOn429 is true
//   - Ephemeral is false (never swap away from an inline -H credential)
//
// Returns false for a nil AuthContext, a nil Active credential, or any
// unmet precondition.
func ShouldAutoPromote(auth *config.AuthContext) bool {
	if auth == nil || auth.Active == nil {
		return false
	}
	if !auth.AutoPromoteOn429 {
		return false
	}
	if auth.Ephemeral {
		return false
	}
	if auth.APIKey == nil {
		return false
	}
	return auth.Active.Slot == config.AuthSlotSubscription
}

// Is429Response reports whether the claude CLI output or exit error looks
// like a 429 from the Anthropic API. Matches:
//   - exit error text containing "429", "rate_limit_error", "rate limit",
//     or "too many requests" (case-insensitive)
//   - stream-json lines containing "rate_limit_error" or an HTTP 429
//     status marker ("status":429 / "status_code":429 / "code":429)
//
// Deliberately does NOT match the informational `rate_limit_event`
// (see pkg/board/logstream/claude.go) — that's a quota heads-up, not an
// actual 429 error on the current request.
func Is429Response(output []byte, runErr error) bool {
	if runErr != nil {
		low := strings.ToLower(runErr.Error())
		if strings.Contains(low, "429") ||
			strings.Contains(low, "rate_limit_error") ||
			strings.Contains(low, "rate limit") ||
			strings.Contains(low, "too many requests") {
			return true
		}
	}
	for _, line := range bytes.Split(output, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if bytes.Contains(line, []byte(`"rate_limit_error"`)) ||
			bytes.Contains(line, []byte(`"status":429`)) ||
			bytes.Contains(line, []byte(`"status_code":429`)) ||
			bytes.Contains(line, []byte(`"code":429`)) {
			return true
		}
	}
	return false
}

// IsMaxTurns reports whether the claude CLI stream-json output contains a
// terminal `result` event whose stop signal indicates the run hit its
// configured turn cap. Matches any of:
//   - result.stop_reason == "max_turns"
//   - result.subtype == "error_max_turns"
//   - result.terminal_reason == "max_turns"
//
// A true return means the run exited because of a real budget exhaustion,
// not a transport-layer cut — callers should NOT retry. Returns false on
// empty input or when no result event is present (see transientStreamCutoff
// for that case).
func IsMaxTurns(output []byte) bool {
	for _, line := range bytes.Split(output, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.Contains(line, []byte(`"type"`)) {
			continue
		}
		var evt struct {
			Type           string `json:"type"`
			Subtype        string `json:"subtype"`
			StopReason     string `json:"stop_reason"`
			TerminalReason string `json:"terminal_reason"`
		}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		if evt.Type != "result" {
			continue
		}
		if evt.StopReason == "max_turns" || evt.Subtype == "error_max_turns" || evt.TerminalReason == "max_turns" {
			return true
		}
	}
	return false
}

// TransientStreamCutoff reports whether a non-zero claude exit looks like a
// transport-layer stream interruption rather than a real failure. The
// signature observed in production is: claude was actively streaming a
// non-terminal stream event (e.g. an `input_json_delta` mid-tool-input),
// the HTTPS connection got cut, claude exits non-zero, and no terminal
// `result` event was ever emitted.
//
// Returns true iff ALL of the following hold:
//   - runErr != nil (the subprocess actually failed)
//   - Is429Response(output, runErr) is false (rate limits aren't transient)
//   - IsMaxTurns(output) is false (max_turns is not transient)
//   - the stream contains NO `type:"result"` event
//   - the last non-empty line looks like a stream-json event whose top-level
//     type is neither `result` nor `error`. A truncated final line (one that
//     fails to parse as JSON but does contain a `"type"` field) is treated
//     as the strongest cutoff signal: it means claude died mid-line.
//
// Returns false on empty output: a subprocess that died before emitting
// any JSONL line is not enough signal — that is more likely a binary-launch
// or environment problem than a stream cut, and the caller should fail
// rather than retry blindly.
func TransientStreamCutoff(output []byte, runErr error) bool {
	if runErr == nil {
		return false
	}
	if Is429Response(output, runErr) {
		return false
	}
	if IsMaxTurns(output) {
		return false
	}
	var lastLine []byte
	hasResultEvent := false
	for _, line := range bytes.Split(output, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		lastLine = line
		if !bytes.Contains(line, []byte(`"type"`)) {
			continue
		}
		var evt struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &evt); err == nil && evt.Type == "result" {
			hasResultEvent = true
		}
	}
	if hasResultEvent {
		return false
	}
	if len(lastLine) == 0 {
		return false
	}
	if !bytes.HasPrefix(lastLine, []byte("{")) {
		return false
	}
	if !bytes.Contains(lastLine, []byte(`"type"`)) {
		return false
	}
	var lastEvt struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(lastLine, &lastEvt); err != nil {
		// Truncated JSON on the last line is a strong cutoff signal:
		// claude died mid-line while streaming a non-terminal event.
		return true
	}
	if lastEvt.Type == "result" || lastEvt.Type == "error" {
		return false
	}
	if lastEvt.StopReason != "" {
		return false
	}
	return true
}

// MergeAuthEnv returns a copy of baseEnv with the AuthContext's
// credential env var added and any conflicting token stripped. Callers
// typically pass os.Environ() as baseEnv; after a swap the old slot's env
// var is removed so the respawned claude CLI picks up the new credential
// unambiguously (the initial spawn may have set CLAUDE_CODE_OAUTH_TOKEN
// via pkg/agent/spawn_process.go's applyProcessEnv).
//
// When auth is nil or its Active credential is empty, baseEnv is returned
// unchanged — callers fall through to whatever was in the parent process
// env (the pre-auth-config behavior).
func MergeAuthEnv(baseEnv []string, auth *config.AuthContext) []string {
	out := make([]string, 0, len(baseEnv)+1)
	if auth == nil || auth.Active == nil || auth.Active.Secret == "" {
		return append(out, baseEnv...)
	}
	var stripKeys []string
	switch auth.Active.Slot {
	case config.AuthSlotAPIKey:
		stripKeys = []string{config.EnvClaudeCodeOAuthToken, config.EnvAnthropicAuthToken}
	case config.AuthSlotSubscription:
		stripKeys = []string{config.EnvAnthropicAPIKey}
	}
	for _, e := range baseEnv {
		keep := true
		for _, k := range stripKeys {
			if strings.HasPrefix(e, k+"=") {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return auth.InjectEnv(out)
}

// DefaultClaudeCLIRunner runs the `claude` binary with the given args and
// env. stdout and stderr are optional tee destinations; the captured stdout
// bytes are returned regardless of whether an explicit stdout writer is
// passed. Use this as the production runner — tests should stub
// ClaudeInvokeParams.Runner with a controlled function.
func DefaultClaudeCLIRunner(ctx context.Context, args []string, dir string, env []string, stdout, stderr io.Writer) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = dir
	cmd.Env = env
	var buf bytes.Buffer
	if stdout != nil {
		cmd.Stdout = io.MultiWriter(&buf, stdout)
	} else {
		cmd.Stdout = &buf
	}
	if stderr != nil {
		cmd.Stderr = stderr
	} else {
		cmd.Stderr = io.Discard
	}
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), fmt.Errorf("claude cli: %w", err)
	}
	return buf.Bytes(), nil
}
