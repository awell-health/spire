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
	"encoding/json"
	"fmt"
	"os/exec"
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
	// Codex CLI uses: codex --approval-mode full-auto -q "prompt"
	// --approval-mode full-auto is the equivalent of --dangerously-skip-permissions
	var args []string
	if opts.SkipPerms {
		args = append(args, "--approval-mode", "full-auto")
	}
	args = append(args, "-q", opts.Prompt)
	if opts.Model != "" {
		args = append(args, "--model", mapCodexModel(opts.Model))
	}
	args = append(args, opts.ExtraArgs...)
	return args
}

func (c *CodexProvider) NormalizeResult(raw []byte) (*ProviderResult, error) {
	// Codex output is plain text by default.
	return &ProviderResult{Text: string(raw)}, nil
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
