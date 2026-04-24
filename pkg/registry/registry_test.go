package registry

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// overrideRegistryFile sets the registry file path to a temp location for the
// duration of the test via the SPIRE_CONFIG_DIR env var, which config.Dir()
// respects.
func overrideRegistryFile(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
}

func TestUpsert_NewAndReplace(t *testing.T) {
	overrideRegistryFile(t)

	// Insert a new entry.
	e1 := Entry{Name: "wizard-spi-aaa", PID: 1001, BeadID: "spi-aaa"}
	if err := Upsert(e1); err != nil {
		t.Fatalf("Upsert new: %v", err)
	}

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].BeadID != "spi-aaa" {
		t.Fatalf("expected BeadID spi-aaa, got %q", entries[0].BeadID)
	}

	// Replace the entry with the same Name but different PID.
	e2 := Entry{Name: "wizard-spi-aaa", PID: 1002, BeadID: "spi-aaa", Phase: "implement"}
	if err := Upsert(e2); err != nil {
		t.Fatalf("Upsert replace: %v", err)
	}

	entries, err = List()
	if err != nil {
		t.Fatalf("List after replace: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after replace, got %d", len(entries))
	}
	if entries[0].PID != 1002 {
		t.Fatalf("expected PID 1002 after replace, got %d", entries[0].PID)
	}
	if entries[0].Phase != "implement" {
		t.Fatalf("expected Phase 'implement' after replace, got %q", entries[0].Phase)
	}

	// Insert a second distinct entry.
	e3 := Entry{Name: "wizard-spi-bbb", PID: 2001, BeadID: "spi-bbb"}
	if err := Upsert(e3); err != nil {
		t.Fatalf("Upsert second: %v", err)
	}
	entries, err = List()
	if err != nil {
		t.Fatalf("List after second: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestRemove_Idempotent(t *testing.T) {
	overrideRegistryFile(t)

	// Remove a nonexistent entry — must return nil.
	if err := Remove("nonexistent"); err != nil {
		t.Fatalf("Remove nonexistent should be nil, got: %v", err)
	}

	// Add one entry, then remove it.
	e := Entry{Name: "wizard-spi-ccc", PID: 3001, BeadID: "spi-ccc"}
	if err := Upsert(e); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := Remove("wizard-spi-ccc"); err != nil {
		t.Fatalf("Remove existing: %v", err)
	}

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries after remove, got %d", len(entries))
	}

	// Remove again — still nil.
	if err := Remove("wizard-spi-ccc"); err != nil {
		t.Fatalf("Remove again should be nil, got: %v", err)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	overrideRegistryFile(t)

	err := Update("does-not-exist", func(e *Entry) {
		e.Phase = "implement"
	})
	if err == nil {
		t.Fatal("expected error for Update on nonexistent entry, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestUpdate_Found(t *testing.T) {
	overrideRegistryFile(t)

	e := Entry{Name: "wizard-spi-ddd", PID: 4001, BeadID: "spi-ddd", Phase: "init"}
	if err := Upsert(e); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := Update("wizard-spi-ddd", func(e *Entry) {
		e.Phase = "implement"
		e.PhaseStartedAt = "2026-01-01T00:00:00Z"
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Phase != "implement" {
		t.Fatalf("expected Phase 'implement', got %q", entries[0].Phase)
	}
	if entries[0].PhaseStartedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("expected PhaseStartedAt set, got %q", entries[0].PhaseStartedAt)
	}
}

func TestSweep_LivePIDs(t *testing.T) {
	overrideRegistryFile(t)

	// Stub pidProbe to always return true (alive).
	orig := pidProbe
	pidProbe = func(pid int) bool { return true }
	defer func() { pidProbe = orig }()

	if err := Upsert(Entry{Name: "w1", PID: 9991, BeadID: "spi-1"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := Upsert(Entry{Name: "w2", PID: 9992, BeadID: "spi-2"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	dead, err := Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("expected 0 dead entries when all PIDs alive, got %d", len(dead))
	}
}

func TestSweep_DeadPIDs(t *testing.T) {
	overrideRegistryFile(t)

	// Stub pidProbe to always return false (dead).
	orig := pidProbe
	pidProbe = func(pid int) bool { return false }
	defer func() { pidProbe = orig }()

	e1 := Entry{Name: "w-dead-1", PID: 8881, BeadID: "spi-x"}
	e2 := Entry{Name: "w-dead-2", PID: 8882, BeadID: "spi-y"}
	if err := Upsert(e1); err != nil {
		t.Fatalf("Upsert e1: %v", err)
	}
	if err := Upsert(e2); err != nil {
		t.Fatalf("Upsert e2: %v", err)
	}

	dead, err := Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(dead) != 2 {
		t.Fatalf("expected 2 dead entries, got %d", len(dead))
	}

	// Verify Sweep did NOT remove the entries from the registry.
	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Sweep must not remove entries: expected 2, got %d", len(entries))
	}
}

func TestFileLock_Contention(t *testing.T) {
	overrideRegistryFile(t)

	const goroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := Entry{
				Name:   fmt.Sprintf("wizard-%d", i),
				PID:    10000 + i,
				BeadID: fmt.Sprintf("spi-%d", i),
			}
			if err := Upsert(e); err != nil {
				errs <- fmt.Errorf("goroutine %d Upsert: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != goroutines {
		t.Fatalf("expected %d entries after concurrent upserts, got %d", goroutines, len(entries))
	}
}
