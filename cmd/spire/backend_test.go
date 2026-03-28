package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
)

// TestBackendProcessSatisfiesInterface verifies processBackend
// satisfies AgentBackend at runtime via type assertion.
func TestBackendProcessSatisfiesInterface(t *testing.T) {
	var b interface{} = newProcessBackend()
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
	var b interface{} = agent.NewDockerBackend()
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

// TestResolveBackend_FromConfig verifies that ResolveBackend("") reads
// the backend from spire.yaml when agent.backend is set.
func TestResolveBackend_FromConfig(t *testing.T) {
	dir := t.TempDir()

	// Write spire.yaml with agent.backend: docker
	yaml := `agent:
  backend: docker
`
	if err := os.WriteFile(filepath.Join(dir, "spire.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	// Change to the temp dir so repoconfig.Load finds spire.yaml
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	b := ResolveBackend("")
	if _, ok := b.(*dockerBackend); !ok {
		t.Fatalf("ResolveBackend(\"\") with agent.backend=docker returned %T, want *dockerBackend", b)
	}
}

// TestResolveBackend_ExplicitOverridesConfig verifies that an explicit
// backend name overrides the spire.yaml config.
func TestResolveBackend_ExplicitOverridesConfig(t *testing.T) {
	dir := t.TempDir()

	// Write spire.yaml with agent.backend: docker
	yaml := `agent:
  backend: docker
`
	if err := os.WriteFile(filepath.Join(dir, "spire.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	// Change to the temp dir so repoconfig.Load finds spire.yaml
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// Explicit "process" should override the config's "docker"
	b := ResolveBackend("process")
	if _, ok := b.(*processBackend); !ok {
		t.Fatalf("ResolveBackend(\"process\") with agent.backend=docker returned %T, want *processBackend", b)
	}
}

// TestResolveBackend_NoConfigFallsToProcess verifies that ResolveBackend("")
// returns processBackend when there is no spire.yaml.
func TestResolveBackend_NoConfigFallsToProcess(t *testing.T) {
	dir := t.TempDir()

	// Change to empty temp dir (no spire.yaml)
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	b := ResolveBackend("")
	if _, ok := b.(*processBackend); !ok {
		t.Fatalf("ResolveBackend(\"\") with no config returned %T, want *processBackend", b)
	}
}

// Ensure io import is used (Logs returns io.ReadCloser).
var _ io.ReadCloser
