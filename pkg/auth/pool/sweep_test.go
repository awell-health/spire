package pool

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

// recordingWake is a PoolWake that captures every Broadcast call so
// tests can assert which pools were woken (and how many times). Wait
// is a no-op since Sweep never calls it.
type recordingWake struct {
	mu         sync.Mutex
	broadcasts []string
}

func (w *recordingWake) Wait(ctx context.Context, pool string) error {
	return nil
}

func (w *recordingWake) Broadcast(pool string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.broadcasts = append(w.broadcasts, pool)
	return nil
}

func (w *recordingWake) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.broadcasts))
	copy(out, w.broadcasts)
	sort.Strings(out)
	return out
}

// twoPoolConfig builds a Config covering one subscription slot ("sub-a")
// and one api-key slot ("key-a") so tests can exercise both branches of
// Sweep's slot->pool lookup with a single fixture.
func twoPoolConfig() *Config {
	return &Config{
		Subscription: []SlotConfig{{Name: "sub-a", MaxConcurrent: 4}},
		APIKey:       []SlotConfig{{Name: "key-a", MaxConcurrent: 4}},
	}
}

func TestSweep_RemovesStaleClaim(t *testing.T) {
	dir := t.TempDir()
	staleAge := 60 * time.Second
	now := time.Now()

	if err := WriteSlotState(dir, &SlotState{
		Slot: "sub-a",
		InFlight: []InFlightClaim{
			{DispatchID: "stale", ClaimedAt: now.Add(-2 * time.Minute), HeartbeatAt: now.Add(-2 * time.Minute)},
			{DispatchID: "fresh", ClaimedAt: now.Add(-10 * time.Second), HeartbeatAt: now.Add(-10 * time.Second)},
		},
	}); err != nil {
		t.Fatalf("seed slot: %v", err)
	}

	wake := &recordingWake{}
	removed, err := Sweep(dir, staleAge, wake, twoPoolConfig())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	got, err := ReadSlotState(dir, "sub-a")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if len(got.InFlight) != 1 || got.InFlight[0].DispatchID != "fresh" {
		t.Errorf("InFlight = %v, want only the fresh claim", got.InFlight)
	}

	if b := wake.snapshot(); len(b) != 1 || b[0] != "subscription" {
		t.Errorf("broadcasts = %v, want [subscription]", b)
	}
}

func TestSweep_PreservesFreshClaim(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	if err := WriteSlotState(dir, &SlotState{
		Slot: "sub-a",
		InFlight: []InFlightClaim{
			{DispatchID: "fresh-1", ClaimedAt: now, HeartbeatAt: now},
			{DispatchID: "fresh-2", ClaimedAt: now.Add(-5 * time.Second), HeartbeatAt: now.Add(-5 * time.Second)},
		},
	}); err != nil {
		t.Fatalf("seed slot: %v", err)
	}

	wake := &recordingWake{}
	removed, err := Sweep(dir, 60*time.Second, wake, twoPoolConfig())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}

	got, err := ReadSlotState(dir, "sub-a")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if len(got.InFlight) != 2 {
		t.Errorf("InFlight len = %d, want 2 (fresh claims should not be dropped)", len(got.InFlight))
	}

	if b := wake.snapshot(); len(b) != 0 {
		t.Errorf("broadcasts = %v, want none (no removals)", b)
	}
}

