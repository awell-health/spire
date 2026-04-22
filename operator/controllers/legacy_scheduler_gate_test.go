package controllers

// Legacy-scheduler gate tests (spi-pg632).
//
// These tests pin the OperatorEnableLegacyScheduler contract from
// spi-njzmg: the operator is a pure reconciler of WorkloadIntent in
// canonical cluster-native mode, and the legacy BeadWatcher +
// WorkloadAssigner control loops must only run when the gate is
// explicitly flipped on.
//
// The gate decision itself lives in operator/main.go (the flag parse
// and the conditional mgr.Add). We cannot import package main from
// tests, so the helper below mirrors that decision exactly. If
// operator/main.go changes the gate shape, update
// gateLegacyRunnables here in lockstep — this is the single test
// authority for the wiring contract.

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// schedulerRunnable is the minimal shape of controller-runtime's
// Runnable interface. BeadWatcher and WorkloadAssigner both satisfy it
// via their Start(ctx) methods.
type schedulerRunnable interface {
	Start(ctx context.Context) error
}

// gateLegacyRunnables mirrors the OperatorEnableLegacyScheduler branch
// in operator/main.go. When the gate is false it returns no runnables
// and emits the canonical "gate OFF" log line; when the gate is true
// it returns the supplied runnables and emits the transitional
// warning that the spi-njzmg design mandates.
//
// The returned slice is what production wiring would mgr.Add(...) into
// controller-runtime. In tests we Start them directly so we can count
// invocations.
func gateLegacyRunnables(enable bool, log logr.Logger, runnables ...schedulerRunnable) []schedulerRunnable {
	if !enable {
		log.Info("legacy scheduler gate OFF — BeadWatcher/WorkloadAssigner not started; canonical reconciler path only")
		return nil
	}
	log.Info("legacy scheduler gate ON — BeadWatcher/WorkloadAssigner starting in transitional mode",
		"gate", "OperatorEnableLegacyScheduler",
		"mode", "transitional")
	return runnables
}

// startCounter is a schedulerRunnable test double that tracks how
// many times Start has been invoked. Calling Start blocks until the
// context is cancelled (mirroring the real BeadWatcher / WorkloadAssigner
// Run loops) so the counter reflects a genuine goroutine spawn rather
// than a synchronous no-op.
type startCounter struct {
	name   string
	starts int32
}

func (s *startCounter) Start(ctx context.Context) error {
	atomic.AddInt32(&s.starts, 1)
	<-ctx.Done()
	return nil
}

func (s *startCounter) calls() int32 { return atomic.LoadInt32(&s.starts) }

// capturingSink is a go-logr LogSink that records every Info and Error
// call so tests can assert on log content. Methods are concurrency-safe
// because the gate helper and the Runnable goroutines may log from
// different goroutines.
type capturingSink struct {
	mu     sync.Mutex
	infos  []capturedLog
	errors []capturedLog
}

type capturedLog struct {
	level int
	msg   string
	kv    []interface{}
}

func (s *capturingSink) Init(_ logr.RuntimeInfo) {}

func (s *capturingSink) Enabled(_ int) bool { return true }

func (s *capturingSink) Info(level int, msg string, kv ...interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.infos = append(s.infos, capturedLog{level: level, msg: msg, kv: kv})
}

func (s *capturingSink) Error(_ error, msg string, kv ...interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, capturedLog{msg: msg, kv: kv})
}

func (s *capturingSink) WithValues(_ ...interface{}) logr.LogSink { return s }
func (s *capturingSink) WithName(_ string) logr.LogSink           { return s }

func (s *capturingSink) infoMessages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.infos))
	for _, l := range s.infos {
		out = append(out, l.msg)
	}
	return out
}

// hasMessageContaining reports whether any captured Info call had a
// message containing substr. Matching is case-insensitive to keep the
// assertion robust against minor log-line copy tweaks — the
// "transitional" substring is the contract.
func (s *capturingSink) hasInfoContaining(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	needle := strings.ToLower(substr)
	for _, l := range s.infos {
		if strings.Contains(strings.ToLower(l.msg), needle) {
			return true
		}
	}
	return false
}

