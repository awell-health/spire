package bundlestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// GCSStore is a Google Cloud Storage-backed BundleStore. Objects live at
//
//	gs://<bucket>/<prefix>/<beadID>/<attemptID>-<idx>.bundle
//
// with the prefix applied at the backend boundary — BundleHandle.Key
// stays backend-agnostic (<beadID>/<attemptID>-<idx>.bundle).
//
// Duplicate rejection uses GCS generation preconditions
// (Conditions.DoesNotExist=true), so two concurrent Put calls with the
// same triple return ErrDuplicate rather than silently overwriting.
//
// The constructor fails loud when the bucket is missing or unauthorized
// — we want misconfiguration surfaced at tower startup, not deep inside
// the first submit flow.
type GCSStore struct {
	client   *storage.Client
	bucket   string
	prefix   string
	maxBytes int64
}

// NewGCSStore constructs a GCSStore using Application Default
// Credentials. In GKE this picks up Workload Identity; in minikube it
// reads GOOGLE_APPLICATION_CREDENTIALS; locally it falls back to the
// gcloud ADC file. No credential fields live in the tower config —
// ADC is the only supported auth path.
//
// The constructor probes the bucket and returns a diagnostic error
// containing a `gsutil mb` hint if the bucket does not exist.
func NewGCSStore(ctx context.Context, cfg Config) (*GCSStore, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs: new storage client: %w", err)
	}
	store, err := newGCSStoreWithClient(ctx, client, cfg)
	if err != nil {
		client.Close()
		return nil, err
	}
	return store, nil
}

// newGCSStoreWithClient wires a GCSStore around an injected storage
// client. Used internally by NewGCSStore and by tests that construct
// a client pointed at a fake GCS server.
func newGCSStoreWithClient(ctx context.Context, client *storage.Client, cfg Config) (*GCSStore, error) {
	cfg = cfg.WithDefaults()
	if cfg.GCSBucket == "" {
		return nil, errors.New("gcs: Config.GCSBucket is required")
	}
	if _, err := client.Bucket(cfg.GCSBucket).Attrs(ctx); err != nil {
		if errors.Is(err, storage.ErrBucketNotExist) {
			return nil, fmt.Errorf("gcs: bucket %q does not exist; create it with: gsutil mb gs://%s", cfg.GCSBucket, cfg.GCSBucket)
		}
		return nil, fmt.Errorf("gcs: probe bucket %q: %w", cfg.GCSBucket, err)
	}
	return &GCSStore{
		client:   client,
		bucket:   cfg.GCSBucket,
		prefix:   normalizeGCSPrefix(cfg.GCSPrefix),
		maxBytes: cfg.MaxBytes,
	}, nil
}

// Close releases the underlying storage client. Callers should Close the
// store when the tower shuts down.
func (s *GCSStore) Close() error { return s.client.Close() }

// Bucket returns the GCS bucket the store writes to. Exposed for
// operational tooling and tests.
func (s *GCSStore) Bucket() string { return s.bucket }

// Prefix returns the normalized object-name prefix. Exposed for tests.
func (s *GCSStore) Prefix() string { return s.prefix }

// Put implements BundleStore.
func (s *GCSStore) Put(ctx context.Context, req PutRequest, bundle io.Reader) (BundleHandle, error) {
	if err := req.Validate(); err != nil {
		return BundleHandle{}, err
	}
	relKey := keyFor(req)
	objName := s.objectName(relKey)

	// DoesNotExist=true makes Close return a 412 PreconditionFailed if
	// the object already exists — that's how duplicate detection rides
	// on a single round-trip.
	obj := s.client.Bucket(s.bucket).Object(objName).If(storage.Conditions{DoesNotExist: true})
	w := obj.NewWriter(ctx)

	// LimitReader(max+1) lets us detect oversize after the copy: if the
	// stream delivered max+1 bytes, the caller provided more than allowed.
	lr := io.LimitReader(bundle, s.maxBytes+1)
	n, copyErr := io.Copy(w, lr)
	closeErr := w.Close()

	if n > s.maxBytes {
		// Oversize wins over any closeErr reporting. Delete any completed
		// upload so nothing partial is visible to List.
		if closeErr == nil {
			_ = s.client.Bucket(s.bucket).Object(objName).Delete(ctx)
		}
		return BundleHandle{}, ErrTooLarge
	}
	if copyErr != nil {
		return BundleHandle{}, fmt.Errorf("gcs: write bundle: %w", copyErr)
	}
	if closeErr != nil {
		if isGCSPreconditionFailed(closeErr) {
			return BundleHandle{}, ErrDuplicate
		}
		return BundleHandle{}, fmt.Errorf("gcs: close writer: %w", closeErr)
	}

	return BundleHandle{BeadID: req.BeadID, Key: relKey}, nil
}

