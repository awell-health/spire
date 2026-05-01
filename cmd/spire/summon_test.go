package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/beadlifecycle"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/wizard"
	"github.com/awell-health/spire/pkg/wizardregistry"
)

// synthHeaderAuth is a SelectFlags value that synthesizes a throwaway
// subscription credential via the -H header path. Tests that exercise the
// spawn flow but don't care about auth use this so SelectAuth succeeds
// without requiring a real credentials file in the test tempdir.
var synthHeaderAuth = wizard.SelectFlags{HeaderToken: "test-token"}

// stubSpawn is a summonSpawnFunc that refuses to spawn a real subprocess.
// Tests that reach summonLocal's spawn path use it to avoid fork/exec'ing the
// test binary, which would inherit SPIRE_CONFIG_DIR and race with t.TempDir
// cleanup.
func stubSpawn(AgentBackend, SpawnConfig) (agent.Handle, error) {
	return nil, fmt.Errorf("test: spawn disabled")
}

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
	// Stub the preflight resolver: this test verifies that a single
	// positional bead ID clears arg parsing, not that the prefix is
	// actually bound. Without the stub, cmdSummon would call the real
	// resolver (hitting dolt) — which is out of scope here.
	prev := wizardResolveRepoForSummon
	wizardResolveRepoForSummon = func(beadID string) (string, string, string, error) {
		return "/tmp/fake", "git@example/fake.git", "main", nil
	}
	t.Cleanup(func() { wizardResolveRepoForSummon = prev })

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
	// See TestCmdSummon_PositionalBeadIDs_PassesParsing for why we stub
	// the resolver.
	prev := wizardResolveRepoForSummon
	wizardResolveRepoForSummon = func(beadID string) (string, string, string, error) {
		return "/tmp/fake", "git@example/fake.git", "main", nil
	}
	t.Cleanup(func() { wizardResolveRepoForSummon = prev })

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
	// See TestCmdSummon_PositionalBeadIDs_PassesParsing for why we stub
	// the resolver.
	prev := wizardResolveRepoForSummon
	wizardResolveRepoForSummon = func(beadID string) (string, string, string, error) {
		return "/tmp/fake", "git@example/fake.git", "main", nil
	}
	t.Cleanup(func() { wizardResolveRepoForSummon = prev })

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
	// Isolate SPIRE_CONFIG_DIR for the same spi-od41sr reason as the
	// gateway dismiss test: cmdSummon → summonLocal → loadWizardRegistry
	// reads ~/.config/spire/wizards.json by default, and scanOrphanedBeads
	// walks ~/.config/spire/runtime/. Without this the test reads the
	// operator's live wizard state.
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())
	resetStore()

	// Drive dispatch through a fake local-native tower so cmdSummon
	// takes the local path. Without this the real activeTowerConfig
	// would try to resolve a tower from the test's temp dir and fail
	// before dispatch validation is exercised.
	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "test", DeploymentMode: config.DeploymentModeLocalNative}, nil
	}

	// Belt-and-suspenders: even though the local-native branch never
	// consults isK8sAvailableFunc, stub it to a no-op so the real probe
	// (which shells out to kubectl) can't fire if the dispatch logic
	// ever changes.
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

	err := summonLocal(1, []string{"spi-closed"}, "", wizard.SelectFlags{})
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

	err := summonLocal(1, []string{"spi-done"}, "", wizard.SelectFlags{})
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

	err := summonLocal(1, []string{"spi-deferred"}, "", wizard.SelectFlags{})
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
	origSpawn := summonSpawnFunc
	defer func() {
		storeGetBeadFunc = orig
		summonSpawnFunc = origSpawn
	}()
	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "open", Title: "test"}, nil
	}
	summonSpawnFunc = stubSpawn

	err := summonLocal(1, []string{"spi-open"}, "", synthHeaderAuth)
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
	origSpawn := summonSpawnFunc
	defer func() {
		storeGetBeadFunc = orig
		summonSpawnFunc = origSpawn
	}()
	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "in_progress", Title: "test"}, nil
	}
	summonSpawnFunc = stubSpawn

	err := summonLocal(1, []string{"spi-wip"}, "", synthHeaderAuth)
	if err != nil && (strings.Contains(err.Error(), "is closed") || strings.Contains(err.Error(), "is deferred")) {
		t.Fatalf("in_progress bead should not be rejected by status gate, got: %v", err)
	}
}