func TestSweep_MultipleSlotsMixedStaleness(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	stale := now.Add(-2 * time.Minute)

	cfg := &Config{
		Subscription: []SlotConfig{
			{Name: "sub-a", MaxConcurrent: 4},
			{Name: "sub-b", MaxConcurrent: 4},
		},
		APIKey: []SlotConfig{
			{Name: "key-a", MaxConcurrent: 4},
		},
	}

	if err := WriteSlotState(dir, &SlotState{
		Slot: "sub-a",
		InFlight: []InFlightClaim{
			{DispatchID: "a-stale-1", HeartbeatAt: stale},
			{DispatchID: "a-stale-2", HeartbeatAt: stale},
			{DispatchID: "a-fresh", HeartbeatAt: now},
		},
	}); err != nil {
		t.Fatalf("seed sub-a: %v", err)
	}
	if err := WriteSlotState(dir, &SlotState{
		Slot:     "sub-b",
		InFlight: []InFlightClaim{{DispatchID: "b-fresh", HeartbeatAt: now}},
	}); err != nil {
		t.Fatalf("seed sub-b: %v", err)
	}
	if err := WriteSlotState(dir, &SlotState{
		Slot:     "key-a",
		InFlight: []InFlightClaim{{DispatchID: "k-stale", HeartbeatAt: stale}},
	}); err != nil {
		t.Fatalf("seed key-a: %v", err)
	}

	wake := &recordingWake{}
	removed, err := Sweep(dir, 60*time.Second, wake, cfg)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}

	subA, err := ReadSlotState(dir, "sub-a")
	if err != nil {
		t.Fatalf("read sub-a: %v", err)
	}
	if len(subA.InFlight) != 1 || subA.InFlight[0].DispatchID != "a-fresh" {
		t.Errorf("sub-a InFlight = %v, want only a-fresh", subA.InFlight)
	}
	subB, err := ReadSlotState(dir, "sub-b")
	if err != nil {
		t.Fatalf("read sub-b: %v", err)
	}
	if len(subB.InFlight) != 1 || subB.InFlight[0].DispatchID != "b-fresh" {
		t.Errorf("sub-b InFlight = %v, want only b-fresh (untouched)", subB.InFlight)
	}
	keyA, err := ReadSlotState(dir, "key-a")
	if err != nil {
		t.Fatalf("read key-a: %v", err)
	}
	if len(keyA.InFlight) != 0 {
		t.Errorf("key-a InFlight = %v, want empty", keyA.InFlight)
	}

	want := []string{"api-key", "subscription"}
	if b := wake.snapshot(); !equalStrings(b, want) {
		t.Errorf("broadcasts = %v, want %v", b, want)
	}
}

// TestSweep_BroadcastFiresOnlyOnRemoval confirms that pools whose slots
// had no stale claims do not generate a wake — Sweep must not perturb a
// pool that's actually idle.
func TestSweep_BroadcastFiresOnlyOnRemoval(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	stale := now.Add(-2 * time.Minute)

	cfg := twoPoolConfig()
	if err := WriteSlotState(dir, &SlotState{
		Slot:     "sub-a",
		InFlight: []InFlightClaim{{DispatchID: "stale", HeartbeatAt: stale}},
	}); err != nil {
		t.Fatalf("seed sub-a: %v", err)
	}
	if err := WriteSlotState(dir, &SlotState{
		Slot:     "key-a",
		InFlight: []InFlightClaim{{DispatchID: "fresh", HeartbeatAt: now}},
	}); err != nil {
		t.Fatalf("seed key-a: %v", err)
	}

	wake := &recordingWake{}
	removed, err := Sweep(dir, 60*time.Second, wake, cfg)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	if b := wake.snapshot(); len(b) != 1 || b[0] != "subscription" {
		t.Errorf("broadcasts = %v, want [subscription] only (api-key had no removals)", b)
	}
}

// TestSweep_BroadcastOncePerPool guards against per-slot broadcasts:
// even when multiple slots in the same pool each lose a claim, the
// pool should only receive one wake.
func TestSweep_BroadcastOncePerPool(t *testing.T) {
	dir := t.TempDir()
	stale := time.Now().Add(-2 * time.Minute)

	cfg := &Config{
		Subscription: []SlotConfig{
			{Name: "sub-a", MaxConcurrent: 4},
			{Name: "sub-b", MaxConcurrent: 4},
		},
	}
	for _, slot := range []string{"sub-a", "sub-b"} {
		if err := WriteSlotState(dir, &SlotState{
			Slot:     slot,
			InFlight: []InFlightClaim{{DispatchID: slot + "-stale", HeartbeatAt: stale}},
		}); err != nil {
			t.Fatalf("seed %s: %v", slot, err)
		}
	}

	wake := &recordingWake{}
	removed, err := Sweep(dir, 60*time.Second, wake, cfg)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	b := wake.snapshot()
	if len(b) != 1 || b[0] != "subscription" {
		t.Errorf("broadcasts = %v, want [subscription] (one wake per affected pool)", b)
	}
}

