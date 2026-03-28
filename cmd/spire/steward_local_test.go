package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- Wrapper-level smoke tests ---
// Full test coverage lives in pkg/steward/steward_test.go.
// These verify the cmd/spire wrappers compile and delegate correctly.

func TestAgentNames_Wrapper(t *testing.T) {
	agents := []AgentInfo{
		{Name: "wizard-1"},
		{Name: "wizard-2"},
	}
	got := agentNames(agents, nil)
	if len(got) != 2 {
		t.Errorf("agentNames = %v, want 2 items", got)
	}
}

func TestBusySet_Wrapper(t *testing.T) {
	agents := []AgentInfo{
		{Name: "wizard-1", Alive: true},
		{Name: "wizard-2", Alive: false},
	}
	busy := busySet(agents)
	if !busy["wizard-1"] || busy["wizard-2"] {
		t.Errorf("busySet = %v, want wizard-1 busy, wizard-2 not", busy)
	}
}

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

func TestLoadLocalStewardConfig_Wrapper(t *testing.T) {
	chdirTemp(t)
	cfg := loadLocalStewardConfig()
	if cfg.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want default", cfg.Model)
	}
	if cfg.Timeout != 15*time.Minute {
		t.Errorf("Timeout = %s, want 15m", cfg.Timeout)
	}
}

func TestIsWizardRunning_Wrapper(t *testing.T) {
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
}

// --- Fail-closed tests verifying wrapper-level var injection ---

func TestStewardAssignment_FailClosed_ExcludesAndAlerts(t *testing.T) {
	origAttempt := storeGetActiveAttemptFunc
	storeGetActiveAttemptFunc = func(parentID string) (*Bead, error) {
		if parentID == "spi-corrupted" {
			return nil, fmt.Errorf("invariant violation: 2 open attempt beads for spi-corrupted")
		}
		return nil, nil
	}
	defer func() { storeGetActiveAttemptFunc = origAttempt }()

	var alertedBeads []string
	origAlert := storeRaiseCorruptedBeadAlertFunc
	storeRaiseCorruptedBeadAlertFunc = func(beadID string, err error) {
		alertedBeads = append(alertedBeads, beadID)
	}
	defer func() { storeRaiseCorruptedBeadAlertFunc = origAlert }()

	bead := Bead{ID: "spi-corrupted", Title: "corrupted task", Status: "open"}
	attempt, aErr := storeGetActiveAttemptFunc(bead.ID)
	if aErr != nil {
		storeRaiseCorruptedBeadAlertFunc(bead.ID, aErr)
	}
	shouldSkip := aErr != nil || attempt != nil

	if !shouldSkip {
		t.Error("expected corrupted bead to be excluded")
	}
	if len(alertedBeads) != 1 || alertedBeads[0] != "spi-corrupted" {
		t.Errorf("expected alert for spi-corrupted, got %v", alertedBeads)
	}
}

func TestRaiseCorruptedBeadAlert_Dedup(t *testing.T) {
	createCount := 0
	origCreate := storeCreateAlertFunc
	storeCreateAlertFunc = func(beadID, msg string) error {
		createCount++
		return nil
	}
	defer func() { storeCreateAlertFunc = origCreate }()

	origCheck := storeCheckExistingAlertFunc
	storeCheckExistingAlertFunc = func(beadID string) bool { return false }
	defer func() { storeCheckExistingAlertFunc = origCheck }()

	storeRaiseCorruptedBeadAlert("spi-dup", fmt.Errorf("invariant violation"))
	if createCount != 1 {
		t.Errorf("expected 1 create on first call, got %d", createCount)
	}

	storeCheckExistingAlertFunc = func(beadID string) bool { return true }
	storeRaiseCorruptedBeadAlert("spi-dup", fmt.Errorf("invariant violation"))
	if createCount != 1 {
		t.Errorf("expected still 1 create after dedup, got %d", createCount)
	}
}
