package pool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestSelector wires a Selector with a tmp stateDir, the supplied config,
// the preemptive policy, and a fresh LocalPoolWake. Tests share the helper
// to keep the wiring noise low.
func newTestSelector(t *testing.T, cfg *Config) *Selector {
	t.Helper()
	dir := t.TempDir()
	return NewSelector(cfg, dir, &PreemptivePolicy{}, NewLocalPoolWake())
}

// subscriptionConfig builds a Config with one named subscription slot at the
// given MaxConcurrent.
func subscriptionConfig(slots ...SlotConfig) *Config {
	return &Config{Subscription: slots}
}

// TestPick_EmptyPoolReturnsDescriptiveError covers the configuration error
// case: an unconfigured pool must fail with a non-ErrAllRateLimited error
// so the caller distinguishes "you forgot to configure tokens" from "all
// tokens are rate-limited".
func TestPick_EmptyPoolReturnsDescriptiveError(t *testing.T) {
	cfg := &Config{}
	sel := newTestSelector(t, cfg)

	_, err := sel.Pick(context.Background(), "subscription", "d-1")
	if err == nil {
		t.Fatal("Pick(empty pool): want error, got nil")
	}
	var rate *ErrAllRateLimited
	if errors.As(err, &rate) {
		t.Fatalf("Pick(empty pool): unexpected ErrAllRateLimited: %v", err)
	}
}

// TestPick_UnknownPoolNameReturnsError covers the typo case: an invalid
// pool name should not silently fall through to a wait loop.
func TestPick_UnknownPoolNameReturnsError(t *testing.T) {
	cfg := subscriptionConfig(SlotConfig{Name: "primary", MaxConcurrent: 1})
	sel := newTestSelector(t, cfg)

	_, err := sel.Pick(context.Background(), "bogus", "d-1")
	if err == nil {
		t.Fatal("Pick(bogus pool): want error, got nil")
	}
}

// TestPick_HappyPathAppendsClaim verifies the success path: Pick returns a
// slot name and the cached state gains a matching InFlightClaim.
func TestPick_HappyPathAppendsClaim(t *testing.T) {
	cfg := subscriptionConfig(SlotConfig{Name: "primary", MaxConcurrent: 1})
	sel := newTestSelector(t, cfg)

	slot, err := sel.Pick(context.Background(), "subscription", "d-1")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if slot != "primary" {
		t.Fatalf("Pick: slot = %q, want %q", slot, "primary")
	}

	state, err := ReadSlotState(sel.stateDir, slot)
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if len(state.InFlight) != 1 {
		t.Fatalf("InFlight len = %d, want 1; got %+v", len(state.InFlight), state.InFlight)
	}
	if state.InFlight[0].DispatchID != "d-1" {
		t.Errorf("InFlight[0].DispatchID = %q, want %q", state.InFlight[0].DispatchID, "d-1")
	}
	if state.InFlight[0].ClaimedAt.IsZero() {
		t.Error("InFlight[0].ClaimedAt is zero")
	}
	if state.InFlight[0].HeartbeatAt.IsZero() {
		t.Error("InFlight[0].HeartbeatAt is zero")
	}
}

// TestPick_AllRejectedReturnsErrAllRateLimited covers the "park" case:
// every slot has at least one rejected bucket, and the returned error
// carries the soonest reset.
func TestPick_AllRejectedReturnsErrAllRateLimited(t *testing.T) {
	cfg := subscriptionConfig(
		SlotConfig{Name: "primary", MaxConcurrent: 1},
		SlotConfig{Name: "secondary", MaxConcurrent: 1},
	)
	sel := newTestSelector(t, cfg)

	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	primaryReset := now.Add(30 * time.Minute)
	secondaryReset := now.Add(15 * time.Minute) // soonest
	if err := WriteSlotState(sel.stateDir, &SlotState{
		Slot: "primary",
		RateLimit: RateLimitInfo{
			FiveHour: RateLimitBucket{Status: RateLimitStatusRejected, ResetsAt: primaryReset},
		},
	}); err != nil {
		t.Fatalf("WriteSlotState primary: %v", err)
	}
	if err := WriteSlotState(sel.stateDir, &SlotState{
		Slot: "secondary",
		RateLimit: RateLimitInfo{
			Overage: RateLimitBucket{Status: RateLimitStatusRejected, ResetsAt: secondaryReset},
		},
	}); err != nil {
		t.Fatalf("WriteSlotState secondary: %v", err)
	}

	_, err := sel.Pick(context.Background(), "subscription", "d-1")
	var rate *ErrAllRateLimited
	if !errors.As(err, &rate) {
		t.Fatalf("Pick: err = %v, want *ErrAllRateLimited", err)
	}
	if !rate.ResetsAt.Equal(secondaryReset) {
		t.Errorf("ResetsAt = %v, want soonest %v", rate.ResetsAt, secondaryReset)
	}
	if rate.Error() == "" {
		t.Error("ErrAllRateLimited.Error() returned empty string")
	}
}

