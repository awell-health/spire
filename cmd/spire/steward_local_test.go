package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- resolveMode tests ---

func TestResolveMode(t *testing.T) {
	tests := []struct {
		name  string
		input StewardMode
		want  StewardMode
		skipInK8s bool
	}{
		{name: "explicit local", input: StewardModeLocal, want: StewardModeLocal},
		{name: "explicit k8s", input: StewardModeK8s, want: StewardModeK8s},
		// Auto outside k8s resolves to local. Skip this case when actually running
		// inside a cluster so the test suite stays green in CI pods.
		{name: "auto outside k8s", input: StewardModeAuto, want: StewardModeLocal, skipInK8s: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skipInK8s && isInK8s() {
				t.Skip("running inside k8s — auto resolves to k8s, not local")
			}
			got := resolveMode(tt.input)
			if got != tt.want {
				t.Errorf("resolveMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
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
