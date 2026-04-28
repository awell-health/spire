package logexport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

// Tailer scans the shared log directory under cfg.Root, tails each
// canonical artifact file, and routes lines through the StdoutSink and
// the Uploader. It is the only component in pkg/logexport with a
// long-running goroutine.
//
// Design notes:
//
//   - Polling, not inotify. fsnotify was mentioned in the plan but the
//     polling fallback is simpler, has no platform-specific behavior,
//     and the production cadence (1s default) is well below human-
//     observable latency. Switching to inotify is a future change.
//
//   - File rotation: detected by inode change. The previous artifact is
//     finalized at the existing sequence; the next batch of bytes lands
//     at sequence+1.
//
//   - Truncation: a smaller file size than the last observed offset is
//     treated as rotation — finalize, advance sequence, restart at 0.
//
//   - Idle finalize: a file that has not grown for IdleFinalize seconds
//     is finalized and forgotten. If subsequent writes arrive, they
//     start a fresh artifact at sequence+1. This protects against the
//     "agent crashed without rotating" failure mode.
//
//   - Disappearance: deleting a tracked file finalizes the artifact and
//     drops the tracker.
type Tailer struct {
	cfg          Config
	store        logartifact.Store
	sink         *StdoutSink
	uploader     *Uploader
	stats        *atomicStats
	scanInterval time.Duration
	idleFinalize time.Duration

	now func() time.Time

	mu       sync.Mutex
	tracked  map[string]*trackedFile
	closed   bool
	finished chan struct{}
}

// trackedFile is the per-file state the tailer maintains. The fields are
// set under Tailer.mu; per-file workers do not hold a long-running lock.
type trackedFile struct {
	path       string
	identity   logartifact.Identity
	inode      uint64
	device     uint64
	size       int64
	offset     int64
	sequence   int
	lastWrite  time.Time
	buffer     []byte // partial-line carry-over across reads
	closed     bool
}

// newTailer constructs a Tailer. db may be nil — the uploader treats
// it as best-effort for status updates.
func newTailer(cfg Config, store logartifact.Store, stdout io.Writer, stats *atomicStats, now func() time.Time, visibility logartifact.Visibility) (*Tailer, error) {
	sink, err := NewStdoutSink(stdout, stats)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	sink.SetClock(now)
	uploader, err := NewUploader(store, dbFromStore(store), visibility, sink, stats)
	if err != nil {
		return nil, err
	}
	return &Tailer{
		cfg:          cfg,
		store:        store,
		sink:         sink,
		uploader:     uploader,
		stats:        stats,
		scanInterval: cfg.EffectiveScanInterval(),
		idleFinalize: cfg.EffectiveIdleFinalize(),
		now:          now,
		tracked:      make(map[string]*trackedFile),
		finished:     make(chan struct{}),
	}, nil
}

// dbFromStore extracts the *sql.DB from a logartifact.Store when one is
// available. The substrate's LocalStore and GCSStore both expose the
// db they were constructed with through a pkg-private accessor; for
// callers that injected a custom Store (typically tests), the result is
// nil and the Uploader's status-update path becomes a no-op.
func dbFromStore(store logartifact.Store) *sql.DB {
	type dbHolder interface {
		DB() *sql.DB
	}
	if s, ok := store.(dbHolder); ok {
		return s.DB()
	}
	return nil
}

// Run drives the tailer's scan loop until ctx is cancelled. Returns
// nil on clean cancel; never returns a non-context error because the
// exporter is designed to keep running through transient failures.
func (t *Tailer) Run(ctx context.Context) error {
	defer close(t.finished)

	// One immediate scan at startup so the first batch of artifacts
	// gets registered without waiting for the first tick.
	t.scan(ctx)

	ticker := time.NewTicker(t.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if t.isClosed() {
				return nil
			}
			t.scan(ctx)
		}
	}
}

// Flush waits up to ctx.Deadline for every open artifact to finalize.
// Returns the most recent finalize error or ctx.Err if the deadline
// elapsed before all artifacts drained.
func (t *Tailer) Flush(ctx context.Context) error {
	// One final scan so any unread bytes are emitted before close.
	t.scan(ctx)

	if err := t.closeAllTracked(ctx, "flush"); err != nil {
		return err
	}
	return t.uploader.CloseAll(ctx, "flush")
}

