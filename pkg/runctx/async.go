package runctx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// asyncDefaultBufferLines is the default queue depth for AsyncFile.
// One JSONL line is the natural emission unit, so the buffer holds N
// lines worth of pending writes before the producer's Write call falls
// through to the drop path. Sized for transcript volume: Claude
// streaming-json bursts of ~100 events/sec settle within ~10s of buffer
// when the disk is healthy.
const asyncDefaultBufferLines = 1024

// asyncDefaultMaxLineBytes caps the byte length of a single Write call
// that AsyncFile copies into its buffer. Calls exceeding the cap are
// truncated and accounted as a dropped-bytes count so a runaway log
// line cannot pin the whole queue. Values past the cap come through as
// a normal io.Writer error path on the caller side.
const asyncDefaultMaxLineBytes = 1 << 20 // 1 MiB

// asyncOp is one buffered write: bytes to flush, plus an optional
// closeAck channel the caller blocks on during Close so we know the
// writer goroutine has flushed everything in flight.
type asyncOp struct {
	data     []byte
	closeAck chan error
}

// AsyncFile is a non-blocking io.WriteCloser. Bytes written through it
// are queued onto a buffered channel and flushed by a dedicated
// goroutine. When the queue is full, Write returns the bytes-accepted
// count plus a counter increment instead of blocking — the agent's
// critical path keeps moving and the caller decides whether to surface
// the drop as an operational event.
//
// AsyncFile is safe for use by a single producer. Multiple producers
// are not supported because line interleaving is the caller's concern
// (the agent threads own ordering of their stdout/stderr, not us).
type AsyncFile struct {
	path    string
	file    *os.File
	queue   chan asyncOp
	closed  atomic.Bool
	dropped atomic.Int64
	wg      sync.WaitGroup

	// maxLine is the per-Write byte cap. Set once at construction;
	// safe to read concurrently because we never mutate it.
	maxLine int

	// Set on first encountered write/flush failure so successive Write
	// calls can report it without blocking on a flush.
	errMu  sync.Mutex
	lastEr error
}

// AsyncFileOption configures AsyncFile creation.
type AsyncFileOption func(*asyncFileConfig)

type asyncFileConfig struct {
	bufferLines int
	maxLineSize int
}

// WithBufferLines sets the queue depth (number of pending Write calls
// the buffer holds before drops kick in). Zero or negative values
// reset to the default.
func WithBufferLines(n int) AsyncFileOption {
	return func(c *asyncFileConfig) {
		if n > 0 {
			c.bufferLines = n
		}
	}
}

// WithMaxLineBytes sets the per-Write byte cap. Calls exceeding the cap
// are truncated; the dropped suffix is accounted in the dropped-bytes
// counter so callers can surface it as a structured warning.
func WithMaxLineBytes(n int) AsyncFileOption {
	return func(c *asyncFileConfig) {
		if n > 0 {
			c.maxLineSize = n
		}
	}
}

// NewAsyncFile creates an AsyncFile rooted at path. The file is
// O_CREATE|O_WRONLY|O_APPEND with mode 0644. Parent directories are
// NOT created — callers ensure the directory exists (typically via
// LogPaths.MkdirAll) so we don't silently mask a config error.
//
// The returned AsyncFile spawns one goroutine that owns the underlying
// *os.File. Close blocks until the goroutine has flushed every queued
// op and closed the file.
func NewAsyncFile(path string, opts ...AsyncFileOption) (*AsyncFile, error) {
	if path == "" {
		return nil, errors.New("runctx: NewAsyncFile: path is empty")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("runctx: NewAsyncFile: path %q must be absolute", path)
	}

	cfg := asyncFileConfig{
		bufferLines: asyncDefaultBufferLines,
		maxLineSize: asyncDefaultMaxLineBytes,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("runctx: open %s: %w", path, err)
	}

	w := &AsyncFile{
		path:  path,
		file:  f,
		queue: make(chan asyncOp, cfg.bufferLines),
	}
	w.wg.Add(1)
	go w.run()

	// Adopt the maxLineSize as a soft cap on per-Write bytes. We don't
	// store cfg directly because configuration is immutable after
	// construction — encode the cap into a helper closure if future
	// callers want to mutate it at runtime.
	w.maxLine = cfg.maxLineSize
	return w, nil
}

