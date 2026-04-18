package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/executor"
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

// writeScanOrphanState writes a GraphState JSON file at
// <configDir>/runtime/<agentName>/graph_state.json.
func writeScanOrphanState(t *testing.T, configDir, agentName string, gs executor.GraphState) {
	t.Helper()
	dir := filepath.Join(configDir, "runtime", agentName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(gs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "graph_state.json"), data, 0644); err != nil {
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

	writeScanOrphanState(t, tmp, "wizard-spi-abc", executor.GraphState{BeadID: "spi-abc", ActiveStep: "implement"})

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

	writeScanOrphanState(t, tmp, "wizard-orphan-1", executor.GraphState{BeadID: "", ActiveStep: "implement"})

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
	if err := os.WriteFile(filepath.Join(dir, "graph_state.json"), []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	got := scanOrphanedBeads(wizardRegistry{})
	if len(got) != 0 {
		t.Errorf("expected 0 orphans for invalid JSON, got %d", len(got))
	}
}

// --- Positional bead-ID parsing tests ---

func TestCmdSummon_PositionalBeadIDs_PassesParsing(t *testing.T) {
	// Single bead ID: should pass arg parsing and fail later at the store layer.
	err := cmdSummon([]string{"spi-xxx"})
	if err == nil {
		return // if no error, parsing and store both worked — fine
	}
	// Must not fail with a parsing error.
	if strings.Contains(err.Error(), "expected a bead ID or number") {
		t.Fatalf("single positional bead ID should not fail parsing, got: %v", err)
	}
	if strings.Contains(err.Error(), "usage:") {
		t.Fatalf("single positional bead ID should not trigger usage error, got: %v", err)
	}
}

func TestCmdSummon_MultiplePositionalBeadIDs_PassesParsing(t *testing.T) {
	// Multiple bead IDs: should pass arg parsing and fail later at the store layer.
	err := cmdSummon([]string{"spi-xxx", "spi-yyy", "spi-zzz"})
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "expected a bead ID or number") {
		t.Fatalf("multiple positional bead IDs should not fail parsing, got: %v", err)
	}
	if strings.Contains(err.Error(), "cannot combine") {
		t.Fatalf("multiple positional bead IDs should not trigger mutual-exclusivity error, got: %v", err)
	}
}

func TestCmdSummon_PositionalBeadIDs_MutualExclWithTargets(t *testing.T) {
	err := cmdSummon([]string{"spi-xxx", "--targets", "spi-yyy"})
	if err == nil {
		t.Fatal("expected error when combining positional bead IDs with --targets")
	}
	if !strings.Contains(err.Error(), "cannot combine positional bead IDs with --targets") {
		t.Fatalf("expected mutual-exclusivity error, got: %v", err)
	}
}

func TestCmdSummon_PositionalBeadIDs_MutualExclWithCount(t *testing.T) {
	err := cmdSummon([]string{"spi-xxx", "3"})
	if err == nil {
		t.Fatal("expected error when combining positional bead IDs with a numeric count")
	}
	if !strings.Contains(err.Error(), "cannot combine positional bead IDs with a numeric count") {
		t.Fatalf("expected count+IDs mutual-exclusivity error, got: %v", err)
	}
}

func TestCmdSummon_PositionalBeadIDs_CountInferred(t *testing.T) {
	// Two positional bead IDs with no explicit count: should infer count=2.
	// This will fail at the store layer, not at parsing — verifying
	// that the count-inference logic doesn't produce a "requires a positive number" error.
	err := cmdSummon([]string{"spi-aaa", "spi-bbb"})
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "summon requires a positive number") {
		t.Fatalf("positional bead IDs should infer count, got: %v", err)
	}
}

func TestCmdSummon_NumericCountStillWorks(t *testing.T) {
	// Bare number: existing behavior should be preserved.
	err := cmdSummon([]string{"3"})
	if err == nil {
		return
	}
	// Must not fail with a parsing error — should pass through to store/k8s.
	if strings.Contains(err.Error(), "expected a bead ID or number") {
		t.Fatalf("bare numeric count should not fail parsing, got: %v", err)
	}
}

func TestCmdSummon_DispatchInvalidMode(t *testing.T) {
	err := cmdSummon([]string{"1", "--dispatch", "bogus"})
	if err == nil {
		t.Fatal("expected error for invalid dispatch mode")
	}
	if !strings.Contains(err.Error(), "invalid dispatch mode") {
		t.Fatalf("expected invalid dispatch mode error, got %v", err)
	}
}

func TestCmdSummon_DispatchMissingArg(t *testing.T) {
	err := cmdSummon([]string{"1", "--dispatch"})
	if err == nil {
		t.Fatal("expected error when --dispatch has no argument")
	}
	if !strings.Contains(err.Error(), "--dispatch requires a mode") {
		t.Fatalf("expected missing mode error, got %v", err)
	}
}