// Close releases tailer state. Idempotent.
func (t *Tailer) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	return nil
}

func (t *Tailer) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// scan walks the configured root and updates per-file state. Newly
// observed canonical artifact files are added; tracked files are
// advanced; files that disappeared from disk are finalized and
// forgotten.
func (t *Tailer) scan(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	root := t.cfg.Root
	if _, err := os.Stat(root); err != nil {
		// Root may not exist yet (e.g. emptyDir mounted but agent
		// hasn't created any files). Treat as nothing-to-do.
		return
	}

	seen := make(map[string]struct{})
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Best-effort: don't abort the walk on a single unreadable
			// dir, just skip it.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		info, ok := ParsePath(rel)
		if !ok {
			return nil
		}
		seen[path] = struct{}{}
		t.advance(ctx, path, info)
		return nil
	})
	if walkErr != nil {
		// Walk-root errors aren't actionable from here — emit as an
		// operational warning and continue.
		t.sink.EmitOperational(SeverityError, t.cfg.Root, "logexport: walk failed", map[string]string{
			"root":  root,
			"error": walkErr.Error(),
		})
	}

	// Disappeared files: anything in tracked but not in seen has been
	// removed (file rotated by deletion + recreate, or operator-driven
	// cleanup). Finalize and drop the tracker so the next appearance
	// starts fresh.
	t.mu.Lock()
	gone := make([]*trackedFile, 0)
	for path, tf := range t.tracked {
		if _, ok := seen[path]; ok {
			continue
		}
		gone = append(gone, tf)
		delete(t.tracked, path)
	}
	t.mu.Unlock()
	for _, tf := range gone {
		t.finalize(ctx, tf, "disappeared")
	}

	// Idle finalize: any tracked file that hasn't grown for
	// IdleFinalize duration is closed and dropped. The next write
	// reopens at sequence+1.
	if t.idleFinalize > 0 {
		now := t.now()
		t.mu.Lock()
		idle := make([]*trackedFile, 0)
		for path, tf := range t.tracked {
			if !tf.lastWrite.IsZero() && now.Sub(tf.lastWrite) > t.idleFinalize {
				idle = append(idle, tf)
				delete(t.tracked, path)
			}
		}
		t.mu.Unlock()
		for _, tf := range idle {
			t.finalize(ctx, tf, "idle")
		}
	}
}

