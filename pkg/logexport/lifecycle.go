package logexport

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// SignalCancelable returns a context that cancels on SIGTERM or SIGINT,
// plus a stop function the caller invokes during shutdown to release
// the signal handler.
//
// Used by the sidecar binary's main and by the in-process embed's
// caller. Tests construct context directly and skip this helper.
func SignalCancelable(parent context.Context) (context.Context, func()) {
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT, os.Interrupt)
	return ctx, cancel
}

// FlushWithDeadline calls e.Flush with a context whose deadline is
// derived from the configured drain deadline. Returns the flush error
// (informational; never propagated to the agent's exit status) and the
// elapsed wall-clock time for telemetry.
//
// The deadline is bounded above by ConfigDrainDeadline so a misbehaving
// upload backend can't hold the pod past terminationGracePeriodSeconds.
func FlushWithDeadline(parent context.Context, e Exporter, deadline time.Duration) (error, time.Duration) {
	if deadline <= 0 {
		deadline = DrainDeadlineDefault
	}
	start := time.Now()
	flushCtx, cancel := context.WithTimeout(parent, deadline)
	defer cancel()
	err := e.Flush(flushCtx)
	return err, time.Since(start)
}
