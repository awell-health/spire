package logexport

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestInProcess_StartAndShutdown drives the in-process embed through
// the same lifecycle the agent's main routine would: NewInProcess,
// Start, do work, Shutdown. The test asserts artifacts produced
// during the run are finalized by Shutdown.
func TestInProcess_StartAndShutdown(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		Root:         root,
		Backend:      BackendLocal,
		ScanInterval: 5 * time.Millisecond,
	}
	store := newFakeStore()
	stdout := &bytes.Buffer{}

	ip, err := NewInProcess(cfg, store, stdout)
	if err != nil {
		t.Fatalf("NewInProcess: %v", err)
	}

	ip.Start(context.Background())

	writeFile(t, root, canonicalRel, []byte("alpha\nbeta\n"))

	waitFor(t, "lines emitted", func() bool {
		return ip.Stats().LinesEmitted >= 2
	}, time.Second)

	if err := ip.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}

	stats := ip.Stats()
	if stats.ArtifactsFinalized < 1 {
		t.Errorf("ArtifactsFinalized = %d, want >= 1", stats.ArtifactsFinalized)
	}
	if ip.RunError() != nil {
		t.Errorf("RunError = %v, want nil", ip.RunError())
	}
}

// TestInProcess_ShutdownIdempotent ensures the embed survives a double
// shutdown — important for agent shutdown paths that may call into
// the same teardown more than once.
func TestInProcess_ShutdownIdempotent(t *testing.T) {
	cfg := Config{Root: t.TempDir(), Backend: BackendLocal, ScanInterval: 5 * time.Millisecond}
	ip, err := NewInProcess(cfg, newFakeStore(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewInProcess: %v", err)
	}
	ip.Start(context.Background())
	if err := ip.Shutdown(context.Background()); err != nil {
		t.Errorf("first Shutdown: %v", err)
	}
	if err := ip.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

// TestInProcess_StartTwiceIsNoop verifies the second Start call is
// silently a no-op so the embed survives a misbehaving caller.
func TestInProcess_StartTwiceIsNoop(t *testing.T) {
	cfg := Config{Root: t.TempDir(), Backend: BackendLocal, ScanInterval: 5 * time.Millisecond}
	ip, err := NewInProcess(cfg, newFakeStore(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewInProcess: %v", err)
	}
	ip.Start(context.Background())
	ip.Start(context.Background())
	if err := ip.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestFlushWithDeadline_HonorsTimeout pins the wrapper around
// Exporter.Flush: a deadline shorter than the flush operation's
// natural latency exits with an informational error rather than
// blocking the kubelet's pre-stop hook past terminationGracePeriod.
func TestFlushWithDeadline_HonorsTimeout(t *testing.T) {
	exp := &slowExporter{flushSleep: 200 * time.Millisecond}
	err, elapsed := FlushWithDeadline(context.Background(), exp, 50*time.Millisecond)
	if err == nil {
		t.Errorf("FlushWithDeadline: nil error, want context.DeadlineExceeded")
	}
	if elapsed >= 200*time.Millisecond {
		t.Errorf("elapsed = %s, want < flushSleep (deadline must short-circuit)", elapsed)
	}
}

// slowExporter is a minimal Exporter stand-in used by
// TestFlushWithDeadline_HonorsTimeout. The other tests use the real
// exporter; only the deadline test needs precise control over
// Flush()'s blocking behavior.
type slowExporter struct {
	flushSleep time.Duration
}

func (s *slowExporter) Run(ctx context.Context) error { return nil }
func (s *slowExporter) Flush(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.flushSleep):
		return nil
	}
}
func (s *slowExporter) Close() error { return nil }
func (s *slowExporter) Stats() Stats { return Stats{} }