// Get implements BundleStore.
func (s *GCSStore) Get(ctx context.Context, h BundleHandle) (io.ReadCloser, error) {
	if err := s.validateKey(h); err != nil {
		return nil, err
	}
	rc, err := s.client.Bucket(s.bucket).Object(s.objectName(h.Key)).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("gcs: open %s: %w", h.Key, err)
	}
	return rc, nil
}

// Delete implements BundleStore. Idempotent: missing objects are not
// an error.
func (s *GCSStore) Delete(ctx context.Context, h BundleHandle) error {
	if err := s.validateKey(h); err != nil {
		// Invalid handle refers to no object; treat as success so
		// callers can't loop forever on a bad entry from List.
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	err := s.client.Bucket(s.bucket).Object(s.objectName(h.Key)).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("gcs: delete %s: %w", h.Key, err)
	}
	return nil
}

// List implements BundleStore. Iterates the bucket with the configured
// prefix, strips the prefix from each object name to rebuild the
// relative Key, and returns every completed bundle. Pagination is
// handled internally by the storage client.
func (s *GCSStore) List(ctx context.Context) ([]BundleHandle, error) {
	var out []BundleHandle
	query := &storage.Query{Prefix: s.prefix}
	it := s.client.Bucket(s.bucket).Objects(ctx, query)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs: list bucket %s: %w", s.bucket, err)
		}
		rel, ok := s.stripPrefix(attrs.Name)
		if !ok {
			continue
		}
		// Key must be <beadID>/<file>. Skip anything that doesn't fit
		// the expected shape rather than surfacing malformed handles.
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) != 2 || parts[0] == "" {
			continue
		}
		out = append(out, BundleHandle{BeadID: parts[0], Key: rel})
	}
	return out, nil
}

// Stat implements BundleStore.
func (s *GCSStore) Stat(ctx context.Context, h BundleHandle) (BundleInfo, error) {
	if err := s.validateKey(h); err != nil {
		return BundleInfo{}, err
	}
	attrs, err := s.client.Bucket(s.bucket).Object(s.objectName(h.Key)).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return BundleInfo{}, ErrNotFound
		}
		return BundleInfo{}, fmt.Errorf("gcs: stat %s: %w", h.Key, err)
	}
	return BundleInfo{Size: attrs.Size, ModTime: attrs.Updated}, nil
}

// validateKey rejects handles with empty keys or shapes that suggest a
// path-traversal attempt. The prefix guard means a traversal could only
// reach other objects in the same bucket — but surfacing the malformed
// handle as ErrNotFound matches LocalStore's behavior and keeps the
// caller's error handling uniform across backends.
func (s *GCSStore) validateKey(h BundleHandle) error {
	if h.Key == "" {
		return ErrNotFound
	}
	if strings.Contains(h.Key, "..") || strings.HasPrefix(h.Key, "/") {
		return ErrNotFound
	}
	return nil
}

// objectName joins the prefix and the relative Key into a full GCS
// object name.
func (s *GCSStore) objectName(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

// stripPrefix returns the object name with the store prefix removed.
// Returns false when the name doesn't live under the configured prefix
// (e.g. another tool wrote to the bucket root).
func (s *GCSStore) stripPrefix(name string) (string, bool) {
	if s.prefix == "" {
		return name, true
	}
	lead := s.prefix + "/"
	if !strings.HasPrefix(name, lead) {
		return "", false
	}
	return strings.TrimPrefix(name, lead), true
}

// normalizeGCSPrefix strips leading and trailing slashes so the prefix
// composes cleanly with relative keys. An empty prefix is valid and
// means "store at the bucket root".
func normalizeGCSPrefix(p string) string {
	p = strings.Trim(p, "/")
	return p
}

// isGCSPreconditionFailed reports whether err is the 412 a GCS writer
// returns from Close when DoesNotExist=true rejects a duplicate upload.
func isGCSPreconditionFailed(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code == 412
	}
	return false
}
