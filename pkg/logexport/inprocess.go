package logexport

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/awell-health/spire/pkg/logartifact"
)

// InProcess is the goroutine-embedded Exporter used by installs that
// keep single-container pods. The agent process constructs an InProcess
// at startup, calls Start to spawn the exporter goroutine, and calls
// Shutdown during its own teardown to flush in-flight artifacts.
//
// Same Exporter contract as the sidecar binary; the only difference is
// who owns the goroutine.
type InProcess struct {
	exp Exporter

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	runErr error
}

// NewInProcess constructs an InProcess Exporter. cfg, store, and stdout
// are passed through to NewExporter; failures at config validation or
// store-handle nil-checking surface here.
func NewInProcess(cfg Config, store logartifact.Store, stdout io.Writer) (*InProcess, error) {
	exp, err := NewExporter(cfg, store, stdout)
	if err != nil {
		return nil, err
	}
	return &InProcess{exp: exp}, nil
}

// Start spawns the exporter goroutine. Returns immediately; the
// goroutine runs until Shutdown is called or its context is cancelled.
//
// Calling Start twice is a no-op on the second call.
func (ip *InProcess) Start(parent context.Context) {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	if ip.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	ip.cancel = cancel
	ip.done = make(chan struct{})
	go func() {
		defer close(ip.done)
		err := ip.exp.Run(ctx)
		ip.mu.Lock()
		ip.runErr = err
		ip.mu.Unlock()
	}()
}

// Shutdown cancels the exporter's run context, flushes in-flight
// artifacts (capped by the configured drain deadline), and closes the
// exporter. Returns the flush error if any (informational).
//
// Idempotent: calling Shutdown twice returns nil from the second call.
func (ip *InProcess) Shutdown(parent context.Context) error {
	ip.mu.Lock()
	cancel := ip.cancel
	done := ip.done
	ip.cancel = nil
	ip.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if done != nil {
		<-done
	}
	flushErr, _ := FlushWithDeadline(parent, ip.exp, 0)
	closeErr := ip.exp.Close()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

// Stats returns the underlying exporter's stats snapshot.
func (ip *InProcess) Stats() Stats {
	return ip.exp.Stats()
}

// RunError returns the run error from the most recent Start, if any.
// Useful for tests that want to detect a Run-time failure without
// blocking on Shutdown.
func (ip *InProcess) RunError() error {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	return ip.runErr
}

// String renders the InProcess for debug logs.
func (ip *InProcess) String() string {
	return fmt.Sprintf("logexport.InProcess(stats=%+v)", ip.Stats())
}
