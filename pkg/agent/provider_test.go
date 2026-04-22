package agent

import (
	"strings"
	"testing"
)

func TestGetProvider_Default(t *testing.T) {
	p, err := GetProvider("")
	if err != nil {
		t.Fatalf("GetProvider(\"\") error: %v", err)
	}
	if p.Name() != "claude" {
		t.Errorf("GetProvider(\"\").Name() = %q, want %q", p.Name(), "claude")
	}
}

func TestGetProvider_Known(t *testing.T) {
	for _, name := range []string{"claude", "codex", "cursor"} {
		t.Run(name, func(t *testing.T) {
			p, err := GetProvider(name)
			if err != nil {
				t.Fatalf("GetProvider(%q) error: %v", name, err)
			}
			if p.Name() != name {
				t.Errorf("Name() = %q, want %q", p.Name(), name)
			}
		})
	}
}

func TestGetProvider_Unknown(t *testing.T) {
	_, err := GetProvider("gemini")
	if err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
}

// --- Claude provider tests ---

func TestClaudeProvider_Binary(t *testing.T) {
	p := &ClaudeProvider{}
	if got := p.Binary(); got != "claude" {
		t.Errorf("Binary() = %q, want %q", got, "claude")
	}
}

func TestClaudeProvider_BuildArgs(t *testing.T) {
	tests := []struct {
		name string
		opts InvokeOpts
		want []string
	}{
		{
			name: "minimal prompt",
			opts: InvokeOpts{Prompt: "hello"},
			want: []string{"-p", "hello"},
		},
		{
			name: "skip perms + model",
			opts: InvokeOpts{Prompt: "plan", SkipPerms: true, Model: "opus"},
			want: []string{"--dangerously-skip-permissions", "-p", "plan", "--model", "opus"},
		},
		{
			name: "output format json",
			opts: InvokeOpts{Prompt: "review", OutputFmt: "json"},
			want: []string{"-p", "review", "--output-format", "json"},
		},
		{
			name: "max turns",
			opts: InvokeOpts{Prompt: "work", MaxTurns: 50},
			want: []string{"-p", "work", "--max-turns", "50"},
		},
		{
			name: "all options",
			opts: InvokeOpts{
				Prompt:    "do it",
				SkipPerms: true,
				Model:     "sonnet",
				OutputFmt: "json",
				MaxTurns:  10,
				ExtraArgs: []string{"--verbose"},
			},
			want: []string{
				"--dangerously-skip-permissions",
				"-p", "do it",
				"--model", "sonnet",
				"--output-format", "json",
				"--max-turns", "10",
				"--verbose",
			},
		},
	}
	p := &ClaudeProvider{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.BuildArgs(tt.opts)
			if !slicesEqual(got, tt.want) {
				t.Errorf("BuildArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaudeProvider_NormalizeResult_JSONEvent(t *testing.T) {
	raw := []byte(`{"type":"start","data":{}}
{"type":"result","result":{"text":"plan output here"}}
`)
	p := &ClaudeProvider{}
	r, err := p.NormalizeResult(raw)
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	if r.Text != "plan output here" {
		t.Errorf("Text = %q, want %q", r.Text, "plan output here")
	}
}

func TestClaudeProvider_NormalizeResult_Fallback(t *testing.T) {
	raw := []byte("plain text output with no JSON")
	p := &ClaudeProvider{}
	r, err := p.NormalizeResult(raw)
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	if r.Text != "plain text output with no JSON" {
		t.Errorf("Text = %q, want raw fallback", r.Text)
	}
}

func TestClaudeProvider_NormalizeResult_EmptyInput(t *testing.T) {
	p := &ClaudeProvider{}
	r, err := p.NormalizeResult([]byte{})
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	if r.Text != "" {
		t.Errorf("Text = %q, want empty", r.Text)
	}
}

// --- Codex provider tests ---

func TestCodexProvider_Binary(t *testing.T) {
	p := &CodexProvider{}
	if got := p.Binary(); got != "codex" {
		t.Errorf("Binary() = %q, want %q", got, "codex")
	}
}

func TestCodexProvider_BuildArgs(t *testing.T) {
	tests := []struct {
		name string
		opts InvokeOpts
		want []string
	}{
		{
			name: "minimal prompt",
			opts: InvokeOpts{Prompt: "my prompt"},
			want: []string{"exec", "--json", "my prompt"},
		},
		{
			name: "skip perms (full-auto)",
			opts: InvokeOpts{Prompt: "plan", SkipPerms: true},
			want: []string{"--approval-mode", "full-auto", "exec", "--json", "plan"},
		},
		{
			name: "model mapping opus",
			opts: InvokeOpts{Prompt: "work", Model: "opus"},
			want: []string{"exec", "--json", "work", "--model", "o3"},
		},
		{
			name: "model mapping claude-opus-4-6",
			opts: InvokeOpts{Prompt: "work", Model: "claude-opus-4-6"},
			want: []string{"exec", "--json", "work", "--model", "o3"},
		},
		{
			name: "model mapping sonnet",
			opts: InvokeOpts{Prompt: "work", Model: "sonnet"},
			want: []string{"exec", "--json", "work", "--model", "o4-mini"},
		},
		{
			name: "model mapping claude-sonnet-4-6",
			opts: InvokeOpts{Prompt: "work", Model: "claude-sonnet-4-6"},
			want: []string{"exec", "--json", "work", "--model", "o4-mini"},
		},
		{
			name: "native codex model passthrough",
			opts: InvokeOpts{Prompt: "work", Model: "o3"},
			want: []string{"exec", "--json", "work", "--model", "o3"},
		},
		{
			name: "extra args",
			opts: InvokeOpts{Prompt: "work", ExtraArgs: []string{"--verbose"}},
			want: []string{"exec", "--json", "work", "--verbose"},
		},
	}
	p := &CodexProvider{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.BuildArgs(tt.opts)
			if !slicesEqual(got, tt.want) {
				t.Errorf("BuildArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodexProvider_NormalizeResult_NoToolRun(t *testing.T) {
	// No-tool run transcript from the design doc.
	raw := []byte(`{"type":"thread.started","thread_id":"t_0"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hello"}}
{"type":"turn.completed","usage":{"input_tokens":18368,"cached_input_tokens":0,"output_tokens":29}}
`)
	p := &CodexProvider{}
	r, err := p.NormalizeResult(raw)
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	if r.Text != "hello" {
		t.Errorf("Text = %q, want %q", r.Text, "hello")
	}
}

func TestCodexProvider_NormalizeResult_ToolUsingRun(t *testing.T) {
	// Tool-using run transcript from the design doc — final agent_message is
	// "/private/tmp" and intervening command_execution items should be ignored.
	raw := []byte(`{"type":"thread.started","thread_id":"t_0"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Running pwd..."}}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc pwd","aggregated_output":"","exit_code":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc pwd","aggregated_output":"/private/tmp\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"/private/tmp"}}
{"type":"turn.completed","usage":{"input_tokens":36974,"cached_input_tokens":36736,"output_tokens":186}}
`)
	p := &CodexProvider{}
	r, err := p.NormalizeResult(raw)
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	if r.Text != "/private/tmp" {
		t.Errorf("Text = %q, want %q", r.Text, "/private/tmp")
	}
}

func TestCodexProvider_NormalizeResult_MultipleAgentMessages(t *testing.T) {
	// Two agent_message events in one turn — expect the later one.
	raw := []byte(`{"type":"thread.started","thread_id":"t_0"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"first"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"second"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
`)
	p := &CodexProvider{}
	r, err := p.NormalizeResult(raw)
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	if r.Text != "second" {
		t.Errorf("Text = %q, want %q", r.Text, "second")
	}
}

func TestCodexProvider_NormalizeResult_LeadingNoise(t *testing.T) {
	// Leading non-JSON lines (mimicking MCP/auth diagnostic output that might
	// leak onto stdout) — parser must skip and still return the agent_message.
	raw := []byte(`[debug] auth provider ready
[info] mcp servers initialized
{"type":"thread.started","thread_id":"t_0"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hello"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
`)
	p := &CodexProvider{}
	r, err := p.NormalizeResult(raw)
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	if r.Text != "hello" {
		t.Errorf("Text = %q, want %q", r.Text, "hello")
	}
}

func TestCodexProvider_NormalizeResult_MalformedJSONMidStream(t *testing.T) {
	// One corrupt line between valid events — parser must skip it, not error.
	raw := []byte(`{"type":"thread.started","thread_id":"t_0"}
{"type":"turn.started"}
{ this is not valid json
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hello"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
`)
	p := &CodexProvider{}
	r, err := p.NormalizeResult(raw)
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	if r.Text != "hello" {
		t.Errorf("Text = %q, want %q", r.Text, "hello")
	}
}

func TestCodexProvider_NormalizeResult_EmptyInput(t *testing.T) {
	p := &CodexProvider{}
	r, err := p.NormalizeResult([]byte{})
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	if r.Text != "" {
		t.Errorf("Text = %q, want empty", r.Text)
	}
}

func TestCodexProvider_NormalizeResult_NoAgentMessage(t *testing.T) {
	// No agent_message events — fall back to trimmed raw stdout so behavior
	// isn't worse than the previous plain-text pass-through for degenerate runs.
	raw := []byte(`{"type":"thread.started","thread_id":"t_0"}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":0}}
`)
	p := &CodexProvider{}
	r, err := p.NormalizeResult(raw)
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	want := strings.TrimSpace(string(raw))
	if r.Text != want {
		t.Errorf("Text = %q, want trimmed raw %q", r.Text, want)
	}
}

// --- Cursor provider tests ---

func TestCursorProvider_Binary(t *testing.T) {
	p := &CursorProvider{}
	if got := p.Binary(); got != "cursor" {
		t.Errorf("Binary() = %q, want %q", got, "cursor")
	}
}

func TestCursorProvider_BuildArgs(t *testing.T) {
	p := &CursorProvider{}
	got := p.BuildArgs(InvokeOpts{Prompt: "fix bug", Model: "gpt-4"})
	want := []string{"--agent", "fix bug", "--model", "gpt-4"}
	if !slicesEqual(got, want) {
		t.Errorf("BuildArgs() = %v, want %v", got, want)
	}
}

// --- mapCodexModel tests ---

func TestMapCodexModel(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"opus", "o3"},
		{"claude-opus-4-6", "o3"},
		{"sonnet", "o4-mini"},
		{"claude-sonnet-4-6", "o4-mini"},
		{"o3", "o3"},           // passthrough
		{"gpt-4", "gpt-4"},     // passthrough
		{"custom", "custom"},   // passthrough
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapCodexModel(tt.input)
			if got != tt.want {
				t.Errorf("mapCodexModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- splitJSONLines tests ---

func TestSplitJSONLines(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  int // expected number of lines
	}{
		{"empty", []byte{}, 0},
		{"single line no newline", []byte(`{"a":1}`), 1},
		{"single line with newline", []byte("{\"a\":1}\n"), 1},
		{"two lines", []byte("{\"a\":1}\n{\"b\":2}\n"), 2},
		{"trailing content no newline", []byte("{\"a\":1}\n{\"b\":2}"), 2},
		{"blank lines filtered", []byte("{\"a\":1}\n\n{\"b\":2}\n"), 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitJSONLines(tt.input)
			if len(got) != tt.want {
				t.Errorf("splitJSONLines() returned %d lines, want %d", len(got), tt.want)
			}
		})
	}
}

// slicesEqual compares two string slices for equality.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