// TestSummonLocal_TransitionsOpenToInProgress verifies that a bead with
// status "open" invokes summonBeginWorkFunc (which handles orphan sweep +
// attempt creation + in_progress transition). This is the core fix for
// spi-corqy: prior to the fix, the status-transition switch had no case for
// "open"/"ready". Now BeginWork handles all non-closed statuses.
func TestSummonLocal_TransitionsOpenToInProgress(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	origGet := storeGetBeadFunc
	origBegin := summonBeginWorkFunc
	origSpawn := summonSpawnFunc
	defer func() {
		storeGetBeadFunc = origGet
		summonBeginWorkFunc = origBegin
		summonSpawnFunc = origSpawn
	}()

	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "open", Title: "test"}, nil
	}

	var gotBeadID string
	summonBeginWorkFunc = func(deps beadlifecycle.Deps, _ wizardregistry.Registry, beadID string, opts beadlifecycle.BeginOpts) (string, error) {
		gotBeadID = beadID
		return "att-stub", nil
	}
	summonSpawnFunc = stubSpawn

	_ = summonLocal(1, []string{"spi-open"}, "", synthHeaderAuth)

	if gotBeadID != "spi-open" {
		t.Fatalf("expected BeginWork called for spi-open, got %q", gotBeadID)
	}
}

// TestSummonLocal_TransitionsReadyToInProgress verifies the same flow
// from "ready" (the other status that falls into the BeginWork case).
func TestSummonLocal_TransitionsReadyToInProgress(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	origGet := storeGetBeadFunc
	origBegin := summonBeginWorkFunc
	origSpawn := summonSpawnFunc
	defer func() {
		storeGetBeadFunc = origGet
		summonBeginWorkFunc = origBegin
		summonSpawnFunc = origSpawn
	}()

	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "ready", Title: "test"}, nil
	}

	var gotBeadID string
	summonBeginWorkFunc = func(deps beadlifecycle.Deps, _ wizardregistry.Registry, beadID string, opts beadlifecycle.BeginOpts) (string, error) {
		gotBeadID = beadID
		return "att-stub", nil
	}
	summonSpawnFunc = stubSpawn

	_ = summonLocal(1, []string{"spi-ready"}, "", synthHeaderAuth)

	if gotBeadID != "spi-ready" {
		t.Fatalf("expected BeginWork called for spi-ready, got %q", gotBeadID)
	}
}

// TestSummonLocal_TransitionFailurePropagates verifies that when BeginWork
// fails, summonLocal aborts with a wrapped error mentioning the bead.
//
// Uses synthHeaderAuth so SelectAuth (now in pass 1, ahead of BeginWork per
// spi-c13c4w) succeeds and the test can exercise BeginWork's failure path.
// Pre-c13c4w the order was inverted, so a no-flags SelectFlags{} happened
// to work — but that ordering was the bug under repair, not a contract.
func TestSummonLocal_TransitionFailurePropagates(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	origGet := storeGetBeadFunc
	origBegin := summonBeginWorkFunc
	defer func() {
		storeGetBeadFunc = origGet
		summonBeginWorkFunc = origBegin
	}()

	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "open", Title: "test"}, nil
	}
	summonBeginWorkFunc = func(deps beadlifecycle.Deps, _ wizardregistry.Registry, beadID string, opts beadlifecycle.BeginOpts) (string, error) {
		return "", fmt.Errorf("db down")
	}

	err := summonLocal(1, []string{"spi-open"}, "", synthHeaderAuth)
	if err == nil {
		t.Fatal("expected error when BeginWork fails")
	}
	if !strings.Contains(err.Error(), "spi-open") {
		t.Fatalf("expected error mentioning bead spi-open, got: %v", err)
	}
	if !strings.Contains(err.Error(), "db down") {
		t.Fatalf("expected wrapped BeginWork error, got: %v", err)
	}
}

