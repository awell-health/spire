package agent

import (
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
			opts: InvokeOpts{Prompt: "hello"},
			want: []string{"-q", "hello"},
		},
		{
			name: "skip perms (full-auto)",
			opts: InvokeOpts{Prompt: "plan", SkipPerms: true},
			want: []string{"--approval-mode", "full-auto", "-q", "plan"},
		},
		{
			name: "model mapping opus",
			opts: InvokeOpts{Prompt: "work", Model: "opus"},
			want: []string{"-q", "work", "--model", "o3"},
		},
		{
			name: "model mapping claude-opus-4-6",
			opts: InvokeOpts{Prompt: "work", Model: "claude-opus-4-6"},
			want: []string{"-q", "work", "--model", "o3"},
		},
		{
			name: "model mapping sonnet",
			opts: InvokeOpts{Prompt: "work", Model: "sonnet"},
			want: []string{"-q", "work", "--model", "o4-mini"},
		},
		{
			name: "model mapping claude-sonnet-4-6",
			opts: InvokeOpts{Prompt: "work", Model: "claude-sonnet-4-6"},
			want: []string{"-q", "work", "--model", "o4-mini"},
		},
		{
			name: "native codex model passthrough",
			opts: InvokeOpts{Prompt: "work", Model: "o3"},
			want: []string{"-q", "work", "--model", "o3"},
		},
		{
			name: "extra args",
			opts: InvokeOpts{Prompt: "work", ExtraArgs: []string{"--verbose"}},
			want: []string{"-q", "work", "--verbose"},
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

func TestCodexProvider_NormalizeResult(t *testing.T) {
	raw := []byte("some codex output")
	p := &CodexProvider{}
	r, err := p.NormalizeResult(raw)
	if err != nil {
		t.Fatalf("NormalizeResult error: %v", err)
	}
	if r.Text != "some codex output" {
		t.Errorf("Text = %q, want %q", r.Text, "some codex output")
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
