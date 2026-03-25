package main

import (
	"os"
	"strings"
	"testing"
)

// --- dockerSpawner unit tests ---
// These test configuration, role mapping, and argument building
// without requiring a running Docker daemon.

func TestDockerSpawner_InvalidRole(t *testing.T) {
	s := &dockerSpawner{}
	_, err := s.Spawn(SpawnConfig{
		Name:   "test",
		BeadID: "test-1",
		Role:   SpawnRole("invalid"),
	})
	if err == nil {
		t.Error("Spawn with invalid role should return error")
	}
	if !strings.Contains(err.Error(), "unknown spawn role") {
		t.Errorf("error should mention unknown role, got: %s", err)
	}
}

func TestDockerSpawner_ResolvedImage_Default(t *testing.T) {
	s := &dockerSpawner{}
	if got := s.resolvedImage(); got != defaultDockerImage {
		t.Errorf("resolvedImage() = %q, want %q", got, defaultDockerImage)
	}
}

func TestDockerSpawner_ResolvedImage_Override(t *testing.T) {
	s := &dockerSpawner{Image: "my-custom-image:v1"}
	if got := s.resolvedImage(); got != "my-custom-image:v1" {
		t.Errorf("resolvedImage() = %q, want %q", got, "my-custom-image:v1")
	}
}

func TestDockerSpawner_ResolvedNetwork_Default(t *testing.T) {
	s := &dockerSpawner{}
	if got := s.resolvedNetwork(); got != "host" {
		t.Errorf("resolvedNetwork() = %q, want %q", got, "host")
	}
}

func TestDockerSpawner_ResolvedNetwork_Override(t *testing.T) {
	s := &dockerSpawner{Network: "bridge"}
	if got := s.resolvedNetwork(); got != "bridge" {
		t.Errorf("resolvedNetwork() = %q, want %q", got, "bridge")
	}
}

func TestSanitizeContainerName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"spi-1dl.2", "spi-1dl.2"},
		{"spi-abc", "spi-abc"},
		{"feat/branch", "feat-branch"},
		{"hello world", "hello-world"},
		{"abc_123.xyz-456", "abc_123.xyz-456"},
		{"special@chars#here", "special-chars-here"},
	}
	for _, tt := range tests {
		got := sanitizeContainerName(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeContainerName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNewSpawner_Docker(t *testing.T) {
	s := NewSpawner("docker")
	if s == nil {
		t.Fatal("NewSpawner(\"docker\") returned nil")
	}
}

func TestDockerHandle_Name(t *testing.T) {
	h := &dockerHandle{name: "test-agent", containerID: "abc123"}
	if h.Name() != "test-agent" {
		t.Errorf("Name() = %q, want %q", h.Name(), "test-agent")
	}
}

func TestDockerHandle_Identifier(t *testing.T) {
	h := &dockerHandle{name: "test-agent", containerID: "abc123def456"}
	if h.Identifier() != "abc123def456" {
		t.Errorf("Identifier() = %q, want %q", h.Identifier(), "abc123def456")
	}
}

func TestDockerHandle_Alive_AfterExited(t *testing.T) {
	h := &dockerHandle{name: "test-agent", containerID: "abc123"}
	h.exited.Store(true)
	if h.Alive() {
		t.Error("Alive() = true after exited set, want false")
	}
}

func TestDockerHandle_Signal_AfterExited(t *testing.T) {
	h := &dockerHandle{name: "test-agent", containerID: "abc123"}
	h.exited.Store(true)
	err := h.Signal(os.Interrupt)
	if err == nil {
		t.Error("Signal after exit should return error")
	}
	if !strings.Contains(err.Error(), "already exited") {
		t.Errorf("error should mention already exited, got: %s", err)
	}
}
