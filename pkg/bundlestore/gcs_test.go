package bundlestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"google.golang.org/api/iterator"
)

// newTestGCSServer spins up an in-memory fake GCS server with the given
// bucket pre-created. The server is torn down at the end of the test.
func newTestGCSServer(t *testing.T, bucket string) *fakestorage.Server {
	t.Helper()
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme:   "http",
		NoListener: false,
	})
	if err != nil {
		t.Fatalf("fake-gcs-server: %v", err)
	}
	t.Cleanup(srv.Stop)
	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucket})
	return srv
}

// newTestGCSStore builds a GCSStore pointed at a fake GCS server and
// asserts the bucket-probe succeeds. The returned store's storage client
// is scoped to the fake server — DO NOT call Close, the fake server
// manages the underlying transport.
func newTestGCSStore(t *testing.T, cfg Config) (*GCSStore, *fakestorage.Server) {
	t.Helper()
	if cfg.GCSBucket == "" {
		cfg.GCSBucket = "spire-test-bundles"
	}
	srv := newTestGCSServer(t, cfg.GCSBucket)
	store, err := newGCSStoreWithClient(context.Background(), srv.Client(), cfg)
	if err != nil {
		t.Fatalf("newGCSStoreWithClient: %v", err)
	}
	return store, srv
}

func TestGCSStore_PutGetRoundTrip(t *testing.T) {
	s, _ := newTestGCSStore(t, Config{MaxBytes: DefaultMaxBytes, GCSPrefix: "bundles"})
	ctx := context.Background()

	payload := []byte("fake git bundle bytes")
	h, err := s.Put(ctx, defaultReq(), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if h.BeadID != "spi-abc" {
		t.Errorf("BeadID = %q, want spi-abc", h.BeadID)
	}
	if h.Key == "" {
		t.Fatal("empty key")
	}
	// Key is store-opaque but must NOT carry the store prefix — the
	// prefix is a backend detail.
	if strings.HasPrefix(h.Key, "bundles/") {
		t.Errorf("Key %q leaks prefix", h.Key)
	}

	rc, err := s.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestGCSStore_EmptyPrefix(t *testing.T) {
	s, srv := newTestGCSStore(t, Config{MaxBytes: DefaultMaxBytes, GCSPrefix: ""})
	ctx := context.Background()

	h, err := s.Put(ctx, defaultReq(), bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The raw object name in the bucket must equal the relative key —
	// no leading slash or prefix artefact.
	client := srv.Client()
	it := client.Bucket(s.Bucket()).Objects(ctx, &storage.Query{})
	seen := []string{}
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("iterate: %v", err)
		}
		seen = append(seen, attrs.Name)
	}
	if len(seen) != 1 || seen[0] != h.Key {
		t.Errorf("bucket objects = %v, want [%q]", seen, h.Key)
	}
}

func TestGCSStore_RejectsDuplicate(t *testing.T) {
	s, _ := newTestGCSStore(t, Config{MaxBytes: DefaultMaxBytes, GCSPrefix: "bundles"})
	ctx := context.Background()

	if _, err := s.Put(ctx, defaultReq(), bytes.NewReader([]byte("one"))); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	_, err := s.Put(ctx, defaultReq(), bytes.NewReader([]byte("two")))
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second Put err = %v, want ErrDuplicate", err)
	}
}

func TestGCSStore_EnforcesSizeLimit(t *testing.T) {
	s, srv := newTestGCSStore(t, Config{MaxBytes: 16, GCSPrefix: "bundles"})
	ctx := context.Background()

	// Exactly at the limit should succeed.
	if _, err := s.Put(ctx, defaultReq(), bytes.NewReader(bytes.Repeat([]byte("x"), 16))); err != nil {
		t.Fatalf("at-limit Put: %v", err)
	}

	// One byte over should fail with ErrTooLarge and leave no object behind.
	over := PutRequest{BeadID: "spi-abc", AttemptID: "spi-att2", ApprenticeIdx: 0}
	_, err := s.Put(ctx, over, bytes.NewReader(bytes.Repeat([]byte("x"), 17)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over-limit Put err = %v, want ErrTooLarge", err)
	}

	// Confirm the oversize attempt didn't land in the bucket. Only the
	// at-limit object should be present.
	client := srv.Client()
	it := client.Bucket(s.Bucket()).Objects(ctx, &storage.Query{})
	count := 0
	for {
		_, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("iterate: %v", err)
		}
		count++
	}
	if count != 1 {
		t.Errorf("bucket object count = %d, want 1", count)
	}
}

