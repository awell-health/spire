package steward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStrategy is a controllable syncStrategy for testing the Daemon.
// Each call to Sync blocks on release (if non-nil) then returns err.
type fakeStrategy struct {
	mu       sync.Mutex
	calls    int32
	release  chan struct{}
	err      error
	describe string
}

func newFakeStrategy() *fakeStrategy {
	return &fakeStrategy{describe: "fake"}
}

func (f *fakeStrategy) Sync(ctx context.Context, _ string) error {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	rel := f.release
	err := f.err
	f.mu.Unlock()
	if rel != nil {
		select {
		case <-rel:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeStrategy) Describe() string { return f.describe }

func (f *fakeStrategy) setRelease(ch chan struct{}) {
	f.mu.Lock()
	f.release = ch
	f.mu.Unlock()
}

func (f *fakeStrategy) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeStrategy) callCount() int32 { return atomic.LoadInt32(&f.calls) }

func silentLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func newTestDaemon(strategy syncStrategy, interval, debounce time.Duration) *Daemon {
	return &Daemon{
		strategy: strategy,
		interval: interval,
		debounce: debounce,
		log:      silentLogger(),
	}
}

func TestDaemon_RunOnce_Success(t *testing.T) {
	fs := newFakeStrategy()
	d := newTestDaemon(fs, time.Minute, 0)

	if err := d.RunOnce("test"); err != nil {
		t.Fatalf("RunOnce: unexpected error: %v", err)
	}
	if got := fs.callCount(); got != 1 {
		t.Errorf("sync calls = %d, want 1", got)
	}
	if d.syncing {
		t.Error("syncing flag not cleared after RunOnce")
	}
	if d.lastSync.IsZero() {
		t.Error("lastSync not updated after RunOnce")
	}
}

func TestDaemon_RunOnce_PropagatesError(t *testing.T) {
	fs := newFakeStrategy()
	want := errors.New("boom")
	fs.setErr(want)
	d := newTestDaemon(fs, time.Minute, 0)

	if err := d.RunOnce("test"); !errors.Is(err, want) {
		t.Errorf("RunOnce err = %v, want %v", err, want)
	}
	if d.syncing {
		t.Error("syncing flag not cleared after error")
	}
}

func TestDaemon_RunOnce_RejectsWhenBusy(t *testing.T) {
	fs := newFakeStrategy()
	gate := make(chan struct{})
	fs.setRelease(gate)
	d := newTestDaemon(fs, time.Minute, 0)

	done := make(chan error, 1)
	go func() { done <- d.RunOnce("first") }()

	// Wait until the first RunOnce has acquired the busy flag.
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.syncing
	}, "first RunOnce to enter sync")

	if err := d.RunOnce("second"); err == nil {
		t.Error("expected error from concurrent RunOnce, got nil")
	}

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("first RunOnce err: %v", err)
	}
}

func TestDaemon_Trigger_Success(t *testing.T) {
	fs := newFakeStrategy()
	d := newTestDaemon(fs, time.Minute, 0)

	if err := d.Trigger("http"); err != nil {
		t.Fatalf("Trigger: unexpected error: %v", err)
	}

	waitFor(t, func() bool { return fs.callCount() == 1 }, "sync to run")
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.syncing
	}, "sync to finish")
}

func TestDaemon_Trigger_RejectsWhenBusy(t *testing.T) {
	fs := newFakeStrategy()
	gate := make(chan struct{})
	fs.setRelease(gate)
	d := newTestDaemon(fs, time.Minute, 0)

	if err := d.Trigger("first"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.syncing
	}, "first Trigger to start")

	if err := d.Trigger("second"); err == nil {
		t.Error("expected concurrent Trigger to be rejected")
	}

	close(gate)
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.syncing
	}, "first Trigger to finish")

	if got := fs.callCount(); got != 1 {
		t.Errorf("sync calls = %d, want 1 (second Trigger should not have spawned sync)", got)
	}
}