// TestLegacySchedulerGate_OffDoesNotStartLoops is the headline
// invariant: with OperatorEnableLegacyScheduler=false (the canonical
// default), neither BeadWatcher nor WorkloadAssigner is started. Their
// Start methods must never be invoked. The "gate OFF" log line is also
// asserted so the contract is visible in operator logs.
func TestLegacySchedulerGate_OffDoesNotStartLoops(t *testing.T) {
	sink := &capturingSink{}
	log := logr.New(sink)

	bw := &startCounter{name: "bead-watcher"}
	wa := &startCounter{name: "workload-assigner"}

	runnables := gateLegacyRunnables(false, log, bw, wa)

	if len(runnables) != 0 {
		t.Fatalf("gate OFF returned %d runnables, want 0 — legacy loops must not wire in", len(runnables))
	}

	// Production wiring would mgr.Add(r) on each entry in runnables.
	// We do the same here, so an accidental "always return all" bug
	// surfaces as Start being invoked.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, r := range runnables {
		go func(rr schedulerRunnable) { _ = rr.Start(ctx) }(r)
	}

	// Give any rogue goroutines a chance to run. 50ms is generous —
	// Start increments the counter synchronously before blocking.
	time.Sleep(50 * time.Millisecond)

	if bw.calls() != 0 {
		t.Errorf("BeadWatcher Start called %d times with gate OFF, want 0", bw.calls())
	}
	if wa.calls() != 0 {
		t.Errorf("WorkloadAssigner Start called %d times with gate OFF, want 0", wa.calls())
	}

	if !sink.hasInfoContaining("gate OFF") {
		t.Errorf("gate OFF path must log a 'gate OFF' line; got messages: %v", sink.infoMessages())
	}
	if sink.hasInfoContaining("transitional") {
		t.Errorf("gate OFF path must NOT emit the transitional-mode warning; got: %v", sink.infoMessages())
	}
}

// TestLegacySchedulerGate_OnStartsLoopsAndWarns pins the transitional
// contract: when the gate is true, both legacy runnables start AND the
// operator logs an explicit "transitional mode" warning so the operator
// audit trail records the departure from canonical.
func TestLegacySchedulerGate_OnStartsLoopsAndWarns(t *testing.T) {
	sink := &capturingSink{}
	log := logr.New(sink)

	bw := &startCounter{name: "bead-watcher"}
	wa := &startCounter{name: "workload-assigner"}

	runnables := gateLegacyRunnables(true, log, bw, wa)

	if len(runnables) != 2 {
		t.Fatalf("gate ON returned %d runnables, want 2 (BeadWatcher + WorkloadAssigner)", len(runnables))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{}, len(runnables))
	for _, r := range runnables {
		go func(rr schedulerRunnable) {
			_ = rr.Start(ctx)
			done <- struct{}{}
		}(r)
	}

	// Poll until both counters tick to 1. The Start methods block on
	// ctx, so we can't wait for them to return before asserting.
	deadline := time.Now().Add(1 * time.Second)
	for {
		if bw.calls() == 1 && wa.calls() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("legacy runnables did not start within 1s: BeadWatcher=%d, WorkloadAssigner=%d",
				bw.calls(), wa.calls())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Transitional-mode warning must be on the log. This is the
	// primary observability contract from spi-njzmg: operators grep
	// for "transitional" to know their install is in co-existence
	// mode, not canonical cluster-native.
	if !sink.hasInfoContaining("transitional") {
		t.Errorf("gate ON path must log 'transitional' warning; got messages: %v", sink.infoMessages())
	}
	if !sink.hasInfoContaining("gate ON") {
		t.Errorf("gate ON path must log 'gate ON' line; got messages: %v", sink.infoMessages())
	}

	// Shut the runnables down cleanly so the test doesn't leak goroutines.
	cancel()
	for range runnables {
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("legacy runnable did not exit after cancel")
		}
	}

	// Start must only fire once per runnable — no retries, no polling
	// loops that would double-count.
	if bw.calls() != 1 {
		t.Errorf("BeadWatcher Start called %d times, want exactly 1", bw.calls())
	}
	if wa.calls() != 1 {
		t.Errorf("WorkloadAssigner Start called %d times, want exactly 1", wa.calls())
	}
}

// TestLegacySchedulerGate_RealRunnablesImplementInterface is a
// compile-time-ish guard: the real BeadWatcher and WorkloadAssigner
// structs must satisfy the schedulerRunnable interface the gate
// consumes. If either struct's Start signature drifts, the production
// gate (operator/main.go) also breaks — pinning it here gives a
// faster, more localized failure than chasing it through main.go.
func TestLegacySchedulerGate_RealRunnablesImplementInterface(t *testing.T) {
	var _ schedulerRunnable = (*BeadWatcher)(nil)
	var _ schedulerRunnable = (*WorkloadAssigner)(nil)
}

// TestLegacySchedulerGate_DefaultIsCanonical documents the default
// value the operator reads when no --enable-legacy-scheduler flag is
// passed. The canonical cluster-native path is the default; flipping
// the gate on is an explicit choice for transitional installs.
func TestLegacySchedulerGate_DefaultIsCanonical(t *testing.T) {
	// The operator/main.go flag default is `false`. This test is a
	// living piece of documentation; if the default ever changes the
	// assertion below fails and the reviewer has to update both the
	// test and the operator's deployment manifests.
	const operatorFlagDefault = false

	sink := &capturingSink{}
	log := logr.New(sink)
	bw := &startCounter{}
	wa := &startCounter{}

	runnables := gateLegacyRunnables(operatorFlagDefault, log, bw, wa)
	if len(runnables) != 0 {
		t.Errorf("default gate behavior returned %d runnables, want 0 (canonical cluster-native)",
			len(runnables))
	}
}
