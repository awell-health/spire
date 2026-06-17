package executor

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
)

// These tests pin the dispatch idempotency guard (ErrDuplicateApprentice):
// when a child bead already has a LIVE apprentice, the dispatch seam must
// refuse to spawn a duplicate and fail loud so the formula step parks instead
// of advancing the epic over an in-flight child. They exercise the
// sleep-induced duplicate-dispatch fix.
//
// The guard keys strictly on Info.Alive==true; a dead/orphaned registry entry
// (the common post-sleep state) must NOT suppress a needed re-spawn — see
// TestDispatch_DeadEntryDoesNotSuppressSpawn, the load-bearing regression test.

func depsForGuard(backend agent.Backend) *Deps {
	return &Deps{
		Spawner:        backend,
		MaxApprentices: 4,
		UpdateBead:     func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch:  func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		CloseBead: func(id string) error { return nil },
	}
}

// TestRunDispatchWave_SkipsLiveDuplicate: a live apprentice already owns the
// child → no spawn, and dispatchWaveCore returns ErrDuplicateApprentice.
func TestRunDispatchWave_SkipsLiveDuplicate(t *testing.T) {
	stagingWt, _ := initEagerCloseTestRepo(t, []string{"spi-epic.1"})
	backend := &concurrentBackend{
		sleepPerJob: time.Millisecond,
		listInfos:   []agent.Info{{BeadID: "spi-epic.1", Alive: true}},
	}
	e := NewForTest("spi-epic", "wizard-test", nil, depsForGuard(backend))
	resolver := func(string, string) error { return nil }

	_, err := e.dispatchWaveCore([][]string{{"spi-epic.1"}}, stagingWt, "claude-sonnet-4-6", resolver, 1)
	if !errors.Is(err, ErrDuplicateApprentice) {
		t.Fatalf("err = %v, want ErrDuplicateApprentice", err)
	}
	if got := atomic.LoadInt32(&backend.spawnCount); got != 0 {
		t.Errorf("spawnCount = %d, want 0 (must not spawn a duplicate)", got)
	}
}

// TestDispatch_DeadEntryDoesNotSuppressSpawn: the registry still lists the
// child but its process is DEAD (Alive=false) — the post-sleep orphan state.
// The guard must NOT match, so a fresh apprentice spawns normally. This is the
// regression guard against turning the dup-fix into a stranding bug.
func TestDispatch_DeadEntryDoesNotSuppressSpawn(t *testing.T) {
	stagingWt, _ := initEagerCloseTestRepo(t, []string{"spi-epic.1"})
	backend := &concurrentBackend{
		sleepPerJob: time.Millisecond,
		listInfos:   []agent.Info{{BeadID: "spi-epic.1", Alive: false}},
	}
	e := NewForTest("spi-epic", "wizard-test", nil, depsForGuard(backend))
	resolver := func(string, string) error { return nil }

	if _, err := e.dispatchWaveCore([][]string{{"spi-epic.1"}}, stagingWt, "claude-sonnet-4-6", resolver, 1); err != nil {
		t.Fatalf("dispatchWaveCore: %v", err)
	}
	if got := atomic.LoadInt32(&backend.spawnCount); got != 1 {
		t.Errorf("spawnCount = %d, want 1 (dead orphan entry must not suppress spawn)", got)
	}
}

// TestRunDispatchWave_MixedWave_SkipsOnlyLive: in a wave of two, only the live
// child is skipped; the other spawns. The wave still fails loud because a
// duplicate was detected.
func TestRunDispatchWave_MixedWave_SkipsOnlyLive(t *testing.T) {
	stagingWt, _ := initEagerCloseTestRepo(t, []string{"spi-epic.1", "spi-epic.2"})
	backend := &concurrentBackend{
		sleepPerJob: time.Millisecond,
		listInfos:   []agent.Info{{BeadID: "spi-epic.1", Alive: true}},
	}
	e := NewForTest("spi-epic", "wizard-test", nil, depsForGuard(backend))
	resolver := func(string, string) error { return nil }

	_, err := e.dispatchWaveCore([][]string{{"spi-epic.1", "spi-epic.2"}}, stagingWt, "claude-sonnet-4-6", resolver, 2)
	if !errors.Is(err, ErrDuplicateApprentice) {
		t.Fatalf("err = %v, want ErrDuplicateApprentice", err)
	}
	// Exactly the non-live child spawns.
	if got := atomic.LoadInt32(&backend.spawnCount); got != 1 {
		t.Errorf("spawnCount = %d, want 1 (only the non-live child spawns)", got)
	}
}

// TestRunDispatchWave_ListError_FailsOpen: a List() error must not block
// dispatch — the guard fails open and the apprentice spawns.
func TestRunDispatchWave_ListError_FailsOpen(t *testing.T) {
	stagingWt, _ := initEagerCloseTestRepo(t, []string{"spi-epic.1"})
	backend := &concurrentBackend{
		sleepPerJob: time.Millisecond,
		listErr:     errors.New("registry read blip"),
	}
	e := NewForTest("spi-epic", "wizard-test", nil, depsForGuard(backend))
	resolver := func(string, string) error { return nil }

	if _, err := e.dispatchWaveCore([][]string{{"spi-epic.1"}}, stagingWt, "claude-sonnet-4-6", resolver, 1); err != nil {
		t.Fatalf("dispatchWaveCore: %v", err)
	}
	if got := atomic.LoadInt32(&backend.spawnCount); got != 1 {
		t.Errorf("spawnCount = %d, want 1 (List error must fail open)", got)
	}
}

