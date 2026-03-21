package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// --- checkDoltBinary tests ---

func TestDoctorCheckDoltBinary_SystemPath(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not in PATH, skipping")
	}
	r := checkDoltBinary()
	if r.Status != statusOK {
		t.Errorf("expected statusOK, got %s: %s", r.Status, r.Detail)
	}
	if r.Detail == "" {
		t.Error("expected Detail to contain version info")
	}
}

func TestDoctorCheckDoltBinary_ManagedPath(t *testing.T) {
	// Create a temp dir to act as dolt global dir, with a fake dolt binary
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a fake dolt script that outputs a version
	fakeDolt := filepath.Join(binDir, "dolt")
	script := "#!/bin/sh\necho 'dolt version 1.99.0'\n"
	if err := os.WriteFile(fakeDolt, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	r := checkDoltBinary()
	if r.Status != statusOK {
		t.Errorf("expected statusOK with managed binary, got %s: %s", r.Status, r.Detail)
	}
	if r.Detail == "" || r.Detail == "(unknown version)" {
		t.Errorf("expected version in Detail, got: %s", r.Detail)
	}
}

func TestDoctorCheckDoltBinary_NotFound(t *testing.T) {
	// Override dolt global dir to an empty temp dir so managed binary is not found
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)
	// Override PATH to exclude dolt
	t.Setenv("PATH", tmpDir)

	r := checkDoltBinary()
	if r.Status != statusMissing {
		t.Errorf("expected statusMissing, got %s: %s", r.Status, r.Detail)
	}
}

// --- checkDoltServer tests ---

func TestDoctorCheckDoltServer_NotRunning(t *testing.T) {
	// Use a port that's almost certainly not in use
	t.Setenv("BEADS_DOLT_SERVER_PORT", "19999")
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	r := checkDoltServer()
	if r.Status != statusMissing {
		t.Errorf("expected statusMissing for non-running server, got %s: %s", r.Status, r.Detail)
	}
}

// --- checkTowerConfig tests ---

func TestDoctorCheckTowerConfig_NoConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "spire")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Point HOME to our temp so configDir() returns our path
	t.Setenv("HOME", tmpDir)

	r := checkTowerConfig("/nonexistent/path")
	if r.Status == statusOK {
		t.Errorf("expected non-OK status with no config, got %s: %s", r.Status, r.Detail)
	}
}

func TestDoctorCheckTowerConfig_WithConfigNoInstances(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, ".config", "spire")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write an empty config
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"instances":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)

	r := checkTowerConfig("/some/path")
	if r.Status != statusOutdated {
		t.Errorf("expected statusOutdated with empty instances, got %s: %s", r.Status, r.Detail)
	}
}

func TestDoctorCheckTowerConfig_WithConfigDirNotRegistered(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, ".config", "spire")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"instances":{"test":{"path":"/other/path","prefix":"tst","role":"standalone"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)

	r := checkTowerConfig("/not/registered")
	if r.Status != statusOutdated {
		t.Errorf("expected statusOutdated for unregistered dir, got %s: %s", r.Status, r.Detail)
	}
}

func TestDoctorCheckTowerConfig_OK(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, ".config", "spire")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(tmpDir, "myrepo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"instances":{"test":{"path":"` + repoDir + `","prefix":"tst","role":"standalone"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)

	r := checkTowerConfig(repoDir)
	if r.Status != statusOK {
		t.Errorf("expected statusOK, got %s: %s", r.Status, r.Detail)
	}
}

// --- checkCredentials tests ---

func TestDoctorCheckCredentials_AllFromEnv(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, ".config", "spire")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)

	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("GITHUB_TOKEN", "ghp_test")
	t.Setenv("DOLT_REMOTE_USER", "testuser")
	t.Setenv("DOLT_REMOTE_PASSWORD", "testpass")

	r := checkCredentials()
	if r.Status != statusOK {
		t.Errorf("expected statusOK with all env vars set, got %s: %s", r.Status, r.Detail)
	}
}

func TestDoctorCheckCredentials_AllFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, ".config", "spire")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := `anthropic-key=sk-ant-test
github-token=ghp_test
dolthub-user=testuser
dolthub-password=testpass
`
	if err := os.WriteFile(filepath.Join(configDir, "credentials"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)
	// Clear env vars that might be set
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("SPIRE_ANTHROPIC_KEY", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("SPIRE_GITHUB_TOKEN", "")
	t.Setenv("DOLT_REMOTE_USER", "")
	t.Setenv("SPIRE_DOLTHUB_USER", "")
	t.Setenv("DOLT_REMOTE_PASSWORD", "")
	t.Setenv("SPIRE_DOLTHUB_PASSWORD", "")

	r := checkCredentials()
	if r.Status != statusOK {
		t.Errorf("expected statusOK with credential file, got %s: %s", r.Status, r.Detail)
	}
}

func TestDoctorCheckCredentials_Partial(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, ".config", "spire")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)
	// Clear all env vars
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("SPIRE_ANTHROPIC_KEY", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("SPIRE_GITHUB_TOKEN", "")
	t.Setenv("DOLT_REMOTE_USER", "")
	t.Setenv("SPIRE_DOLTHUB_USER", "")
	t.Setenv("DOLT_REMOTE_PASSWORD", "")
	t.Setenv("SPIRE_DOLTHUB_PASSWORD", "")

	// Only set one via env
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")

	r := checkCredentials()
	if r.Status != statusOutdated {
		t.Errorf("expected statusOutdated with partial creds, got %s: %s", r.Status, r.Detail)
	}
}