func TestGCSStore_DeleteIdempotent(t *testing.T) {
	s, _ := newTestGCSStore(t, Config{MaxBytes: DefaultMaxBytes, GCSPrefix: "bundles"})
	ctx := context.Background()

	h, err := s.Put(ctx, defaultReq(), bytes.NewReader([]byte("bytes")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, h); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := s.Delete(ctx, h); err != nil {
		t.Fatalf("second Delete (should be idempotent): %v", err)
	}
	if _, err := s.Get(ctx, h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete err = %v, want ErrNotFound", err)
	}
}

func TestGCSStore_GetMissing(t *testing.T) {
	s, _ := newTestGCSStore(t, Config{MaxBytes: DefaultMaxBytes, GCSPrefix: "bundles"})
	ctx := context.Background()
	_, err := s.Get(ctx, BundleHandle{BeadID: "spi-abc", Key: "spi-abc/missing-0.bundle"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}
}

func TestGCSStore_ListStripsPrefix(t *testing.T) {
	s, _ := newTestGCSStore(t, Config{MaxBytes: DefaultMaxBytes, GCSPrefix: "bundles"})
	ctx := context.Background()

	reqs := []PutRequest{
		{BeadID: "spi-epic", AttemptID: "spi-att", ApprenticeIdx: 0},
		{BeadID: "spi-epic", AttemptID: "spi-att", ApprenticeIdx: 1},
		{BeadID: "spi-other", AttemptID: "spi-att", ApprenticeIdx: 0},
	}
	for _, r := range reqs {
		if _, err := s.Put(ctx, r, bytes.NewReader([]byte("b"))); err != nil {
			t.Fatalf("Put(%+v): %v", r, err)
		}
	}

	handles, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(handles) != 3 {
		t.Fatalf("List = %d handles, want 3 (%v)", len(handles), handles)
	}
	for _, h := range handles {
		// Key is the relative form (<beadID>/<attempt>-<idx>.bundle)
		// with the prefix stripped.
		if strings.HasPrefix(h.Key, "bundles/") {
			t.Errorf("handle.Key %q leaks prefix", h.Key)
		}
		if !strings.HasSuffix(h.Key, ".bundle") {
			t.Errorf("handle.Key %q missing .bundle suffix", h.Key)
		}
	}
}

func TestGCSStore_ListIgnoresForeignObjects(t *testing.T) {
	cfg := Config{MaxBytes: DefaultMaxBytes, GCSPrefix: "bundles", GCSBucket: "spire-test-bundles"}
	s, srv := newTestGCSStore(t, cfg)
	ctx := context.Background()

	if _, err := s.Put(ctx, defaultReq(), bytes.NewReader([]byte("mine"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Drop an object outside the configured prefix — e.g. another tool
	// sharing the bucket. List must ignore it.
	srv.CreateObject(fakestorage.Object{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName: cfg.GCSBucket,
			Name:       "foreign/thing.bin",
		},
		Content: []byte("not mine"),
	})

	handles, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("List = %d handles, want 1 (%v)", len(handles), handles)
	}
}

func TestGCSStore_ValidatesIDs(t *testing.T) {
	s, _ := newTestGCSStore(t, Config{MaxBytes: DefaultMaxBytes, GCSPrefix: "bundles"})
	ctx := context.Background()

	bad := []PutRequest{
		{BeadID: "", AttemptID: "spi-a", ApprenticeIdx: 0},
		{BeadID: "spi-a", AttemptID: "", ApprenticeIdx: 0},
		{BeadID: "../etc/passwd", AttemptID: "spi-a", ApprenticeIdx: 0},
		{BeadID: "spi-a", AttemptID: "sp/../b", ApprenticeIdx: 0},
		{BeadID: "spi-a", AttemptID: "spi-a", ApprenticeIdx: -1},
		{BeadID: "SPI-UPPER", AttemptID: "spi-a", ApprenticeIdx: 0},
	}
	for _, req := range bad {
		if _, err := s.Put(ctx, req, bytes.NewReader([]byte("x"))); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Put(%+v) err = %v, want ErrInvalidRequest", req, err)
		}
	}
}

func TestGCSStore_RejectsTraversal(t *testing.T) {
	s, _ := newTestGCSStore(t, Config{MaxBytes: DefaultMaxBytes, GCSPrefix: "bundles"})
	ctx := context.Background()

	bad := []BundleHandle{
		{BeadID: "spi-a", Key: "../escape.bundle"},
		{BeadID: "spi-a", Key: "/etc/passwd"},
		{BeadID: "spi-a", Key: "spi-a/../../etc"},
		{BeadID: "spi-a", Key: ""},
	}
	for _, h := range bad {
		if _, err := s.Get(ctx, h); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%+v) err = %v, want ErrNotFound", h, err)
		}
	}
}

