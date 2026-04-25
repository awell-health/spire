package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/process"
	"github.com/awell-health/spire/pkg/wizardregistry"
)

// TestIsAlive_ZombiePID asserts a zombie (defunct) PID is classified
// dead by the Local registry — same fix as spi-k2bz93 but in the new
// package. The bug it closes: kill -0 alone reports zombies as alive
// because the kernel keeps the process entry until the parent reaps
// it; that masked dead wizards as alive on the board and in the orphan
// sweep.
func TestIsAlive_ZombiePID(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("zombie detection only implemented on linux/darwin (have %s)", runtime.GOOS)
	}

	// Spawn a child that exits immediately. Don't reap it (no Wait
	// until cleanup) so the kernel parks it in zombie state.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Wait() }()

	// Poll until the child enters zombie state. Cap at ~3s.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("child reaped before zombie was observed: %v", err)
		}
		if !process.ProcessAlive(pid) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if process.ProcessAlive(pid) {
		t.Fatalf("expected pid %d in zombie state within deadline", pid)
	}

	// Wire the zombie PID through the registry. Local.IsAlive must
	// consult process.ProcessAlive — not raw syscall.Kill — so the
	// answer is "dead" even though kill -0 succeeds against zombies.
	l, err := New(filepath.Join(t.TempDir(), "wizards.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	w := wizardregistry.Wizard{
		ID:     "wizard-zombie",
		Mode:   wizardregistry.ModeLocal,
		PID:    pid,
		BeadID: "spi-zombie",
	}
	if err := l.Upsert(ctx, w); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	alive, err := l.IsAlive(ctx, w.ID)
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if alive {
		t.Fatalf("IsAlive(%s) = true for zombie pid %d; want false", w.ID, pid)
	}

	// Sweep should also classify the zombie as dead, by the same
	// path. Belt-and-suspenders: it would be a real footgun if
	// Sweep took a different code path that bypassed ProcessAlive.
	dead, err := l.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	zombieFound := false
	for _, d := range dead {
		if d.ID == w.ID {
			zombieFound = true
			break
		}
	}
	if !zombieFound {
		t.Fatalf("Sweep did not include zombie wizard %q in dead set", w.ID)
	}
}