// TestPick_AtCapBlocksUntilRelease verifies the wait-for-release path:
// when every slot is at MaxConcurrent (but not rejected), Pick blocks until
// a peer Release wakes it.
func TestPick_AtCapBlocksUntilRelease(t *testing.T) {
	cfg := subscriptionConfig(SlotConfig{Name: "primary", MaxConcurrent: 1})
	sel := newTestSelector(t, cfg)

	// Hold the slot at cap by performing the first Pick.
	first, err := sel.Pick(context.Background(), "subscription", "first")
	if err != nil {
		t.Fatalf("first Pick: %v", err)
	}
	if first != "primary" {
		t.Fatalf("first Pick slot = %q", first)
	}

	// Second Pick should block.
	type result struct {
		slot string
		err  error
	}
	out := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		slot, err := sel.Pick(ctx, "subscription", "second")
		out <- result{slot, err}
	}()

	select {
	case r := <-out:
		t.Fatalf("second Pick returned before release: slot=%q err=%v", r.slot, r.err)
	case <-time.After(150 * time.Millisecond):
	}

	// Release the first claim.
	if err := sel.Release("subscription", "primary", "first"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("second Pick after Release: err = %v", r.err)
		}
		if r.slot != "primary" {
			t.Fatalf("second Pick slot = %q, want %q", r.slot, "primary")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Pick never returned after Release")
	}
}

// TestPick_CtxCancelWhileWaiting verifies that ctx.Done() during the wait
// loop returns ctx.Err().
func TestPick_CtxCancelWhileWaiting(t *testing.T) {
	cfg := subscriptionConfig(SlotConfig{Name: "primary", MaxConcurrent: 1})
	sel := newTestSelector(t, cfg)

	if _, err := sel.Pick(context.Background(), "subscription", "first"); err != nil {
		t.Fatalf("first Pick: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		err error
	}
	out := make(chan result, 1)
	go func() {
		_, err := sel.Pick(ctx, "subscription", "second")
		out <- result{err}
	}()

	// Give the second Pick time to enter wake.Wait, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case r := <-out:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Pick err = %v, want context.Canceled", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pick did not return after ctx cancel")
	}
}

// TestPick_PrecancelledCtxReturnsImmediately verifies the entry-point ctx
// check rather than going through a full cycle.
func TestPick_PrecancelledCtxReturnsImmediately(t *testing.T) {
	cfg := subscriptionConfig(SlotConfig{Name: "primary", MaxConcurrent: 1})
	sel := newTestSelector(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := sel.Pick(ctx, "subscription", "d-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Pick err = %v, want context.Canceled", err)
	}
}