func TestGCSStore_Stat(t *testing.T) {
	s, _ := newTestGCSStore(t, Config{MaxBytes: DefaultMaxBytes, GCSPrefix: "bundles"})
	ctx := context.Background()

	payload := []byte("twelve bytes")
	h, err := s.Put(ctx, defaultReq(), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := s.Stat(ctx, h)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", info.Size, len(payload))
	}
	if info.ModTime.IsZero() {
		t.Error("ModTime is zero")
	}
}

func TestGCSStore_ConstructorFailsOnMissingBucket(t *testing.T) {
	// Bring up a fake server but do NOT create the target bucket.
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http"})
	if err != nil {
		t.Fatalf("fake-gcs-server: %v", err)
	}
	defer srv.Stop()

	_, err = newGCSStoreWithClient(context.Background(), srv.Client(), Config{GCSBucket: "no-such-bucket"})
	if err == nil {
		t.Fatal("expected constructor error for missing bucket")
	}
	if !strings.Contains(err.Error(), "no-such-bucket") {
		t.Errorf("error %q missing bucket name", err.Error())
	}
	if !strings.Contains(err.Error(), "gsutil mb") {
		t.Errorf("error %q missing gsutil hint", err.Error())
	}
}

func TestGCSStore_ConstructorRequiresBucket(t *testing.T) {
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http"})
	if err != nil {
		t.Fatalf("fake-gcs-server: %v", err)
	}
	defer srv.Stop()

	_, err = newGCSStoreWithClient(context.Background(), srv.Client(), Config{})
	if err == nil {
		t.Fatal("expected constructor error when GCSBucket is empty")
	}
}

func TestNew_DispatchesOnBackend(t *testing.T) {
	ctx := context.Background()

	// Empty backend resolves to local.
	bs, err := New(ctx, Config{LocalRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("New empty backend: %v", err)
	}
	if _, ok := bs.(*LocalStore); !ok {
		t.Errorf("empty backend produced %T, want *LocalStore", bs)
	}

	// Explicit local.
	bs, err = New(ctx, Config{Backend: "local", LocalRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("New local: %v", err)
	}
	if _, ok := bs.(*LocalStore); !ok {
		t.Errorf("local backend produced %T, want *LocalStore", bs)
	}

	// Unknown backend surfaces a diagnostic error.
	_, err = New(ctx, Config{Backend: "azureblob"})
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
	if !strings.Contains(err.Error(), "azureblob") {
		t.Errorf("error %q should name the offending backend", err.Error())
	}
	if !strings.Contains(err.Error(), "supported:") {
		t.Errorf("error %q should list supported backends", err.Error())
	}
}

func TestNormalizeGCSPrefix(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		"/":                  "",
		"bundles":            "bundles",
		"/bundles":           "bundles",
		"bundles/":           "bundles",
		"/bundles/":          "bundles",
		"foo/bar":            "foo/bar",
		"/a/b/c/":            "a/b/c",
	}
	for in, want := range cases {
		if got := normalizeGCSPrefix(in); got != want {
			t.Errorf("normalizeGCSPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
