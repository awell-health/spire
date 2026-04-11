package store

import (
	"strings"
	"testing"
)

func TestOpen_EmptyPath(t *testing.T) {
	s, err := Open("")
	if err == nil {
		t.Fatal("expected error for empty beadsDir, got nil")
	}
	if s != nil {
		t.Fatal("expected nil storage for empty beadsDir")
	}
	if !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestOpen_InvalidPath(t *testing.T) {
	s, err := Open("/nonexistent/path/that/does/not/exist")
	if err == nil {
		if s != nil {
			s.Close()
		}
		t.Fatal("expected error for invalid beadsDir path, got nil")
	}
}

func TestOpen_DoesNotTouchSingleton(t *testing.T) {
	prev := activeStore
	defer func() { activeStore = prev }()

	activeStore = nil
	_, _ = Open("/nonexistent/path") // will error, that's fine
	if activeStore != nil {
		t.Fatal("Open() must not modify the package-level singleton")
	}
}
