package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Compile-time interface check.
var _ AgentBackend = (*processBackend)(nil)

// TestProcessBackend_SatisfiesInterface verifies processBackend satisfies
// AgentBackend at runtime via type assertion.
func TestProcessBackend_SatisfiesInterface(t *testing.T) {
	var b interface{} = newProcessBackend()
	if _, ok := b.(AgentBackend); !ok {
		t.Fatal("processBackend does not satisfy AgentBackend")
	}
	if _, ok := b.(AgentSpawner); !ok {
		t.Fatal("processBackend does not satisfy AgentSpawner")
	}
}

// TestProcessBackend_List creates a temp wizard registry with entries and
// verifies List returns correct AgentInfo.
func TestProcessBackend_List(t *testing.T) {
	// Create an isolated SPIRE_DOLT_DIR so we don't interfere with real state.
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	// Also redirect the wizard registry (it uses configDir, not SPIRE_DOLT_DIR).
	// We need to create the registry file directly at the path wizardRegistryPath() returns.
	// Override XDG_CONFIG_HOME so configDir() returns our temp dir.
	configHome := filepath.Join(tmpDir, "config")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	reg := wizardRegistry{
		Wizards: []localWizard{
			{
				Name:      "wizard-spi-abc",
				PID:       99999999, // not a real process — should show as not alive
				BeadID:    "spi-abc",
				Phase:     "implement",
				StartedAt: "2026-03-24T10:00:00Z",
			},
			{
				Name:      "wizard-spi-def",
				PID:       0, // no PID
				BeadID:    "spi-def",
				Phase:     "review",
				StartedAt: "2026-03-24T11:00:00Z",
			},
		},
	}
	regPath := wizardRegistryPath()
	os.MkdirAll(filepath.Dir(regPath), 0755)
	data, _ := json.MarshalIndent(reg, "", "  ")
	if err := os.WriteFile(regPath, data, 0644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	b := newProcessBackend()
	infos, err := b.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	if len(infos) != 2 {
		t.Fatalf("List() returned %d entries, want 2", len(infos))
	}

	// First entry.
	if infos[0].Name != "wizard-spi-abc" {
		t.Errorf("infos[0].Name = %q, want %q", infos[0].Name, "wizard-spi-abc")
	}
	if infos[0].BeadID != "spi-abc" {
		t.Errorf("infos[0].BeadID = %q, want %q", infos[0].BeadID, "spi-abc")
	}
	if infos[0].Phase != "implement" {
		t.Errorf("infos[0].Phase = %q, want %q", infos[0].Phase, "implement")
	}
	if infos[0].Alive {
		t.Error("infos[0].Alive = true, want false (PID 99999999 should not be alive)")
	}
	if infos[0].Identifier != "99999999" {
		t.Errorf("infos[0].Identifier = %q, want %q", infos[0].Identifier, "99999999")
	}
	if infos[0].StartedAt.IsZero() {
		t.Error("infos[0].StartedAt is zero, want parsed time")
	}

	// Second entry.
	if infos[1].Name != "wizard-spi-def" {
		t.Errorf("infos[1].Name = %q, want %q", infos[1].Name, "wizard-spi-def")
	}
	if infos[1].Alive {
		t.Error("infos[1].Alive = true, want false (PID 0)")
	}
}

// TestProcessBackend_Logs creates a temp log file and verifies Logs returns
// a reader with correct content.
func TestProcessBackend_Logs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	logDir := filepath.Join(tmpDir, "wizards")
	os.MkdirAll(logDir, 0755)

	content := "line 1\nline 2\nline 3\n"
	if err := os.WriteFile(filepath.Join(logDir, "wizard-spi-abc.log"), []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	b := newProcessBackend()
	rc, err := b.Logs("wizard-spi-abc")
	if err != nil {
		t.Fatalf("Logs() error: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != content {
		t.Errorf("Logs content = %q, want %q", string(got), content)
	}
}

// TestProcessBackend_Logs_Fallback verifies that Logs tries the fallback name
// wizard-<name>.log when <name>.log doesn't exist.
func TestProcessBackend_Logs_Fallback(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	logDir := filepath.Join(tmpDir, "wizards")
	os.MkdirAll(logDir, 0755)

	content := "fallback log\n"
	// Write the fallback name pattern: wizard-<name>.log
	if err := os.WriteFile(filepath.Join(logDir, "wizard-my-agent.log"), []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	b := newProcessBackend()
	rc, err := b.Logs("my-agent")
	if err != nil {
		t.Fatalf("Logs() error: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != content {
		t.Errorf("Logs content = %q, want %q", string(got), content)
	}
}

// TestProcessBackend_Logs_NotFound verifies Logs returns os.ErrNotExist for
// a missing agent.
func TestProcessBackend_Logs_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	b := newProcessBackend()
	_, err := b.Logs("nonexistent-agent")
	if err == nil {
		t.Fatal("Logs() returned nil error, want os.ErrNotExist")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Logs() error = %v, want os.ErrNotExist", err)
	}
}

// TestProcessBackend_Kill_NoPID verifies Kill handles a missing agent
// gracefully (returns an error, does not panic).
func TestProcessBackend_Kill_NoPID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	// Override config dir so we get an empty registry.
	configHome := filepath.Join(tmpDir, "config")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	b := newProcessBackend()
	err := b.Kill("nonexistent-wizard")
	if err == nil {
		t.Fatal("Kill() returned nil error, want error for missing agent")
	}
}
