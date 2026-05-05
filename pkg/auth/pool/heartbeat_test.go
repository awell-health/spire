package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// seedClaim writes a slot state containing exactly one InFlightClaim so
// the heartbeat tests have something to refresh. ClaimedAt and
// HeartbeatAt are seeded at the same moment so a later HeartbeatAt
// update is visible.
func seedClaim(t *testing.T, dir, slot, dispatchID string, at time.Time) {
	t.Helper()
	if err := WriteSlotState(dir, &SlotState{
		Slot: slot,
		InFlight: []InFlightClaim{
			{DispatchID: dispatchID, ClaimedAt: at, HeartbeatAt: at},
		},
	}); err != nil {
		t.Fatalf("seed slot state: %v", err)
	}
}

// readClaim returns the InFlightClaim with the given DispatchID from the
// slot state, or nil if no matching claim exists.
func readClaim(t *testing.T, dir, slot, dispatchID string) *InFlightClaim {
	t.Helper()
	s, err := ReadSlotState(dir, slot)
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	for i := range s.InFlight {
		if s.InFlight[i].DispatchID == dispatchID {
			c := s.InFlight[i]
			return &c
		}
	}
	return nil
}

// waitFor polls fn until it returns true or the deadline elapses. Uses
// short sleeps rather than blocking on cache events so the helper works
// against the synchronous Mutate/Read cache primitives.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return fn()
}

// TestHeartbeat_RefreshesEachTick asserts that HeartbeatAt advances
// while the loop is running. We seed a stale HeartbeatAt, run the
// heartbeat with a short interval, then cancel and verify the persisted
// HeartbeatAt is strictly after the seed.
func TestHeartbeat_RefreshesEachTick(t *testing.T) {
	dir := t.TempDir()
	const slot = "primary"
	const dispatchID = "d-refresh"

	seed := time.Now().Add(-time.Hour)
	seedClaim(t, dir, slot, dispatchID, seed)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Heartbeat(ctx, dir, slot, dispatchID, 5*time.Millisecond)
	}()

	ok := waitFor(t, 2*time.Second, func() bool {
		c := readClaim(t, dir, slot, dispatchID)
		return c != nil && c.HeartbeatAt.After(seed)
	})
	if !ok {
		t.Fatalf("HeartbeatAt did not advance past seed %v within timeout", seed)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Heartbeat returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Heartbeat did not exit after cancel")
	}
}

// TestHeartbeat_AdvancesAcrossTicks asserts HeartbeatAt advances on
// subsequent ticks, not only the first. We capture HeartbeatAt after
// the first observed bump, then wait for a strictly later value.
func TestHeartbeat_AdvancesAcrossTicks(t *testing.T) {
	dir := t.TempDir()
	const slot = "primary"
	const dispatchID = "d-multi"

	seed := time.Now().Add(-time.Hour)
	seedClaim(t, dir, slot, dispatchID, seed)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Heartbeat(ctx, dir, slot, dispatchID, 10*time.Millisecond)
	}()

	var first time.Time
	if !waitFor(t, 2*time.Second, func() bool {
		c := readClaim(t, dir, slot, dispatchID)
		if c != nil && c.HeartbeatAt.After(seed) {
			first = c.HeartbeatAt
			return true
		}
		return false
	}) {
		t.Fatal("first heartbeat tick never landed")
	}

	if !waitFor(t, 2*time.Second, func() bool {
		c := readClaim(t, dir, slot, dispatchID)
		return c != nil && c.HeartbeatAt.After(first)
	}) {
		t.Fatalf("second heartbeat tick did not advance HeartbeatAt past %v", first)
	}

	cancel()
	<-done
}

// TestHeartbeat_ReturnsContextErrOnCancel asserts that ctx.Err() is the
// returned error when ctx is cancelled. Verifies we surface ctx.Err()
// rather than a hardcoded sentinel.
func TestHeartbeat_ReturnsContextErrOnCancel(t *testing.T) {
	dir := t.TempDir()
	const slot = "primary"
	const dispatchID = "d-cancel"

	seedClaim(t, dir, slot, dispatchID, time.Now())

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- Heartbeat(ctx, dir, slot, dispatchID, 50*time.Millisecond)
	}()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Heartbeat returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Heartbeat did not exit promptly after cancel")
	}
}