// advance updates the per-file state for path with the provided
// PathInfo (Identity + classification). Reads any newly-appended bytes
// off disk, splits them into lines, and routes them through the sink
// and uploader.
func (t *Tailer) advance(ctx context.Context, path string, info PathInfo) {
	stat, err := os.Stat(path)
	if err != nil {
		// File vanished between WalkDir and Stat — handled in next
		// scan as a disappearance.
		return
	}
	inode, device := fileInodeDevice(stat)

	t.mu.Lock()
	tf, ok := t.tracked[path]
	if !ok {
		tf = &trackedFile{
			path:     path,
			identity: info.Identity,
			inode:    inode,
			device:   device,
			size:     stat.Size(),
			offset:   0,
			sequence: 0,
		}
		t.tracked[path] = tf
		if t.stats != nil {
			t.stats.addFiles(1)
		}
	}
	t.mu.Unlock()

	// Detect rotation/truncation. A new inode means the file the agent
	// is now writing to is not the one we were tailing — finalize the
	// old artifact and bump sequence.
	rotated := false
	if tf.inode != inode || tf.device != device {
		rotated = true
	}
	if stat.Size() < tf.offset {
		// Truncation: same inode but smaller size. Treat as rotation
		// for upload purposes — the bytes beyond the new size are
		// gone, and the next bytes belong to a fresh artifact.
		rotated = true
	}
	if rotated {
		t.finalize(ctx, tf, "rotated")
		tf.sequence++
		tf.inode = inode
		tf.device = device
		tf.offset = 0
		tf.size = stat.Size()
		tf.buffer = nil
		tf.closed = false
	}

	if stat.Size() <= tf.offset {
		// Nothing new to read; update size for next-scan comparison.
		tf.size = stat.Size()
		return
	}

	data, n, err := readBytes(path, tf.offset)
	if err != nil {
		t.sink.EmitOperational(SeverityError, info.Identity.Tower, "logexport: read tail failed", map[string]string{
			"path":   path,
			"offset": fmt.Sprintf("%d", tf.offset),
			"error":  err.Error(),
		})
		return
	}
	if n == 0 {
		return
	}

	// Carry over any partial line from previous reads so a JSON record
	// split across two scan cycles emits exactly once.
	combined := tf.buffer
	combined = append(combined, data...)
	lines, remainder := splitLines(combined)
	tf.buffer = remainder

	for _, line := range lines {
		// Emit one stdout record per line. Offsets passed to the sink
		// are the byte offset of the line's first byte in the file —
		// not the offset of the next read — so live-follow consumers
		// can de-duplicate by (file, offset).
		t.sink.Emit(info.Identity, path, info.Sequence+tf.sequence, tf.offset, line.bytes)
		tf.offset += int64(len(line.fullBytes))
	}
	if t.stats != nil {
		t.stats.addBytes(n)
	}

	// Push the full slab (including the trailing partial line that
	// might land in the next scan) into the uploader so the artifact
	// preserves byte-for-byte fidelity. The uploader appends to the
	// open writer; the running checksum and size land on Finalize.
	if err := t.uploader.Append(ctx, info.Identity, tf.sequence+info.Sequence, data); err != nil {
		// Append failure already accounted by Uploader; nothing more
		// to do here besides record the lastWrite stamp so the idle-
		// finalize path treats the file as recently active. The
		// uploader entry is now in failed state; subsequent writes
		// short-circuit through errEntryFailed/closed without re-
		// dropping bytes.
		_ = err
	}
	tf.size = stat.Size()
	tf.lastWrite = t.now()
}

// finalize closes the uploader entry for tf and decrements the tracked
// counter.
func (t *Tailer) finalize(ctx context.Context, tf *trackedFile, reason string) {
	if tf == nil || tf.closed {
		return
	}
	tf.closed = true
	if err := t.uploader.Close(ctx, tf.identity, tf.sequence, reason); err != nil {
		// Already accounted in uploader stats; no further action here.
		_ = err
	}
	if t.stats != nil {
		t.stats.addFiles(-1)
	}
}

// closeAllTracked finalizes every tracked file. Called from Flush
// during shutdown.
func (t *Tailer) closeAllTracked(ctx context.Context, reason string) error {
	t.mu.Lock()
	files := make([]*trackedFile, 0, len(t.tracked))
	for _, tf := range t.tracked {
		files = append(files, tf)
	}
	t.tracked = make(map[string]*trackedFile)
	t.mu.Unlock()

	var lastErr error
	for _, tf := range files {
		t.finalize(ctx, tf, reason)
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			lastErr = ctx.Err()
		}
	}
	return lastErr
}

// line is a parsed slice of the per-file read. fullBytes carries the
// trailing newline (if any) so the offset advance matches what's in the
// file; bytes is the trimmed payload the sink emits.
type line struct {
	bytes     []byte // trimmed of trailing newline
	fullBytes []byte // includes trailing newline; used for offset accounting
}

// splitLines splits buf into complete lines plus a trailing remainder
// that holds the last partial line (if any). The remainder is empty
// when buf ends on a newline, so a clean read leaves no carry-over.
func splitLines(buf []byte) ([]line, []byte) {
	if len(buf) == 0 {
		return nil, buf
	}
	var lines []line
	start := 0
	for i := 0; i < len(buf); i++ {
		if buf[i] == '\n' {
			full := buf[start : i+1]
			lines = append(lines, line{
				bytes:     trimNewline(full),
				fullBytes: full,
			})
			start = i + 1
		}
	}
	return lines, buf[start:]
}

// trimNewline trims the trailing CR/LF pair so JSON lines and
// operational logs render without a duplicated terminator.
func trimNewline(buf []byte) []byte {
	for len(buf) > 0 && (buf[len(buf)-1] == '\n' || buf[len(buf)-1] == '\r') {
		buf = buf[:len(buf)-1]
	}
	return buf
}
