package runctx

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

func TestAsyncFile_WriteAndCloseFlushesQueue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	w, err := NewAsyncFile(path)
	if err != nil {
		t.Fatalf("NewAsyncFile: %v", err)
	}

	for i := 0; i < 50; i++ {
		if _, err := w.Write([]byte("line\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	wantLines := bytes.Repeat([]byte("line\n"), 50)
	if !bytes.Equal(got, wantLines) {
		t.Fatalf("file contents mismatch: got=%q want=%q", got, wantLines)
	}
}

func TestAsyncFile_NonBlockingUnderBackpressure(t *testing.T) {
	// A tiny buffer + a barrage of writes should never block the
	// producer past a small constant — instead, the writer drops
	// bytes once the queue saturates. The invariant under test:
	// 1000 calls to Write complete in << "1000 × disk-write latency".
	dir := t.TempDir()
	path := filepath.Join(dir, "noisy.jsonl")

	w, err := NewAsyncFile(path, WithBufferLines(2))
	if err != nil {
		t.Fatalf("NewAsyncFile: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	const writeCount = 5000
	start := time.Now()
	var produced atomic.Int64
	for i := 0; i < writeCount; i++ {
		w.Write([]byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n"))
		produced.Add(1)
	}
	elapsed := time.Since(start)

	if produced.Load() != writeCount {
		t.Fatalf("produced = %d, want %d", produced.Load(), writeCount)
	}
	// 5000 non-blocking writes against a 2-slot buffer should be near-
	// instantaneous (microseconds-to-low-ms). A 1s threshold leaves
	// ample headroom for slow CI; anything past that means Write
	// blocked.
	if elapsed > time.Second {
		t.Fatalf("5000 writes took %s — Write blocked under backpressure", elapsed)
	}
	// Drops are scheduler-dependent on extremely fast disks; if a
	// machine drains faster than we can produce, drops can be zero.
	// Log it but don't fail.
	if w.Dropped() == 0 {
		t.Logf("note: zero drops — drain kept up with 5000 writes")
	}
}

func TestAsyncFile_WriteAfterCloseIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "closed.jsonl")

	w, err := NewAsyncFile(path)
	if err != nil {
		t.Fatalf("NewAsyncFile: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.Write([]byte("late\n")); err == nil {
		t.Fatalf("Write on closed AsyncFile must error")
	}
}

func TestAsyncFile_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "double.jsonl")

	w, err := NewAsyncFile(path)
	if err != nil {
		t.Fatalf("NewAsyncFile: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close should be no-op, got %v", err)
	}
}

func TestAsyncFile_RequiresAbsolutePath(t *testing.T) {
	if _, err := NewAsyncFile("relative.log"); err == nil {
		t.Fatalf("expected error for relative path")
	}
	if _, err := NewAsyncFile(""); err == nil {
		t.Fatalf("expected error for empty path")
	}
}

func TestAsyncFile_TruncatesOversizedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")

	w, err := NewAsyncFile(path, WithMaxLineBytes(8))
	if err != nil {
		t.Fatalf("NewAsyncFile: %v", err)
	}
	if _, err := w.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "01234567" {
		t.Fatalf("oversize write was not truncated to maxLine=8: %q", got)
	}
	if w.Dropped() != int64(len("89ABCDEF")) {
		t.Fatalf("Dropped = %d, want %d", w.Dropped(), len("89ABCDEF"))
	}
}

func TestAsyncFile_OperationalAndTranscript_LandUnderSamePrefix(t *testing.T) {
	// End-to-end check: open one operational log and one provider
	// transcript for a single RunContext, write a line to each, and
	// assert both files exist on disk under the same bead/attempt/run
	// prefix. This is the joint-write acceptance for spi-egw26j.
	root := t.TempDir()
	rc := fullRC()
	p := New(rc, root)

	if err := p.MkdirAll(); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	opPath, err := p.OperationalLog()
	if err != nil {
		t.Fatalf("OperationalLog: %v", err)
	}
	trPath, err := p.TranscriptFile("claude", logartifact.StreamStdout)
	if err != nil {
		t.Fatalf("TranscriptFile: %v", err)
	}

	// The transcript path includes a provider subdirectory the
	// per-run mkdir doesn't cover; create it explicitly so the
	// AsyncFile open succeeds.
	if err := os.MkdirAll(filepath.Dir(trPath), 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}

	op, err := NewAsyncFile(opPath)
	if err != nil {
		t.Fatalf("NewAsyncFile op: %v", err)
	}
	tr, err := NewAsyncFile(trPath)
	if err != nil {
		t.Fatalf("NewAsyncFile transcript: %v", err)
	}

	if _, err := op.Write([]byte("operational line\n")); err != nil {
		t.Fatalf("op.Write: %v", err)
	}
	if _, err := tr.Write([]byte(`{"type":"system"}` + "\n")); err != nil {
		t.Fatalf("tr.Write: %v", err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("op.Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("tr.Close: %v", err)
	}

	for _, path := range []string{opPath, trPath} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if st.Size() == 0 {
			t.Errorf("file %s empty after Close", path)
		}
	}
}

func TestAsyncFile_PathAccessor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accessor.jsonl")

	w, err := NewAsyncFile(path)
	if err != nil {
		t.Fatalf("NewAsyncFile: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if got := w.Path(); !strings.HasSuffix(got, "accessor.jsonl") {
		t.Errorf("Path() = %q, want suffix accessor.jsonl", got)
	}
}
