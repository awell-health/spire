package logartifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"cloud.google.com/go/storage"
	pkgstore "github.com/awell-health/spire/pkg/store"
)

// GCSStore is a Google Cloud Storage-backed log artifact store.
//
// Object keys are derived from BuildObjectKey, so the local and GCS
// backends produce identical relative shapes; only the prefix differs.
// The manifest still lives in pkg/store.agent_log_artifacts; List
// queries the manifest table only — never `LIST` against the bucket.
//
// The store does NOT own the GCS client lifecycle. Callers construct a
// client (typically via storage.NewClient with Application Default
// Credentials, matching pkg/bundlestore's pattern) and pass it in.
// Helm wiring lands in spi-hzeyz9; this package only consumes the
// supplied client.
type GCSStore struct {
	client *storage.Client
	bucket string
	prefix string
	db     *sql.DB
}

// NewGCS constructs a GCSStore over a pre-existing bucket. The bucket
// must exist before construction — the design forecloses
// auto-provisioning so log buckets stay separate from bundleStore /
// backup buckets with their distinct retention rules.
//
// prefix is sanitized: leading/trailing slashes are stripped, and a
// non-empty prefix is composed with the canonical relative key as
// `<prefix>/<key>`. An empty prefix stores objects at the bucket root.
func NewGCS(ctx context.Context, client *storage.Client, bucket, prefix string, db *sql.DB) (*GCSStore, error) {
	if client == nil {
		return nil, errors.New("logartifact: NewGCS: client must not be nil")
	}
	if bucket == "" {
		return nil, errors.New("logartifact: NewGCS: bucket is required")
	}
	if db == nil {
		return nil, errors.New("logartifact: NewGCS: db must not be nil")
	}
	prefix = strings.Trim(prefix, "/")
	// Probe the bucket so configuration errors surface at construction
	// rather than at first write. Mirrors pkg/bundlestore's pattern.
	if _, err := client.Bucket(bucket).Attrs(ctx); err != nil {
		if errors.Is(err, storage.ErrBucketNotExist) {
			return nil, fmt.Errorf("logartifact: bucket %q does not exist; create it with: gsutil mb gs://%s", bucket, bucket)
		}
		return nil, fmt.Errorf("logartifact: probe bucket %q: %w", bucket, err)
	}
	return &GCSStore{
		client: client,
		bucket: bucket,
		prefix: prefix,
		db:     db,
	}, nil
}

// Bucket returns the GCS bucket the store writes to. Exposed for
// operational tooling.
func (s *GCSStore) Bucket() string { return s.bucket }

// Prefix returns the normalized object-name prefix.
func (s *GCSStore) Prefix() string { return s.prefix }

// gcsWriter is the GCSStore-side Writer.
type gcsWriter struct {
	identity   Identity
	sequence   int
	objectKey  string
	objectURI  string
	manifestID string
	gcs        *storage.Writer
	chunked    *chunkedHash
	closed     bool
}

func (w *gcsWriter) Identity() Identity   { return w.identity }
func (w *gcsWriter) Sequence() int        { return w.sequence }
func (w *gcsWriter) Size() int64          { return w.chunked.size }
func (w *gcsWriter) ChecksumHex() string  { return hex.EncodeToString(w.chunked.hasher.Sum(nil)) }
func (w *gcsWriter) ObjectURI() string    { return w.objectURI }
func (w *gcsWriter) ManifestID() string   { return w.manifestID }

func (w *gcsWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("logartifact: write on closed writer")
	}
	return w.chunked.Write(p)
}

// Close releases the underlying GCS writer without finalizing the
// manifest. The GCS object remains in whatever state Close left it —
// successful uploads may already be visible if the caller wrote
// enough bytes for the resumable upload to commit. Use Store.Finalize
// to make the manifest reflect the artifact.
func (w *gcsWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.gcs == nil {
		return nil
	}
	return w.gcs.Close()
}

// Put implements Store.
func (s *GCSStore) Put(ctx context.Context, identity Identity, sequence int) (Writer, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if sequence < 0 {
		return nil, fmt.Errorf("logartifact: sequence must be >= 0 (got %d)", sequence)
	}
	relKey, err := BuildObjectKey("", identity, sequence)
	if err != nil {
		return nil, err
	}
	objectKey := relKey
	if s.prefix != "" {
		objectKey = s.prefix + "/" + relKey
	}
	objectURI := fmt.Sprintf("gs://%s/%s", s.bucket, objectKey)

	manifestID, err := insertOrFetchManifest(ctx, s.db, identity, sequence, objectURI)
	if err != nil {
		return nil, err
	}

	w := s.client.Bucket(s.bucket).Object(objectKey).NewWriter(ctx)
	return &gcsWriter{
		identity:   identity,
		sequence:   sequence,
		objectKey:  objectKey,
		objectURI:  objectURI,
		manifestID: manifestID,
		gcs:        w,
		chunked: &chunkedHash{
			dst:    w,
			hasher: sha256.New(),
		},
	}, nil
}