// TestRunDispatchWave_ListOncePerWave: the live snapshot is taken once per
// wave, not once per child.
func TestRunDispatchWave_ListOncePerWave(t *testing.T) {
	stagingWt, _ := initEagerCloseTestRepo(t, []string{"spi-epic.1", "spi-epic.2", "spi-epic.3"})
	backend := &concurrentBackend{sleepPerJob: time.Millisecond}
	e := NewForTest("spi-epic", "wizard-test", nil, depsForGuard(backend))
	resolver := func(string, string) error { return nil }

	if _, err := e.dispatchWaveCore([][]string{{"spi-epic.1", "spi-epic.2", "spi-epic.3"}}, stagingWt, "claude-sonnet-4-6", resolver, 3); err != nil {
		t.Fatalf("dispatchWaveCore: %v", err)
	}
	if got := atomic.LoadInt32(&backend.listCount); got != 1 {
		t.Errorf("listCount = %d, want 1 (List once per wave)", got)
	}
}

// TestDispatchSequentialCore_SkipsLiveDuplicate mirrors the wave guard for the
// sequential path.
func TestDispatchSequentialCore_SkipsLiveDuplicate(t *testing.T) {
	stagingWt, _ := initEagerCloseTestRepo(t, []string{"spi-epic.1"})
	backend := &concurrentBackend{
		sleepPerJob: time.Millisecond,
		listInfos:   []agent.Info{{BeadID: "spi-epic.1", Alive: true}},
	}
	e := NewForTest("spi-epic", "wizard-test", nil, depsForGuard(backend))
	resolver := func(string, string) error { return nil }

	_, err := e.dispatchSequentialCore([]string{"spi-epic.1"}, stagingWt, "claude-sonnet-4-6", resolver)
	if !errors.Is(err, ErrDuplicateApprentice) {
		t.Fatalf("err = %v, want ErrDuplicateApprentice", err)
	}
	if got := atomic.LoadInt32(&backend.spawnCount); got != 0 {
		t.Errorf("spawnCount = %d, want 0", got)
	}
}

// TestDispatchDirectCore_SkipsLiveDuplicate mirrors the guard for the direct
// (single-apprentice) path, which spawns against the parent bead's own ID. The
// live entry here is an APPRENTICE (a name distinct from the wizard's own).
func TestDispatchDirectCore_SkipsLiveDuplicate(t *testing.T) {
	stagingWt, _ := initEagerCloseTestRepo(t, []string{"spi-task"})
	backend := &concurrentBackend{
		sleepPerJob: time.Millisecond,
		listInfos:   []agent.Info{{Name: "wizard-test-impl", BeadID: "spi-task", Alive: true}},
	}
	e := NewForTest("spi-task", "wizard-test", nil, depsForGuard(backend))
	resolver := func(string, string) error { return nil }

	err := e.dispatchDirectCore(stagingWt, "claude-sonnet-4-6", resolver)
	if !errors.Is(err, ErrDuplicateApprentice) {
		t.Fatalf("err = %v, want ErrDuplicateApprentice", err)
	}
	if got := atomic.LoadInt32(&backend.spawnCount); got != 0 {
		t.Errorf("spawnCount = %d, want 0", got)
	}
}

// TestDispatchDirectCore_WizardSelfEntryDoesNotSuppressSpawn is the load-bearing
// regression guard for the direct path: the apprentice is spawned under the
// wizard's OWN bead ID, and the wizard itself is registered in the same registry
// under that bead ID. The guard must exclude the wizard's own entry (matched by
// agent name) so a fresh apprentice still spawns — otherwise direct dispatch
// would refuse to spawn anything.
func TestDispatchDirectCore_WizardSelfEntryDoesNotSuppressSpawn(t *testing.T) {
	stagingWt, _ := initEagerCloseTestRepo(t, []string{"spi-task"})
	backend := &concurrentBackend{
		sleepPerJob: time.Millisecond,
		// The only live entry is the wizard itself (Name == e.agentName).
		listInfos: []agent.Info{{Name: "wizard-test", BeadID: "spi-task", Alive: true}},
	}
	e := NewForTest("spi-task", "wizard-test", nil, depsForGuard(backend))
	resolver := func(string, string) error { return nil }

	if err := e.dispatchDirectCore(stagingWt, "claude-sonnet-4-6", resolver); err != nil {
		t.Fatalf("dispatchDirectCore: %v (wizard self-entry must not suppress the apprentice spawn)", err)
	}
	if got := atomic.LoadInt32(&backend.spawnCount); got != 1 {
		t.Errorf("spawnCount = %d, want 1 (apprentice must spawn despite wizard self-entry)", got)
	}
}