// TestRelease_RemovesMatchingClaim covers the basic Release contract.
func TestRelease_RemovesMatchingClaim(t *testing.T) {
	cfg := subscriptionConfig(SlotConfig{Name: "primary", MaxConcurrent: 4})
	sel := newTestSelector(t, cfg)

	for _, id := range []string{"a", "b", "c"} {
		if _, err := sel.Pick(context.Background(), "subscription", id); err != nil {
			t.Fatalf("Pick %s: %v", id, err)
		}
	}

	if err := sel.Release("subscription", "primary", "b"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	state, err := ReadSlotState(sel.stateDir, "primary")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if len(state.InFlight) != 2 {
		t.Fatalf("InFlight len = %d, want 2", len(state.InFlight))
	}
	for _, c := range state.InFlight {
		if c.DispatchID == "b" {
			t.Errorf("Release left dispatch %q in InFlight", c.DispatchID)
		}
	}
}

// TestRelease_NoOpForUnknownDispatchID verifies that releasing a claim that
// is not present succeeds (e.g. a sweep already removed it).
func TestRelease_NoOpForUnknownDispatchID(t *testing.T) {
	cfg := subscriptionConfig(SlotConfig{Name: "primary", MaxConcurrent: 1})
	sel := newTestSelector(t, cfg)

	if _, err := sel.Pick(context.Background(), "subscription", "real"); err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if err := sel.Release("subscription", "primary", "ghost"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	state, err := ReadSlotState(sel.stateDir, "primary")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if len(state.InFlight) != 1 || state.InFlight[0].DispatchID != "real" {
		t.Errorf("Release(ghost) corrupted InFlight: %+v", state.InFlight)
	}
}

// TestHeartbeat_UpdatesMatchingClaim covers the heartbeat contract.
func TestHeartbeat_UpdatesMatchingClaim(t *testing.T) {
	cfg := subscriptionConfig(SlotConfig{Name: "primary", MaxConcurrent: 1})
	sel := newTestSelector(t, cfg)

	if _, err := sel.Pick(context.Background(), "subscription", "d-1"); err != nil {
		t.Fatalf("Pick: %v", err)
	}

	before, err := ReadSlotState(sel.stateDir, "primary")
	if err != nil {
		t.Fatalf("ReadSlotState before: %v", err)
	}
	hbBefore := before.InFlight[0].HeartbeatAt

	// Sleep a hair so the new HeartbeatAt is observably later than the
	// initial one set by Pick.
	time.Sleep(10 * time.Millisecond)

	if err := sel.Heartbeat("primary", "d-1"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	after, err := ReadSlotState(sel.stateDir, "primary")
	if err != nil {
		t.Fatalf("ReadSlotState after: %v", err)
	}
	if !after.InFlight[0].HeartbeatAt.After(hbBefore) {
		t.Errorf("HeartbeatAt did not advance: before=%v after=%v", hbBefore, after.InFlight[0].HeartbeatAt)
	}
}

// TestHeartbeat_UnknownDispatchReturnsError verifies that heartbeating a
// claim that doesn't exist surfaces an error so the dispatch can abort.
func TestHeartbeat_UnknownDispatchReturnsError(t *testing.T) {
	cfg := subscriptionConfig(SlotConfig{Name: "primary", MaxConcurrent: 1})
	sel := newTestSelector(t, cfg)

	if err := sel.Heartbeat("primary", "ghost"); err == nil {
		t.Fatal("Heartbeat(ghost): want error, got nil")
	}
}

// TestPick_ConcurrentRespectsMaxConcurrent spawns more goroutines than the
// pool's total capacity and verifies that, after every goroutine has picked
// (using release-then-pick to free the slot) the visible InFlight count
// never exceeds MaxConcurrent on any slot. Real flock is in play — no mocks.
func TestPick_ConcurrentRespectsMaxConcurrent(t *testing.T) {
	cfg := subscriptionConfig(
		SlotConfig{Name: "a", MaxConcurrent: 2},
		SlotConfig{Name: "b", MaxConcurrent: 2},
	)
	sel := newTestSelector(t, cfg)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Track the maximum simultaneous in-flight count observed on each slot
	// via the cached state. A goroutine acquires, briefly holds, and
	// releases; concurrent acquires should never let a slot's InFlight
	// exceed its MaxConcurrent.
	var maxA, maxB atomic.Int64

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("d-%d", i)
			slot, err := sel.Pick(ctx, "subscription", id)
			if err != nil {
				t.Errorf("goroutine %d Pick: %v", i, err)
				return
			}
			// Read state; record max.
			st, err := ReadSlotState(sel.stateDir, slot)
			if err != nil {
				t.Errorf("goroutine %d ReadSlotState: %v", i, err)
				return
			}
			n := int64(len(st.InFlight))
			switch slot {
			case "a":
				updateMax(&maxA, n)
			case "b":
				updateMax(&maxB, n)
			}
			// Hold briefly so concurrency is exercised.
			time.Sleep(2 * time.Millisecond)
			if err := sel.Release("subscription", slot, id); err != nil {
				t.Errorf("goroutine %d Release: %v", i, err)
			}
		}()
	}
	wg.Wait()

	if got := maxA.Load(); got > 2 {
		t.Errorf("slot a peak InFlight = %d, exceeds MaxConcurrent=2", got)
	}
	if got := maxB.Load(); got > 2 {
		t.Errorf("slot b peak InFlight = %d, exceeds MaxConcurrent=2", got)
	}

	// And after everyone released, both slots should be empty.
	for _, name := range []string{"a", "b"} {
		st, err := ReadSlotState(sel.stateDir, name)
		if err != nil {
			t.Fatalf("ReadSlotState %s: %v", name, err)
		}
		if len(st.InFlight) != 0 {
			t.Errorf("slot %s still has %d InFlight after all releases: %+v", name, len(st.InFlight), st.InFlight)
		}
	}
}

// updateMax atomically bumps p to v iff v is larger.
func updateMax(p *atomic.Int64, v int64) {
	for {
		cur := p.Load()
		if v <= cur {
			return
		}
		if p.CompareAndSwap(cur, v) {
			return
		}
	}
}

// TestPick_APIKeyPoolRoute verifies the api-key pool name path is wired
// through to cfg.APIKey rather than cfg.Subscription.
func TestPick_APIKeyPoolRoute(t *testing.T) {
	cfg := &Config{
		APIKey: []SlotConfig{{Name: "key-1", MaxConcurrent: 1}},
	}
	sel := newTestSelector(t, cfg)

	slot, err := sel.Pick(context.Background(), "api-key", "d-1")
	if err != nil {
		t.Fatalf("Pick(api-key): %v", err)
	}
	if slot != "key-1" {
		t.Errorf("Pick(api-key) slot = %q, want %q", slot, "key-1")
	}
}

// TestPick_AllAtCapWithFreeReleaseEventuallyServesAllWaiters spawns N
// concurrent Picks against a pool of capacity 1, releasing after each
// Pick succeeds, and verifies that every Pick eventually receives the
// slot. This stress-tests the wake/release cycle under contention.
func TestPick_AllAtCapWithFreeReleaseEventuallyServesAllWaiters(t *testing.T) {
	cfg := subscriptionConfig(SlotConfig{Name: "only", MaxConcurrent: 1})
	sel := newTestSelector(t, cfg)

	const goroutines = 6
	var wg sync.WaitGroup
	wg.Add(goroutines)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var (
		mu      sync.Mutex
		serviced []string
	)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("w-%d", i)
			slot, err := sel.Pick(ctx, "subscription", id)
			if err != nil {
				t.Errorf("Pick %s: %v", id, err)
				return
			}
			mu.Lock()
			serviced = append(serviced, id)
			mu.Unlock()
			time.Sleep(3 * time.Millisecond)
			if err := sel.Release("subscription", slot, id); err != nil {
				t.Errorf("Release %s: %v", id, err)
			}
		}()
	}
	wg.Wait()

	if len(serviced) != goroutines {
		mu.Lock()
		got := append([]string(nil), serviced...)
		mu.Unlock()
		sort.Strings(got)
		t.Fatalf("serviced %d/%d goroutines: %v", len(serviced), goroutines, got)
	}
}

