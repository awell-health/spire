package wizard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
)

func TestParseClaudeResultJSON(t *testing.T) {
	t.Run("valid result event", func(t *testing.T) {
		input := []byte(`{"type":"assistant","message":"thinking..."}
{"type":"result","subtype":"success","cost_usd":0.05,"duration_ms":45000,"is_error":false,"num_turns":5,"result":"Here is the implementation.","session_id":"abc","stop_reason":"end_turn","total_cost_usd":0.12,"usage":{"input_tokens":3000,"output_tokens":2000,"cache_read_input_tokens":12345,"cache_creation_input_tokens":6789}}
`)
		text, metrics := parseClaudeResultJSON(input)
		if text != "Here is the implementation." {
			t.Errorf("resultText = %q, want %q", text, "Here is the implementation.")
		}
		if metrics.InputTokens != 3000 {
			t.Errorf("InputTokens = %d, want 3000", metrics.InputTokens)
		}
		if metrics.OutputTokens != 2000 {
			t.Errorf("OutputTokens = %d, want 2000", metrics.OutputTokens)
		}
		if metrics.TotalTokens != 5000 {
			t.Errorf("TotalTokens = %d, want 5000", metrics.TotalTokens)
		}
		if metrics.Turns != 5 {
			t.Errorf("Turns = %d, want 5", metrics.Turns)
		}
		if metrics.CostUSD != 0.12 {
			t.Errorf("CostUSD = %f, want 0.12", metrics.CostUSD)
		}
		if metrics.StopReason != "end_turn" {
			t.Errorf("StopReason = %q, want %q", metrics.StopReason, "end_turn")
		}
		if metrics.Subtype != "success" {
			t.Errorf("Subtype = %q, want success", metrics.Subtype)
		}
		if metrics.IsError {
			t.Error("IsError = true, want false")
		}
		if metrics.CacheReadTokens != 12345 {
			t.Errorf("CacheReadTokens = %d, want 12345", metrics.CacheReadTokens)
		}
		if metrics.CacheWriteTokens != 6789 {
			t.Errorf("CacheWriteTokens = %d, want 6789", metrics.CacheWriteTokens)
		}
	})

	t.Run("error result event", func(t *testing.T) {
		input := []byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"api_error_status":429,"num_turns":151,"result":"limit","stop_reason":"tool_use","terminal_reason":"max_turns","total_cost_usd":1.23,"usage":{"input_tokens":10,"output_tokens":20}}
`)
		_, metrics := parseClaudeResultJSON(input)
		if !metrics.IsError {
			t.Error("IsError = false, want true")
		}
		if metrics.Subtype != "error_max_turns" {
			t.Errorf("Subtype = %q, want error_max_turns", metrics.Subtype)
		}
		if metrics.TerminalReason != "max_turns" {
			t.Errorf("TerminalReason = %q, want max_turns", metrics.TerminalReason)
		}
		if metrics.APIErrorStatus != 429 {
			t.Errorf("APIErrorStatus = %d, want 429", metrics.APIErrorStatus)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		text, metrics := parseClaudeResultJSON([]byte(""))
		if text != "" {
			t.Errorf("resultText = %q, want empty", text)
		}
		if metrics.TotalTokens != 0 {
			t.Errorf("TotalTokens = %d, want 0", metrics.TotalTokens)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		input := []byte(`{bad json}
not even close
`)
		text, metrics := parseClaudeResultJSON(input)
		if text != "" {
			t.Errorf("resultText = %q, want empty", text)
		}
		if metrics.TotalTokens != 0 {
			t.Errorf("TotalTokens = %d, want 0", metrics.TotalTokens)
		}
	})

	t.Run("no result event", func(t *testing.T) {
		input := []byte(`{"type":"assistant","message":"hello"}
{"type":"tool_use","name":"read"}
`)
		text, metrics := parseClaudeResultJSON(input)
		if text != "" {
			t.Errorf("resultText = %q, want empty", text)
		}
		if metrics.TotalTokens != 0 {
			t.Errorf("TotalTokens = %d, want 0", metrics.TotalTokens)
		}
	})

	t.Run("result event without usage", func(t *testing.T) {
		input := []byte(`{"type":"result","result":"done","num_turns":3}
`)
		text, metrics := parseClaudeResultJSON(input)
		if text != "done" {
			t.Errorf("resultText = %q, want %q", text, "done")
		}
		if metrics.Turns != 3 {
			t.Errorf("Turns = %d, want 3", metrics.Turns)
		}
		if metrics.TotalTokens != 0 {
			t.Errorf("TotalTokens = %d, want 0 (no usage)", metrics.TotalTokens)
		}
	})

	t.Run("result event at end with trailing newline", func(t *testing.T) {
		input := []byte(`{"type":"system","text":"init"}
{"type":"result","result":"ok","usage":{"input_tokens":100,"output_tokens":50},"num_turns":1,"total_cost_usd":0.001}
`)
		text, metrics := parseClaudeResultJSON(input)
		if text != "ok" {
			t.Errorf("resultText = %q, want %q", text, "ok")
		}
		if metrics.InputTokens != 100 {
			t.Errorf("InputTokens = %d, want 100", metrics.InputTokens)
		}
		if metrics.OutputTokens != 50 {
			t.Errorf("OutputTokens = %d, want 50", metrics.OutputTokens)
		}
		if metrics.TotalTokens != 150 {
			t.Errorf("TotalTokens = %d, want 150", metrics.TotalTokens)
		}
	})
}

func TestParseClaudeResultJSON_ToolUseCounting(t *testing.T) {
	t.Run("no tool_use events", func(t *testing.T) {
		input := []byte(`{"type":"assistant","message":"thinking..."}
{"type":"result","result":"done","num_turns":1,"total_cost_usd":0.01,"usage":{"input_tokens":100,"output_tokens":50}}
`)
		_, metrics := parseClaudeResultJSON(input)
		if metrics.ToolCalls != nil {
			t.Errorf("ToolCalls = %v, want nil", metrics.ToolCalls)
		}
	})

	t.Run("single tool_use event", func(t *testing.T) {
		input := []byte(`{"type":"tool_use","name":"Read"}
{"type":"result","result":"done","num_turns":1,"total_cost_usd":0.01,"usage":{"input_tokens":100,"output_tokens":50}}
`)
		_, metrics := parseClaudeResultJSON(input)
		if metrics.ToolCalls == nil {
			t.Fatal("ToolCalls is nil, want non-nil")
		}
		if metrics.ToolCalls["Read"] != 1 {
			t.Errorf("ToolCalls[Read] = %d, want 1", metrics.ToolCalls["Read"])
		}
	})

	t.Run("multiple different tools", func(t *testing.T) {
		input := []byte(`{"type":"tool_use","name":"Read"}
{"type":"tool_use","name":"Edit"}
{"type":"tool_use","name":"Bash"}
{"type":"tool_use","name":"Read"}
{"type":"tool_use","name":"Read"}
{"type":"tool_use","name":"Grep"}
{"type":"result","result":"done","num_turns":3,"total_cost_usd":0.05,"usage":{"input_tokens":500,"output_tokens":200}}
`)
		_, metrics := parseClaudeResultJSON(input)
		if metrics.ToolCalls == nil {
			t.Fatal("ToolCalls is nil, want non-nil")
		}
		expected := map[string]int{"Read": 3, "Edit": 1, "Bash": 1, "Grep": 1}
		for tool, want := range expected {
			if got := metrics.ToolCalls[tool]; got != want {
				t.Errorf("ToolCalls[%s] = %d, want %d", tool, got, want)
			}
		}
		if len(metrics.ToolCalls) != len(expected) {
			t.Errorf("ToolCalls has %d keys, want %d", len(metrics.ToolCalls), len(expected))
		}
	})

	t.Run("tool_use without name is ignored", func(t *testing.T) {
		input := []byte(`{"type":"tool_use"}
{"type":"tool_use","name":"Read"}
{"type":"result","result":"done","num_turns":1,"total_cost_usd":0.01,"usage":{"input_tokens":100,"output_tokens":50}}
`)
		_, metrics := parseClaudeResultJSON(input)
		if metrics.ToolCalls == nil {
			t.Fatal("ToolCalls is nil, want non-nil")
		}
		if metrics.ToolCalls["Read"] != 1 {
			t.Errorf("ToolCalls[Read] = %d, want 1", metrics.ToolCalls["Read"])
		}
		if len(metrics.ToolCalls) != 1 {
			t.Errorf("ToolCalls has %d keys, want 1 (nameless tool_use should be skipped)", len(metrics.ToolCalls))
		}
	})

	t.Run("malformed lines mixed with valid tool_use", func(t *testing.T) {
		input := []byte(`{bad json
{"type":"tool_use","name":"Read"}
not json at all
{"type":"tool_use","name":"Edit"}
{"type":"result","result":"done","num_turns":2,"total_cost_usd":0.02,"usage":{"input_tokens":200,"output_tokens":100}}
`)
		_, metrics := parseClaudeResultJSON(input)
		if metrics.ToolCalls == nil {
			t.Fatal("ToolCalls is nil, want non-nil")
		}
		if metrics.ToolCalls["Read"] != 1 {
			t.Errorf("ToolCalls[Read] = %d, want 1", metrics.ToolCalls["Read"])
		}
		if metrics.ToolCalls["Edit"] != 1 {
			t.Errorf("ToolCalls[Edit] = %d, want 1", metrics.ToolCalls["Edit"])
		}
	})

	t.Run("tool_use events but no result event returns empty metrics", func(t *testing.T) {
		input := []byte(`{"type":"tool_use","name":"Read"}
{"type":"tool_use","name":"Edit"}
`)
		text, metrics := parseClaudeResultJSON(input)
		if text != "" {
			t.Errorf("resultText = %q, want empty", text)
		}
		// Without a result event, ToolCalls should not be populated.
		if metrics.ToolCalls != nil {
			t.Errorf("ToolCalls = %v, want nil (no result event)", metrics.ToolCalls)
		}
	})
}

func TestWizardBuildClaudeArgsStreamJSON(t *testing.T) {
	args := WizardBuildClaudeArgs("hello", "claude-sonnet-4-6", 50)

	// Must request stream-json (one JSONL event per turn, not a single result summary).
	var streamJSON bool
	for i, a := range args {
		if a == "--output-format" && i+1 < len(args) && args[i+1] == "stream-json" {
			streamJSON = true
			break
		}
	}
	if !streamJSON {
		t.Errorf("expected --output-format stream-json, got %v", args)
	}

	// stream-json silently degrades without --verbose; we MUST include it.
	if !containsArg(args, "--verbose") {
		t.Errorf("expected --verbose (required for stream-json intermediate events), got %v", args)
	}

	// Partial-message deltas help post-mortem of stuck agents.
	if !containsArg(args, "--include-partial-messages") {
		t.Errorf("expected --include-partial-messages, got %v", args)
	}

	// Regression guard: the old plain-json mode lost everything but the
	// result event — make sure we never go back.
	for i, a := range args {
		if a == "--output-format" && i+1 < len(args) && args[i+1] == "json" {
			t.Errorf("must not use plain --output-format json (emits only final result), got %v", args)
		}
	}
}

func TestWizardBuildClaudeArgsZeroMaxTurnsOmitsFlag(t *testing.T) {
	args := WizardBuildClaudeArgs("hello", "claude-sonnet-4-6", 0)
	if containsArg(args, "--max-turns") {
		t.Fatalf("expected zero maxTurns to omit --max-turns, got %v", args)
	}
}

func TestWizardConfiguredMaxTurnsZeroMeansUnlimited(t *testing.T) {
	if got := wizardConfiguredMaxTurns(nil); got != 0 {
		t.Fatalf("nil config maxTurns = %d, want 0", got)
	}
	if got := wizardConfiguredMaxTurns(&repoconfig.RepoConfig{}); got != 0 {
		t.Fatalf("zero config maxTurns = %d, want 0", got)
	}
	if got := wizardConfiguredMaxTurns(&repoconfig.RepoConfig{Agent: repoconfig.AgentConfig{MaxTurns: 12}}); got != 12 {
		t.Fatalf("positive config maxTurns = %d, want 12", got)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestParseClaudeResultJSON_StreamJSON(t *testing.T) {
	// A realistic stream-json output: system init, a few tool_use / tool_result
	// events, an assistant message, then the final result event as the last line.
	// The result.json contract (input/output tokens, turns, cost, tool counts)
	// must be parsed identically to the old single-line json mode.
	input := []byte(`{"type":"system","subtype":"init","session_id":"s1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"starting"}]}}
{"type":"tool_use","name":"Read","input":{"path":"x"}}
{"type":"tool_result","tool_use_id":"tu_1","content":"file contents"}
{"type":"tool_use","name":"Edit","input":{}}
{"type":"tool_result","tool_use_id":"tu_2","content":"edited"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial"}]}}
{"type":"tool_use","name":"Read","input":{"path":"y"}}
{"type":"tool_result","tool_use_id":"tu_3","content":"more"}
{"type":"result","subtype":"success","is_error":false,"num_turns":9,"result":"done","session_id":"s1","total_cost_usd":0.34,"usage":{"input_tokens":4200,"output_tokens":1800}}
`)

	text, metrics := parseClaudeResultJSON(input)
	if text != "done" {
		t.Errorf("resultText = %q, want %q", text, "done")
	}
	if metrics.InputTokens != 4200 {
		t.Errorf("InputTokens = %d, want 4200", metrics.InputTokens)
	}
	if metrics.OutputTokens != 1800 {
		t.Errorf("OutputTokens = %d, want 1800", metrics.OutputTokens)
	}
	if metrics.TotalTokens != 6000 {
		t.Errorf("TotalTokens = %d, want 6000", metrics.TotalTokens)
	}
	if metrics.Turns != 9 {
		t.Errorf("Turns = %d, want 9", metrics.Turns)
	}
	if metrics.CostUSD != 0.34 {
		t.Errorf("CostUSD = %f, want 0.34", metrics.CostUSD)
	}
	// Tool counts aggregated across the stream.
	if metrics.ToolCalls == nil {
		t.Fatal("expected non-nil ToolCalls, got nil")
	}
	if metrics.ToolCalls["Read"] != 2 {
		t.Errorf("ToolCalls[Read] = %d, want 2", metrics.ToolCalls["Read"])
	}
	if metrics.ToolCalls["Edit"] != 1 {
		t.Errorf("ToolCalls[Edit] = %d, want 1", metrics.ToolCalls["Edit"])
	}
}

func TestOpenProviderStreamLog(t *testing.T) {
	t.Run("creates provider subdir and stdout+stderr transcript pair", func(t *testing.T) {
		base := t.TempDir()

		stream, err := openProviderStreamLog(base, "claude", "implement")
		if err != nil {
			t.Fatalf("openProviderStreamLog: %v", err)
		}
		defer stream.stdout.Close()
		defer stream.stderr.Close()

		providerDir := filepath.Join(base, "claude")
		if _, err := os.Stat(providerDir); err != nil {
			t.Fatalf("expected claude subdir at %s: %v", providerDir, err)
		}

		if filepath.Dir(stream.stdoutPath) != providerDir {
			t.Errorf("stdout dir = %q, want %q", filepath.Dir(stream.stdoutPath), providerDir)
		}
		if filepath.Dir(stream.stderrPath) != providerDir {
			t.Errorf("stderr dir = %q, want %q", filepath.Dir(stream.stderrPath), providerDir)
		}

		stdoutName := filepath.Base(stream.stdoutPath)
		if !strings.HasPrefix(stdoutName, "implement-") || !strings.HasSuffix(stdoutName, ".jsonl") {
			t.Errorf("stdout filename %q should look like implement-<ts>.jsonl", stdoutName)
		}
		stderrName := filepath.Base(stream.stderrPath)
		if !strings.HasPrefix(stderrName, "implement-") || !strings.HasSuffix(stderrName, ".stderr.log") {
			t.Errorf("stderr filename %q should look like implement-<ts>.stderr.log", stderrName)
		}

		// Both files must share the <label>-<ts> stem.
		stdoutStem := strings.TrimSuffix(stdoutName, ".jsonl")
		stderrStem := strings.TrimSuffix(stderrName, ".stderr.log")
		if stdoutStem != stderrStem {
			t.Errorf("stdout/stderr stems differ: %q vs %q", stdoutStem, stderrStem)
		}
	})

	t.Run("provider keys the subdir", func(t *testing.T) {
		base := t.TempDir()
		stream, err := openProviderStreamLog(base, "codex", "implement")
		if err != nil {
			t.Fatalf("openProviderStreamLog: %v", err)
		}
		defer stream.stdout.Close()
		defer stream.stderr.Close()

		codexDir := filepath.Join(base, "codex")
		if _, err := os.Stat(codexDir); err != nil {
			t.Fatalf("expected codex subdir at %s: %v", codexDir, err)
		}
		if filepath.Dir(stream.stdoutPath) != codexDir {
			t.Errorf("stdout dir = %q, want %q", filepath.Dir(stream.stdoutPath), codexDir)
		}
	})

	t.Run("writes to both stdout and stderr handles land on disk", func(t *testing.T) {
		base := t.TempDir()
		stream, err := openProviderStreamLog(base, "claude", "review-fix")
		if err != nil {
			t.Fatalf("openProviderStreamLog: %v", err)
		}

		stdoutStream := `{"type":"system","subtype":"init"}
{"type":"tool_use","name":"Read"}
{"type":"tool_result","content":"ok"}
{"type":"result","result":"done","num_turns":3,"total_cost_usd":0.02,"usage":{"input_tokens":100,"output_tokens":50}}
`
		if _, err := stream.stdout.Write([]byte(stdoutStream)); err != nil {
			t.Fatalf("stdout write: %v", err)
		}
		if _, err := stream.stderr.Write([]byte("claude: warning on stderr\n")); err != nil {
			t.Fatalf("stderr write: %v", err)
		}
		stream.stdout.Close()
		stream.stderr.Close()

		gotStdout, err := os.ReadFile(stream.stdoutPath)
		if err != nil {
			t.Fatalf("read stdout log: %v", err)
		}
		if !strings.Contains(string(gotStdout), `"type":"tool_use"`) {
			t.Errorf("stdout log missing tool_use events; got:\n%s", gotStdout)
		}
		if !strings.Contains(string(gotStdout), `"type":"result"`) {
			t.Errorf("stdout log missing result event; got:\n%s", gotStdout)
		}

		gotStderr, err := os.ReadFile(stream.stderrPath)
		if err != nil {
			t.Fatalf("read stderr log: %v", err)
		}
		if !strings.Contains(string(gotStderr), "claude: warning on stderr") {
			t.Errorf("stderr log missing expected line; got:\n%s", gotStderr)
		}
	})

	t.Run("empty agentResultDir errors", func(t *testing.T) {
		stream, err := openProviderStreamLog("", "claude", "implement")
		if err == nil {
			if stream != nil {
				stream.stdout.Close()
				stream.stderr.Close()
			}
			t.Fatal("expected error for empty agentResultDir")
		}
	})

	t.Run("empty provider errors (never silently write under \"\")", func(t *testing.T) {
		base := t.TempDir()
		stream, err := openProviderStreamLog(base, "", "implement")
		if err == nil {
			if stream != nil {
				stream.stdout.Close()
				stream.stderr.Close()
			}
			t.Fatal("expected error for empty provider")
		}
	})

	t.Run("unwritable dir errors", func(t *testing.T) {
		base := t.TempDir()
		blocker := filepath.Join(base, "file")
		if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		stream, err := openProviderStreamLog(filepath.Join(blocker, "nested"), "claude", "implement")
		if err == nil {
			if stream != nil {
				stream.stdout.Close()
				stream.stderr.Close()
			}
			t.Fatal("expected error when mkdir fails")
		}
	})

	t.Run("sanitizes label with slash/space/colon", func(t *testing.T) {
		base := t.TempDir()
		stream, err := openProviderStreamLog(base, "claude", "weird/label name:x")
		if err != nil {
			t.Fatalf("openProviderStreamLog: %v", err)
		}
		defer stream.stdout.Close()
		defer stream.stderr.Close()

		name := filepath.Base(stream.stdoutPath)
		stem := strings.TrimSuffix(name, ".jsonl")
		if strings.ContainsAny(stem, "/: ") {
			t.Errorf("sanitized stem %q still contains unsafe chars", stem)
		}
		if !strings.HasPrefix(name, "weird-label-name-x-") {
			t.Errorf("expected sanitized prefix weird-label-name-x-, got %q", name)
		}
	})
}

func TestWizardAgentResultDir(t *testing.T) {
	t.Run("returns <DoltGlobalDir>/wizards/<name>", func(t *testing.T) {
		deps := &Deps{DoltGlobalDir: func() string { return "/tmp/spire" }}
		got := WizardAgentResultDir(deps, "wizard-spi-abc-w1-1")
		want := filepath.Join("/tmp/spire", "wizards", "wizard-spi-abc-w1-1")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("nil deps returns empty string", func(t *testing.T) {
		if got := WizardAgentResultDir(nil, "x"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("nil DoltGlobalDir func returns empty string", func(t *testing.T) {
		deps := &Deps{}
		if got := WizardAgentResultDir(deps, "x"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestWizardWriteResultIncludesMetrics(t *testing.T) {
	dir := t.TempDir()
	deps := &Deps{
		DoltGlobalDir: func() string { return dir },
	}
	noop := func(string, ...interface{}) {}

	metrics := ClaudeMetrics{
		InputTokens:      3000,
		OutputTokens:     2000,
		TotalTokens:      5000,
		Turns:            5,
		MaxTurns:         75,
		StopReason:       "end_turn",
		CacheReadTokens:  12345,
		CacheWriteTokens: 6789,
		CostUSD:          0.12,
	}
	WizardWriteResult("test-wizard", "spi-test", "success", "feat/test", "abc123",
		42*time.Second, metrics, deps, noop)

	data, err := os.ReadFile(filepath.Join(dir, "wizards", "test-wizard", "result.json"))
	if err != nil {
		t.Fatalf("read result.json: %s", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parse result.json: %s", err)
	}

	checks := map[string]float64{
		"context_tokens_in":  3000,
		"context_tokens_out": 2000,
		"total_tokens":       5000,
		"turns":              5,
		"max_turns":          75,
		"cache_read_tokens":  12345,
		"cache_write_tokens": 6789,
		"cost_usd":           0.12,
	}
	for key, want := range checks {
		got, ok := result[key].(float64)
		if !ok {
			t.Errorf("result.json missing %s", key)
			continue
		}
		if got != want {
			t.Errorf("result.json %s = %v, want %v", key, got, want)
		}
	}

	// stop_reason is a string; check separately.
	if got, ok := result["stop_reason"].(string); !ok {
		t.Errorf("result.json missing stop_reason or wrong type: %v", result["stop_reason"])
	} else if got != "end_turn" {
		t.Errorf("result.json stop_reason = %q, want %q", got, "end_turn")
	}
}

func TestClaudeMetricsAdd(t *testing.T) {
	a := ClaudeMetrics{
		InputTokens:      100,
		OutputTokens:     50,
		TotalTokens:      150,
		Turns:            3,
		CacheReadTokens:  1000,
		CacheWriteTokens: 200,
		CostUSD:          0.05,
	}
	b := ClaudeMetrics{
		InputTokens:      200,
		OutputTokens:     100,
		TotalTokens:      300,
		Turns:            5,
		CacheReadTokens:  3000,
		CacheWriteTokens: 400,
		CostUSD:          0.10,
	}
	sum := a.Add(b)

	if sum.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300", sum.InputTokens)
	}
	if sum.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150", sum.OutputTokens)
	}
	if sum.TotalTokens != 450 {
		t.Errorf("TotalTokens = %d, want 450", sum.TotalTokens)
	}
	if sum.Turns != 8 {
		t.Errorf("Turns = %d, want 8", sum.Turns)
	}
	if sum.CacheReadTokens != 4000 {
		t.Errorf("CacheReadTokens = %d, want 4000", sum.CacheReadTokens)
	}
	if sum.CacheWriteTokens != 600 {
		t.Errorf("CacheWriteTokens = %d, want 600", sum.CacheWriteTokens)
	}
	if diff := sum.CostUSD - 0.15; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("CostUSD = %f, want 0.15", sum.CostUSD)
	}
}

func TestClaudeMetricsAdd_MaxTurnsAndStopReasonFallback(t *testing.T) {
	t.Run("receiver wins when both set", func(t *testing.T) {
		a := ClaudeMetrics{MaxTurns: 75, StopReason: "end_turn"}
		b := ClaudeMetrics{MaxTurns: 150, StopReason: "max_turns"}
		sum := a.Add(b)
		if sum.MaxTurns != 75 {
			t.Errorf("MaxTurns = %d, want 75 (receiver wins)", sum.MaxTurns)
		}
		if sum.StopReason != "end_turn" {
			t.Errorf("StopReason = %q, want %q (receiver wins)", sum.StopReason, "end_turn")
		}
	})

	t.Run("falls back to other when receiver unset", func(t *testing.T) {
		a := ClaudeMetrics{}
		b := ClaudeMetrics{MaxTurns: 150, StopReason: "tool_use"}
		sum := a.Add(b)
		if sum.MaxTurns != 150 {
			t.Errorf("MaxTurns = %d, want 150 (fallback to other)", sum.MaxTurns)
		}
		if sum.StopReason != "tool_use" {
			t.Errorf("StopReason = %q, want %q (fallback to other)", sum.StopReason, "tool_use")
		}
	})

	t.Run("both unset stays unset", func(t *testing.T) {
		a := ClaudeMetrics{}
		b := ClaudeMetrics{}
		sum := a.Add(b)
		if sum.MaxTurns != 0 {
			t.Errorf("MaxTurns = %d, want 0", sum.MaxTurns)
		}
		if sum.StopReason != "" {
			t.Errorf("StopReason = %q, want empty", sum.StopReason)
		}
	})

	t.Run("partial: receiver has MaxTurns, other has StopReason", func(t *testing.T) {
		a := ClaudeMetrics{MaxTurns: 75}
		b := ClaudeMetrics{StopReason: "end_turn"}
		sum := a.Add(b)
		if sum.MaxTurns != 75 {
			t.Errorf("MaxTurns = %d, want 75", sum.MaxTurns)
		}
		if sum.StopReason != "end_turn" {
			t.Errorf("StopReason = %q, want %q", sum.StopReason, "end_turn")
		}
	})
}

func TestClaudeMetricsAdd_ToolCallsMerge(t *testing.T) {
	t.Run("both maps populated with overlapping keys", func(t *testing.T) {
		a := ClaudeMetrics{ToolCalls: map[string]int{"Read": 5, "Edit": 2}}
		b := ClaudeMetrics{ToolCalls: map[string]int{"Read": 3, "Bash": 1}}
		sum := a.Add(b)
		if sum.ToolCalls == nil {
			t.Fatal("ToolCalls is nil, want non-nil")
		}
		if sum.ToolCalls["Read"] != 8 {
			t.Errorf("ToolCalls[Read] = %d, want 8", sum.ToolCalls["Read"])
		}
		if sum.ToolCalls["Edit"] != 2 {
			t.Errorf("ToolCalls[Edit] = %d, want 2", sum.ToolCalls["Edit"])
		}
		if sum.ToolCalls["Bash"] != 1 {
			t.Errorf("ToolCalls[Bash] = %d, want 1", sum.ToolCalls["Bash"])
		}
		if len(sum.ToolCalls) != 3 {
			t.Errorf("ToolCalls has %d keys, want 3", len(sum.ToolCalls))
		}
	})

	t.Run("first nil second populated", func(t *testing.T) {
		a := ClaudeMetrics{ToolCalls: nil}
		b := ClaudeMetrics{ToolCalls: map[string]int{"Read": 3}}
		sum := a.Add(b)
		if sum.ToolCalls == nil {
			t.Fatal("ToolCalls is nil, want non-nil")
		}
		if sum.ToolCalls["Read"] != 3 {
			t.Errorf("ToolCalls[Read] = %d, want 3", sum.ToolCalls["Read"])
		}
	})

	t.Run("first populated second nil", func(t *testing.T) {
		a := ClaudeMetrics{ToolCalls: map[string]int{"Edit": 7}}
		b := ClaudeMetrics{ToolCalls: nil}
		sum := a.Add(b)
		if sum.ToolCalls == nil {
			t.Fatal("ToolCalls is nil, want non-nil")
		}
		if sum.ToolCalls["Edit"] != 7 {
			t.Errorf("ToolCalls[Edit] = %d, want 7", sum.ToolCalls["Edit"])
		}
	})

	t.Run("both nil", func(t *testing.T) {
		a := ClaudeMetrics{ToolCalls: nil}
		b := ClaudeMetrics{ToolCalls: nil}
		sum := a.Add(b)
		if sum.ToolCalls != nil {
			t.Errorf("ToolCalls = %v, want nil", sum.ToolCalls)
		}
	})

	t.Run("both empty maps", func(t *testing.T) {
		a := ClaudeMetrics{ToolCalls: map[string]int{}}
		b := ClaudeMetrics{ToolCalls: map[string]int{}}
		sum := a.Add(b)
		// Empty maps have len > 0 == false, so ToolCalls should be nil.
		if sum.ToolCalls != nil {
			t.Errorf("ToolCalls = %v, want nil (both empty)", sum.ToolCalls)
		}
	})

	t.Run("does not mutate originals", func(t *testing.T) {
		a := ClaudeMetrics{ToolCalls: map[string]int{"Read": 5}}
		b := ClaudeMetrics{ToolCalls: map[string]int{"Read": 3}}
		_ = a.Add(b)
		if a.ToolCalls["Read"] != 5 {
			t.Errorf("original a.ToolCalls[Read] = %d, want 5 (mutation detected)", a.ToolCalls["Read"])
		}
		if b.ToolCalls["Read"] != 3 {
			t.Errorf("original b.ToolCalls[Read] = %d, want 3 (mutation detected)", b.ToolCalls["Read"])
		}
	})
}