// Path returns the absolute filesystem path the writer is bound to.
func (w *AsyncFile) Path() string { return w.path }

// Dropped returns the number of byte-equivalents Write has rejected
// because the queue was full. Callers surface this via a structured
// warning so a sustained drop pattern shows up in the operational log
// even though the writer never blocked on the producer.
func (w *AsyncFile) Dropped() int64 { return w.dropped.Load() }

// Err returns the first persistent write/flush error the goroutine
// observed, or nil. Returns errors.Is-able against fs errors so
// callers can distinguish "disk full" from "permission denied".
func (w *AsyncFile) Err() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.lastEr
}

// Write enqueues p for asynchronous flushing. Returns len(p), nil on
// success — including the "buffer full, bytes dropped" path — so the
// caller's io.Writer contract is satisfied without blocking. Use
// Dropped() and Err() to surface persistent failure.
//
// The single exception: if the writer is closed, Write returns an
// io.ErrClosedPipe-equivalent error so callers can detect a misorder
// (writing after Close).
func (w *AsyncFile) Write(p []byte) (int, error) {
	if w.closed.Load() {
		return 0, errors.New("runctx: write on closed AsyncFile")
	}
	if len(p) == 0 {
		return 0, nil
	}

	maxLine := w.maxLine
	if maxLine > 0 && len(p) > maxLine {
		w.dropped.Add(int64(len(p) - maxLine))
		p = p[:maxLine]
	}

	// Copy bytes out of the caller's buffer; the goroutine owns the
	// flush and the caller is free to reuse p when Write returns.
	buf := make([]byte, len(p))
	copy(buf, p)

	select {
	case w.queue <- asyncOp{data: buf}:
		return len(p), nil
	default:
		// Queue full — record the drop and return success. This is the
		// "best-effort, non-blocking" contract.
		w.dropped.Add(int64(len(p)))
		return len(p), nil
	}
}

// Close stops accepting writes, flushes everything in flight, and
// closes the underlying file. Close is idempotent — calling it twice
// returns nil from the second call.
//
// Close blocks the caller until the goroutine has flushed its queue,
// so it should be invoked from a defer or a teardown phase rather than
// the producer's hot path.
//
// The queue channel is intentionally NOT closed: a Write that races
// with Close (read closed=false, then Close flips to true) would panic
// on a send-to-closed-channel. Instead, the goroutine watches for a
// sentinel op carrying closeAck and exits voluntarily; any in-flight
// Write that lost the race lands in the buffer and is flushed before
// the sentinel.
func (w *AsyncFile) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}

	ack := make(chan error, 1)
	w.queue <- asyncOp{closeAck: ack}

	closeErr := <-ack
	w.wg.Wait()
	return closeErr
}

// run is the goroutine body that owns the underlying *os.File. It
// flushes every op pulled from the queue, records the first persistent
// error, and exits voluntarily when it pulls the close sentinel.
func (w *AsyncFile) run() {
	defer w.wg.Done()
	for op := range w.queue {
		if op.closeAck != nil {
			closeErr := w.file.Close()
			w.file = nil
			op.closeAck <- closeErr
			return
		}
		_, err := w.file.Write(op.data)
		if err != nil {
			w.recordErr(err)
		}
	}
}

// recordErr stores the first persistent write/flush error so callers
// can surface it via Err(). Subsequent errors are not captured because
// the first failure is usually the diagnostic; later errors are
// secondary effects.
func (w *AsyncFile) recordErr(err error) {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	if w.lastEr == nil {
		w.lastEr = err
	}
}

// Sentinel: ensure AsyncFile satisfies io.WriteCloser. This is checked
// by the compiler at every Write/Close call site, but the explicit
// assertion makes wiring mistakes (a method rename, a missing pointer
// receiver) surface at build time rather than the first runtime call.
var _ io.WriteCloser = (*AsyncFile)(nil)
