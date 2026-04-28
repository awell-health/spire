package logexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

// tailerTestKit composes the collaborators a typical tailer test needs.
type tailerTestKit struct {
	root     string
	stdout   *bytes.Buffer
	stats    *atomicStats
	tailer   *Tailer
	store    *fakeStore
	uploader *Uploader
}

func newTailerTestKit(t *testing.T) *tailerTestKit {
	t.Helper()
	root := t.TempDir()
	stats := &atomicStats{}
	stdout := &bytes.Buffer{}

	store := newFakeStore()

	cfg := Config{
		Root:         root,
		Backend:      BackendLocal, // backend value is informational here; tests inject a fakeStore.
		ScanInterval: 5 * time.Millisecond,
		IdleFinalize: 0, // disable idle for deterministic tests.
	}
	tailer, err := newTailer(cfg, store, stdout, stats, time.Now, logartifact.VisibilityEngineerOnly)
	if err != nil {
		t.Fatalf("newTailer: %v", err)
	}
	tailer.uploader.SetRetryPolicy(noRetry)

	return &tailerTestKit{
		root:     root,
		stdout:   stdout,
		stats:    stats,
		tailer:   tailer,
		store:    store,
		uploader: tailer.uploader,
	}
}

// writeFile creates path under root with bytes, returning the absolute
// path. Mkdir for parents is implicit so tests can declare deeply-
// nested artifact paths in a single call.
func writeFile(t *testing.T, root, rel string, data []byte) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", abs, err)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
	return abs
}

// appendFile appends bytes to an existing file.
func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open append %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

const (
	canonicalRel = "tower-a/spi-bead/spi-attempt/run-001/wizard-spi-bead/wizard/implement/operational.log"
	transcriptRel = "tower-a/spi-bead/spi-attempt/run-001/wizard-spi-bead/wizard/implement/claude/transcript.jsonl"
)

// TestTailer_DiscoversAndEmitsExistingFiles asserts the first scan
// picks up files that already exist when the tailer starts. Each line
// turns into one StdoutSink record; the file's bytes flow through the
// uploader so a subsequent finalize records the right size + checksum.
func TestTailer_DiscoversAndEmitsExistingFiles(t *testing.T) {
	kit := newTailerTestKit(t)

	payload := []byte("line one\nline two\n")
	writeFile(t, kit.root, canonicalRel, payload)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = kit.tailer.Run(ctx) }()
	waitFor(t, "lines emitted", func() bool {
		return kit.stats.Snapshot().LinesEmitted >= 2
	}, time.Second)

	cancel()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	if err := kit.tailer.Flush(flushCtx); err != nil {
		t.Errorf("Flush: %v", err)
	}

	manifests, _ := kit.store.List(context.Background(), logartifact.Filter{BeadID: "spi-bead"})
	if len(manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(manifests))
	}
	m := manifests[0]
	if m.Status != logartifact.StatusFinalized {
		t.Errorf("Status = %q, want %q", m.Status, logartifact.StatusFinalized)
	}
	if m.ByteSize != int64(len(payload)) {
		t.Errorf("ByteSize = %d, want %d", m.ByteSize, len(payload))
	}
	wantHash := sha256.Sum256(payload)
	if m.Checksum != "sha256:"+hex.EncodeToString(wantHash[:]) {
		t.Errorf("Checksum = %q, want sha256:%x", m.Checksum, wantHash)
	}
}