func TestDaemon_Trigger_Debounce(t *testing.T) {
	fs := newFakeStrategy()
	d := newTestDaemon(fs, time.Minute, 100*time.Millisecond)

	if err := d.Trigger("first"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	waitFor(t, func() bool { return fs.callCount() == 1 }, "first sync to run")
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.syncing
	}, "first sync to finish")

	// Immediately after, Trigger should be rejected by debounce.
	if err := d.Trigger("second"); err == nil {
		t.Error("expected debounce to reject second Trigger")
	}
	if got := fs.callCount(); got != 1 {
		t.Errorf("sync calls after debounce = %d, want 1", got)
	}
}

func TestDaemon_Trigger_ConcurrentRaceSafety(t *testing.T) {
	// Fire many Triggers in parallel; at most one sync should run at a time.
	fs := newFakeStrategy()
	gate := make(chan struct{})
	fs.setRelease(gate)
	d := newTestDaemon(fs, time.Minute, 0)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.Trigger("race")
		}()
	}
	wg.Wait()

	// Exactly one should have acquired the busy flag; the rest rejected.
	d.mu.Lock()
	if !d.syncing {
		d.mu.Unlock()
		t.Fatal("expected exactly one Trigger to acquire busy flag")
	}
	d.mu.Unlock()

	if got := fs.callCount(); got != 1 {
		t.Errorf("concurrent sync count = %d, want 1 (race condition)", got)
	}

	close(gate)
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.syncing
	}, "sync to finish")
}

func TestDaemon_Run_StopsOnCancel(t *testing.T) {
	fs := newFakeStrategy()
	d := newTestDaemon(fs, 50*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Wait for startup sync to run.
	waitFor(t, func() bool { return fs.callCount() >= 1 }, "startup sync")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestDaemon_Run_TickerDoesNotSpawnConcurrentSyncs(t *testing.T) {
	// Regression test for the TOCTOU race: a slow sync plus fast ticker
	// used to spawn a second sync if the ticker's check-and-set was split
	// across two lock acquisitions. With the fix, only one sync runs.
	fs := newFakeStrategy()
	gate := make(chan struct{})
	fs.setRelease(gate)
	// Ticker much faster than the sync so the ticker fires repeatedly
	// while the first sync is still blocked.
	d := newTestDaemon(fs, 5*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Let the ticker fire many times while the first sync blocks.
	time.Sleep(100 * time.Millisecond)

	// At this point: one sync is running (blocked), the ticker has fired
	// ~20 times and rejected each, so call count must be exactly 1.
	if got := fs.callCount(); got != 1 {
		t.Errorf("concurrent syncs detected: call count = %d, want 1", got)
	}

	close(gate)
	cancel()
	<-done
}

func TestDaemon_RunOnce_BypassesDebounce(t *testing.T) {
	// RunOnce promises to bypass debounce. With a 1-hour debounce, two
	// back-to-back RunOnce calls must both execute.
	fs := newFakeStrategy()
	d := newTestDaemon(fs, time.Minute, time.Hour)

	if err := d.RunOnce("first"); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if err := d.RunOnce("second"); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if got := fs.callCount(); got != 2 {
		t.Errorf("sync calls = %d, want 2 (RunOnce must ignore debounce)", got)
	}
}

func TestDaemon_Trigger_DebounceAllowsAfterWindow(t *testing.T) {
	// Complements TestDaemon_Trigger_Debounce: after the debounce window
	// elapses, a subsequent Trigger must be accepted.
	fs := newFakeStrategy()
	debounce := 50 * time.Millisecond
	d := newTestDaemon(fs, time.Minute, debounce)

	if err := d.Trigger("first"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	// Wait until the first sync has completed AND lastSync has been set
	// (runOnce defers update of lastSync before clearing syncing, so
	// !syncing implies lastSync is already written).
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.syncing && !d.lastSync.IsZero()
	}, "first sync to finish")

	// Sleep past the debounce window.
	time.Sleep(debounce + 30*time.Millisecond)

	if err := d.Trigger("second"); err != nil {
		t.Fatalf("second Trigger after debounce window: %v", err)
	}
	waitFor(t, func() bool { return fs.callCount() == 2 }, "second sync to run")
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.syncing
	}, "second sync to finish")
}

