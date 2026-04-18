package steward

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStrategy is a syncStrategy that records calls and optionally blocks until
// released. Used to pin down exact concurrency behavior in Daemon.
type fakeStrategy struct {
	mu       sync.Mutex
	calls    int32
	block    chan struct{} // if non-nil, Sync waits on this before returning
	syncErr  error
	reasonCh chan string // if non-nil, receives the reason string on each call
}

func (f *fakeStrategy) Describe() string { return "fake" }

func (f *fakeStrategy) Sync(_ context.Context, reason string) error {
	atomic.AddInt32(&f.calls, 1)
	if f.reasonCh != nil {
		select {
		case f.reasonCh <- reason:
		default:
		}
	}
	if f.block != nil {
		<-f.block
	}
	return f.syncErr
}

func (f *fakeStrategy) callCount() int { return int(atomic.LoadInt32(&f.calls)) }

func newTestDaemon(strat syncStrategy, interval, debounce time.Duration) *Daemon {
	return &Daemon{
		strategy: strat,
		interval: interval,
		debounce: debounce,
		log:      log.New(io.Discard, "", 0),
	}
}

func TestDaemon_RunOnce_CallsStrategy(t *testing.T) {
	strat := &fakeStrategy{}
	d := newTestDaemon(strat, time.Minute, 0)

	if err := d.RunOnce("test"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := strat.callCount(); got != 1 {
		t.Fatalf("strategy call count = %d, want 1", got)
	}
}

func TestDaemon_RunOnce_RejectsWhenSyncing(t *testing.T) {
	strat := &fakeStrategy{block: make(chan struct{})}
	d := newTestDaemon(strat, time.Minute, 0)

	// Start the first sync in the background. It will block on strat.block.
	done := make(chan error, 1)
	go func() { done <- d.RunOnce("first") }()

	// Wait until the first sync has claimed the busy flag.
	if !waitForSyncing(d, true, time.Second) {
		t.Fatal("first RunOnce never set syncing=true")
	}

	// A second synchronous RunOnce must reject.
	if err := d.RunOnce("second"); err == nil {
		t.Fatal("second RunOnce returned nil, want in-progress error")
	}

	// Release the first and wait for it to complete.
	close(strat.block)
	if err := <-done; err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if got := strat.callCount(); got != 1 {
		t.Fatalf("strategy call count = %d, want 1 (second call must have been rejected)", got)
	}
}

func TestDaemon_Trigger_RejectsWhenSyncing(t *testing.T) {
	strat := &fakeStrategy{block: make(chan struct{})}
	d := newTestDaemon(strat, time.Minute, 0)

	if err := d.Trigger("first"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	if !waitForSyncing(d, true, time.Second) {
		t.Fatal("first Trigger never set syncing=true")
	}

	err := d.Trigger("second")
	if err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("second Trigger err = %v, want in-progress", err)
	}

	close(strat.block)
	if !waitForSyncing(d, false, time.Second) {
		t.Fatal("daemon never cleared syncing flag")
	}
	if got := strat.callCount(); got != 1 {
		t.Fatalf("strategy call count = %d, want 1", got)
	}
}

func TestDaemon_Trigger_DebouncesWithinWindow(t *testing.T) {
	strat := &fakeStrategy{}
	d := newTestDaemon(strat, time.Minute, 500*time.Millisecond)

	if err := d.Trigger("first"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	// Wait for the goroutine to finish and stamp lastSync.
	if !waitForSyncing(d, false, time.Second) {
		t.Fatal("first Trigger never completed")
	}

	err := d.Trigger("second")
	if err == nil || !strings.Contains(err.Error(), "debounced") {
		t.Fatalf("second Trigger err = %v, want debounced", err)
	}
	if got := strat.callCount(); got != 1 {
		t.Fatalf("strategy call count = %d, want 1 (second debounced)", got)
	}
}

func TestDaemon_Trigger_AllowsAfterDebounceWindow(t *testing.T) {
	strat := &fakeStrategy{}
	d := newTestDaemon(strat, time.Minute, 20*time.Millisecond)

	if err := d.Trigger("first"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	if !waitForSyncing(d, false, time.Second) {
		t.Fatal("first Trigger never completed")
	}

	time.Sleep(40 * time.Millisecond)

	if err := d.Trigger("second"); err != nil {
		t.Fatalf("second Trigger after debounce window: %v", err)
	}
	if !waitForSyncing(d, false, time.Second) {
		t.Fatal("second Trigger never completed")
	}
	if got := strat.callCount(); got != 2 {
		t.Fatalf("strategy call count = %d, want 2", got)
	}
}

func TestDaemon_Run_TickerTriggersSync(t *testing.T) {
	strat := &fakeStrategy{}
	d := newTestDaemon(strat, 30*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()

	// Run triggers a startup sync plus ticker syncs. Wait long enough for
	// at least two total (startup + ~2 ticks).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strat.callCount() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := strat.callCount(); got < 2 {
		t.Fatalf("strategy call count = %d, want >=2", got)
	}

	cancel()
	<-done
}

// waitForSyncing busy-polls d.syncing until it reaches want or timeout passes.
// Returns whether the desired state was observed.
func waitForSyncing(d *Daemon, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		cur := d.syncing
		d.mu.Unlock()
		if cur == want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
