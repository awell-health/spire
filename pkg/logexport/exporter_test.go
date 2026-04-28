package logexport

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

// TestConfig_ValidateRejectsMissingRoot pins the construction-time
// error path so a misconfigured chart surfaces a clear typed message
// instead of a half-running exporter.
func TestConfig_ValidateRejectsMissingRoot(t *testing.T) {
	c := Config{Backend: BackendLocal}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "Root") {
		t.Errorf("Validate() = %v, want error mentioning Root", err)
	}
}

// TestConfig_ValidateRejectsGCSWithoutBucket asserts the GCS backend
// requires an explicit bucket name (the design forecloses bucket
// auto-detection).
func TestConfig_ValidateRejectsGCSWithoutBucket(t *testing.T) {
	c := Config{Root: "/tmp", Backend: BackendGCS}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "GCSBucket") {
		t.Errorf("Validate() = %v, want error mentioning GCSBucket", err)
	}
}

// TestConfig_ValidateRejectsUnknownBackend covers the typo path: a
// chart override like backend="gsc" should fail fast.
func TestConfig_ValidateRejectsUnknownBackend(t *testing.T) {
	c := Config{Root: "/tmp", Backend: "gsc"}
	if err := c.Validate(); err == nil {
		t.Errorf("Validate() = nil, want error for unknown backend")
	}
}

// TestConfig_ValidateAcceptsLocalDefaults asserts the empty-backend
// path coerces to local without error so local-native installs do not
// have to set LOGSTORE_BACKEND.
func TestConfig_ValidateAcceptsLocalDefaults(t *testing.T) {
	c := Config{Root: "/tmp"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	if c.EffectiveBackend() != BackendLocal {
		t.Errorf("EffectiveBackend = %q, want %q", c.EffectiveBackend(), BackendLocal)
	}
}

// TestConfig_EffectiveDefaultsCoerceZeros asserts the duration getters
// fall back to documented defaults when the field is zero.
func TestConfig_EffectiveDefaultsCoerceZeros(t *testing.T) {
	c := Config{Root: "/tmp"}
	if got := c.EffectiveScanInterval(); got != ScanIntervalDefault {
		t.Errorf("ScanInterval default = %s, want %s", got, ScanIntervalDefault)
	}
	if got := c.EffectiveIdleFinalize(); got != IdleFinalizeDefault {
		t.Errorf("IdleFinalize default = %s, want %s", got, IdleFinalizeDefault)
	}
	if got := c.EffectiveDrainDeadline(); got != DrainDeadlineDefault {
		t.Errorf("DrainDeadline default = %s, want %s", got, DrainDeadlineDefault)
	}
	if got := c.EffectiveVisibility(); got != logartifact.VisibilityEngineerOnly {
		t.Errorf("Visibility default = %q, want %q", got, logartifact.VisibilityEngineerOnly)
	}
}

// TestExporter_RunFlushClose drives the canonical lifecycle through
// the public Exporter interface so a refactor that breaks composition
// surfaces immediately. The exporter starts → tails an existing file →
// receives a cancel → flushes → closes.
func TestExporter_RunFlushClose(t *testing.T) {
	root := t.TempDir()
	store := newFakeStore()
	stdout := &bytes.Buffer{}

	cfg := Config{
		Root:         root,
		Backend:      BackendLocal,
		ScanInterval: 5 * time.Millisecond,
	}
	exp, err := NewExporter(cfg, store, stdout)
	if err != nil {
		t.Fatalf("NewExporter: %v", err)
	}

	writeFile(t, root, canonicalRel, []byte("alpha\nbeta\n"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- exp.Run(ctx) }()

	waitFor(t, "lines emitted", func() bool {
		return exp.Stats().LinesEmitted >= 2
	}, time.Second)

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v, want nil on cancel", err)
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	if err := exp.Flush(flushCtx); err != nil {
		t.Errorf("Flush: %v", err)
	}
	if err := exp.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	stats := exp.Stats()
	if stats.ArtifactsFinalized < 1 {
		t.Errorf("ArtifactsFinalized = %d, want >= 1", stats.ArtifactsFinalized)
	}
}

// TestExporter_RunRejectsAfterClose pins the misuse path: calling Run
// after Close should return an error rather than silently restarting.
func TestExporter_RunRejectsAfterClose(t *testing.T) {
	exp, err := NewExporter(Config{Root: t.TempDir(), Backend: BackendLocal}, newFakeStore(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewExporter: %v", err)
	}
	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err = exp.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "after Close") {
		t.Errorf("Run after Close = %v, want error mentioning Close", err)
	}
}

// TestNewExporter_RejectsNilStore covers the construction-time guard.
func TestNewExporter_RejectsNilStore(t *testing.T) {
	_, err := NewExporter(Config{Root: t.TempDir()}, nil, &bytes.Buffer{})
	if err == nil {
		t.Errorf("NewExporter(nil store) = nil, want error")
	}
}

// TestNewExporter_RejectsNilStdout covers the construction-time guard.
func TestNewExporter_RejectsNilStdout(t *testing.T) {
	_, err := NewExporter(Config{Root: t.TempDir()}, newFakeStore(), nil)
	if err == nil {
		t.Errorf("NewExporter(nil stdout) = nil, want error")
	}
}

// TestExporter_FailureModeVisible asserts the design's "exporter
// failure does not falsely mark agent work successful or failed, but
// it is visible in logs/manifest status" property: a finalize error
// produces an ERROR-severity stdout record, increments the failed
// counter, and Run still exits 0.
func TestExporter_FailureModeVisible(t *testing.T) {
	root := t.TempDir()
	store := newFakeStore()
	store.finalizeFailures = []error{
		errors.New("dolt connection reset"),
		errors.New("dolt connection reset"),
		errors.New("dolt connection reset"),
		errors.New("dolt connection reset"),
		errors.New("dolt connection reset"),
		errors.New("dolt connection reset"),
		errors.New("dolt connection reset"),
		errors.New("dolt connection reset"),
		errors.New("dolt connection reset"),
		errors.New("dolt connection reset"),
	}
	stdout := &bytes.Buffer{}

	cfg := Config{
		Root:         root,
		Backend:      BackendLocal,
		ScanInterval: 5 * time.Millisecond,
	}
	exp, err := NewExporter(cfg, store, stdout)
	if err != nil {
		t.Fatalf("NewExporter: %v", err)
	}

	writeFile(t, root, canonicalRel, []byte("alpha\n"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- exp.Run(ctx) }()

	waitFor(t, "alpha tailed", func() bool {
		return exp.Stats().LinesEmitted >= 1
	}, time.Second)

	cancel()
	runErr := <-done
	if runErr != nil {
		t.Errorf("Run returned %v after finalize failures, want nil (failures must not break Run)", runErr)
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	_ = exp.Flush(flushCtx) // informational; we just check observed state.

	stats := exp.Stats()
	if stats.ArtifactsFailed < 1 {
		t.Errorf("ArtifactsFailed = %d, want >= 1 (failure must be visible in stats)", stats.ArtifactsFailed)
	}
	if !strings.Contains(stdout.String(), `"severity":"ERROR"`) {
		t.Errorf("expected ERROR record on stdout for visibility; got %s", stdout.String())
	}
}
