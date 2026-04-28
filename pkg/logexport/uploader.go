package logexport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

// Uploader manages the per-file lifecycle of a single artifact write.
// It opens a logartifact.Writer the first time the tailer reports bytes
// for a (file, sequence) pair, streams subsequent bytes through, and
// finalizes (or marks failed) when the file is closed by the tailer.
//
// Behavior matches the design (spi-7wzwk2) and the plan's edge-case
// list:
//   - Bounded retry: Put / Finalize go through retryPolicy so transient
//     manifest errors don't immediately surface as failed rows.
//   - Failure independence: a terminal write/finalize error stamps the
//     manifest row with status=failed (via the best-effort path) and is
//     visible to operators, but does NOT propagate past the exporter
//     to the agent process.
//   - Sequence monotonicity: each rotation/truncation observed by the
//     tailer creates a fresh entry under a sequence > 0; the previous
//     entry has already been finalized.
type Uploader struct {
	store      logartifact.Store
	db         *sql.DB
	visibility logartifact.Visibility
	policy     retryPolicy
	stats      *atomicStats
	sink       *StdoutSink

	mu      sync.Mutex
	entries map[uploadKey]*uploadEntry
}

// uploadKey is the (Identity, sequence) tuple keyed into Uploader.entries.
// File paths cannot collide because the Identity tuple already uniquely
// addresses a tailed source file: the same file path under the root
// always produces the same Identity, and rotation produces a fresh
// (Identity, sequence) by advancing sequence.
type uploadKey struct {
	identity logartifact.Identity
	sequence int
}

// uploadEntry is the per-(identity, sequence) state. The Writer is held
// across multiple Append calls so the running checksum / size are
// continuous; Close finalizes (or marks failed) and removes the entry.
type uploadEntry struct {
	mu          sync.Mutex
	writer      logartifact.Writer
	manifestID  string
	objectURI   string
	openedAt    time.Time
	lastWrite   time.Time
	bytesPushed int64
	closed      bool
	failed      bool
}

// NewUploader constructs an Uploader over store and db. visibility is
// applied to every artifact opened through this uploader; the field
// accepts logartifact.VisibilityEngineerOnly as the safe default.
//
// db may be nil — it's only used for the best-effort markManifestFailed
// path. Tests that exercise the upload pipeline without a tower
// connection pass nil.
func NewUploader(store logartifact.Store, db *sql.DB, visibility logartifact.Visibility, sink *StdoutSink, stats *atomicStats) (*Uploader, error) {
	if store == nil {
		return nil, fmt.Errorf("logexport: NewUploader: store must not be nil")
	}
	if visibility == "" {
		visibility = logartifact.VisibilityEngineerOnly
	}
	if !visibility.Valid() {
		return nil, fmt.Errorf("logexport: NewUploader: invalid visibility %q", visibility)
	}
	return &Uploader{
		store:      store,
		db:         db,
		visibility: visibility,
		policy:     DefaultRetryPolicy,
		stats:      stats,
		sink:       sink,
		entries:    make(map[uploadKey]*uploadEntry),
	}, nil
}

// SetRetryPolicy overrides the retry policy. Tests use this to
// disable backoff so manifest-failure tests do not sleep.
func (u *Uploader) SetRetryPolicy(p retryPolicy) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.policy = p
}