// TestSummonLocal_AuthFailureSkipsBeginWork is the regression test for
// spi-c13c4w. Pre-fix, summon ran BeginWork (orphan sweep + attempt
// creation + status flip to in_progress) before SelectAuth in the spawn
// loop, so an auth-failure invocation left an orphan attempt bead behind
// that the next summon's OrphanSweep had to clean up — labeling the
// source bead `dead-letter:orphan` for runs that never started. After
// the fix, SelectAuth runs in a pre-flight pass; auth errors abort the
// summon BEFORE BeginWork is ever called.
//
// We assert the absence of state mutation by counting BeginWork calls
// (the sole producer of the attempt bead and status transition under
// summonLocal). Counter must be 0 on auth-failure paths.
func TestSummonLocal_AuthFailureSkipsBeginWork(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	// Auth config has subscription only — a P0 bead under rule 5
	// (priority-0 → api-key required) will fail SelectAuth.
	prevRead := selectAuthReadConfig
	defer func() { selectAuthReadConfig = prevRead }()
	selectAuthReadConfig = func() (*config.AuthConfig, error) {
		return &config.AuthConfig{
			AutoPromoteOn429: true,
			Subscription:     &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "tok"},
		}, nil
	}

	origGet := storeGetBeadFunc
	origBegin := summonBeginWorkFunc
	origSpawn := summonSpawnFunc
	defer func() {
		storeGetBeadFunc = origGet
		summonBeginWorkFunc = origBegin
		summonSpawnFunc = origSpawn
	}()
	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "open", Title: "p0", Priority: 0}, nil
	}
	beginCalls := 0
	summonBeginWorkFunc = func(_ beadlifecycle.Deps, _ wizardregistry.Registry, _ string, _ beadlifecycle.BeginOpts) (string, error) {
		beginCalls++
		return "att-should-not-be-created", nil
	}
	spawnCalls := 0
	summonSpawnFunc = func(AgentBackend, SpawnConfig) (agent.Handle, error) {
		spawnCalls++
		return nil, fmt.Errorf("spawn must not be called on auth-failure path")
	}

	err := summonLocal(1, []string{"spi-p0"}, "", wizard.SelectFlags{})
	if err == nil {
		t.Fatal("expected auth error for P0 bead with no api-key configured")
	}

	// Error parity with the user-visible message (per acceptance criteria
	// and the bug report's reproduction): the wrapper prefix is unchanged.
	if !strings.Contains(err.Error(), "auth selection for spi-p0") {
		t.Errorf("error must keep the `auth selection for <id>` prefix, got: %v", err)
	}
	if !strings.Contains(err.Error(), "api-key slot required") {
		t.Errorf("error must mention the unconfigured slot, got: %v", err)
	}

	// Core regression assertions: no BeginWork → no attempt bead, no
	// status transition, no orphan label.
	if beginCalls != 0 {
		t.Errorf("BeginWork called %d times; expected 0 — auth failure must abort before BeginWork", beginCalls)
	}
	if spawnCalls != 0 {
		t.Errorf("spawn called %d times; expected 0 — auth failure must abort before spawn", spawnCalls)
	}

	// No graph_state.json should have been written either: attachAuthToRunState
	// runs in pass 3, after auth has already succeeded.
	gsPath := filepath.Join(tmp, "runtime", "wizard-spi-p0", "graph_state.json")
	if _, err := os.Stat(gsPath); !os.IsNotExist(err) {
		t.Errorf("graph_state.json must not be written on auth-failure path, stat err = %v", err)
	}
}