// TestPick_PartiallyRejectedSlotsWaitNotPark verifies that when at least one
// slot is non-rejected (just at cap), Pick takes the wait branch rather than
// returning ErrAllRateLimited.
func TestPick_PartiallyRejectedSlotsWaitNotPark(t *testing.T) {
	cfg := subscriptionConfig(
		SlotConfig{Name: "rej", MaxConcurrent: 1},
		SlotConfig{Name: "cap", MaxConcurrent: 1},
	)
	sel := newTestSelector(t, cfg)

	// Reject one slot.
	if err := WriteSlotState(sel.stateDir, &SlotState{
		Slot: "rej",
		RateLimit: RateLimitInfo{
			FiveHour: RateLimitBucket{Status: RateLimitStatusRejected, ResetsAt: time.Now().Add(time.Hour)},
		},
	}); err != nil {
		t.Fatalf("WriteSlotState rej: %v", err)
	}

	// Fill the other slot.
	if _, err := sel.Pick(context.Background(), "subscription", "first"); err != nil {
		t.Fatalf("first Pick: %v", err)
	}

	// Subsequent Pick should NOT return ErrAllRateLimited (one slot is just
	// at cap, not rejected). It should block. Use a short ctx to confirm.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := sel.Pick(ctx, "subscription", "second")
	if err == nil {
		t.Fatal("Pick: want error (timeout), got nil")
	}
	var rate *ErrAllRateLimited
	if errors.As(err, &rate) {
		t.Fatalf("Pick: got ErrAllRateLimited; expected wait/timeout. err=%v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Pick: err = %v, want context.DeadlineExceeded", err)
	}
}

// TestErrAllRateLimited_ZeroResetsAtMessage covers the message path when no
// reset hint was carried.
func TestErrAllRateLimited_ZeroResetsAtMessage(t *testing.T) {
	e := &ErrAllRateLimited{}
	if e.Error() == "" {
		t.Error("ErrAllRateLimited{}.Error() empty")
	}
}