// TestHeartbeat_ReturnsContextErrOnDeadline asserts that
// context.DeadlineExceeded is propagated when ctx hits its deadline.
func TestHeartbeat_ReturnsContextErrOnDeadline(t *testing.T) {
	dir := t.TempDir()
	const slot = "primary"
	const dispatchID = "d-deadline"

	seedClaim(t, dir, slot, dispatchID, time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := Heartbeat(ctx, dir, slot, dispatchID, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Heartbeat returned %v, want context.DeadlineExceeded", err)
	}
}

// TestHeartbeat_ExitsWhenClaimRemoved asserts graceful nil return when
// the matching claim is removed mid-run. Other claims may still exist
// in the slot — only ours has gone away.
func TestHeartbeat_ExitsWhenClaimRemoved(t *testing.T) {
	dir := t.TempDir()
	const slot = "primary"
	const dispatchID = "d-removed"
	const otherID = "d-other"

	now := time.Now()
	if err := WriteSlotState(dir, &SlotState{
		Slot: slot,
		InFlight: []InFlightClaim{
			{DispatchID: dispatchID, ClaimedAt: now, HeartbeatAt: now},
			{DispatchID: otherID, ClaimedAt: now, HeartbeatAt: now},
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Heartbeat(ctx, dir, slot, dispatchID, 5*time.Millisecond)
	}()

	if err := MutateSlotState(dir, slot, func(s *SlotState) error {
		filtered := s.InFlight[:0]
		for _, c := range s.InFlight {
			if c.DispatchID != dispatchID {
				filtered = append(filtered, c)
			}
		}
		s.InFlight = filtered
		return nil
	}); err != nil {
		t.Fatalf("remove claim: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Heartbeat returned %v, want nil after claim removed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Heartbeat did not exit after claim removal")
	}

	other := readClaim(t, dir, slot, otherID)
	if other == nil {
		t.Fatalf("unrelated claim should still be present, got nil")
	}
}

// TestHeartbeat_ExitsWhenSlotFileMissing asserts graceful nil return
// when the slot file does not exist at all. ReadSlotState constructs a
// zero-value SlotState for a missing file, so the scan finds no match
// and we exit cleanly.
func TestHeartbeat_ExitsWhenSlotFileMissing(t *testing.T) {
	dir := t.TempDir()
	const slot = "ghost"
	const dispatchID = "d-ghost"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := Heartbeat(ctx, dir, slot, dispatchID, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("Heartbeat returned %v, want nil for missing slot file", err)
	}
}

// TestHeartbeat_NoPanicOnConcurrentRelease hammers a single slot with
// concurrent releases racing the heartbeat ticker. The cache's
// exclusive lock serializes them; whichever lands second observes the
// post-removal state and triggers the graceful exit. No panic, no
// error.
func TestHeartbeat_NoPanicOnConcurrentRelease(t *testing.T) {
	dir := t.TempDir()
	const slot = "primary"
	const dispatchID = "d-race"

	seedClaim(t, dir, slot, dispatchID, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Heartbeat(ctx, dir, slot, dispatchID, 1*time.Millisecond)
	}()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = MutateSlotState(dir, slot, func(s *SlotState) error {
				filtered := s.InFlight[:0]
				for _, c := range s.InFlight {
					if c.DispatchID != dispatchID {
						filtered = append(filtered, c)
					}
				}
				s.InFlight = filtered
				return nil
			})
		}()
	}
	wg.Wait()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Heartbeat returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Heartbeat did not exit after concurrent releases")
	}
}

// TestHeartbeat_RejectsNonPositiveInterval guards the input contract.
// A zero or negative interval is a programming error, not something to
// silently absorb.
func TestHeartbeat_RejectsNonPositiveInterval(t *testing.T) {
	dir := t.TempDir()

	if err := Heartbeat(context.Background(), dir, "primary", "d", 0); err == nil {
		t.Error("Heartbeat(interval=0): want error, got nil")
	}
	if err := Heartbeat(context.Background(), dir, "primary", "d", -time.Second); err == nil {
		t.Error("Heartbeat(interval=-1s): want error, got nil")
	}
}
