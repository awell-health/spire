package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- agentNames tests (replaces loadRoster tests) ---

func TestAgentNames_Override(t *testing.T) {
	agents := []AgentInfo{
		{Name: "wizard-1"},
		{Name: "wizard-2"},
	}
	override := []string{"explicit-a", "explicit-b"}
	got := agentNames(agents, override)
	if len(got) != 2 || got[0] != "explicit-a" || got[1] != "explicit-b" {
		t.Errorf("agentNames with override = %v, want [explicit-a explicit-b]", got)
	}
}

func TestAgentNames_FromAgentInfo(t *testing.T) {
	agents := []AgentInfo{
		{Name: "wizard-1"},
		{Name: "wizard-2"},
		{Name: "wizard-1"}, // duplicate
	}
	got := agentNames(agents, nil)
	if len(got) != 2 || got[0] != "wizard-1" || got[1] != "wizard-2" {
		t.Errorf("agentNames = %v, want [wizard-1 wizard-2]", got)
	}
}

func TestAgentNames_Empty(t *testing.T) {
	got := agentNames(nil, nil)
	if len(got) != 0 {
		t.Errorf("agentNames(nil, nil) = %v, want []", got)
	}
}

// --- busySet tests (replaces findBusyAgents/localBusyAgents tests) ---

func TestBusySet_AliveOnly(t *testing.T) {
	agents := []AgentInfo{
		{Name: "wizard-1", Alive: true},
		{Name: "wizard-2", Alive: false},
		{Name: "wizard-3", Alive: true},
	}
	busy := busySet(agents)
	if !busy["wizard-1"] {
		t.Error("expected wizard-1 to be busy (alive)")
	}
	if busy["wizard-2"] {
		t.Error("expected wizard-2 to NOT be busy (dead)")
	}
	if !busy["wizard-3"] {
		t.Error("expected wizard-3 to be busy (alive)")
	}
}

func TestBusySet_Empty(t *testing.T) {
	busy := busySet(nil)
	if len(busy) != 0 {
		t.Errorf("busySet(nil) = %v, want empty", busy)
	}
}

// --- loadLocalStewardConfig tests ---

// chdirTemp changes the working directory to a new temp dir for the duration
// of the test and restores it on cleanup.
func chdirTemp(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	return tmpDir
}

func TestLoadLocalStewardConfig_Defaults(t *testing.T) {
	chdirTemp(t) // no spire.yaml in the temp dir

	cfg := loadLocalStewardConfig()

	if cfg.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-sonnet-4-6")
	}
	if cfg.MaxTurns != 30 {
		t.Errorf("MaxTurns = %d, want 30", cfg.MaxTurns)
	}
	if cfg.Timeout != 15*time.Minute {
		t.Errorf("Timeout = %s, want 15m", cfg.Timeout)
	}
	if cfg.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q", cfg.BaseBranch, "main")
	}
	if cfg.BranchPattern != "feat/{bead-id}" {
		t.Errorf("BranchPattern = %q, want %q", cfg.BranchPattern, "feat/{bead-id}")
	}
}

func TestLoadLocalStewardConfig_Overrides(t *testing.T) {
	dir := chdirTemp(t)

	yaml := `agent:
  model: claude-opus-4-6
  max-turns: 50
  timeout: 30m
branch:
  base: develop
  pattern: "work/{bead-id}"
`
	if err := os.WriteFile(filepath.Join(dir, "spire.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadLocalStewardConfig()

	if cfg.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-opus-4-6")
	}
	if cfg.MaxTurns != 50 {
		t.Errorf("MaxTurns = %d, want 50", cfg.MaxTurns)
	}
	if cfg.Timeout != 30*time.Minute {
		t.Errorf("Timeout = %s, want 30m", cfg.Timeout)
	}
	if cfg.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want %q", cfg.BaseBranch, "develop")
	}
	if cfg.BranchPattern != "work/{bead-id}" {
		t.Errorf("BranchPattern = %q, want %q", cfg.BranchPattern, "work/{bead-id}")
	}
}

func TestLoadLocalStewardConfig_PartialOverride(t *testing.T) {
	dir := chdirTemp(t)

	// Only override model; everything else should stay at defaults.
	yaml := `agent:
  model: claude-haiku-4-5-20251001
`
	if err := os.WriteFile(filepath.Join(dir, "spire.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadLocalStewardConfig()

	if cfg.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-haiku-4-5-20251001")
	}
	// Remaining fields come from repoconfig defaults (same as loadLocalStewardConfig defaults).
	if cfg.MaxTurns != 30 {
		t.Errorf("MaxTurns = %d, want 30 (default)", cfg.MaxTurns)
	}
	if cfg.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q (default)", cfg.BaseBranch, "main")
	}
}

// --- isWizardRunning tests ---

func TestIsWizardRunning_NoPIDFile(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())
	if isWizardRunning("nonexistent-wizard") {
		t.Error("expected false for wizard with no PID file")
	}
}

func TestIsWizardRunning_SelfPID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	name := "test-wizard"
	if err := writePID(wizardPIDPath(name), os.Getpid()); err != nil {
		t.Fatal(err)
	}

	if !isWizardRunning(name) {
		t.Error("expected true for wizard with current process PID")
	}
}

func TestIsWizardRunning_DeadPID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	name := "dead-wizard"
	// PID 0 is never a valid process; processAlive returns false for it.
	if err := writePID(wizardPIDPath(name), 0); err != nil {
		t.Fatal(err)
	}

	if isWizardRunning(name) {
		t.Error("expected false for wizard with PID 0")
	}
}
