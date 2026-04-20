package wizard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeResultJSON(t *testing.T) {
	t.Run("valid result event", func(t *testing.T) {
		input := []byte(`{"type":"assistant","message":"thinking..."}
{"type":"result","subtype":"success","cost_usd":0.05,"duration_ms":45000,"is_error":false,"num_turns":5,"result":"Here is the implementation.","session_id":"abc","total_cost_usd":0.12,"usage":{"input_tokens":3000,"output_tokens":2000}}
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

func TestOpenClaudeStreamLog(t *testing.T) {
	t.Run("creates claude subdir and log file", func(t *testing.T) {
		base := t.TempDir()

		f, path := openClaudeStreamLog(base, "implement")
		if f == nil {
			t.Fatal("expected non-nil file handle")
		}
		defer f.Close()

		if path == "" {
			t.Fatal("expected non-empty path")
		}

		claudeDir := filepath.Join(base, "claude")
		if _, err := os.Stat(claudeDir); err != nil {
			t.Fatalf("expected claude subdir at %s: %v", claudeDir, err)
		}

		// Filename must match the board glob <dir>/claude/*.log and start with label.
		if filepath.Dir(path) != claudeDir {
			t.Errorf("log dir = %q, want %q", filepath.Dir(path), claudeDir)
		}
		name := filepath.Base(path)
		if !strings.HasPrefix(name, "implement-") || !strings.HasSuffix(name, ".log") {
			t.Errorf("log filename %q should look like implement-<ts>.log", name)
		}
	})

	t.Run("writing a multi-event stream lands on disk", func(t *testing.T) {
		base := t.TempDir()
		f, path := openClaudeStreamLog(base, "review-fix")
		if f == nil {
			t.Fatal("expected non-nil file handle")
		}

		stream := `{"type":"system","subtype":"init"}
{"type":"tool_use","name":"Read"}
{"type":"tool_result","content":"ok"}
{"type":"result","result":"done","num_turns":3,"total_cost_usd":0.02,"usage":{"input_tokens":100,"output_tokens":50}}
`
		if _, err := f.Write([]byte(stream)); err != nil {
			t.Fatalf("write: %v", err)
		}
		f.Close()

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read log: %v", err)
		}
		if !strings.Contains(string(got), `"type":"tool_use"`) {
			t.Errorf("log missing tool_use events; got:\n%s", got)
		}
		if !strings.Contains(string(got), `"type":"result"`) {
			t.Errorf("log missing result event; got:\n%s", got)
		}
	})

	t.Run("empty agentResultDir disables log file (best-effort, no error)", func(t *testing.T) {
		f, path := openClaudeStreamLog("", "implement")
		if f != nil {
			t.Error("expected nil file for empty dir")
			f.Close()
		}
		if path != "" {
			t.Errorf("expected empty path, got %q", path)
		}
	})

	t.Run("unwritable dir falls back silently (no panic)", func(t *testing.T) {
		// Point at a path where mkdir will fail (a path under an existing
		// regular file). Best-effort contract: return (nil, "") without erroring.
		base := t.TempDir()
		blocker := filepath.Join(base, "file")
		if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		f, path := openClaudeStreamLog(filepath.Join(blocker, "nested"), "implement")
		if f != nil {
			t.Error("expected nil file when mkdir fails")
			f.Close()
		}
		if path != "" {
			t.Errorf("expected empty path on failure, got %q", path)
		}
	})

	t.Run("sanitizes label with slash/space/colon", func(t *testing.T) {
		base := t.TempDir()
		f, path := openClaudeStreamLog(base, "weird/label name:x")
		if f == nil {
			t.Fatal("expected non-nil file handle")
		}
		defer f.Close()
		name := filepath.Base(path)
		if strings.ContainsAny(name, "/: ") {
			// filenames with slashes would land in a subdir; spaces/colons are
			// legal on most filesystems but portability + the executor's
			// pattern — keep them out.
			t.Errorf("sanitized name %q still contains unsafe chars", name)
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
		InputTokens:  3000,
		OutputTokens: 2000,
		TotalTokens:  5000,
		Turns:        5,
		CostUSD:      0.12,
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
}

func TestClaudeMetricsAdd(t *testing.T) {
	a := ClaudeMetrics{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, Turns: 3, CostUSD: 0.05}
	b := ClaudeMetrics{InputTokens: 200, OutputTokens: 100, TotalTokens: 300, Turns: 5, CostUSD: 0.10}
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
	if diff := sum.CostUSD - 0.15; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("CostUSD = %f, want 0.15", sum.CostUSD)
	}
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