// TestIsAlive_LivePID is the sanity check that pairs with the zombie
// test: a live, non-zombie process must still report alive. Without
// this the zombie tightening could silently flip every PID dead and
// we'd never notice.
func TestIsAlive_LivePID(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	l, err := New(filepath.Join(t.TempDir(), "wizards.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	w := wizardregistry.Wizard{
		ID:     "wizard-live",
		Mode:   wizardregistry.ModeLocal,
		PID:    cmd.Process.Pid,
		BeadID: "spi-live",
	}
	if err := l.Upsert(ctx, w); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	alive, err := l.IsAlive(ctx, w.ID)
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if !alive {
		t.Fatalf("IsAlive(%s) = false for live pid %d", w.ID, cmd.Process.Pid)
	}
}

// TestSweep_ConcurrentUpsertNotMisclassified is the spi-5bzu9r
// regression test for race-safety of Sweep.
//
// Scenario: a goroutine inserts a fresh wizard (with control.alive
// flipped to true after Upsert) and then runs Sweep. A buggy backend
// that lists entries first and then probes liveness against a stale
// snapshot would mis-classify the fresh wizard as dead. The lock
// pattern in Local.Sweep prevents this by construction.
//
// The test layers a control-driven probe on top of Local so that the
// arbitrary test PIDs (10000+i) can have deterministic alive flags.
func TestSweep_ConcurrentUpsertNotMisclassified(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "wizards.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	var aliveMu sync.Mutex
	aliveByID := map[string]bool{}
	setAlive := func(id string, alive bool) {
		aliveMu.Lock()
		aliveByID[id] = alive
		aliveMu.Unlock()
	}
	l.probe = func(w wizardregistry.Wizard) bool {
		aliveMu.Lock()
		defer aliveMu.Unlock()
		return aliveByID[w.ID]
	}

	// Pre-seed with a live entry so Sweep has work besides the fresh
	// upserts: tests that Sweep iterates more than the freshly-added
	// entry without breaking.
	seed := wizardregistry.Wizard{
		ID:     "seed",
		Mode:   wizardregistry.ModeLocal,
		PID:    1,
		BeadID: "spi-seed",
	}
	if err := l.Upsert(ctx, seed); err != nil {
		t.Fatalf("Upsert seed: %v", err)
	}
	setAlive(seed.ID, true)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("fresh-%d", i)
			w := wizardregistry.Wizard{
				ID:     id,
				Mode:   wizardregistry.ModeLocal,
				PID:    10000 + i,
				BeadID: "spi-fresh",
			}
			if err := l.Upsert(ctx, w); err != nil {
				t.Errorf("Upsert %q: %v", id, err)
				return
			}
			setAlive(id, true)
			dead, err := l.Sweep(ctx)
			if err != nil {
				t.Errorf("Sweep %q: %v", id, err)
				return
			}
			for _, d := range dead {
				if d.ID == id {
					t.Errorf("Sweep mis-classified fresh upsert %q as dead — spi-5bzu9r race regression", id)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestBeginWorkRace_FreshAttemptStaysAlive reproduces the spi-5bzu9r
// incident scenario at the registry layer: a steward orphan sweep
// running concurrently with a BeginWork that registers a fresh wizard
// MUST NOT classify the fresh wizard as dead.
//
// Symptom of the original bug: parent bead reverted from in_progress
// to open, the fresh attempt was closed-orphaned, and the wizard
// process was alive throughout. The registry-level fix here is the
// foundation — .3 will refactor OrphanSweep to consume Registry.
func TestBeginWorkRace_FreshAttemptStaysAlive(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "wizards.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	livePID := os.Getpid()

	// Pre-seed with the existing wizard W (the test process itself,
	// guaranteed alive).
	seed := wizardregistry.Wizard{
		ID:     "wizard-W",
		Mode:   wizardregistry.ModeLocal,
		PID:    livePID,
		BeadID: "spi-W",
	}
	if err := l.Upsert(ctx, seed); err != nil {
		t.Fatalf("Upsert seed: %v", err)
	}

	const iters = 50
	for i := 0; i < iters; i++ {
		freshID := fmt.Sprintf("fresh-%d", i)
		var wg sync.WaitGroup
		var sweepResult []wizardregistry.Wizard
		var sweepErr error

		wg.Add(2)
		// Steward sweep goroutine: simulates the OrphanSweep
		// scanner that lists registered wizards and would close
		// attempts for any classified dead.
		go func() {
			defer wg.Done()
			sweepResult, sweepErr = l.Sweep(ctx)
		}()
		// BeginWork goroutine: simulates the wizard registering a
		// fresh attempt during the steward's sweep window.
		go func() {
			defer wg.Done()
			err := l.Upsert(ctx, wizardregistry.Wizard{
				ID:     freshID,
				Mode:   wizardregistry.ModeLocal,
				PID:    livePID,
				BeadID: "spi-fresh",
			})
			if err != nil {
				t.Errorf("Upsert fresh %q: %v", freshID, err)
			}
		}()
		wg.Wait()

		if sweepErr != nil {
			t.Fatalf("iteration %d: Sweep: %v", i, sweepErr)
		}
		// The fresh wizard MUST NOT appear in dead. With the
		// race-safe lock, Upsert and Sweep serialize: either Sweep
		// observed an empty (or seed-only) registry, or it
		// observed the fresh entry and probed it alive. Both
		// outcomes leave fresh out of dead.
		for _, d := range sweepResult {
			if d.ID == freshID {
				t.Fatalf("iteration %d: spi-5bzu9r race regression — fresh attempt %q mis-classified as dead by concurrent Sweep", i, freshID)
			}
		}
	}
}

// TestUpsert_NewAndReplace covers the basic insert-then-replace path
// the original pkg/registry tested. Sanity check that the port didn't
// drop the ID-keyed replace semantics.
func TestUpsert_NewAndReplace(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "wizards.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	first := wizardregistry.Wizard{
		ID:     "wizard-spi-aaa",
		Mode:   wizardregistry.ModeLocal,
		PID:    1001,
		BeadID: "spi-aaa",
	}
	if err := l.Upsert(ctx, first); err != nil {
		t.Fatalf("Upsert first: %v", err)
	}

	// Replace with same ID, different PID + BeadID.
	second := wizardregistry.Wizard{
		ID:     "wizard-spi-aaa",
		Mode:   wizardregistry.ModeLocal,
		PID:    1002,
		BeadID: "spi-aaa-v2",
	}
	if err := l.Upsert(ctx, second); err != nil {
		t.Fatalf("Upsert replace: %v", err)
	}

	got, err := l.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List len = %d after replace, want 1", len(got))
	}
	if got[0].PID != 1002 || got[0].BeadID != "spi-aaa-v2" {
		t.Errorf("Replace did not overwrite fields: got %+v", got[0])
	}

	// Distinct ID inserts a separate entry.
	third := wizardregistry.Wizard{
		ID:     "wizard-spi-bbb",
		Mode:   wizardregistry.ModeLocal,
		PID:    2001,
		BeadID: "spi-bbb",
	}
	if err := l.Upsert(ctx, third); err != nil {
		t.Fatalf("Upsert third: %v", err)
	}
	got, err = l.List(ctx)
	if err != nil {
		t.Fatalf("List after third: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List len = %d, want 2", len(got))
	}
}

// TestRemove_NotFoundError covers the Remove contract: missing IDs
// MUST return ErrNotFound (the contract differs from pkg/registry's
// idempotent Remove — that was the old behavior).
func TestRemove_NotFoundError(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "wizards.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	if err := l.Remove(ctx, "no-such-wizard"); !errors.Is(err, wizardregistry.ErrNotFound) {
		t.Fatalf("Remove missing: err = %v, want ErrNotFound", err)
	}

	w := wizardregistry.Wizard{
		ID:     "wizard-rm",
		Mode:   wizardregistry.ModeLocal,
		PID:    3001,
		BeadID: "spi-rm",
	}
	if err := l.Upsert(ctx, w); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := l.Remove(ctx, w.ID); err != nil {
		t.Fatalf("Remove existing: %v", err)
	}
	if err := l.Remove(ctx, w.ID); !errors.Is(err, wizardregistry.ErrNotFound) {
		t.Fatalf("second Remove: err = %v, want ErrNotFound", err)
	}
}

// TestPersistsAcrossInstances confirms the file-based store survives
// process boundaries: a second Local pointed at the same path sees the
// entries written by the first.
func TestPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wizards.json")

	a, err := New(path)
	if err != nil {
		t.Fatalf("New a: %v", err)
	}
	ctx := context.Background()
	w := wizardregistry.Wizard{
		ID:        "wizard-persist",
		Mode:      wizardregistry.ModeLocal,
		PID:       4242,
		BeadID:    "spi-persist",
		StartedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := a.Upsert(ctx, w); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	b, err := New(path)
	if err != nil {
		t.Fatalf("New b: %v", err)
	}
	got, err := b.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("Get from second instance: %v", err)
	}
	if got.ID != w.ID || got.PID != w.PID || got.BeadID != w.BeadID {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, w)
	}
}
