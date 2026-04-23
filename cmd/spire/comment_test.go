package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCommentText_Positional(t *testing.T) {
	got, err := resolveCommentText("hello world", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestResolveCommentText_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "body.txt")
	if err := os.WriteFile(path, []byte("from file body"), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	got, err := resolveCommentText("", path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "from file body" {
		t.Errorf("got %q, want %q", got, "from file body")
	}
}

func TestResolveCommentText_Stdin(t *testing.T) {
	orig := commentStdinReader
	defer func() { commentStdinReader = orig }()
	commentStdinReader = strings.NewReader("stdin body")

	got, err := resolveCommentText("", "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "stdin body" {
		t.Errorf("got %q, want %q", got, "stdin body")
	}
}

func TestResolveCommentText_NoSource(t *testing.T) {
	_, err := resolveCommentText("", "", false)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("expected error to mention body is required, got: %v", err)
	}
}

func TestResolveCommentText_MultipleSources(t *testing.T) {
	cases := []struct {
		name       string
		positional string
		file       string
		stdin      bool
	}{
		{name: "positional+file", positional: "hi", file: "x"},
		{name: "positional+stdin", positional: "hi", stdin: true},
		{name: "file+stdin", file: "x", stdin: true},
		{name: "all three", positional: "hi", file: "x", stdin: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveCommentText(tc.positional, tc.file, tc.stdin)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "exactly one") {
				t.Errorf("expected 'exactly one' in error, got: %v", err)
			}
		})
	}
}

func TestResolveCommentText_WhitespaceOnlyPositionalIsMissing(t *testing.T) {
	// Whitespace-only positional shouldn't count as a source.
	_, err := resolveCommentText("   ", "", false)
	if err == nil {
		t.Fatalf("expected error for whitespace-only positional, got nil")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("expected 'required' error, got: %v", err)
	}
}
