package store

import (
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