// TestSummonLocal_RejectsMultipleTargets_FirstBadFails verifies that when
// multiple targets are provided, the first closed one causes an immediate error.
func TestSummonLocal_RejectsMultipleTargets_FirstBadFails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	origGet := storeGetBeadFunc
	origBegin := summonBeginWorkFunc
	defer func() {
		storeGetBeadFunc = origGet
		summonBeginWorkFunc = origBegin
	}()

	storeGetBeadFunc = func(id string) (Bead, error) {
		if id == "spi-good" {
			return Bead{ID: id, Status: "in_progress", Title: "good"}, nil
		}
		return Bead{ID: id, Status: "closed", Title: "bad"}, nil
	}
	// Stub BeginWork so spi-good doesn't need a live store.
	summonBeginWorkFunc = func(deps beadlifecycle.Deps, _ wizardregistry.Registry, beadID string, opts beadlifecycle.BeginOpts) (string, error) {
		return "att-stub", nil
	}

	err := summonLocal(2, []string{"spi-good", "spi-bad"}, "", wizard.SelectFlags{})
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

// TestPreflightResolveTargets_UnboundAborts is the layer-0 guard
// (spi-rpuzs6). `spire summon spd-1jd` where the spd prefix is unbound
// must exit non-zero with a bind-instructions error before any wizard
// is spawned.
func TestPreflightResolveTargets_UnboundAborts(t *testing.T) {
	prev := wizardResolveRepoForSummon
	wizardResolveRepoForSummon = func(beadID string) (string, string, string, error) {
		return "", "", "", fmt.Errorf("no local repo registered for prefix %q (bead %s)", "spd", beadID)
	}
	t.Cleanup(func() { wizardResolveRepoForSummon = prev })

	err := preflightResolveTargets([]string{"spd-1jd"})
	if err == nil {
		t.Fatal("preflightResolveTargets with unbound prefix = nil, want aborting error")
	}
	msg := err.Error()
	for _, want := range []string{"spd-1jd", "spd", "spire repo bind", "unbound"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

// TestPreflightResolveTargets_BoundPasses confirms that a resolvable
// prefix short-circuits with a nil error — the normal summon path.
func TestPreflightResolveTargets_BoundPasses(t *testing.T) {
	prev := wizardResolveRepoForSummon
	wizardResolveRepoForSummon = func(beadID string) (string, string, string, error) {
		return "/tmp/spire", "git@github.com:example/spire.git", "main", nil
	}
	t.Cleanup(func() { wizardResolveRepoForSummon = prev })

	if err := preflightResolveTargets([]string{"spi-abc"}); err != nil {
		t.Fatalf("preflightResolveTargets with bound prefix err = %v, want nil", err)
	}
}

// TestPreflightResolveTargets_ReportsAllUnbound verifies the pre-flight
// surfaces every unbound prefix in one pass — so operators fix them
// together instead of playing whack-a-mole.
func TestPreflightResolveTargets_ReportsAllUnbound(t *testing.T) {
	prev := wizardResolveRepoForSummon
	wizardResolveRepoForSummon = func(beadID string) (string, string, string, error) {
		prefix := beadID
		if idx := strings.Index(beadID, "-"); idx > 0 {
			prefix = beadID[:idx]
		}
		return "", "", "", fmt.Errorf("no local repo registered for prefix %q (bead %s)", prefix, beadID)
	}
	t.Cleanup(func() { wizardResolveRepoForSummon = prev })

	err := preflightResolveTargets([]string{"spd-1jd", "oo-abc"})
	if err == nil {
		t.Fatal("preflightResolveTargets = nil, want error listing both unbound prefixes")
	}
	for _, want := range []string{"spd-1jd", "oo-abc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err.Error())
		}
	}
}

// --- Dispatch routing tests (spi-jsxa3v) ---
//
// cmdSummon and cmdDismiss must dispatch on the active tower's
// deployment mode, NOT on kubectl reachability. The bug being fixed:
// a local-native tower with a reachable minikube on the side was
// silently routed to the k8s path because isK8sAvailable() returned
// true. These tests pin that the active tower's mode wins.

// TestCmdSummon_ClusterNative_UnreachableErrors verifies that a
// cluster-native tower with no reachable cluster fails loudly rather
// than silently falling back to the local path.
func TestCmdSummon_ClusterNative_UnreachableErrors(t *testing.T) {
	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "remote", DeploymentMode: config.DeploymentModeClusterNative}, nil
	}

	origK8s := isK8sAvailableFunc
	defer func() { isK8sAvailableFunc = origK8s }()
	isK8sAvailableFunc = func() bool { return false }

	err := cmdSummon([]string{"1"})
	if err == nil {
		t.Fatal("expected error when cluster-native tower can't reach kubectl")
	}
	if !strings.Contains(err.Error(), "cluster-native") {
		t.Fatalf("error should mention cluster-native, got: %v", err)
	}
	if !strings.Contains(err.Error(), "remote") {
		t.Fatalf("error should mention tower name, got: %v", err)
	}
}

