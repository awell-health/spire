// Package agent — provider.go defines the AIProvider interface for multi-provider
// CLI invocation. Each provider (Claude, Codex, Cursor) knows how to build CLI
// arguments and normalize output for its respective AI tool.
//
// Provider dispatch happens inside the spawned spire subprocess (apprentice run,
// sage review), not at the Backend level. The Backend interface manages
// process lifecycle (spawn/kill/logs); the AIProvider interface manages
// AI CLI invocation within a running process.
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// AIProvider abstracts the invocation of an AI CLI tool.
// Implementations exist for Claude, Codex, and Cursor.
type AIProvider interface {
	// Name returns the provider identifier (e.g. "claude", "codex", "cursor").
	Name() string

	// Binary returns the CLI executable name (e.g. "claude", "codex").
	Binary() string

	// BuildArgs constructs CLI arguments for the given invocation options.
	BuildArgs(opts InvokeOpts) []string

	// NormalizeResult extracts a normalized result from provider-specific output.
	// Returns the result text and any error.
	NormalizeResult(raw []byte) (*ProviderResult, error)
}

// InvokeOpts describes the parameters for an AI CLI invocation.
type InvokeOpts struct {
	Prompt    string   // The prompt text to send
	Model     string   // Model identifier (provider-specific mapping applied internally)
	MaxTurns  int      // Turn budget (0 = unlimited)
	OutputFmt string   // "json" or "text"
	SkipPerms bool     // --dangerously-skip-permissions or equivalent
	ExtraArgs []string // Additional CLI flags
}

// ProviderResult is the normalized output from any AI provider.
type ProviderResult struct {
	Text string // Extracted result text
}

// --- Claude provider ---

// ClaudeProvider implements AIProvider for the Claude CLI.
type ClaudeProvider struct{}

func (c *ClaudeProvider) Name() string   { return "claude" }
func (c *ClaudeProvider) Binary() string { return "claude" }

func (c *ClaudeProvider) BuildArgs(opts InvokeOpts) []string {
	var args []string
	if opts.SkipPerms {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, "-p", opts.Prompt)
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.OutputFmt != "" {
		args = append(args, "--output-format", opts.OutputFmt)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opts.MaxTurns))
	}
	args = append(args, opts.ExtraArgs...)
	return args
}

func (c *ClaudeProvider) NormalizeResult(raw []byte) (*ProviderResult, error) {
	// Claude JSON output contains result events; extract the text.
	// Try to parse as streaming JSON events (newline-delimited).
	for _, line := range splitJSONLines(raw) {
		var evt struct {
			Type   string `json:"type"`
			Result struct {
				Text string `json:"text"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &evt); err == nil && evt.Type == "result" {
			return &ProviderResult{Text: evt.Result.Text}, nil
		}
	}
	// Fallback: return raw output as text.
	return &ProviderResult{Text: string(raw)}, nil
}

// --- Codex provider ---

// CodexProvider implements AIProvider for the OpenAI Codex CLI.
type CodexProvider struct{}

func (c *CodexProvider) Name() string   { return "codex" }
func (c *CodexProvider) Binary() string { return "codex" }

func (c *CodexProvider) BuildArgs(opts InvokeOpts) []string {
	// Codex CLI uses: codex [--approval-mode full-auto] exec --json "prompt"
	// --approval-mode full-auto is the equivalent of --dangerously-skip-permissions
	var args []string
	if opts.SkipPerms {
		args = append(args, "--approval-mode", "full-auto")
	}
	args = append(args, "exec", "--json", opts.Prompt)
	if opts.Model != "" {
		args = append(args, "--model", mapCodexModel(opts.Model))
	}
	args = append(args, opts.ExtraArgs...)
	return args
}

// codexEvent is one line of 'codex exec --json' JSONL output.
// Only the fields NormalizeResult needs are modeled here; other fields
// (usage totals, command_execution detail) are handled by the transcript
// adapter in a separate subtask.
type codexEvent struct {
	Type string          `json:"type"`
	Item *codexEventItem `json:"item,omitempty"`
}

type codexEventItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (c *CodexProvider) NormalizeResult(raw []byte) (*ProviderResult, error) {
	text, err := parseCodexFinalMessage(string(raw))
	if err != nil {
		return nil, err
	}
	return &ProviderResult{Text: text}, nil
}

// parseCodexFinalMessage scans 'codex exec --json' stdout line by line,
// tracking the last item.completed agent_message text and returning it.
// Non-JSON lines and malformed JSON lines are skipped — the stream may
// contain diagnostic noise before the JSONL begins. If no agent_message
// event is observed, the trimmed raw stdout is returned as a fallback so
// degenerate runs aren't worse than the previous plain-text behavior.
func parseCodexFinalMessage(stdout string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	// A single agent_message may exceed the default 64 KiB line cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var lastAgentText string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Type == "item.completed" && ev.Item != nil && ev.Item.Type == "agent_message" {
			lastAgentText = ev.Item.Text
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if lastAgentText == "" {
		return strings.TrimSpace(stdout), nil
	}
	return lastAgentText, nil
}

// mapCodexModel translates Claude model identifiers to Codex equivalents.
// Unknown models pass through unchanged.
func mapCodexModel(model string) string {
	// Codex CLI uses OpenAI model identifiers.
	// Claude-specific model names need translation.
	switch {
	case model == "claude-opus-4-6" || model == "opus":
		return "o3" // map Claude's most capable to Codex's most capable
	case model == "claude-sonnet-4-6" || model == "sonnet":
		return "o4-mini"
	default:
		return model // pass through (user may have specified a Codex-native model)
	}
}

// --- Cursor provider ---

// CursorProvider implements AIProvider for the Cursor CLI.
type CursorProvider struct{}

func (c *CursorProvider) Name() string   { return "cursor" }
func (c *CursorProvider) Binary() string { return "cursor" }

func (c *CursorProvider) BuildArgs(opts InvokeOpts) []string {
	// Cursor CLI agent mode: cursor --agent "prompt"
	var args []string
	args = append(args, "--agent", opts.Prompt)
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	args = append(args, opts.ExtraArgs...)
	return args
}

func (c *CursorProvider) NormalizeResult(raw []byte) (*ProviderResult, error) {
	return &ProviderResult{Text: string(raw)}, nil
}

// --- Registry ---

// providers is the global registry of known AI providers.
var providers = map[string]AIProvider{
	"claude": &ClaudeProvider{},
	"codex":  &CodexProvider{},
	"cursor": &CursorProvider{},
}

// GetProvider returns the AIProvider for the given name.
// Returns the Claude provider if name is empty (default).
// Returns an error if the provider name is unknown.
func GetProvider(name string) (AIProvider, error) {
	if name == "" {
		name = "claude"
	}
	p, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("unknown AI provider: %q (known: claude, codex, cursor)", name)
	}
	return p, nil
}

// ProviderAvailable checks whether the provider's CLI binary is on PATH.
// Returns nil if available, or an error describing what's missing.
func ProviderAvailable(name string) error {
	p, err := GetProvider(name)
	if err != nil {
		return err
	}
	_, err = exec.LookPath(p.Binary())
	if err != nil {
		return fmt.Errorf("provider %q requires %q on PATH: %w", name, p.Binary(), err)
	}
	return nil
}

// splitJSONLines splits raw bytes into individual JSON lines.
func splitJSONLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			line := raw[start:i]
			if len(line) > 0 {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}