func TestDoctorCheckCredentials_None(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, ".config", "spire")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)
	// Clear all env vars
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("SPIRE_ANTHROPIC_KEY", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("SPIRE_GITHUB_TOKEN", "")
	t.Setenv("DOLT_REMOTE_USER", "")
	t.Setenv("SPIRE_DOLTHUB_USER", "")
	t.Setenv("DOLT_REMOTE_PASSWORD", "")
	t.Setenv("SPIRE_DOLTHUB_PASSWORD", "")

	r := checkCredentials()
	if r.Status != statusMissing {
		t.Errorf("expected statusMissing with no creds, got %s: %s", r.Status, r.Detail)
	}
}

func TestDoctorCheckCredentials_MixedFileAndEnv(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, ".config", "spire")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Set two in file
	content := `anthropic-key=sk-ant-test
github-token=ghp_test
`
	if err := os.WriteFile(filepath.Join(configDir, "credentials"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)
	// Clear all env vars first
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("SPIRE_ANTHROPIC_KEY", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("SPIRE_GITHUB_TOKEN", "")
	t.Setenv("DOLT_REMOTE_USER", "")
	t.Setenv("SPIRE_DOLTHUB_USER", "")
	t.Setenv("DOLT_REMOTE_PASSWORD", "")
	t.Setenv("SPIRE_DOLTHUB_PASSWORD", "")
	// Set remaining two via env
	t.Setenv("DOLT_REMOTE_USER", "testuser")
	t.Setenv("SPIRE_DOLTHUB_PASSWORD", "testpass")

	r := checkCredentials()
	if r.Status != statusOK {
		t.Errorf("expected statusOK with mixed file+env, got %s: %s", r.Status, r.Detail)
	}
}

// --- checkDocker tests ---

func TestDoctorCheckDocker_Available(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available, skipping")
	}
	r := checkDocker()
	if !r.Optional {
		t.Error("docker check should be marked as Optional")
	}
	// Don't assert statusOK since docker daemon might not be running
}

func TestDoctorCheckDocker_NotAvailable(t *testing.T) {
	// Override PATH so docker is not found
	tmpDir := t.TempDir()
	t.Setenv("PATH", tmpDir)

	r := checkDocker()
	if r.Status != statusMissing {
		t.Errorf("expected statusMissing, got %s: %s", r.Status, r.Detail)
	}
	if !r.Optional {
		t.Error("docker check should be marked as Optional")
	}
}

// --- Category and summary tests ---

func TestDoctorCategorySummaryCount(t *testing.T) {
	checks := []checkResult{
		{Name: "a", Status: statusOK},
		{Name: "b", Status: statusMissing},
		{Name: "c", Status: statusOK},
		{Name: "d", Status: statusMissing, Optional: true},
	}

	total := len(checks)
	passed := 0
	for _, c := range checks {
		if c.Status == statusOK {
			passed++
		}
	}

	if total != 4 {
		t.Errorf("expected 4 total checks, got %d", total)
	}
	if passed != 2 {
		t.Errorf("expected 2 passed checks, got %d", passed)
	}

	// Optional check with non-OK status should not count as passed
	// but it also should not block doctor from reporting success-ish
	optionalFailing := 0
	requiredFailing := 0
	for _, c := range checks {
		if c.Status != statusOK {
			if c.Optional {
				optionalFailing++
			} else {
				requiredFailing++
			}
		}
	}
	if optionalFailing != 1 {
		t.Errorf("expected 1 optional failing, got %d", optionalFailing)
	}
	if requiredFailing != 1 {
		t.Errorf("expected 1 required failing, got %d", requiredFailing)
	}
}

// --- parseCredentialFile tests ---

func TestDoctorParseCredentialFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "credentials")
	content := `# Comment line
anthropic-key=sk-ant-xxx
github-token=ghp_yyy

# Empty value should be ignored
empty-key=
dolthub-user=testuser
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	keys := parseCredentialFile(path)
	if !keys["anthropic-key"] {
		t.Error("expected anthropic-key to be present")
	}
	if !keys["github-token"] {
		t.Error("expected github-token to be present")
	}
	if !keys["dolthub-user"] {
		t.Error("expected dolthub-user to be present")
	}
	if keys["empty-key"] {
		t.Error("expected empty-key to NOT be present (empty value)")
	}
	if keys["nonexistent"] {
		t.Error("expected nonexistent key to NOT be present")
	}
}

func TestDoctorParseCredentialFile_NotExists(t *testing.T) {
	keys := parseCredentialFile("/nonexistent/path/credentials")
	if len(keys) != 0 {
		t.Errorf("expected empty map for nonexistent file, got %d keys", len(keys))
	}
}
