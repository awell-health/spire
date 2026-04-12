package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCmdMetrics_NoOLAP_NoFallback_Errors verifies that when DuckDB is
// unavailable and --fallback is NOT set, cmdMetrics returns a clear,
// actionable error instead of silently falling back to Dolt.
func TestCmdMetrics_NoOLAP_NoFallback_Errors(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	t.Setenv("SPIRE_TOWER", "")

	err := cmdMetrics(nil)
	if err == nil {
		t.Fatal("expected error when DuckDB unavailable and --fallback not set, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "OLAP database unavailable") {
		t.Errorf("error should mention OLAP unavailable, got: %s", msg)
	}
	if !strings.Contains(msg, "spire up") {
		t.Errorf("error should suggest `spire up`, got: %s", msg)
	}
	if !strings.Contains(msg, "--fallback") {
		t.Errorf("error should mention --fallback flag, got: %s", msg)
	}
}

// TestCmdMetrics_NoOLAP_WithFallback_SkipsOLAPError verifies that the
// --fallback flag bypasses the DuckDB requirement and proceeds to the
// Dolt path instead of erroring about OLAP.
func TestCmdMetrics_NoOLAP_WithFallback_SkipsOLAPError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	t.Setenv("SPIRE_TOWER", "")

	err := cmdMetrics([]string{"--fallback"})
	if err == nil {
		// The Dolt path may also fail (no Dolt running), but that's OK —
		// the point is we didn't get the OLAP-specific error.
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "OLAP database unavailable") {
		t.Errorf("--fallback should bypass OLAP error, but got: %s", msg)
	}
}

// TestCmdMetrics_OLAPAvailable_UsesDuckDB verifies that when DuckDB is
// available, the command uses it for queries (not Dolt).
func TestCmdMetrics_OLAPAvailable_UsesDuckDB(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	towersDir := filepath.Join(configDir, "towers")

	if err := os.MkdirAll(towersDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create DuckDB parent directory. OLAPPath returns:
	//   <XDG_DATA_HOME>/spire/<slug>/analytics.db
	olapDir := filepath.Join(dataDir, "spire", "test-tower")
	if err := os.MkdirAll(olapDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a minimal tower config.
	towerJSON := `{"name":"test-tower","project_id":"test","hub_prefix":"tst","database":"test"}`
	if err := os.WriteFile(filepath.Join(towersDir, "test-tower.json"), []byte(towerJSON), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SPIRE_CONFIG_DIR", configDir)
	t.Setenv("SPIRE_TOWER", "test-tower")
	t.Setenv("XDG_DATA_HOME", dataDir)

	// Capture stdout to verify JSON output.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	cmdErr := cmdMetrics([]string{"--json"})

	w.Close()
	os.Stdout = origStdout

	if cmdErr != nil {
		t.Fatalf("expected no error with valid DuckDB, got: %v", cmdErr)
	}

	// Read captured output and verify it's valid JSON.
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	r.Close()
	output := string(buf[:n])

	if !json.Valid([]byte(output)) {
		t.Errorf("expected valid JSON output from DuckDB path, got: %s", output)
	}
}

// TestCmdMetrics_OLAPAvailable_DORAFlag verifies the --dora flag works
// with DuckDB and returns valid output.
func TestCmdMetrics_OLAPAvailable_DORAFlag(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	towersDir := filepath.Join(configDir, "towers")

	if err := os.MkdirAll(towersDir, 0755); err != nil {
		t.Fatal(err)
	}
	olapDir := filepath.Join(dataDir, "spire", "test-tower")
	if err := os.MkdirAll(olapDir, 0755); err != nil {
		t.Fatal(err)
	}
	towerJSON := `{"name":"test-tower","project_id":"test","hub_prefix":"tst","database":"test"}`
	if err := os.WriteFile(filepath.Join(towersDir, "test-tower.json"), []byte(towerJSON), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SPIRE_CONFIG_DIR", configDir)
	t.Setenv("SPIRE_TOWER", "test-tower")
	t.Setenv("XDG_DATA_HOME", dataDir)

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	cmdErr := cmdMetrics([]string{"--dora", "--json"})

	w.Close()
	os.Stdout = origStdout

	if cmdErr != nil {
		t.Fatalf("--dora with DuckDB should not error, got: %v", cmdErr)
	}

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	r.Close()
	output := string(buf[:n])

	if !json.Valid([]byte(output)) {
		t.Errorf("expected valid JSON from --dora, got: %s", output)
	}
}

// TestCmdMetrics_FallbackFlag_ParsedCorrectly verifies --fallback is
// parsed from the args slice without error and doesn't conflict with
// other flags.
func TestCmdMetrics_FallbackFlag_ParsedCorrectly(t *testing.T) {
	// Verify --fallback isn't treated as an unknown flag.
	err := cmdMetrics([]string{"--fallback", "--nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	// The error should be about --nonexistent, not about --fallback.
	if !strings.Contains(err.Error(), "unknown flag: --nonexistent") {
		t.Errorf("expected unknown flag error for --nonexistent, got: %v", err)
	}
}

// TestCmdMetrics_UnknownFlag verifies unknown flags are rejected.
func TestCmdMetrics_UnknownFlag(t *testing.T) {
	err := cmdMetrics([]string{"--nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("expected 'unknown flag' error, got: %v", err)
	}
}

// TestCmdMetrics_AllFlags_WithoutOLAP verifies each metric flag errors
// consistently when DuckDB is unavailable and --fallback is not set.
func TestCmdMetrics_AllFlags_WithoutOLAP(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	t.Setenv("SPIRE_TOWER", "")

	flags := [][]string{
		nil,
		{"--dora"},
		{"--trends"},
		{"--failures"},
		{"--tools"},
		{"--bugs"},
		{"--model"},
		{"--phase"},
		{"--bead", "spi-test"},
	}

	for _, args := range flags {
		name := "default"
		if len(args) > 0 {
			name = args[0]
		}
		t.Run(name, func(t *testing.T) {
			err := cmdMetrics(args)
			if err == nil {
				t.Fatal("expected OLAP error")
			}
			if !strings.Contains(err.Error(), "OLAP database unavailable") {
				t.Errorf("expected OLAP error, got: %v", err)
			}
		})
	}
}