func TestCmdSummon_DispatchValidModes(t *testing.T) {
	// Point BEADS_DIR at an empty temp dir and reset the store so that
	// storeGetReadyWork fails fast ("no .beads directory found" or
	// immediate open error) instead of hanging on a dolt connection.
	tmp := t.TempDir()
	t.Setenv("BEADS_DIR", tmp)
	resetStore()

	// Force the k8s-availability probe to return false so cmdSummon
	// takes the local path. Without this the real probe shells out to
	// kubectl, which hangs indefinitely on machines whose current
	// context targets an unreachable API server.
	origK8s := isK8sAvailableFunc
	defer func() { isK8sAvailableFunc = origK8s }()
	isK8sAvailableFunc = func() bool { return false }

	for _, mode := range []string{"sequential", "wave", "direct"} {
		t.Run(mode, func(t *testing.T) {
			// Valid modes pass validation but will fail later when hitting
			// the store (no dolt server). We just verify they don't fail
			// at the dispatch validation step.
			err := cmdSummon([]string{"1", "--dispatch", mode})
			if err != nil && strings.Contains(err.Error(), "invalid dispatch mode") {
				t.Fatalf("mode %q should be valid, got: %v", mode, err)
			}
		})
	}
}

// --- Status gate tests for summonLocal ---

// TestSummonLocal_RejectsClosedBead verifies that summonLocal returns an error
// when a directly-targeted bead has status "closed".
func TestSummonLocal_RejectsClosedBead(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	orig := storeGetBeadFunc
	defer func() { storeGetBeadFunc = orig }()
	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "closed", Title: "test"}, nil
	}

	err := summonLocal(1, []string{"spi-closed"}, "")
	if err == nil {
		t.Fatal("expected error for closed bead")
	}
	if !strings.Contains(err.Error(), "spi-closed is closed") {
		t.Fatalf("expected closed error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "reopen it first") {
		t.Fatalf("expected actionable hint, got: %v", err)
	}
}

// TestSummonLocal_RejectsDoneBead verifies that summonLocal returns an error
// when a directly-targeted bead has status "done".
func TestSummonLocal_RejectsDoneBead(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	orig := storeGetBeadFunc
	defer func() { storeGetBeadFunc = orig }()
	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "done", Title: "test"}, nil
	}

	err := summonLocal(1, []string{"spi-done"}, "")
	if err == nil {
		t.Fatal("expected error for done bead")
	}
	if !strings.Contains(err.Error(), "spi-done is closed") {
		t.Fatalf("expected closed error for done status, got: %v", err)
	}
}

// TestSummonLocal_RejectsDeferredBead verifies that summonLocal returns an error
// when a directly-targeted bead has status "deferred".
func TestSummonLocal_RejectsDeferredBead(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	orig := storeGetBeadFunc
	defer func() { storeGetBeadFunc = orig }()
	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "deferred", Title: "test"}, nil
	}

	err := summonLocal(1, []string{"spi-deferred"}, "")
	if err == nil {
		t.Fatal("expected error for deferred bead")
	}
	if !strings.Contains(err.Error(), "spi-deferred is deferred") {
		t.Fatalf("expected deferred error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "set to open or ready first") {
		t.Fatalf("expected actionable hint, got: %v", err)
	}
}

// TestSummonLocal_AllowsOpenBead verifies that open beads pass the status gate.
// The function will proceed past status checking and fail later (no DB), which
// confirms the gate did not reject the bead.
func TestSummonLocal_AllowsOpenBead(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	orig := storeGetBeadFunc
	defer func() { storeGetBeadFunc = orig }()
	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "open", Title: "test"}, nil
	}

	err := summonLocal(1, []string{"spi-open"}, "")
	// Should NOT get a status rejection error. It will fail later
	// (no formula, no DB, etc.) but that's fine — we're testing the gate.
	if err != nil && (strings.Contains(err.Error(), "is closed") || strings.Contains(err.Error(), "is deferred")) {
		t.Fatalf("open bead should not be rejected by status gate, got: %v", err)
	}
}

// TestSummonLocal_AllowsInProgressBead verifies that in_progress beads pass the status gate.
func TestSummonLocal_AllowsInProgressBead(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	orig := storeGetBeadFunc
	defer func() { storeGetBeadFunc = orig }()
	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "in_progress", Title: "test"}, nil
	}

	err := summonLocal(1, []string{"spi-wip"}, "")
	if err != nil && (strings.Contains(err.Error(), "is closed") || strings.Contains(err.Error(), "is deferred")) {
		t.Fatalf("in_progress bead should not be rejected by status gate, got: %v", err)
	}
}

// TestSummonLocal_RejectsMultipleTargets_FirstBadFails verifies that when
// multiple targets are provided, the first invalid one causes an immediate error.
func TestSummonLocal_RejectsMultipleTargets_FirstBadFails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	orig := storeGetBeadFunc
	defer func() { storeGetBeadFunc = orig }()

	callCount := 0
	storeGetBeadFunc = func(id string) (Bead, error) {
		callCount++
		if id == "spi-good" {
			return Bead{ID: id, Status: "open", Title: "good"}, nil
		}
		return Bead{ID: id, Status: "closed", Title: "bad"}, nil
	}

	err := summonLocal(2, []string{"spi-good", "spi-bad"}, "")
	if err == nil {
		t.Fatal("expected error when second target is closed")
	}
	if !strings.Contains(err.Error(), "spi-bad is closed") {
		t.Fatalf("expected closed error for spi-bad, got: %v", err)
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
	writeScanOrphanState(t, tmp, "wizard-run-1", executor.GraphState{BeadID: "spi-dup", ActiveStep: "implement"})
	writeScanOrphanState(t, tmp, "wizard-run-2", executor.GraphState{BeadID: "spi-dup", ActiveStep: "review"})

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
