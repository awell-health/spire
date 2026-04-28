package logexport

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

// Exporter is the contract every passive log exporter implementation
// satisfies. The two production implementations are:
//
//   - The cmd/spire-log-exporter sidecar binary, which composes
//     NewExporter into a long-running process.
//   - The in-process embed (RunInProcess), which the agent process
//     starts as a goroutine when single-container pods are preferred.
//
// Tests construct Exporter with a fake artifact store so the tailer,
// stdout sink, and uploader stack can be exercised without touching
// GCS or a live database.
//
// Lifecycle:
//
//	e := NewExporter(...)
//	go e.Run(ctx)        // blocks until ctx is cancelled
//	...
//	cancel()             // begin shutdown
//	e.Flush(flushCtx)    // wait for in-flight artifacts to finalize
//	e.Close()            // release the underlying tailer + writers
//
// Calling Flush after Close is a no-op. Run must not be invoked twice
// on the same Exporter.
type Exporter interface {
	// Run drives the exporter's tailer and uploader goroutines until
	// ctx is cancelled or an unrecoverable internal error fires. The
	// caller usually pairs this with a SIGTERM/SIGINT handler that
	// cancels ctx and then calls Flush+Close.
	//
	// Manifest/upload failures are visible (status=failed rows + ERROR
	// stderr lines) but do NOT fail Run — the agent's success/failure
	// verdict must not depend on the exporter's manifest writes.
	Run(ctx context.Context) error

	// Flush blocks until every in-flight artifact has been finalized,
	// or until ctx's deadline elapses (whichever comes first).
	//
	// Returns nil when all open artifacts finalized cleanly; returns
	// the most recent finalize error when at least one finalize failed
	// or ctx expired before the queue drained. The error is informational
	// — callers should NOT exit non-zero on flush failure.
	Flush(ctx context.Context) error

	// Close releases the tailer, watcher, and any writer state. Idempotent.
	Close() error

	// Stats returns a snapshot of the exporter's operational counters.
	// Used by the binary's optional /healthz endpoint and by tests.
	Stats() Stats
}

// Stats is a snapshot of the exporter's operational counters. The fields
// are advisory — they exist so the sidecar's optional health surface and
// the tests can observe behavior without reaching into private state.
type Stats struct {
	// FilesTracked is the number of files the tailer is currently
	// watching.
	FilesTracked int64

	// BytesEmitted is the total number of bytes the tailer has read
	// since startup.
	BytesEmitted int64

	// LinesEmitted is the total number of records the StdoutSink has
	// emitted to stdout.
	LinesEmitted int64

	// ArtifactsFinalized is the number of artifacts that finalized
	// cleanly.
	ArtifactsFinalized int64

	// ArtifactsFailed is the number of artifacts whose manifest row
	// landed with status=failed (upload failure, manifest retry
	// exhaustion, or rotation-during-flush).
	ArtifactsFailed int64

	// ManifestRetries is the total number of bounded retry attempts
	// the manifest writer has performed across the exporter's lifetime.
	// A growing counter signals tower latency, not exporter health.
	ManifestRetries int64
}

// exporter is the canonical Exporter implementation. The sidecar binary
// and the in-process embed both wrap this type — the only difference
// between them is who owns the goroutine running Run.
type exporter struct {
	cfg    Config
	store  logartifact.Store
	stdout io.Writer

	// nowFn returns the current time. Tests substitute a deterministic
	// clock so tail/idle tests do not race on wall-clock timing.
	nowFn func() time.Time

	mu     sync.Mutex
	closed bool
	tailer *Tailer
	stats  *atomicStats
}

// NewExporter constructs an exporter from a validated Config plus the
// supporting collaborators. The Store is consumed by the upload path;
// the stdout writer receives one JSON record per tailed line; the clock
// is injected so tests can simulate time without sleeps.
//
// Returns an error when the config is invalid (the only construction-
// time check) so callers see the failure at startup, not on the first
// tail.
func NewExporter(cfg Config, store logartifact.Store, stdout io.Writer) (Exporter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("logexport: NewExporter: store must not be nil")
	}
	if stdout == nil {
		return nil, fmt.Errorf("logexport: NewExporter: stdout writer must not be nil")
	}
	return &exporter{
		cfg:    cfg,
		store:  store,
		stdout: stdout,
		nowFn:  time.Now,
		stats:  &atomicStats{},
	}, nil
}

// SetClock overrides the exporter's clock. Called only from tests that
// need to advance idle-finalize timers without sleeping.
func SetClock(e Exporter, clock func() time.Time) {
	if ee, ok := e.(*exporter); ok && clock != nil {
		ee.nowFn = clock
	}
}

// Run implements Exporter.
func (e *exporter) Run(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return fmt.Errorf("logexport: Run after Close")
	}
	if e.tailer != nil {
		e.mu.Unlock()
		return fmt.Errorf("logexport: Run already in progress")
	}
	t, err := newTailer(e.cfg, e.store, e.stdout, e.stats, e.nowFn, e.cfg.EffectiveVisibility())
	if err != nil {
		e.mu.Unlock()
		return err
	}
	e.tailer = t
	e.mu.Unlock()

	return t.Run(ctx)
}

// Flush implements Exporter.
func (e *exporter) Flush(ctx context.Context) error {
	e.mu.Lock()
	t := e.tailer
	e.mu.Unlock()
	if t == nil {
		return nil
	}
	return t.Flush(ctx)
}

// Close implements Exporter.
func (e *exporter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.tailer != nil {
		return e.tailer.Close()
	}
	return nil
}

// Stats implements Exporter.
func (e *exporter) Stats() Stats {
	return e.stats.Snapshot()
}
