package wizard

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestWizardBuildClaudeArgsIncludesJSONFormat(t *testing.T) {
	args := WizardBuildClaudeArgs("hello", "claude-sonnet-4-6", 50)
	found := false
	for i, a := range args {
		if a == "--output-format" && i+1 < len(args) && args[i+1] == "json" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("WizardBuildClaudeArgs missing --output-format json, got %v", args)
	}
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