// Finalize implements Store.
func (s *GCSStore) Finalize(ctx context.Context, w Writer) (Manifest, error) {
	gw, ok := w.(*gcsWriter)
	if !ok {
		return Manifest{}, fmt.Errorf("logartifact: GCSStore.Finalize: writer is %T, expected *gcsWriter", w)
	}

	// Idempotent: already-finalized rows return the existing manifest.
	rec, err := pkgstore.GetLogArtifact(ctx, s.db, gw.manifestID)
	if err != nil {
		return Manifest{}, fmt.Errorf("logartifact: lookup manifest: %w", err)
	}
	if rec != nil && rec.Status == pkgstore.LogArtifactStatusFinalized {
		// Best-effort: close the GCS writer if the caller hasn't.
		// Don't propagate close errors here — the manifest already
		// reflects the canonical state.
		if gw.gcs != nil && !gw.closed {
			_ = gw.gcs.Close()
			gw.closed = true
		}
		return recordToManifest(*rec), nil
	}

	if gw.gcs == nil {
		return Manifest{}, fmt.Errorf("logartifact: Finalize on already-closed writer (id=%s)", gw.manifestID)
	}

	// Close the GCS writer to flush the resumable upload.
	if err := gw.gcs.Close(); err != nil {
		_ = pkgstore.UpdateLogArtifactStatus(ctx, s.db, gw.manifestID, pkgstore.LogArtifactStatusFailed)
		return Manifest{}, fmt.Errorf("logartifact: close GCS writer: %w", err)
	}
	gw.closed = true
	gw.gcs = nil

	checksum := "sha256:" + gw.ChecksumHex()
	if err := pkgstore.FinalizeLogArtifact(ctx, s.db, gw.manifestID, gw.Size(), checksum, "", ""); err != nil {
		return Manifest{}, fmt.Errorf("logartifact: finalize manifest: %w", err)
	}

	rec, err = pkgstore.GetLogArtifact(ctx, s.db, gw.manifestID)
	if err != nil {
		return Manifest{}, fmt.Errorf("logartifact: re-fetch manifest: %w", err)
	}
	if rec == nil {
		return Manifest{}, fmt.Errorf("logartifact: manifest %s vanished after finalize", gw.manifestID)
	}
	return recordToManifest(*rec), nil
}

// Get implements Store.
func (s *GCSStore) Get(ctx context.Context, ref ManifestRef) (io.ReadCloser, Manifest, error) {
	rec, err := resolveManifest(ctx, s.db, ref)
	if err != nil {
		return nil, Manifest{}, err
	}
	bucket, key, err := parseGCSURI(rec.ObjectURI)
	if err != nil {
		return nil, Manifest{}, err
	}
	if bucket != s.bucket {
		return nil, Manifest{}, fmt.Errorf("logartifact: artifact bucket %q does not match store bucket %q", bucket, s.bucket)
	}
	rc, err := s.client.Bucket(bucket).Object(key).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, Manifest{}, ErrNotFound
		}
		return nil, Manifest{}, fmt.Errorf("logartifact: open gs://%s/%s: %w", bucket, key, err)
	}
	return rc, recordToManifest(*rec), nil
}

// Stat implements Store.
func (s *GCSStore) Stat(ctx context.Context, ref ManifestRef) (Manifest, error) {
	rec, err := resolveManifest(ctx, s.db, ref)
	if err != nil {
		return Manifest{}, err
	}
	return recordToManifest(*rec), nil
}

// List implements Store.
func (s *GCSStore) List(ctx context.Context, filter Filter) ([]Manifest, error) {
	return listManifests(ctx, s.db, filter)
}

// parseGCSURI splits "gs://<bucket>/<key>" into its bucket and key
// components. Returns an error for non-gs URIs (e.g. file:// rows that
// belong to the local backend).
func parseGCSURI(uri string) (string, string, error) {
	if !strings.HasPrefix(uri, "gs://") {
		return "", "", fmt.Errorf("logartifact: not a GCS URI: %s", uri)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", "", fmt.Errorf("logartifact: parse uri %q: %w", uri, err)
	}
	if parsed.Host == "" || parsed.Path == "" {
		return "", "", fmt.Errorf("logartifact: malformed GCS URI %q", uri)
	}
	return parsed.Host, strings.TrimPrefix(parsed.Path, "/"), nil
}
