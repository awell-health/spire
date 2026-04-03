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