// Append routes bytes for (identity, sequence) into the uploader's
// per-file writer. The entry is opened lazily on the first call so a
// transient open failure (e.g. tower restart during exporter startup)
// is reported once per artifact rather than at scan time.
//
// Append is best-effort: a write or open error marks the entry failed
// and the bytes are dropped at the upload sink. The tailer's stdout
// sink still emits the line because StdoutSink runs upstream — Cloud
// Logging is unaffected by upload-side failures.
func (u *Uploader) Append(ctx context.Context, identity logartifact.Identity, sequence int, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	entry, err := u.openOrGet(ctx, identity, sequence)
	if err != nil {
		return err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.closed || entry.failed {
		return errEntryClosed
	}
	if _, werr := entry.writer.Write(data); werr != nil {
		entry.failed = true
		// Try to flip the manifest to failed so a reader sees the row
		// with the right status. Best-effort; failure here is a
		// secondary effect.
		retries, _ := markManifestFailed(ctx, u.db, entry.manifestID, u.policy)
		if u.stats != nil {
			u.stats.addRetries(retries)
			u.stats.incFailed()
		}
		u.emitWriteFailure(identity, sequence, werr)
		return werr
	}
	entry.bytesPushed += int64(len(data))
	entry.lastWrite = time.Now()
	return nil
}

// Close finalizes the artifact for (identity, sequence). After Close
// the entry is removed and a subsequent Append for the same key opens
// a fresh artifact at sequence+1 (the tailer is responsible for picking
// the new sequence; Uploader does not advance it on its own).
//
// closeReason is recorded in the operational stdout line so log
// consumers can distinguish rotation, idle finalize, and shutdown.
func (u *Uploader) Close(ctx context.Context, identity logartifact.Identity, sequence int, closeReason string) error {
	u.mu.Lock()
	entry, ok := u.entries[uploadKey{identity: identity, sequence: sequence}]
	if !ok {
		u.mu.Unlock()
		return nil
	}
	delete(u.entries, uploadKey{identity: identity, sequence: sequence})
	u.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.closed {
		return nil
	}
	entry.closed = true

	if entry.failed {
		// Already accounted; ensure the writer's tmpfile (if any) is
		// released by closing the Writer once.
		_ = entry.writer.Close()
		return errEntryFailed
	}

	var manifest logartifact.Manifest
	retries, err := retry(ctx, u.policy, func() error {
		var ferr error
		manifest, ferr = u.store.Finalize(ctx, entry.writer)
		return ferr
	})
	if u.stats != nil {
		u.stats.addRetries(retries)
	}
	if err != nil {
		entry.failed = true
		_ = entry.writer.Close()
		// Mark the row failed so consumers don't read a writing-status
		// row that will never finalize.
		markRetries, _ := markManifestFailed(ctx, u.db, entry.manifestID, u.policy)
		if u.stats != nil {
			u.stats.addRetries(markRetries)
			u.stats.incFailed()
		}
		u.emitFinalizeFailure(identity, sequence, closeReason, err)
		return err
	}
	if u.stats != nil {
		u.stats.incFinalized()
	}
	u.emitFinalizeSuccess(identity, sequence, closeReason, manifest)
	return nil
}

// CloseAll finalizes every open entry. Used by Flush during shutdown.
// Returns the most recent finalize error (or nil) so the caller can
// propagate a meaningful error to its parent context.
func (u *Uploader) CloseAll(ctx context.Context, closeReason string) error {
	u.mu.Lock()
	keys := make([]uploadKey, 0, len(u.entries))
	for k := range u.entries {
		keys = append(keys, k)
	}
	u.mu.Unlock()

	var lastErr error
	for _, k := range keys {
		if err := u.Close(ctx, k.identity, k.sequence, closeReason); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// Tracking returns the number of currently-open uploader entries. Used
// by tests and the optional /healthz endpoint — never on the data path.
func (u *Uploader) Tracking() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.entries)
}

// openOrGet returns the existing entry for (identity, sequence) or
// opens a new one. Concurrent callers see exactly one Put — the entries
// map is guarded by Uploader.mu and the per-entry state is then
// accessed under entry.mu.
func (u *Uploader) openOrGet(ctx context.Context, identity logartifact.Identity, sequence int) (*uploadEntry, error) {
	key := uploadKey{identity: identity, sequence: sequence}

	u.mu.Lock()
	if entry, ok := u.entries[key]; ok {
		u.mu.Unlock()
		return entry, nil
	}
	u.mu.Unlock()

	var writer logartifact.Writer
	retries, err := retry(ctx, u.policy, func() error {
		var perr error
		writer, perr = u.store.Put(ctx, identity, sequence, u.visibility)
		return perr
	})
	if u.stats != nil {
		u.stats.addRetries(retries)
	}
	if err != nil {
		// Open failure: the artifact never had a manifest row to mark
		// failed (or, on ErrLogArtifactExists, the row already exists
		// finalized). Either way, the upload path can't recover. Emit
		// an operational ERROR line so the failure is visible.
		u.emitOpenFailure(identity, sequence, err)
		if u.stats != nil {
			u.stats.incFailed()
		}
		return nil, err
	}

	entry := &uploadEntry{
		writer:     writer,
		manifestID: writer.ManifestID(),
		objectURI:  writer.ObjectURI(),
		openedAt:   time.Now(),
		lastWrite:  time.Now(),
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	if existing, ok := u.entries[key]; ok {
		// Lost the race against another goroutine — release the writer
		// we just opened (best-effort) and use the existing one.
		_ = writer.Close()
		return existing, nil
	}
	u.entries[key] = entry
	return entry, nil
}

func (u *Uploader) emitOpenFailure(id logartifact.Identity, seq int, err error) {
	if u.sink == nil {
		return
	}
	u.sink.EmitOperational(SeverityError, id.Tower, "logexport: open artifact failed", map[string]string{
		"bead_id":  id.BeadID,
		"run_id":   id.RunID,
		"agent":    id.AgentName,
		"role":     string(id.Role),
		"phase":    id.Phase,
		"provider": id.Provider,
		"stream":   string(id.Stream),
		"sequence": fmt.Sprintf("%d", seq),
		"error":    err.Error(),
	})
}

func (u *Uploader) emitWriteFailure(id logartifact.Identity, seq int, err error) {
	if u.sink == nil {
		return
	}
	u.sink.EmitOperational(SeverityError, id.Tower, "logexport: append artifact failed", map[string]string{
		"bead_id":  id.BeadID,
		"run_id":   id.RunID,
		"stream":   string(id.Stream),
		"sequence": fmt.Sprintf("%d", seq),
		"error":    err.Error(),
	})
}

func (u *Uploader) emitFinalizeFailure(id logartifact.Identity, seq int, reason string, err error) {
	if u.sink == nil {
		return
	}
	u.sink.EmitOperational(SeverityError, id.Tower, "logexport: finalize artifact failed", map[string]string{
		"bead_id":  id.BeadID,
		"run_id":   id.RunID,
		"stream":   string(id.Stream),
		"sequence": fmt.Sprintf("%d", seq),
		"reason":   reason,
		"error":    err.Error(),
	})
}

func (u *Uploader) emitFinalizeSuccess(id logartifact.Identity, seq int, reason string, manifest logartifact.Manifest) {
	if u.sink == nil {
		return
	}
	u.sink.EmitOperational(SeverityInfo, id.Tower, "logexport: finalized artifact", map[string]string{
		"bead_id":    id.BeadID,
		"run_id":     id.RunID,
		"stream":     string(id.Stream),
		"sequence":   fmt.Sprintf("%d", seq),
		"reason":     reason,
		"object_uri": manifest.ObjectURI,
		"byte_size":  fmt.Sprintf("%d", manifest.ByteSize),
		"checksum":   manifest.Checksum,
	})
}

// Sentinel errors. Internal, not exported because the exporter's
// callers don't discriminate between them — only Stats does.
var (
	errEntryClosed = errors.New("logexport: uploader entry already closed")
	errEntryFailed = errors.New("logexport: uploader entry already failed")
)

// readBytes returns the bytes of file from offset to EOF. Used by the
// tailer when handing a slab of newly-written data to the uploader.
// Kept here so the uploader's interaction with the filesystem is in
// one place.
func readBytes(file string, offset int64) ([]byte, int, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	return data, len(data), nil
}
