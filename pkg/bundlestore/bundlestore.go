// Package bundlestore provides storage for git-bundle artifacts produced
// by apprentices and consumed by wizards during the submit/fetch flow.
//
// The store is intentionally minimal: Put/Get/Delete/List over opaque
// handles. Role/RBAC concepts live on the bead signal, not here.
//
// Bundles are binary artifacts (output of `git bundle create`). Dolt
// carries only the pointer (the handle's Key) on the task bead; the
// artifact itself lives in whichever backend the tower is configured with.
package bundlestore

import (
	"context"
	"errors"
	"io"
	"regexp"
	"time"
)

// Default configuration values.
const (
	DefaultMaxBytes        int64         = 10 * 1024 * 1024 // 10 MB
	DefaultJanitorInterval time.Duration = 5 * time.Minute
	DefaultSealedGrace     time.Duration = 30 * time.Minute
	DefaultOrphanAge       time.Duration = 7 * 24 * time.Hour
)

// Sentinel errors returned by BundleStore implementations.
var (
	// ErrTooLarge is returned by Put when the bundle exceeds Config.MaxBytes.
	ErrTooLarge = errors.New("bundlestore: bundle exceeds max size")

	// ErrDuplicate is returned by Put when a bundle already exists for the
	// same (BeadID, AttemptID, ApprenticeIdx) triple. Put never silently
	// overwrites — two submissions for the same triple is a bug.
	ErrDuplicate = errors.New("bundlestore: bundle already exists for this triple")

	// ErrNotFound is returned by Get when the handle resolves to no artifact.
	ErrNotFound = errors.New("bundlestore: bundle not found")

	// ErrInvalidRequest is returned by Put when a PutRequest field fails
	// validation (empty bead id, path-unsafe characters, etc.).
	ErrInvalidRequest = errors.New("bundlestore: invalid put request")
)

// idPattern matches bead and attempt IDs: lowercase alphanumeric + dashes,
// 1–64 characters. This is the path-hygiene guard — anything that fails
// this check is rejected before touching the filesystem.
var idPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// BundleHandle is an opaque pointer returned by Put and consumed by Get /
// Delete. The Key is store-defined; callers must treat it as opaque and
// round-trip it through bead metadata.
type BundleHandle struct {
	// BeadID is the task bead the bundle belongs to (not the attempt bead).
	BeadID string
	// Key is store-opaque. For the local backend it is the relative file
	// path under the store root; other backends may use different schemes.
	// Callers MUST NOT parse this value.
	Key string
}

// PutRequest identifies the submission that produced a bundle.
// The (BeadID, AttemptID, ApprenticeIdx) triple must be globally unique:
// the store rejects duplicates rather than silently overwriting.
type PutRequest struct {
	// BeadID is the task bead the bundle is for. Required.
	BeadID string
	// AttemptID is the attempt bead ID (from the apprentice's attempt
	// record). Required; disambiguates cleric-retries and sage-fixes.
	AttemptID string
	// ApprenticeIdx is the apprentice slot within the attempt. 0 for
	// single-apprentice tasks; >0 only in fan-out scenarios.
	ApprenticeIdx int
}

// Validate checks that the request is well-formed and safe for use as a
// filesystem path. Implementations should call this at the start of Put
// before doing any I/O.
func (r PutRequest) Validate() error {
	if !idPattern.MatchString(r.BeadID) {
		return ErrInvalidRequest
	}
	if !idPattern.MatchString(r.AttemptID) {
		return ErrInvalidRequest
	}
	if r.ApprenticeIdx < 0 {
		return ErrInvalidRequest
	}
	return nil
}

// BundleStore is the storage substrate for git-bundle artifacts.
//
// Implementations must be safe for concurrent use by multiple goroutines.
// Put is reject-on-duplicate; callers that want replace-on-submit must
// Delete first.
type BundleStore interface {
	// Put uploads a git bundle from r. Returns a BundleHandle containing
	// the store-opaque Key. Fails with ErrDuplicate if a bundle already
	// exists for the same (BeadID, AttemptID, ApprenticeIdx) triple, and
	// with ErrTooLarge if the stream exceeds the configured max size.
	//
	// Implementations must write atomically: a crashed Put must leave
	// nothing that looks like a complete bundle behind.
	Put(ctx context.Context, req PutRequest, bundle io.Reader) (BundleHandle, error)

	// Get returns an io.ReadCloser that streams the bundle bytes. The
	// caller owns the returned reader and must Close it. Returns
	// ErrNotFound when the handle resolves to no artifact.
	Get(ctx context.Context, h BundleHandle) (io.ReadCloser, error)

	// Delete removes the bundle identified by h. Idempotent: deleting a
	// missing bundle is not an error.
	Delete(ctx context.Context, h BundleHandle) error

	// List returns every handle currently in the store. Used by the
	// janitor; must be cheap enough to run on the janitor's cadence.
	// Cloud backends SHOULD implement pagination internally (but still
	// return the full list to the caller).
	List(ctx context.Context) ([]BundleHandle, error)

	// Stat returns metadata about a handle — primarily the modification
	// time, which the janitor uses to decide whether an orphaned bundle
	// is old enough to reap. Returns ErrNotFound for missing handles.
	Stat(ctx context.Context, h BundleHandle) (BundleInfo, error)
}

// BundleInfo describes a stored bundle without reading its contents.
type BundleInfo struct {
	Size    int64
	ModTime time.Time
}

// Config controls BundleStore construction.
type Config struct {
	// Backend selects the implementation. Currently only "local" ships;
	// "pvc", "http", "gcs", "s3" are planned follow-ups.
	Backend string
	// LocalRoot is the filesystem root for the local backend. Empty
	// means "use the platform default under XDG_DATA_HOME/spire/bundles".
	LocalRoot string
	// MaxBytes caps individual bundle size. 0 means use DefaultMaxBytes.
	MaxBytes int64
	// JanitorInterval controls how often the janitor runs a retention
	// sweep. 0 means use DefaultJanitorInterval.
	JanitorInterval time.Duration
}

// WithDefaults returns a copy of c with zero-valued fields filled from
// the package defaults.
func (c Config) WithDefaults() Config {
	if c.Backend == "" {
		c.Backend = "local"
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.JanitorInterval == 0 {
		c.JanitorInterval = DefaultJanitorInterval
	}
	return c
}
