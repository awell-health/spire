package workshop

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestPromptString(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		defVal   string
		expected string
	}{
		{"with input", "hello\n", "default", "hello"},
		{"empty input uses default", "\n", "default", "default"},
		{"no default empty input", "\n", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tt.input))
			var out bytes.Buffer
			got := promptString(r, &out, "Prompt", tt.defVal)
			if got != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestPromptChoice(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		defIdx   int
		expected int
	}{
		{"select second", "2\n", 0, 1},
		{"default on empty", "\n", 0, 0},
		{"out of range uses default", "99\n", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tt.input))
			var out bytes.Buffer
			got, _ := promptChoice(r, &out, "Choose", []string{"a", "b", "c"}, tt.defIdx)
			if got != tt.expected {
				t.Fatalf("expected %d, got %d", tt.expected, got)
			}
		})
	}
}

func TestPromptBool(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		defVal   bool
		expected bool
	}{
		{"yes", "y\n", false, true},
		{"no", "n\n", true, false},
		{"default true", "\n", true, true},
		{"default false", "\n", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tt.input))
			var out bytes.Buffer
			got := promptBool(r, &out, "Confirm", tt.defVal)
			if got != tt.expected {
				t.Fatalf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestPromptMultiSelect(t *testing.T) {
	// Toggle item 2 (deselect), then confirm
	input := "2\n\n"
	r := bufio.NewReader(strings.NewReader(input))
	var out bytes.Buffer
	selected := []bool{true, true, false}
	result := promptMultiSelect(r, &out, "Select", []string{"a", "b", "c"}, selected)
	if result[0] != true || result[1] != false || result[2] != false {
		t.Fatalf("expected [true false false], got %v", result)
	}
}

func TestComposeInteractive_BasicFlow(t *testing.T) {
	// Simulate: version (v2=1), description, bead type (task=1), phase selection (confirm defaults),
	// per-phase config (accept all defaults), no vars, quit
	lines := []string{
		"1",                // formula version: v2
		"My test formula",  // description
		"1",                // bead type: task
		"",                 // confirm default phase selection
		"",                 // plan: role (default)
		"",                 // plan: model (default)
		"",                 // plan: timeout (default)
		"",                 // implement: role (default)
		"",                 // implement: model (default)
		"",                 // implement: timeout (default)
		"",                 // implement: dispatch (default)
		"",                 // implement: worktree (default)
		"",                 // implement: apprentice (default)
		"",                 // implement: context (default)
		"",                 // review: role (default)
		"",                 // review: model (default)
		"",                 // review: timeout (default)
		"",                 // review: verdict_only (default)
		"",                 // review: judgment (default)
		"",                 // review: max rounds (default)
		"",                 // review: arbiter model (default)
		"",                 // merge: role (default)
		"",                 // merge: strategy (default)
		"",                 // merge: auto (default)
		"",                 // vars: empty name to finish
		"3",                // action: quit (skip save to avoid filesystem)
	}
	input := strings.Join(lines, "\n") + "\n"

	var out bytes.Buffer
	f, tomlBytes, err := ComposeInteractive("test-compose", strings.NewReader(input), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out.String())
	}
	if f == nil {
		t.Fatal("expected non-nil formula")
	}
	if f.Name != "test-compose" {
		t.Fatalf("expected name test-compose, got %s", f.Name)
	}
	if len(f.Phases) != 4 {
		t.Fatalf("expected 4 phases, got %d", len(f.Phases))
	}
	if len(tomlBytes) == 0 {
		t.Fatal("expected non-empty TOML bytes")
	}

	// Verify the TOML can be parsed back
	s := string(tomlBytes)
	if !strings.Contains(s, `name = "test-compose"`) {
		t.Fatalf("TOML missing name:\n%s", s)
	}
	if !strings.Contains(s, "[phases.plan]") {
		t.Fatalf("TOML missing plan phase:\n%s", s)
	}
}

func TestComposeInteractive_EpicType(t *testing.T) {
	// Select epic type — should include all 5 phases by default
	lines := []string{
		"1",                // formula version: v2
		"Epic formula",     // description
		"4",                // bead type: epic
		"",                 // confirm default phase selection (all 5)
		"",                 // design: role
		"",                 // design: model
		"",                 // design: timeout
		"",                 // plan: role
		"",                 // plan: model
		"",                 // plan: timeout
		"",                 // implement: role
		"",                 // implement: model
		"",                 // implement: timeout
		"",                 // implement: dispatch
		"",                 // implement: worktree
		"",                 // implement: apprentice
		"",                 // implement: staging branch
		"",                 // implement: context
		"",                 // review: role
		"",                 // review: model
		"",                 // review: timeout
		"",                 // review: verdict_only
		"",                 // review: judgment
		"",                 // review: max rounds
		"",                 // review: arbiter model
		"",                 // merge: role
		"",                 // merge: strategy
		"",                 // merge: auto
		"",                 // merge: staging branch
		"",                 // vars: empty name
		"3",                // quit
	}
	input := strings.Join(lines, "\n") + "\n"

	var out bytes.Buffer
	f, _, err := ComposeInteractive("epic-test", strings.NewReader(input), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out.String())
	}
	if len(f.Phases) != 5 {
		t.Fatalf("expected 5 phases for epic, got %d", len(f.Phases))
	}
	if _, ok := f.Phases["design"]; !ok {
		t.Fatal("expected design phase for epic")
	}
}
