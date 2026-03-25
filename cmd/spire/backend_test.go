package main

import (
	"errors"
	"io"
	"testing"
	"time"
)

// TestBackendProcessShimSatisfiesInterface verifies processBackendShim
// satisfies AgentBackend at runtime via type assertion.
func TestBackendProcessShimSatisfiesInterface(t *testing.T) {
	var b interface{} = &processBackendShim{spawner: &processSpawner{}}
	if _, ok := b.(AgentBackend); !ok {
		t.Fatal("processBackendShim does not satisfy AgentBackend")
	}
	// Also verify it satisfies AgentSpawner (superset guarantee).
	if _, ok := b.(AgentSpawner); !ok {
		t.Fatal("processBackendShim does not satisfy AgentSpawner")
	}
}

// TestBackendDockerShimSatisfiesInterface verifies dockerBackendShim
// satisfies AgentBackend at runtime via type assertion.
func TestBackendDockerShimSatisfiesInterface(t *testing.T) {
	var b interface{} = &dockerBackendShim{spawner: &dockerSpawner{}}
	if _, ok := b.(AgentBackend); !ok {
		t.Fatal("dockerBackendShim does not satisfy AgentBackend")
	}
	if _, ok := b.(AgentSpawner); !ok {
		t.Fatal("dockerBackendShim does not satisfy AgentSpawner")
	}
}

// TestBackendResolveProcess verifies ResolveBackend("process") returns a
// processBackendShim.
func TestBackendResolveProcess(t *testing.T) {
	b := ResolveBackend("process")
	if _, ok := b.(*processBackendShim); !ok {
		t.Fatalf("ResolveBackend(\"process\") returned %T, want *processBackendShim", b)
	}
}

// TestBackendResolveEmpty verifies ResolveBackend("") defaults to process.
func TestBackendResolveEmpty(t *testing.T) {
	b := ResolveBackend("")
	if _, ok := b.(*processBackendShim); !ok {
		t.Fatalf("ResolveBackend(\"\") returned %T, want *processBackendShim", b)
	}
}

// TestBackendResolveDocker verifies ResolveBackend("docker") returns a
// dockerBackendShim.
func TestBackendResolveDocker(t *testing.T) {
	b := ResolveBackend("docker")
	if _, ok := b.(*dockerBackendShim); !ok {
		t.Fatalf("ResolveBackend(\"docker\") returned %T, want *dockerBackendShim", b)
	}
}

// TestBackendResolveUnknown verifies that an unknown backend name falls back
// to processBackendShim with a warning (no panic, no error return).
func TestBackendResolveUnknown(t *testing.T) {
	b := ResolveBackend("unknown")
	if _, ok := b.(*processBackendShim); !ok {
		t.Fatalf("ResolveBackend(\"unknown\") returned %T, want *processBackendShim (fallback)", b)
	}
}

// TestBackendShimListNotImplemented verifies the shims return errNotImplemented
// for List.
func TestBackendShimListNotImplemented(t *testing.T) {
	for _, name := range []string{"process", "docker"} {
		b := ResolveBackend(name)
		_, err := b.List()
		if err == nil {
			t.Fatalf("ResolveBackend(%q).List() returned nil error, want errNotImplemented", name)
		}
		if !errors.Is(err, errNotImplemented) {
			t.Fatalf("ResolveBackend(%q).List() error = %v, want errNotImplemented", name, err)
		}
	}
}

// TestBackendShimLogsNotImplemented verifies the shims return errNotImplemented
// for Logs.
func TestBackendShimLogsNotImplemented(t *testing.T) {
	for _, name := range []string{"process", "docker"} {
		b := ResolveBackend(name)
		_, err := b.Logs("wizard-test")
		if err == nil {
			t.Fatalf("ResolveBackend(%q).Logs() returned nil error, want errNotImplemented", name)
		}
		if !errors.Is(err, errNotImplemented) {
			t.Fatalf("ResolveBackend(%q).Logs() error = %v, want errNotImplemented", name, err)
		}
	}
}

// TestBackendShimKillNotImplemented verifies the shims return errNotImplemented
// for Kill.
func TestBackendShimKillNotImplemented(t *testing.T) {
	for _, name := range []string{"process", "docker"} {
		b := ResolveBackend(name)
		err := b.Kill("wizard-test")
		if err == nil {
			t.Fatalf("ResolveBackend(%q).Kill() returned nil error, want errNotImplemented", name)
		}
		if !errors.Is(err, errNotImplemented) {
			t.Fatalf("ResolveBackend(%q).Kill() error = %v, want errNotImplemented", name, err)
		}
	}
}

// TestBackendAgentInfoConstruction verifies that AgentInfo can be constructed
// and its fields accessed correctly.
func TestBackendAgentInfoConstruction(t *testing.T) {
	now := time.Now()
	info := AgentInfo{
		Name:       "wizard-spi-abc",
		BeadID:     "spi-abc",
		Phase:      "implement",
		Alive:      true,
		Identifier: "12345",
		StartedAt:  now,
	}

	if info.Name != "wizard-spi-abc" {
		t.Errorf("Name = %q, want %q", info.Name, "wizard-spi-abc")
	}
	if info.BeadID != "spi-abc" {
		t.Errorf("BeadID = %q, want %q", info.BeadID, "spi-abc")
	}
	if info.Phase != "implement" {
		t.Errorf("Phase = %q, want %q", info.Phase, "implement")
	}
	if !info.Alive {
		t.Error("Alive = false, want true")
	}
	if info.Identifier != "12345" {
		t.Errorf("Identifier = %q, want %q", info.Identifier, "12345")
	}
	if !info.StartedAt.Equal(now) {
		t.Errorf("StartedAt = %v, want %v", info.StartedAt, now)
	}
}

// TestBackendNewSpawnerDelegates verifies that the deprecated NewSpawner
// returns an AgentBackend (since ResolveBackend returns AgentBackend which
// embeds AgentSpawner).
func TestBackendNewSpawnerDelegates(t *testing.T) {
	s := NewSpawner("process")
	if s == nil {
		t.Fatal("NewSpawner(\"process\") returned nil")
	}
	// The returned value should also satisfy AgentBackend.
	if _, ok := s.(AgentBackend); !ok {
		t.Fatalf("NewSpawner(\"process\") returned %T, which does not satisfy AgentBackend", s)
	}
}

// Ensure io import is used (Logs returns io.ReadCloser).
var _ io.ReadCloser
