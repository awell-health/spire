package logartifact

import (
	"context"
	"hash"
	"io"
	"time"

	pkgstore "github.com/awell-health/spire/pkg/store"
)

// Writer is the streaming write handle returned by Store.Put. The
// caller writes bytes through Write; the backend persists them and the
// writer tracks the running size and SHA-256. Finalize closes the
// underlying byte-store handle, computes the final checksum, and writes
// or updates the manifest row.
//
// Implementations must be safe for sequential use by a single
// goroutine. Concurrent Write/Close on the same writer is undefined.
type Writer interface {
	io.WriteCloser

	// Identity returns the artifact identity this writer is bound to.
	// Stable across the writer's lifetime.
	Identity() Identity

	// Sequence returns the chunk sequence this writer is bound to.
	// 0 is the canonical single-shot artifact; > 0 is a chunk of a
	// larger logical artifact (used for live-follow / live-export
	// scenarios).
	Sequence() int

	// Size returns the number of bytes Write has accepted so far.
	// Useful for backends that want to enforce per-artifact size
	// budgets.
	Size() int64

	// ChecksumHex returns the running SHA-256 over the bytes Write has
	// accepted, lower-case hex without the "sha256:" prefix. Reading
	// this before Close is allowed but reflects only the bytes seen so
	// far — Finalize uses this value at the moment of close.
	ChecksumHex() string

	// ObjectURI returns the URI the bytes are being written to. For
	// the local backend this is `file://<absolute>`; for GCS it is
	// `gs://<bucket>/<key>`. Useful for tests and operational logs.
	ObjectURI() string

	// ManifestID returns the row ID the writer is bound to. Empty
	// before Put has reserved the manifest row (it never is, in
	// practice — Put always inserts before returning a writer).
	ManifestID() string

	// Visibility returns the access class the writer was opened at.
	// Used by Finalize to decide whether to stamp redaction_version on
	// the manifest row.
	Visibility() Visibility
}

// Store is the contract every artifact backend implements. Local and
// GCS are the two backends shipped here; future backends (PVC, S3) can
// satisfy the same interface.
type Store interface {
	// Put reserves a manifest row for (identity, sequence) and returns
	// a Writer that streams bytes into the backend. If a manifest row
	// already exists for this identity/sequence and is in
	// StatusFinalized, Put returns ErrLogArtifactExists from pkg/store
	// — callers performing idempotent re-uploads should fetch the
	// existing manifest via Stat or List rather than rewriting.
	//
	// Visibility is required and gates the redaction behavior:
	//   - VisibilityEngineerOnly: bytes pass through unmodified (forensic
	//     fidelity); render-time access is gated to engineer scope.
	//   - VisibilityDesktopSafe / VisibilityPublic: bytes are redacted
	//     before they hit the byte store; the manifest's
	//     redaction_version is stamped to the version that ran. Render
	//     re-applies the current redactor on read.
	// The empty string is rejected — callers must declare visibility
	// explicitly so a forgetful caller fails to compile or fails fast,
	// not silently leaks raw bytes through a default. See design
	// spi-cmy90h.
	Put(ctx context.Context, identity Identity, sequence int, visibility Visibility) (Writer, error)

	// Finalize closes the writer, persists the final size and
	// checksum, and updates the manifest row to StatusFinalized. The
	// returned Manifest is the canonical post-write shape.
	//
	// Calling Finalize on a writer whose manifest row is already
	// finalized is a no-op — the existing manifest is returned.
	Finalize(ctx context.Context, w Writer) (Manifest, error)

	// Get returns a reader for the artifact's bytes plus the manifest
	// row. The returned ReadCloser must be closed by the caller. Get
	// returns ErrNotFound if the manifest row is missing.
	Get(ctx context.Context, ref ManifestRef) (io.ReadCloser, Manifest, error)

	// Stat returns just the manifest row, without opening the byte
	// store. Returns ErrNotFound if the row is missing.
	Stat(ctx context.Context, ref ManifestRef) (Manifest, error)

	// List returns every manifest row matching the filter. The order
	// is the same as pkg/store.ListLogArtifactsForBead /
	// ListLogArtifactsForAttempt: deterministic by (attempt, run,
	// sequence, created_at) ascending.
	List(ctx context.Context, filter Filter) ([]Manifest, error)
}

// chunkedHash bundles an io.Writer that fans bytes into the backend
// writer and a SHA-256 hasher so we keep one running checksum per
// artifact. Used by both backends so the checksum semantics are
// identical regardless of where the bytes ultimately land.
type chunkedHash struct {
	dst    io.Writer
	hasher hash.Hash
	size   int64
}

func (c *chunkedHash) Write(p []byte) (int, error) {
	n, err := c.dst.Write(p)
	if n > 0 {
		c.hasher.Write(p[:n])
		c.size += int64(n)
	}
	return n, err
}

// recordToManifest converts a pkg/store row into the Manifest shape this
// package returns. Centralized here so backends share one mapping and
// future column additions land in a single place.
func recordToManifest(rec pkgstore.LogArtifactRecord) Manifest {
	visibility := Visibility(rec.Visibility)
	if visibility == "" || !visibility.Valid() {
		visibility = VisibilityEngineerOnly
	}
	m := Manifest{
		ID: rec.ID,
		Identity: Identity{
			Tower:     rec.Tower,
			BeadID:    rec.BeadID,
			AttemptID: rec.AttemptID,
			RunID:     rec.RunID,
			AgentName: rec.AgentName,
			Role:      Role(rec.Role),
			Phase:     rec.Phase,
			Provider:  rec.Provider,
			Stream:    Stream(rec.Stream),
		},
		Sequence:         rec.Sequence,
		ObjectURI:        rec.ObjectURI,
		Checksum:         rec.Checksum,
		Status:           Status(rec.Status),
		CreatedAt:        rec.CreatedAt,
		UpdatedAt:        rec.UpdatedAt,
		RedactionVersion: rec.RedactionVersion,
		Visibility:       visibility,
		Summary:          rec.Summary,
		Tail:             rec.Tail,
	}
	if rec.ByteSize != nil {
		m.ByteSize = *rec.ByteSize
	}
	if rec.StartedAt != nil {
		m.StartedAt = *rec.StartedAt
	}
	if rec.EndedAt != nil {
		m.EndedAt = *rec.EndedAt
	}
	return m
}

// recordsToManifests is a slice convenience over recordToManifest.
func recordsToManifests(rows []pkgstore.LogArtifactRecord) []Manifest {
	out := make([]Manifest, len(rows))
	for i, rec := range rows {
		out[i] = recordToManifest(rec)
	}
	return out
}

// startedAtPtr returns a pointer to t when t is not the zero value, nil
// otherwise. Used when constructing pkgstore.LogArtifactRecord values
// from in-memory state where StartedAt may be unset.
func startedAtPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	tt := t
	return &tt
}
