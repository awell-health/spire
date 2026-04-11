package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeProvider_SetupToolMetrics(t *testing.T) {
	dir := t.TempDir()
	p := &ClaudeProvider{}

	if err := p.SetupToolMetrics(dir); err != nil {
		t.Fatalf("SetupToolMetrics() error: %v", err)
	}

	// Verify hook script was created.
	hookScript := filepath.Join(dir, ".claude", "spire-tool-counter.sh")
	info, err := os.Stat(hookScript)
	if err != nil {
		t.Fatalf("hook script not created: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Error("hook script is not executable")
	}

	// Verify settings.json was created with PostToolUse hook.
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.json not created: %v", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings.json parse error: %v", err)
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		t.Fatal("settings.json missing hooks map")
	}
	postToolUse, ok := hooks["PostToolUse"]
	if !ok {
		t.Fatal("settings.json missing PostToolUse hook")
	}
	arr, ok := postToolUse.([]interface{})
	if !ok || len(arr) == 0 {
		t.Fatal("PostToolUse should be a non-empty array")
	}
}

func TestClaudeProvider_SetupToolMetrics_MergesExisting(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	os.MkdirAll(claudeDir, 0755)

	// Write existing settings with a SessionStart hook.
	existing := map[string]interface{}{
		"hooks": map[string]interface{}{
			"SessionStart": []interface{}{
				map[string]interface{}{
					"hooks": []interface{}{
						map[string]interface{}{
							"type":    "command",
							"command": "echo hello",
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), data, 0644)

	p := &ClaudeProvider{}
	if err := p.SetupToolMetrics(dir); err != nil {
		t.Fatalf("SetupToolMetrics() error: %v", err)
	}

	// Verify both hooks are present.
	settingsData, _ := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	var settings map[string]interface{}
	json.Unmarshal(settingsData, &settings)

	hooks := settings["hooks"].(map[string]interface{})
	if _, ok := hooks["SessionStart"]; !ok {
		t.Error("existing SessionStart hook was clobbered")
	}
	if _, ok := hooks["PostToolUse"]; !ok {
		t.Error("PostToolUse hook was not added")
	}
}

func TestClaudeProvider_CollectToolMetrics(t *testing.T) {
	t.Run("typical JSONL file", func(t *testing.T) {
		dir := t.TempDir()
		counterFile := filepath.Join(dir, ToolCounterFile)
		content := `{"tool":"Read"}
{"tool":"Edit"}
{"tool":"Read"}
{"tool":"Bash"}
{"tool":"Read"}
{"tool":"Grep"}
`
		os.WriteFile(counterFile, []byte(content), 0644)

		p := &ClaudeProvider{}
		counts, err := p.CollectToolMetrics(dir)
		if err != nil {
			t.Fatalf("CollectToolMetrics() error: %v", err)
		}

		expected := map[string]int{"Read": 3, "Edit": 1, "Bash": 1, "Grep": 1}
		for tool, want := range expected {
			if got := counts[tool]; got != want {
				t.Errorf("counts[%s] = %d, want %d", tool, got, want)
			}
		}
		if len(counts) != len(expected) {
			t.Errorf("counts has %d keys, want %d", len(counts), len(expected))
		}

		// Verify counter file was cleaned up.
		if _, err := os.Stat(counterFile); !os.IsNotExist(err) {
			t.Error("counter file was not cleaned up after collection")
		}
	})

	t.Run("no counter file (no tool calls)", func(t *testing.T) {
		dir := t.TempDir()

		p := &ClaudeProvider{}
		counts, err := p.CollectToolMetrics(dir)
		if err != nil {
			t.Fatalf("CollectToolMetrics() error: %v", err)
		}
		if counts != nil {
			t.Errorf("counts = %v, want nil", counts)
		}
	})

	t.Run("empty counter file", func(t *testing.T) {
		dir := t.TempDir()
		counterFile := filepath.Join(dir, ToolCounterFile)
		os.WriteFile(counterFile, []byte(""), 0644)

		p := &ClaudeProvider{}
		counts, err := p.CollectToolMetrics(dir)
		if err != nil {
			t.Fatalf("CollectToolMetrics() error: %v", err)
		}
		if counts != nil {
			t.Errorf("counts = %v, want nil (empty file)", counts)
		}
	})

	t.Run("malformed lines are skipped", func(t *testing.T) {
		dir := t.TempDir()
		counterFile := filepath.Join(dir, ToolCounterFile)
		content := `{"tool":"Read"}
{bad json}
{"tool":""}
{"tool":"Edit"}
not json
`
		os.WriteFile(counterFile, []byte(content), 0644)

		p := &ClaudeProvider{}
		counts, err := p.CollectToolMetrics(dir)
		if err != nil {
			t.Fatalf("CollectToolMetrics() error: %v", err)
		}
		if counts["Read"] != 1 {
			t.Errorf("counts[Read] = %d, want 1", counts["Read"])
		}
		if counts["Edit"] != 1 {
			t.Errorf("counts[Edit] = %d, want 1", counts["Edit"])
		}
		if len(counts) != 2 {
			t.Errorf("counts has %d keys, want 2", len(counts))
		}
	})

	t.Run("large file with many entries", func(t *testing.T) {
		dir := t.TempDir()
		counterFile := filepath.Join(dir, ToolCounterFile)

		f, _ := os.Create(counterFile)
		for i := 0; i < 1000; i++ {
			f.WriteString(`{"tool":"Read"}` + "\n")
		}
		for i := 0; i < 500; i++ {
			f.WriteString(`{"tool":"Bash"}` + "\n")
		}
		f.Close()

		p := &ClaudeProvider{}
		counts, err := p.CollectToolMetrics(dir)
		if err != nil {
			t.Fatalf("CollectToolMetrics() error: %v", err)
		}
		if counts["Read"] != 1000 {
			t.Errorf("counts[Read] = %d, want 1000", counts["Read"])
		}
		if counts["Bash"] != 500 {
			t.Errorf("counts[Bash] = %d, want 500", counts["Bash"])
		}
	})
}

func TestAggregateToolCounts(t *testing.T) {
	t.Run("typical file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "counts.jsonl")
		os.WriteFile(path, []byte(`{"tool":"Read"}
{"tool":"Write"}
{"tool":"Read"}
`), 0644)

		counts, err := AggregateToolCounts(path)
		if err != nil {
			t.Fatalf("AggregateToolCounts() error: %v", err)
		}
		if counts["Read"] != 2 {
			t.Errorf("counts[Read] = %d, want 2", counts["Read"])
		}
		if counts["Write"] != 1 {
			t.Errorf("counts[Write] = %d, want 1", counts["Write"])
		}
	})

	t.Run("nonexistent file returns nil", func(t *testing.T) {
		counts, err := AggregateToolCounts("/nonexistent/path/file.jsonl")
		if err != nil {
			t.Fatalf("AggregateToolCounts() error: %v", err)
		}
		if counts != nil {
			t.Errorf("counts = %v, want nil", counts)
		}
	})
}

func TestSetupToolMetrics_ConvenienceFunction(t *testing.T) {
	t.Run("claude provider sets up hooks", func(t *testing.T) {
		dir := t.TempDir()
		if err := SetupToolMetrics("claude", dir); err != nil {
			t.Fatalf("SetupToolMetrics() error: %v", err)
		}
		// Verify hook script exists.
		if _, err := os.Stat(filepath.Join(dir, ".claude", "spire-tool-counter.sh")); err != nil {
			t.Errorf("hook script not found: %v", err)
		}
	})

	t.Run("codex provider is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		if err := SetupToolMetrics("codex", dir); err != nil {
			t.Fatalf("SetupToolMetrics() error: %v", err)
		}
		// No .claude dir should be created for codex.
		if _, err := os.Stat(filepath.Join(dir, ".claude")); !os.IsNotExist(err) {
			t.Error("codex provider should not create .claude directory")
		}
	})

	t.Run("unknown provider is a no-op", func(t *testing.T) {
		if err := SetupToolMetrics("gemini", t.TempDir()); err != nil {
			t.Fatalf("SetupToolMetrics() error: %v", err)
		}
	})
}

func TestCollectToolMetrics_ConvenienceFunction(t *testing.T) {
	t.Run("claude provider collects from file", func(t *testing.T) {
		dir := t.TempDir()
		counterFile := filepath.Join(dir, ToolCounterFile)
		os.WriteFile(counterFile, []byte(`{"tool":"Read"}
{"tool":"Edit"}
`), 0644)

		counts, err := CollectToolMetrics("claude", dir)
		if err != nil {
			t.Fatalf("CollectToolMetrics() error: %v", err)
		}
		if counts["Read"] != 1 || counts["Edit"] != 1 {
			t.Errorf("counts = %v, want Read:1 Edit:1", counts)
		}
	})

	t.Run("codex provider returns nil", func(t *testing.T) {
		counts, err := CollectToolMetrics("codex", t.TempDir())
		if err != nil {
			t.Fatalf("CollectToolMetrics() error: %v", err)
		}
		if counts != nil {
			t.Errorf("counts = %v, want nil", counts)
		}
	})
}
