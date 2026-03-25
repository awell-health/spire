package main

import (
	"io"
	"testing"
	"time"
)

// TestBackendProcessSatisfiesInterface verifies processBackend
// satisfies AgentBackend at runtime via type assertion.
func TestBackendProcessSatisfiesInterface(t *testing.T) {
	var b interface{} = &processBackend{spawner: &processSpawner{}}
	if _, ok := b.(AgentBackend); !ok {
		t.Fatal("processBackend does not satisfy AgentBackend")
	}
	if _, ok := b.(AgentSpawner); !ok {
		t.Fatal("processBackend does not satisfy AgentSpawner")
	}
}

// TestBackendDockerSatisfiesInterface verifies dockerBackend
// satisfies AgentBackend at runtime via type assertion.
func TestBackendDockerSatisfiesInterface(t *testing.T) {
	var b interface{} = &dockerBackend{spawner: &dockerSpawner{}}
	if _, ok := b.(AgentBackend); !ok {
		t.Fatal("dockerBackend does not satisfy AgentBackend")
	}
	if _, ok := b.(AgentSpawner); !ok {
		t.Fatal("dockerBackend does not satisfy AgentSpawner")
	}
}

// TestBackendResolveProcess verifies ResolveBackend("process") returns a processBackend.
func TestBackendResolveProcess(t *testing.T) {
	b := ResolveBackend("process")
	if _, ok := b.(*processBackend); !ok {
		t.Fatalf("ResolveBackend(\"process\") returned %T, want *processBackend", b)
	}
}

// TestBackendResolveEmpty verifies ResolveBackend("") defaults to process.
func TestBackendResolveEmpty(t *testing.T) {
	b := ResolveBackend("")
	if _, ok := b.(*processBackend); !ok {
		t.Fatalf("ResolveBackend(\"\") returned %T, want *processBackend", b)
	}
}

// TestBackendResolveDocker verifies ResolveBackend("docker") returns a dockerBackend.
func TestBackendResolveDocker(t *testing.T) {
	b := ResolveBackend("docker")
	if _, ok := b.(*dockerBackend); !ok {
		t.Fatalf("ResolveBackend(\"docker\") returned %T, want *dockerBackend", b)
	}
}

// TestBackendResolveUnknown verifies that an unknown backend name falls back
// to processBackend with a warning.
func TestBackendResolveUnknown(t *testing.T) {
	b := ResolveBackend("unknown")
	if _, ok := b.(*processBackend); !ok {
		t.Fatalf("ResolveBackend(\"unknown\") returned %T, want *processBackend (fallback)", b)
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
	if _, ok := s.(AgentBackend); !ok {
		t.Fatalf("NewSpawner(\"process\") returned %T, which does not satisfy AgentBackend", s)
	}
}

// Ensure io import is used (Logs returns io.ReadCloser).
var _ io.ReadCloser
