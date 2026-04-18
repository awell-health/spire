package steward

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStrategy is a controllable syncStrategy for tests. Sync blocks on
// release (nil = don't block) and records call count.
type fakeStrategy struct {
	calls   atomic.Int32
	reasons []string
	mu      sync.Mutex
	err     error
	release chan struct{}
}

func (f *fakeStrategy) Describe() string { return "fake" }

func (f *fakeStrategy) Sync(_ context.Context, reason string) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.reasons = append(f.reasons, reason)
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	return f.err
}

func (f *fakeStrategy) lastReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reasons) == 0 {
		return ""
	}
	return f.reasons[len(f.reasons)-1]
}

func quietLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestRunOnce_Success(t *testing.T) {
	f := &fakeStrategy{}
	d := &Daemon{strategy: f, log: quietLogger()}

	if err := d.RunOnce("test"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
	if r := f.lastReason(); r != "test" {
		t.Errorf("reason = %q, want %q", r, "test")
	}
	// lastSync should be updated.
	if d.lastSync.IsZero() {
		t.Error("lastSync not updated after RunOnce")
	}
	if d.syncing {
		t.Error("syncing flag leaked after RunOnce")
	}
}

func TestRunOnce_SurfaceError(t *testing.T) {
	want := errors.New("boom")
	f := &fakeStrategy{err: want}
	d := &Daemon{strategy: f, log: quietLogger()}

	err := d.RunOnce("test")
	if !errors.Is(err, want) {
		t.Errorf("RunOnce err = %v, want %v", err, want)
	}
}

func TestRunOnce_RejectsWhenSyncing(t *testing.T) {
	release := make(chan struct{})
	f := &fakeStrategy{release: release}
	d := &Daemon{strategy: f, log: quietLogger()}

	// Start a Trigger that will block on release.
	if err := d.Trigger("first"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	waitUntilSyncing(t, d)

	// RunOnce should refuse while a sync is in progress.
	err := d.RunOnce("second")
	if err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Errorf("RunOnce while syncing: err = %v, want 'in progress'", err)
	}
	close(release)
}

func TestTrigger_AcceptsFirstCall(t *testing.T) {
	release := make(chan struct{})
	f := &fakeStrategy{release: release}
	d := &Daemon{strategy: f, log: quietLogger()}

	if err := d.Trigger("http"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	waitUntilSyncing(t, d)
	close(release)
	waitUntilIdle(t, d)

	if got := f.calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

func TestTrigger_RejectsConcurrent(t *testing.T) {
	release := make(chan struct{})
	f := &fakeStrategy{release: release}
	d := &Daemon{strategy: f, log: quietLogger()}

	if err := d.Trigger("first"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	waitUntilSyncing(t, d)

	err := d.Trigger("second")
	if err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Errorf("second Trigger: err = %v, want 'in progress'", err)
	}
	close(release)
	waitUntilIdle(t, d)

	if got := f.calls.Load(); got != 1 {
		t.Errorf("strategy calls = %d, want 1 (second should have been rejected)", got)
	}
}

func TestTrigger_Debounces(t *testing.T) {
	f := &fakeStrategy{}
	d := &Daemon{strategy: f, debounce: 100 * time.Millisecond, log: quietLogger()}

	// First call runs to completion.
	if err := d.Trigger("first"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	waitUntilIdle(t, d)

	// Immediate second call should be debounced.
	err := d.Trigger("second")
	if err == nil || !strings.Contains(err.Error(), "debounced") {
		t.Errorf("second Trigger: err = %v, want 'debounced'", err)
	}

	// After the debounce window passes, a Trigger succeeds.
	time.Sleep(120 * time.Millisecond)
	if err := d.Trigger("third"); err != nil {
		t.Errorf("third Trigger after debounce: %v", err)
	}
	waitUntilIdle(t, d)

	if got := f.calls.Load(); got != 2 {
		t.Errorf("strategy calls = %d, want 2 (first + third)", got)
	}
}

func TestTrigger_ZeroDebounceAllowsImmediateRetry(t *testing.T) {
	f := &fakeStrategy{}
	d := &Daemon{strategy: f, debounce: 0, log: quietLogger()}

	if err := d.Trigger("first"); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	waitUntilIdle(t, d)
	if err := d.Trigger("second"); err != nil {
		t.Errorf("second Trigger with zero debounce: %v", err)
	}
	waitUntilIdle(t, d)

	if got := f.calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

func TestRun_StartupAndCancel(t *testing.T) {
	f := &fakeStrategy{}
	d := &Daemon{strategy: f, interval: time.Hour, debounce: 0, log: quietLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Startup Trigger should fire roughly immediately.
	waitForCalls(t, f, 1)
	if r := f.lastReason(); r != "startup" {
		t.Errorf("first reason = %q, want 'startup'", r)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error after cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}

// --- helpers ---

func waitUntilSyncing(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		busy := d.syncing
		d.mu.Unlock()
		if busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for syncing=true")
}

func waitUntilIdle(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		busy := d.syncing
		d.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for syncing=false")
}

func waitForCalls(t *testing.T, f *fakeStrategy, n int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.calls.Load() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d strategy calls (got %d)", n, f.calls.Load())
}
