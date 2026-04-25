// Package conformance provides a reusable test suite that any
// [wizardregistry.Registry] implementation must pass.
//
// Backends invoke [Run] from a `_test.go` file in their own package,
// passing a [Factory] that returns a fresh registry and matching
// [Control]. The suite drives the registry through the public
// [wizardregistry.Registry] surface only — it never touches backend
// internals — and the [Control] indirection is the single seam by which
// each backend exposes its authoritative-source liveness toggle.
//
// Read-only backends (those that return
// [wizardregistry.ErrReadOnly] from Upsert/Remove) are accommodated:
// individual cases that require write access [testing.T.Skip] when the
// backend reports read-only, so the same suite is the contract for
// every backend regardless of write discipline.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/wizardregistry"
)

// Factory builds a fresh registry plus its matching Control for one
// test case. Implementations MUST return a fully-isolated registry on
// each invocation (no shared state between cases) so individual cases
// cannot interfere with one another.
type Factory func(t *testing.T) (registry wizardregistry.Registry, ctl Control)

// Control is the seam test cases use to manipulate the backend's
// authoritative-source view of liveness. Each backend brings its own
// implementation: the fake flips an in-memory map; the local backend
// would manage real PIDs; the cluster backend would manage k8s pod
// phase via a fake client.
type Control interface {
	// SetAlive toggles the backend's authoritative-source view of
	// whether the wizard with the given ID is alive.
	//
	// Setting alive == true on an ID that has not been Upserted is
	// permitted: it mirrors how a real authoritative source can have
	// a process exist before it appears in the registry.
	SetAlive(id string, alive bool)
}

// Run executes the full conformance suite against the registry built
// by factory.
//
// Each case calls factory(t) to obtain a fresh registry/control pair,
// so cases run independently.
func Run(t *testing.T, factory Factory) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"UpsertGetList", testUpsertGetList},
		{"Remove", testRemove},
		{"IsAliveTracksControl", testIsAliveTracksControl},
		{"IsAliveMissing", testIsAliveMissing},
		{"SweepReturnsOnlyDead", testSweepReturnsOnlyDead},
		{"SweepEmptyWhenAllAlive", testSweepEmptyWhenAllAlive},
		{"SweepRaceFreshUpsertNotMisclassified", testSweepRaceFreshUpsertNotMisclassified},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { c.fn(t, factory) })
	}
}

