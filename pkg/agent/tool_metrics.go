package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ToolMetricsCollector is an optional interface that AIProviders can implement
// to track per-tool invocation counts during a session. The wizard calls
// SetupToolMetrics before invoking the agent CLI and CollectToolMetrics after
// it exits. Providers that don't support tool metrics simply don't implement
// this interface — the convenience helpers below return nil in that case.
type ToolMetricsCollector interface {
	// SetupToolMetrics configures tool-use tracking before invoking the agent.
	// worktreePath is the working directory where the agent runs.
	SetupToolMetrics(worktreePath string) error

	// CollectToolMetrics reads and aggregates tool invocation data after the
	// agent exits. Returns a map of tool_name → invocation count.
	// Returns nil (not an error) when no data is available — this is graceful
	// degradation equivalent to today's NULL.
	CollectToolMetrics(worktreePath string) (map[string]int, error)
}

// ToolCounterFile is the name of the JSONL file where tool invocations are logged.
const ToolCounterFile = ".spire-tool-counts.jsonl"

// SetupToolMetrics is a convenience function that sets up tool metrics collection
// for the named provider. Returns nil if the provider doesn't support metrics
// or the provider name is unknown.
func SetupToolMetrics(providerName, worktreePath string) error {
	p, err := GetProvider(providerName)
	if err != nil {
		return nil
	}
	if tmc, ok := p.(ToolMetricsCollector); ok {
		return tmc.SetupToolMetrics(worktreePath)
	}
	return nil
}

// CollectToolMetrics is a convenience function that collects tool metrics
// for the named provider. Returns nil if the provider doesn't support metrics
// or the provider name is unknown.
func CollectToolMetrics(providerName, worktreePath string) (map[string]int, error) {
	p, err := GetProvider(providerName)
	if err != nil {
		return nil, nil
	}
	if tmc, ok := p.(ToolMetricsCollector); ok {
		return tmc.CollectToolMetrics(worktreePath)
	}
	return nil, nil
}

// --- ClaudeProvider ToolMetricsCollector implementation ---
//
// Uses Claude Code's PostToolUse hook to log each tool invocation to a JSONL
// counter file in the worktree. After Claude exits, the wizard reads and
// aggregates the file.

// SetupToolMetrics writes a PostToolUse hook script into the worktree's .claude/
// directory and merges the hook into .claude/settings.json.
func (c *ClaudeProvider) SetupToolMetrics(worktreePath string) error {
	claudeDir := filepath.Join(worktreePath, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}

	counterFile := filepath.Join(worktreePath, ToolCounterFile)

	// Write the hook script. Uses jq to extract the tool name from stdin JSON.
	// If jq is unavailable the hook fails silently — graceful degradation.
	hookScript := filepath.Join(claudeDir, "spire-tool-counter.sh")
	scriptContent := fmt.Sprintf(`#!/bin/sh
# Spire PostToolUse hook — logs tool invocations to a JSONL counter file.
read -r input
tool=$(echo "$input" | jq -r '.tool_name // .tool // empty' 2>/dev/null)
if [ -n "$tool" ]; then
  echo "{\"tool\":\"$tool\"}" >> "%s"
fi
`, counterFile)

	if err := os.WriteFile(hookScript, []byte(scriptContent), 0755); err != nil {
		return fmt.Errorf("write hook script: %w", err)
	}

	// Merge hook into .claude/settings.json
	return mergePostToolUseHook(claudeDir, hookScript)
}

// CollectToolMetrics reads the JSONL counter file, aggregates tool counts,
// and removes the file. Returns nil if the file doesn't exist (no tool calls
// recorded or hook failed silently).
func (c *ClaudeProvider) CollectToolMetrics(worktreePath string) (map[string]int, error) {
	return AggregateToolCounts(filepath.Join(worktreePath, ToolCounterFile))
}

// AggregateToolCounts reads a JSONL file of {"tool":"Name"} entries, aggregates
// by tool name, removes the file, and returns the counts. Exported for testing.
func AggregateToolCounts(counterFilePath string) (map[string]int, error) {
	f, err := os.Open(counterFilePath)
	if err != nil {
		// File doesn't exist — no tool calls recorded. Graceful degradation.
		return nil, nil
	}
	defer f.Close()

	counts := make(map[string]int)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry struct {
			Tool string `json:"tool"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.Tool != "" {
			counts[entry.Tool]++
		}
	}
	f.Close()

	// Clean up the counter file.
	os.Remove(counterFilePath)

	if len(counts) == 0 {
		return nil, nil
	}
	return counts, nil
}

// mergePostToolUseHook adds a PostToolUse hook entry to .claude/settings.json,
// merging with any existing settings without clobbering them.
func mergePostToolUseHook(claudeDir, hookScript string) error {
	settingsPath := filepath.Join(claudeDir, "settings.json")

	var settings map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = make(map[string]interface{})
	}

	// Build the hook matcher entry (no matcher pattern = matches all tools).
	hookEntry := map[string]interface{}{
		"hooks": []interface{}{
			map[string]interface{}{
				"type":    "command",
				"command": hookScript,
				"timeout": 5,
			},
		},
	}

	// Get or create hooks map.
	hooksMap := make(map[string]interface{})
	if existing, ok := settings["hooks"]; ok {
		if existingMap, ok := existing.(map[string]interface{}); ok {
			hooksMap = existingMap
		}
	}

	// Append to existing PostToolUse hooks (don't replace).
	if existing, ok := hooksMap["PostToolUse"]; ok {
		if arr, ok := existing.([]interface{}); ok {
			hooksMap["PostToolUse"] = append(arr, hookEntry)
		} else {
			hooksMap["PostToolUse"] = []interface{}{hookEntry}
		}
	} else {
		hooksMap["PostToolUse"] = []interface{}{hookEntry}
	}
	settings["hooks"] = hooksMap

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	return os.WriteFile(settingsPath, append(data, '\n'), 0644)
}