// TestCmdSummon_AttachedReservedErrors verifies the attached-reserved
// mode produces a not-yet-supported error rather than silently
// dispatching to either path.
func TestCmdSummon_AttachedReservedErrors(t *testing.T) {
	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "attached", DeploymentMode: config.DeploymentModeAttachedReserved}, nil
	}

	err := cmdSummon([]string{"1"})
	if err == nil {
		t.Fatal("expected error for attached-reserved mode")
	}
	if !strings.Contains(err.Error(), "attached-reserved") {
		t.Fatalf("error should mention attached-reserved, got: %v", err)
	}
}

// TestCmdSummon_UnknownModeErrors verifies the default branch fires for
// any unrecognized mode value rather than silently falling back.
func TestCmdSummon_UnknownModeErrors(t *testing.T) {
	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "weird", DeploymentMode: config.DeploymentMode("bogus")}, nil
	}

	err := cmdSummon([]string{"1"})
	if err == nil {
		t.Fatal("expected error for unknown deployment mode")
	}
	if !strings.Contains(err.Error(), "unknown deployment mode") {
		t.Fatalf("error should mention unknown deployment mode, got: %v", err)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error should mention the bad mode value, got: %v", err)
	}
}

// TestCmdSummon_TowerLookupErrors verifies a tower-resolution failure
// surfaces with the "summon: resolve active tower" prefix rather than
// silently picking a path.
func TestCmdSummon_TowerLookupErrors(t *testing.T) {
	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return nil, fmt.Errorf("no tower configured")
	}

	err := cmdSummon([]string{"1"})
	if err == nil {
		t.Fatal("expected error when tower lookup fails")
	}
	if !strings.Contains(err.Error(), "resolve active tower") {
		t.Fatalf("error should mention resolve active tower, got: %v", err)
	}
}

// TestCmdDismiss_ClusterNative_UnreachableErrors mirrors the summon
// case for the dismiss path: cluster-native tower + unreachable
// kubectl must fail loudly.
func TestCmdDismiss_ClusterNative_UnreachableErrors(t *testing.T) {
	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "remote", DeploymentMode: config.DeploymentModeClusterNative}, nil
	}

	origK8s := isK8sAvailableFunc
	defer func() { isK8sAvailableFunc = origK8s }()
	isK8sAvailableFunc = func() bool { return false }

	err := cmdDismiss([]string{"1"})
	if err == nil {
		t.Fatal("expected error when cluster-native tower can't reach kubectl")
	}
	if !strings.Contains(err.Error(), "cluster-native") {
		t.Fatalf("error should mention cluster-native, got: %v", err)
	}
	if !strings.Contains(err.Error(), "dismiss:") {
		t.Fatalf("error should be prefixed with dismiss:, got: %v", err)
	}
}