// TestTailer_AppendedBytesEmittedIncrementally writes a file, then
// appends two more chunks while the tailer is running. The stdout sink
// must emit one record per appended line.
func TestTailer_AppendedBytesEmittedIncrementally(t *testing.T) {
	kit := newTailerTestKit(t)

	abs := writeFile(t, kit.root, canonicalRel, []byte("first\n"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = kit.tailer.Run(ctx) }()

	waitFor(t, "first line emitted", func() bool {
		return kit.stats.Snapshot().LinesEmitted >= 1
	}, time.Second)

	appendFile(t, abs, []byte("second\nthird\n"))

	waitFor(t, "all three lines emitted", func() bool {
		return kit.stats.Snapshot().LinesEmitted >= 3
	}, time.Second)

	cancel()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	_ = kit.tailer.Flush(flushCtx)

	if !strings.Contains(kit.stdout.String(), "first") ||
		!strings.Contains(kit.stdout.String(), "second") ||
		!strings.Contains(kit.stdout.String(), "third") {
		t.Errorf("stdout missing expected lines: %s", kit.stdout.String())
	}
}

// TestTailer_RotationFinalizesAndAdvancesSequence simulates atomic log
// rotation: the canonical path is replaced by a fresh file (new inode)
// via rename. The tailer should finalize the original artifact at
// sequence 0 and open a new one at sequence 1 for the replacement.
func TestTailer_RotationFinalizesAndAdvancesSequence(t *testing.T) {
	kit := newTailerTestKit(t)

	abs := writeFile(t, kit.root, canonicalRel, []byte("alpha\n"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = kit.tailer.Run(ctx) }()

	waitFor(t, "first artifact opened", func() bool {
		return kit.uploader.Tracking() >= 1
	}, time.Second)
	waitFor(t, "alpha emitted", func() bool {
		return kit.stats.Snapshot().LinesEmitted >= 1
	}, time.Second)

	// Atomic rotation: write a replacement to a side path under the
	// same parent dir, then rename onto the canonical path. The path
	// is always present; the inode changes mid-scan.
	replacement := abs + ".new"
	if err := os.WriteFile(replacement, []byte("beta\n"), 0o644); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Rename(replacement, abs); err != nil {
		t.Fatalf("rename: %v", err)
	}

	waitFor(t, "beta emitted", func() bool {
		return kit.stats.Snapshot().LinesEmitted >= 2
	}, 2*time.Second)

	cancel()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	_ = kit.tailer.Flush(flushCtx)

	manifests, _ := kit.store.List(context.Background(), logartifact.Filter{BeadID: "spi-bead"})
	if len(manifests) < 2 {
		t.Fatalf("manifests = %d, want >= 2 (rotation should produce a new sequence)", len(manifests))
	}
	seqs := make(map[int]bool)
	for _, m := range manifests {
		seqs[m.Sequence] = true
		if m.Status != logartifact.StatusFinalized {
			t.Errorf("artifact seq=%d Status = %q, want %q", m.Sequence, m.Status, logartifact.StatusFinalized)
		}
	}
	if !seqs[0] {
		t.Errorf("missing sequence 0 manifest among %v", seqs)
	}
	if !seqs[1] {
		t.Errorf("missing sequence 1 manifest (rotation must advance sequence) among %v", seqs)
	}
}

// TestTailer_TruncationFinalizesAndRestarts shrinks the file in place
// (same inode, smaller size). The tailer should treat that as rotation
// — finalize the current artifact, advance sequence, restart at 0.
func TestTailer_TruncationFinalizesAndRestarts(t *testing.T) {
	kit := newTailerTestKit(t)

	abs := writeFile(t, kit.root, canonicalRel, []byte("alpha\nbeta\n"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = kit.tailer.Run(ctx) }()

	waitFor(t, "first artifact opened", func() bool {
		return kit.uploader.Tracking() >= 1
	}, time.Second)
	waitFor(t, "alpha + beta emitted", func() bool {
		return kit.stats.Snapshot().LinesEmitted >= 2
	}, time.Second)

	if err := os.Truncate(abs, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := os.WriteFile(abs, []byte("gamma\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	waitFor(t, "gamma emitted", func() bool {
		return kit.stats.Snapshot().LinesEmitted >= 3
	}, 2*time.Second)

	cancel()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	_ = kit.tailer.Flush(flushCtx)

	manifests, _ := kit.store.List(context.Background(), logartifact.Filter{BeadID: "spi-bead"})
	if len(manifests) < 2 {
		t.Errorf("manifests = %d, want >= 2 (truncation should produce a new sequence)", len(manifests))
	}
}

// TestTailer_RejectsNonCanonicalFiles asserts the path-parser guards
// against random files in the log root. A file at a non-canonical path
// should be silently skipped — no manifest row, no stdout record.
func TestTailer_RejectsNonCanonicalFiles(t *testing.T) {
	kit := newTailerTestKit(t)

	// Wrong segment count.
	writeFile(t, kit.root, "tower-a/operational.log", []byte("noise\n"))
	// Wrong leaf extension.
	writeFile(t, kit.root, "tower-a/spi-bead/spi-attempt/run-001/wizard-spi-bead/wizard/implement/note.txt", []byte("noise\n"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = kit.tailer.Run(ctx) }()
	time.Sleep(100 * time.Millisecond) // give a couple of scan cycles

	cancel()
	_ = kit.tailer.Flush(context.Background())

	if got := kit.stats.Snapshot().LinesEmitted; got != 0 {
		t.Errorf("LinesEmitted = %d, want 0 (no canonical files)", got)
	}
	manifests, _ := kit.store.List(context.Background(), logartifact.Filter{BeadID: "spi-bead"})
	if len(manifests) != 0 {
		t.Errorf("manifests = %d, want 0", len(manifests))
	}
}

// TestTailer_DisappearedFileFinalizes covers the cleanup path: a
// tracked file removed from disk should finalize the artifact in the
// next scan.
func TestTailer_DisappearedFileFinalizes(t *testing.T) {
	kit := newTailerTestKit(t)

	abs := writeFile(t, kit.root, transcriptRel, []byte(`{"event":"hi"}`+"\n"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = kit.tailer.Run(ctx) }()

	waitFor(t, "tracking", func() bool {
		return kit.uploader.Tracking() >= 1
	}, time.Second)

	if err := os.Remove(abs); err != nil {
		t.Fatalf("remove: %v", err)
	}

	waitFor(t, "finalized after disappearance", func() bool {
		return kit.stats.Snapshot().ArtifactsFinalized >= 1
	}, 2*time.Second)

	cancel()
	_ = kit.tailer.Flush(context.Background())
}

// TestTailer_FlushFinalizesOpenArtifacts asserts shutdown semantics:
// any artifact still open at Flush time becomes finalized rather than
// abandoned.
func TestTailer_FlushFinalizesOpenArtifacts(t *testing.T) {
	kit := newTailerTestKit(t)
	writeFile(t, kit.root, transcriptRel, []byte(`{"event":"a"}`+"\n"+`{"event":"b"}`+"\n"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = kit.tailer.Run(ctx) }()

	waitFor(t, "lines emitted", func() bool {
		return kit.stats.Snapshot().LinesEmitted >= 2
	}, time.Second)

	cancel()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	if err := kit.tailer.Flush(flushCtx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := kit.stats.Snapshot().ArtifactsFinalized; got < 1 {
		t.Errorf("ArtifactsFinalized = %d, want >= 1", got)
	}
}

// TestTailer_PartialLineCompletesAcrossScans pins the regression for
// the buffer/offset duplication bug fixed in round 3: when a file is
// written without a trailing newline, multiple scan cycles see the
// partial bytes. The fix advances tf.offset past every byte read on
// each scan so subsequent reads do not re-pull the partial tail. When
// the line eventually completes with a newline, the emitted line and
// the artifact's bytes/checksum must equal the original file content
// exactly — no duplication.
func TestTailer_PartialLineCompletesAcrossScans(t *testing.T) {
	kit := newTailerTestKit(t)

	// Initial write: complete line + partial-line tail (no newline).
	abs := writeFile(t, kit.root, canonicalRel, []byte("alpha\nbeta"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = kit.tailer.Run(ctx) }()

	// First complete line emits immediately; the partial "beta" stays
	// in the carry-over buffer until a newline arrives.
	waitFor(t, "alpha emitted", func() bool {
		return kit.stats.Snapshot().LinesEmitted >= 1
	}, time.Second)

	// Let several scan cycles run with the file unchanged so the
	// (broken) old code would re-read "beta" from disk on every cycle
	// and double-append it to the uploader. The new code advances
	// tf.offset past all bytes read — no re-read.
	time.Sleep(50 * time.Millisecond)

	// Append the missing newline. The next scan combines the carry-
	// over "beta" with the new "\n" and emits exactly one "beta" line.
	appendFile(t, abs, []byte("\n"))

	waitFor(t, "beta emitted", func() bool {
		return kit.stats.Snapshot().LinesEmitted >= 2
	}, time.Second)

	cancel()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	if err := kit.tailer.Flush(flushCtx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	stdout := kit.stdout.String()
	// The emitted "beta" line must equal the original — no duplication
	// (e.g. "betabeta") from the partial-line tail being read twice.
	if strings.Count(stdout, `"message":"beta"`) != 1 {
		t.Errorf("expected exactly one beta line, got stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "betabeta") {
		t.Errorf("found duplicated partial-line content in stdout:\n%s", stdout)
	}

	// Most important acceptance criterion: artifact byte size and
	// checksum match the original file exactly. Old code re-appended
	// "beta" on each scan — N scans of a partial line → N duplicates
	// in the artifact, breaking the checksum.
	want := []byte("alpha\nbeta\n")
	manifests, _ := kit.store.List(context.Background(), logartifact.Filter{BeadID: "spi-bead"})
	if len(manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(manifests))
	}
	m := manifests[0]
	if m.ByteSize != int64(len(want)) {
		t.Errorf("ByteSize = %d, want %d (partial-line bytes must not be double-appended)",
			m.ByteSize, len(want))
	}
	wantHash := sha256.Sum256(want)
	wantChecksum := "sha256:" + hex.EncodeToString(wantHash[:])
	if m.Checksum != wantChecksum {
		t.Errorf("Checksum = %q, want %q (artifact must equal file byte-for-byte)",
			m.Checksum, wantChecksum)
	}
}

// TestSplitLines covers the line-splitter directly. The helper is
// reused on every scan; correctness is load-bearing for offset math
// and stdout emission.
func TestSplitLines(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantLines []string
		wantRem   string
	}{
		{"empty", "", nil, ""},
		{"one full line", "alpha\n", []string{"alpha"}, ""},
		{"two full lines", "alpha\nbeta\n", []string{"alpha", "beta"}, ""},
		{"trailing partial", "alpha\nbeta", []string{"alpha"}, "beta"},
		{"crlf line", "alpha\r\nbeta\n", []string{"alpha", "beta"}, ""},
		{"only partial", "alpha", nil, "alpha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines, rem := splitLines([]byte(tc.input))
			if len(lines) != len(tc.wantLines) {
				t.Fatalf("lines = %d, want %d", len(lines), len(tc.wantLines))
			}
			for i, want := range tc.wantLines {
				if string(lines[i].bytes) != want {
					t.Errorf("lines[%d] = %q, want %q", i, lines[i].bytes, want)
				}
			}
			if string(rem) != tc.wantRem {
				t.Errorf("remainder = %q, want %q", rem, tc.wantRem)
			}
		})
	}
}

// waitFor polls cond until it returns true or timeout elapses. Failed
// polls don't block on a fixed sleep — the tailer's scan cadence is in
// the millisecond range, so we re-check rapidly.
func waitFor(t *testing.T, label string, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waitFor(%s): timed out after %s", label, timeout)
}