func TestSweep_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	wake := &recordingWake{}
	removed, err := Sweep(dir, 60*time.Second, wake, twoPoolConfig())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if b := wake.snapshot(); len(b) != 0 {
		t.Errorf("broadcasts = %v, want none", b)
	}
}

// TestSweep_SlotNotInConfig covers the case where stateDir contains a
// state file for a slot that's been removed from cfg (e.g. the operator
// rotated out a credential). The stale claim should still be removed
// from the file, but no broadcast fires for an unknown pool.
func TestSweep_SlotNotInConfig(t *testing.T) {
	dir := t.TempDir()
	stale := time.Now().Add(-2 * time.Minute)

	if err := WriteSlotState(dir, &SlotState{
		Slot:     "ghost",
		InFlight: []InFlightClaim{{DispatchID: "stale", HeartbeatAt: stale}},
	}); err != nil {
		t.Fatalf("seed ghost: %v", err)
	}

	wake := &recordingWake{}
	removed, err := Sweep(dir, 60*time.Second, wake, twoPoolConfig())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (orphaned slot still gets cleaned)", removed)
	}

	got, err := ReadSlotState(dir, "ghost")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if len(got.InFlight) != 0 {
		t.Errorf("InFlight = %v, want empty", got.InFlight)
	}
	if b := wake.snapshot(); len(b) != 0 {
		t.Errorf("broadcasts = %v, want none (orphan has no pool)", b)
	}
}

func TestSweep_NilConfig(t *testing.T) {
	dir := t.TempDir()
	stale := time.Now().Add(-2 * time.Minute)

	if err := WriteSlotState(dir, &SlotState{
		Slot:     "sub-a",
		InFlight: []InFlightClaim{{DispatchID: "stale", HeartbeatAt: stale}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	wake := &recordingWake{}
	removed, err := Sweep(dir, 60*time.Second, wake, nil)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if b := wake.snapshot(); len(b) != 0 {
		t.Errorf("broadcasts = %v, want none (nil cfg cannot map slots to pools)", b)
	}
}

// TestSweep_BoundaryHeartbeat asserts the bead's contract: a claim is
// dropped iff time.Since(HeartbeatAt) > staleAge — equality is kept.
// We synthesize the boundary by writing one claim a hair past the
// threshold and one strictly inside it.
func TestSweep_BoundaryHeartbeat(t *testing.T) {
	dir := t.TempDir()
	staleAge := 60 * time.Second
	now := time.Now()

	if err := WriteSlotState(dir, &SlotState{
		Slot: "sub-a",
		InFlight: []InFlightClaim{
			{DispatchID: "edge-keep", HeartbeatAt: now.Add(-staleAge + 5*time.Second)},
			{DispatchID: "edge-drop", HeartbeatAt: now.Add(-staleAge - 5*time.Second)},
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	wake := &recordingWake{}
	removed, err := Sweep(dir, staleAge, wake, twoPoolConfig())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	got, err := ReadSlotState(dir, "sub-a")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if len(got.InFlight) != 1 || got.InFlight[0].DispatchID != "edge-keep" {
		t.Errorf("InFlight = %v, want only edge-keep", got.InFlight)
	}
}

// TestSweep_ZeroHeartbeat: a claim written with a zero-value HeartbeatAt
// (a stuck or malformed claim) is treated as stale because time.Since on
// the zero time returns a huge duration.
func TestSweep_ZeroHeartbeat(t *testing.T) {
	dir := t.TempDir()

	if err := WriteSlotState(dir, &SlotState{
		Slot:     "sub-a",
		InFlight: []InFlightClaim{{DispatchID: "stuck"}}, // HeartbeatAt zero
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	wake := &recordingWake{}
	removed, err := Sweep(dir, 60*time.Second, wake, twoPoolConfig())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (zero heartbeat counts as stale)", removed)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