// TestCmdDismiss_AttachedReservedErrors verifies the dismiss path also
// rejects attached-reserved.
func TestCmdDismiss_AttachedReservedErrors(t *testing.T) {
	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "attached", DeploymentMode: config.DeploymentModeAttachedReserved}, nil
	}

	err := cmdDismiss([]string{"1"})
	if err == nil {
		t.Fatal("expected error for attached-reserved mode")
	}
	if !strings.Contains(err.Error(), "attached-reserved") {
		t.Fatalf("error should mention attached-reserved, got: %v", err)
	}
}

// TestCmdDismiss_UnknownModeErrors verifies the dismiss default branch.
func TestCmdDismiss_UnknownModeErrors(t *testing.T) {
	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "weird", DeploymentMode: config.DeploymentMode("bogus")}, nil
	}

	err := cmdDismiss([]string{"1"})
	if err == nil {
		t.Fatal("expected error for unknown deployment mode")
	}
	if !strings.Contains(err.Error(), "unknown deployment mode") {
		t.Fatalf("error should mention unknown deployment mode, got: %v", err)
	}
}

// --- Targeted-summon-in-cluster-mode guard tests (spi-v1hcrs) ---
//
// `spire summon <bead-id>` in cluster-native mode used to silently route
// to summonK8s(count), which creates generic WizardGuild capacity and
// discards the parsed target IDs. These tests pin the new behavior:
// targeted summon must be rejected with a redirect to `spire ready`,
// while bare-count summon continues to work.

// TestCmdSummon_K8sTargetedRejected verifies that a positional bead ID
// in cluster-native mode is refused with a clear redirect to
// `spire ready`, and that summonK8s is NOT invoked.
func TestCmdSummon_K8sTargetedRejected(t *testing.T) {
	// Stub the preflight resolver so the test doesn't touch dolt for
	// prefix binding — we're testing the cluster-mode guard, not the
	// preflight.
	prevResolve := wizardResolveRepoForSummon
	wizardResolveRepoForSummon = func(beadID string) (string, string, string, error) {
		return "/tmp/fake", "git@example/fake.git", "main", nil
	}
	t.Cleanup(func() { wizardResolveRepoForSummon = prevResolve })

	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "remote", DeploymentMode: config.DeploymentModeClusterNative}, nil
	}

	origK8s := isK8sAvailableFunc
	defer func() { isK8sAvailableFunc = origK8s }()
	isK8sAvailableFunc = func() bool { return true }

	// Spy: if the gate is wrong and we route to summonK8s, the test
	// fails loudly rather than silently creating capacity.
	origK8sFn := summonK8sFunc
	defer func() { summonK8sFunc = origK8sFn }()
	called := false
	summonK8sFunc = func(count int) error {
		called = true
		return nil
	}

	err := cmdSummon([]string{"spi-abc123"})
	if err == nil {
		t.Fatal("expected error for targeted summon in cluster-native mode")
	}
	if called {
		t.Fatal("summonK8s must NOT be called for targeted summon in cluster mode")
	}
	if !strings.Contains(err.Error(), "spire ready spi-abc123") {
		t.Fatalf("error must redirect to `spire ready spi-abc123`, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not supported in cluster mode") {
		t.Fatalf("error must explain cluster-mode incompatibility, got: %v", err)
	}
}