func testUpsertGetList(t *testing.T, factory Factory) {
	ctx := context.Background()
	reg, ctl := factory(t)
	_ = ctl

	local := wizardregistry.Wizard{
		ID:        "wizard-local",
		Mode:      wizardregistry.ModeLocal,
		PID:       1234,
		BeadID:    "spi-aaa",
		StartedAt: time.Unix(1700000000, 0),
	}
	cluster := wizardregistry.Wizard{
		ID:        "wizard-cluster",
		Mode:      wizardregistry.ModeCluster,
		PodName:   "wizard-spi-bbb-w1-0",
		Namespace: "spire",
		BeadID:    "spi-bbb",
		StartedAt: time.Unix(1700000100, 0),
	}

	if err := reg.Upsert(ctx, local); err != nil {
		if errors.Is(err, wizardregistry.ErrReadOnly) {
			t.Skip("backend is read-only")
		}
		t.Fatalf("Upsert local: %v", err)
	}
	if err := reg.Upsert(ctx, cluster); err != nil {
		t.Fatalf("Upsert cluster: %v", err)
	}

	got, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List len = %d, want 2: %+v", len(got), got)
	}
	byID := make(map[string]wizardregistry.Wizard, len(got))
	for _, w := range got {
		byID[w.ID] = w
	}
	if _, ok := byID[local.ID]; !ok {
		t.Errorf("List missing %q", local.ID)
	}
	if _, ok := byID[cluster.ID]; !ok {
		t.Errorf("List missing %q", cluster.ID)
	}

	gotLocal, err := reg.Get(ctx, local.ID)
	if err != nil {
		t.Fatalf("Get %q: %v", local.ID, err)
	}
	if gotLocal.ID != local.ID || gotLocal.Mode != wizardregistry.ModeLocal || gotLocal.PID != 1234 {
		t.Errorf("Get %q = %+v, want local with PID=1234", local.ID, gotLocal)
	}

	gotCluster, err := reg.Get(ctx, cluster.ID)
	if err != nil {
		t.Fatalf("Get %q: %v", cluster.ID, err)
	}
	if gotCluster.ID != cluster.ID || gotCluster.Mode != wizardregistry.ModeCluster || gotCluster.PodName != "wizard-spi-bbb-w1-0" {
		t.Errorf("Get %q = %+v, want cluster with PodName=wizard-spi-bbb-w1-0", cluster.ID, gotCluster)
	}

	if _, err := reg.Get(ctx, "missing"); !errors.Is(err, wizardregistry.ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func testRemove(t *testing.T, factory Factory) {
	ctx := context.Background()
	reg, ctl := factory(t)
	_ = ctl

	w := wizardregistry.Wizard{
		ID:     "wizard-removable",
		Mode:   wizardregistry.ModeLocal,
		PID:    4242,
		BeadID: "spi-rm",
	}

	if err := reg.Upsert(ctx, w); err != nil {
		if errors.Is(err, wizardregistry.ErrReadOnly) {
			t.Skip("backend is read-only")
		}
		t.Fatalf("Upsert: %v", err)
	}

	if err := reg.Remove(ctx, w.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := reg.Get(ctx, w.ID); !errors.Is(err, wizardregistry.ErrNotFound) {
		t.Errorf("Get after Remove: error = %v, want ErrNotFound", err)
	}

	if err := reg.Remove(ctx, w.ID); !errors.Is(err, wizardregistry.ErrNotFound) {
		t.Errorf("second Remove: error = %v, want ErrNotFound", err)
	}
}

func testIsAliveTracksControl(t *testing.T, factory Factory) {
	ctx := context.Background()
	reg, ctl := factory(t)

	w := wizardregistry.Wizard{
		ID:     "wizard-alive",
		Mode:   wizardregistry.ModeLocal,
		PID:    5555,
		BeadID: "spi-alive",
	}

	if err := reg.Upsert(ctx, w); err != nil {
		if errors.Is(err, wizardregistry.ErrReadOnly) {
			t.Skip("backend is read-only")
		}
		t.Fatalf("Upsert: %v", err)
	}

	ctl.SetAlive(w.ID, true)
	alive, err := reg.IsAlive(ctx, w.ID)
	if err != nil {
		t.Fatalf("IsAlive (alive): %v", err)
	}
	if !alive {
		t.Errorf("IsAlive after SetAlive(true) = false, want true")
	}

	ctl.SetAlive(w.ID, false)
	alive, err = reg.IsAlive(ctx, w.ID)
	if err != nil {
		t.Fatalf("IsAlive (dead): %v", err)
	}
	if alive {
		t.Errorf("IsAlive after SetAlive(false) = true, want false (no cross-call caching)")
	}

	ctl.SetAlive(w.ID, true)
	alive, err = reg.IsAlive(ctx, w.ID)
	if err != nil {
		t.Fatalf("IsAlive (alive again): %v", err)
	}
	if !alive {
		t.Errorf("IsAlive after second SetAlive(true) = false, want true (no cross-call caching)")
	}
}

func testIsAliveMissing(t *testing.T, factory Factory) {
	ctx := context.Background()
	reg, _ := factory(t)

	alive, err := reg.IsAlive(ctx, "never-registered")
	if !errors.Is(err, wizardregistry.ErrNotFound) {
		t.Errorf("IsAlive(missing) error = %v, want ErrNotFound", err)
	}
	if alive {
		t.Errorf("IsAlive(missing) = true, want false")
	}
}

func testSweepReturnsOnlyDead(t *testing.T, factory Factory) {
	ctx := context.Background()
	reg, ctl := factory(t)

	wizards := []wizardregistry.Wizard{
		{ID: "A", Mode: wizardregistry.ModeLocal, PID: 1, BeadID: "spi-a"},
		{ID: "B", Mode: wizardregistry.ModeLocal, PID: 2, BeadID: "spi-b"},
		{ID: "C", Mode: wizardregistry.ModeLocal, PID: 3, BeadID: "spi-c"},
	}
	for _, w := range wizards {
		if err := reg.Upsert(ctx, w); err != nil {
			if errors.Is(err, wizardregistry.ErrReadOnly) {
				t.Skip("backend is read-only")
			}
			t.Fatalf("Upsert %q: %v", w.ID, err)
		}
	}

	ctl.SetAlive("A", true)
	ctl.SetAlive("B", false)
	ctl.SetAlive("C", true)

	dead, err := reg.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(dead) != 1 || dead[0].ID != "B" {
		ids := make([]string, len(dead))
		for i, w := range dead {
			ids[i] = w.ID
		}
		t.Errorf("Sweep dead = %v, want [B]", ids)
	}

	got, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("List after Sweep: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("List after Sweep len = %d, want 3 (Sweep MUST NOT mutate)", len(got))
	}
}

func testSweepEmptyWhenAllAlive(t *testing.T, factory Factory) {
	ctx := context.Background()
	reg, ctl := factory(t)

	wizards := []wizardregistry.Wizard{
		{ID: "X", Mode: wizardregistry.ModeLocal, PID: 10, BeadID: "spi-x"},
		{ID: "Y", Mode: wizardregistry.ModeLocal, PID: 20, BeadID: "spi-y"},
	}
	for _, w := range wizards {
		if err := reg.Upsert(ctx, w); err != nil {
			if errors.Is(err, wizardregistry.ErrReadOnly) {
				t.Skip("backend is read-only")
			}
			t.Fatalf("Upsert %q: %v", w.ID, err)
		}
		ctl.SetAlive(w.ID, true)
	}

	dead, err := reg.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(dead) != 0 {
		ids := make([]string, len(dead))
		for i, w := range dead {
			ids[i] = w.ID
		}
		t.Errorf("Sweep dead = %v, want []", ids)
	}
}

// testSweepRaceFreshUpsertNotMisclassified is the load-bearing race
// test for the [Registry] contract.
//
// Run under `-race`. A backend that snapshots the wizard set before
// per-entry liveness checks will mis-classify B_i and fail this test.
//
// Scenario: a pre-seeded entry A is registered alive. N goroutines
// each upsert their own unique B_i alive and immediately call Sweep.
// A compliant backend MUST NOT include B_i in B_i's own Sweep result,
// because B_i was alive at the moment Sweep evaluated it. A backend
// that lists entries first and then evaluates liveness against a stale
// snapshot will see B_i as "missing from the live set" and report it
// as dead — that is the bug this test is designed to catch.
func testSweepRaceFreshUpsertNotMisclassified(t *testing.T, factory Factory) {
	ctx := context.Background()
	reg, ctl := factory(t)

	seed := wizardregistry.Wizard{
		ID:     "A",
		Mode:   wizardregistry.ModeLocal,
		PID:    1,
		BeadID: "spi-seed",
	}
	if err := reg.Upsert(ctx, seed); err != nil {
		if errors.Is(err, wizardregistry.ErrReadOnly) {
			t.Skip("backend is read-only")
		}
		t.Fatalf("Upsert seed: %v", err)
	}
	ctl.SetAlive(seed.ID, true)

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("B_%d", i)
			w := wizardregistry.Wizard{
				ID:     id,
				Mode:   wizardregistry.ModeLocal,
				PID:    1000 + i,
				BeadID: "spi-race-" + id,
			}
			if err := reg.Upsert(ctx, w); err != nil {
				t.Errorf("Upsert %q: %v", id, err)
				return
			}
			ctl.SetAlive(id, true)

			dead, err := reg.Sweep(ctx)
			if err != nil {
				t.Errorf("Sweep for %q: %v", id, err)
				return
			}
			for _, d := range dead {
				if d.ID == id {
					t.Errorf("Sweep mis-classified fresh upsert %q as dead", id)
					return
				}
			}
		}()
	}
	wg.Wait()
}
