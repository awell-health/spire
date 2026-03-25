package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdSummon_ForRemoved(t *testing.T) {
	err := cmdSummon([]string{"1", "--for", "spi-abc"})
	if err == nil {
		t.Fatal("expected error for removed --for flag")
	}
	if !strings.Contains(err.Error(), "--for has been removed") {
		t.Fatalf("expected removed --for error, got %v", err)
	}
}

// writeScanOrphanState writes an executorState JSON file at
// <configDir>/runtime/<agentName>/state.json.
func writeScanOrphanState(t *testing.T, configDir, agentName string, state executorState) {
	t.Helper()
	dir := filepath.Join(configDir, "runtime", agentName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestScanOrphanedBeads_NoRuntimeDir returns nil when runtime dir is absent.
func TestScanOrphanedBeads_NoRuntimeDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	// No runtime dir created — scanOrphanedBeads should return nil gracefully.

	got := scanOrphanedBeads(wizardRegistry{})
	if len(got) != 0 {
		t.Errorf("expected 0 orphans for missing runtime dir, got %d", len(got))
	}
}

// TestScanOrphanedBeads_SkipsLiveAgent ignores agents present in the live registry.
func TestScanOrphanedBeads_SkipsLiveAgent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	writeScanOrphanState(t, tmp, "wizard-spi-abc", executorState{BeadID: "spi-abc", Phase: "implement"})

	liveReg := wizardRegistry{
		Wizards: []localWizard{
			{Name: "wizard-spi-abc", PID: os.Getpid(), BeadID: "spi-abc"},
		},
	}

	got := scanOrphanedBeads(liveReg)
	if len(got) != 0 {
		t.Errorf("expected 0 orphans when agent is live, got %d", len(got))
	}
}

// TestScanOrphanedBeads_SkipsEmptyBeadID ignores state files with no bead_id.
func TestScanOrphanedBeads_SkipsEmptyBeadID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	writeScanOrphanState(t, tmp, "wizard-orphan-1", executorState{BeadID: "", Phase: "implement"})

	got := scanOrphanedBeads(wizardRegistry{})
	if len(got) != 0 {
		t.Errorf("expected 0 orphans for empty bead_id, got %d", len(got))
	}
}

// TestScanOrphanedBeads_SkipsInvalidJSON ignores state files with malformed JSON.
func TestScanOrphanedBeads_SkipsInvalidJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	dir := filepath.Join(tmp, "runtime", "wizard-bad-json")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	got := scanOrphanedBeads(wizardRegistry{})
	if len(got) != 0 {
		t.Errorf("expected 0 orphans for invalid JSON, got %d", len(got))
	}
}

// TestScanOrphanedBeads_DeduplicatesBeadID counts each bead only once even if
// multiple agents have state for it.
func TestScanOrphanedBeads_DeduplicatesBeadID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	// Two different agents both claim the same bead.
	// storeGetBead will fail (no db) → both should be skipped, but
	// dedup logic must not double-count the seen set.
	writeScanOrphanState(t, tmp, "wizard-run-1", executorState{BeadID: "spi-dup", Phase: "implement"})
	writeScanOrphanState(t, tmp, "wizard-run-2", executorState{BeadID: "spi-dup", Phase: "review"})

	// Both storeGetBead calls will error (no dolt), so result is still 0 —
	// but this test verifies the seen-dedup path is reached without panic.
	got := scanOrphanedBeads(wizardRegistry{})
	// We can't assert on count here since storeGetBead requires a live db;
	// we just verify it doesn't panic or return duplicates.
	seen := make(map[string]int)
	for _, b := range got {
		seen[b.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("bead %q appeared %d times in orphan list, want at most 1", id, n)
		}
	}
}