// TestCmdSummon_K8sTargetedRejected_MultipleIDs verifies that all
// passed-in target IDs appear in the redirect so the operator can
// re-run the right ready command.
func TestCmdSummon_K8sTargetedRejected_MultipleIDs(t *testing.T) {
	prevResolve := wizardResolveRepoForSummon
	wizardResolveRepoForSummon = func(beadID string) (string, string, string, error) {
		return "/tmp/fake", "git@example/fake.git", "main", nil
	}
	t.Cleanup(func() { wizardResolveRepoForSummon = prevResolve })

	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "remote", DeploymentMode: config.DeploymentModeClusterNative}, nil
	}

	origK8s := isK8sAvailableFunc
	defer func() { isK8sAvailableFunc = origK8s }()
	isK8sAvailableFunc = func() bool { return true }

	origK8sFn := summonK8sFunc
	defer func() { summonK8sFunc = origK8sFn }()
	summonK8sFunc = func(count int) error {
		t.Fatalf("summonK8s must NOT be called with %d, got call", count)
		return nil
	}

	err := cmdSummon([]string{"spi-aaa", "spi-bbb"})
	if err == nil {
		t.Fatal("expected error for targeted summon in cluster-native mode")
	}
	for _, id := range []string{"spi-aaa", "spi-bbb"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error must list bead %q, got: %v", id, err)
		}
	}
}

// TestCmdSummon_K8sCountStillWorks verifies that a bare numeric count
// in cluster-native mode still routes to summonK8s — the targeted
// guard is scoped to len(targetIDs) > 0, not "all summons in cluster
// mode".
func TestCmdSummon_K8sCountStillWorks(t *testing.T) {
	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "remote", DeploymentMode: config.DeploymentModeClusterNative}, nil
	}

	origK8s := isK8sAvailableFunc
	defer func() { isK8sAvailableFunc = origK8s }()
	isK8sAvailableFunc = func() bool { return true }

	origK8sFn := summonK8sFunc
	defer func() { summonK8sFunc = origK8sFn }()
	var gotCount int
	summonK8sFunc = func(count int) error {
		gotCount = count
		return nil
	}

	if err := cmdSummon([]string{"3"}); err != nil {
		t.Fatalf("bare count in cluster-native mode should succeed, got: %v", err)
	}
	if gotCount != 3 {
		t.Fatalf("expected summonK8s to be called with count=3, got %d", gotCount)
	}
}

// TestCmdSummon_LocalNative_IgnoresK8sReachability is the central
// regression test for spi-jsxa3v: a local-native tower with kubectl
// reporting the spire namespace as reachable must STILL take the
// local path. Before the fix, isK8sAvailable() returning true would
// silently route to the k8s path regardless of the tower's mode.
//
// We can't observe summonLocal directly from cmdSummon without
// spawning a subprocess, but we can pin the negative: if the dispatch
// had taken the k8s path it would have called summonK8s, which never
// touches BEADS_DIR or the local store. Taking the local path means
// the failure mode is store-related, not k8s-related. We assert the
// error doesn't look like a k8s reachability complaint.
func TestCmdSummon_LocalNative_IgnoresK8sReachability(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("BEADS_DIR", tmp)
	// Isolate SPIRE_CONFIG_DIR for the same spi-od41sr reason as the
	// gateway dismiss test: cmdSummon → summonLocal reads the wizard
	// registry and scans the runtime directory under ~/.config/spire/.
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())
	resetStore()

	origTower := activeTowerConfigFunc
	defer func() { activeTowerConfigFunc = origTower }()
	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "local", DeploymentMode: config.DeploymentModeLocalNative}, nil
	}

	// Simulate the original symptom: kubectl reachable to a cluster
	// with the spire namespace. Before the fix this silently switched
	// to the k8s path. After the fix, a local-native tower means the
	// local path runs regardless.
	origK8s := isK8sAvailableFunc
	defer func() { isK8sAvailableFunc = origK8s }()
	isK8sAvailableFunc = func() bool { return true }

	// summonLocal will fail downstream (no dolt server); we just
	// verify the failure path is the local one. Any error mentioning
	// cluster-native / kubectl indicates we mistakenly took the k8s
	// branch.
	err := cmdSummon([]string{"1"})
	if err != nil {
		if strings.Contains(err.Error(), "cluster-native") {
			t.Fatalf("local-native tower should not produce cluster-native error: %v", err)
		}
		if strings.Contains(err.Error(), "kubectl") {
			t.Fatalf("local-native tower should not produce kubectl error: %v", err)
		}
	}
}