func TestDaemon_Run_TickerFiresSubsequentSyncs(t *testing.T) {
	// Proves the ticker drives additional syncs after startup. Existing
	// Run tests only verify startup sync + TOCTOU safety.
	fs := newFakeStrategy()
	d := newTestDaemon(fs, 20*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitFor(t, func() bool { return fs.callCount() >= 3 }, "ticker to drive >=3 syncs")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestDaemon_Trigger_ReturnsBeforeSyncCompletes(t *testing.T) {
	// Trigger is documented as non-blocking: it returns as soon as the
	// sync goroutine has been scheduled, not after it completes.
	fs := newFakeStrategy()
	gate := make(chan struct{})
	fs.setRelease(gate)
	d := newTestDaemon(fs, time.Minute, 0)

	if err := d.Trigger("nonblocking"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	// Trigger returned. Wait for the sync goroutine to enter Sync (gate
	// keeps it blocked), then assert syncing is still true and call count
	// has not advanced past 1 — proving Trigger did not wait for completion.
	waitFor(t, func() bool { return fs.callCount() == 1 }, "sync goroutine to enter Sync")

	d.mu.Lock()
	busy := d.syncing
	d.mu.Unlock()
	if !busy {
		t.Error("expected syncing=true while sync is gated (Trigger should not wait for completion)")
	}
	if got := fs.callCount(); got != 1 {
		t.Errorf("sync calls = %d, want 1 (sync should still be in-flight)", got)
	}

	close(gate)
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.syncing
	}, "sync to finish")
}

func TestLocalStrategy_Describe(t *testing.T) {
	s := localStrategy{}
	if got := s.Describe(); got != "local-multi-tower" {
		t.Errorf("Describe = %q, want %q", got, "local-multi-tower")
	}
}

func TestClusterStrategy_Describe(t *testing.T) {
	s := &clusterStrategy{database: "beads_acm", remote: "origin", branch: "main"}
	want := "cluster-sql:beads_acm:origin/main"
	if got := s.Describe(); got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}
}

func TestNewClusterDaemon_AppliesDefaults(t *testing.T) {
	d := NewClusterDaemon("beads_acm", "", "", time.Minute, 0, nil)
	cs, ok := d.strategy.(*clusterStrategy)
	if !ok {
		t.Fatalf("strategy type = %T, want *clusterStrategy", d.strategy)
	}
	if cs.remote != "origin" {
		t.Errorf("remote = %q, want %q", cs.remote, "origin")
	}
	if cs.branch != "main" {
		t.Errorf("branch = %q, want %q", cs.branch, "main")
	}
	if d.log == nil {
		t.Error("log should default to log.Default(), got nil")
	}
}

func TestNewLocalDaemon_AppliesDefaults(t *testing.T) {
	d := NewLocalDaemon(time.Minute, time.Second, nil)
	if _, ok := d.strategy.(*localStrategy); !ok {
		t.Fatalf("strategy type = %T, want *localStrategy", d.strategy)
	}
	if d.log == nil {
		t.Error("log should default to log.Default(), got nil")
	}
}

func TestLoggerOr(t *testing.T) {
	custom := silentLogger()
	if got := loggerOr(custom); got != custom {
		t.Error("loggerOr should return the supplied logger")
	}
	if got := loggerOr(nil); got == nil {
		t.Error("loggerOr(nil) should return a non-nil logger")
	}
}

// waitFor polls cond until it returns true or a short timeout expires.
// Failures include the description so the test output points at which
// invariant never became true.
func waitFor(t *testing.T, cond func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("timed out waiting for: %s", desc))
}
