package logartifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fsouza/fake-gcs-server/fakestorage"

	pkgstore "github.com/awell-health/spire/pkg/store"
)

const testGCSBucket = "spire-test-logs"

// newTestGCSServer brings up an in-memory fake GCS server with the
// bucket pre-created. Stopped at test cleanup. Mirrors the bundlestore
// test helper so the substrate matches the rest of the codebase's
// test infra.
func newTestGCSServer(t *testing.T, bucket string) *fakestorage.Server {
	t.Helper()
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme:     "http",
		NoListener: false,
	})
	if err != nil {
		t.Fatalf("fake-gcs-server: %v", err)
	}
	t.Cleanup(srv.Stop)
	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucket})
	return srv
}

// newGCSForTest constructs a GCSStore over a fake server and a sqlmock
// connection. Returns the store, mock controller, and *sql.DB so tests
// can register manifest expectations.
func newGCSForTest(t *testing.T, prefix string) (*GCSStore, sqlmock.Sqlmock, *sql.DB, *fakestorage.Server) {
	t.Helper()
	srv := newTestGCSServer(t, testGCSBucket)
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewGCS(context.Background(), srv.Client(), testGCSBucket, prefix, db)
	if err != nil {
		t.Fatalf("NewGCS: %v", err)
	}
	return store, mock, db, srv
}

// TestNewGCS_RejectsBadInputs covers the constructor's required-field
// validation. The plumbing for credentials / client construction lives
// in the caller (spi-hzeyz9), so we only test the local invariants.
func TestNewGCS_RejectsBadInputs(t *testing.T) {
	srv := newTestGCSServer(t, testGCSBucket)
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := NewGCS(context.Background(), nil, testGCSBucket, "", db); err == nil {
		t.Error("expected error for nil client")
	}
	if _, err := NewGCS(context.Background(), srv.Client(), "", "", db); err == nil {
		t.Error("expected error for empty bucket")
	}
	if _, err := NewGCS(context.Background(), srv.Client(), testGCSBucket, "", nil); err == nil {
		t.Error("expected error for nil db")
	}
}

// TestNewGCS_FailsLoudOnMissingBucket mirrors the bundlestore probe
// behavior: a missing bucket surfaces with a `gsutil mb` hint at
// construction time, not on first write. Configuration errors should
// be loud at startup.
func TestNewGCS_FailsLoudOnMissingBucket(t *testing.T) {
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http"})
	if err != nil {
		t.Fatalf("fake-gcs-server: %v", err)
	}
	defer srv.Stop()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = NewGCS(context.Background(), srv.Client(), "no-such-log-bucket", "", db)
	if err == nil {
		t.Fatal("expected probe error for missing bucket")
	}
	if !strings.Contains(err.Error(), "no-such-log-bucket") {
		t.Errorf("error %q missing bucket name", err)
	}
}

// TestGCSStore_ObjectKeyShape proves the GCS backend uses the same
// path layout as the local backend. This is the contract spi-k1cnof
// (exporter) and spi-j3r694 (gateway) rely on to reach the same byte
// without coordinating with this package at runtime.
func TestGCSStore_ObjectKeyShape(t *testing.T) {
	store, mock, _, srv := newGCSForTest(t, "logs")
	ctx := context.Background()

	identity := validIdentity()
	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w, err := store.Put(ctx, identity, 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	wantURI := "gs://" + testGCSBucket + "/logs/awell-test/spi-b986in/spi-attempt/run-001/wizard-spi-b986in/wizard/implement/claude/transcript.jsonl"
	if w.ObjectURI() != wantURI {
		t.Errorf("ObjectURI = %q, want %q", w.ObjectURI(), wantURI)
	}

	// A close without finalize is a no-op for the manifest, but it
	// still flushes the GCS writer. This shouldn't error.
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	_ = srv
}

// TestGCSStore_PutFinalizeRoundTrip is the GCS-side acceptance test:
// bytes uploaded through Put + Finalize land in the bucket and the
// manifest reflects the size and checksum.
func TestGCSStore_PutFinalizeRoundTrip(t *testing.T) {
	store, mock, _, srv := newGCSForTest(t, "logs")
	ctx := context.Background()

	identity := validIdentity()
	payload := []byte(`{"event":"hello"}` + "\n")
	wantSum := sha256.Sum256(payload)
	wantHex := "sha256:" + hex.EncodeToString(wantSum[:])

	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w, err := store.Put(ctx, identity, 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Finalize: lookup writing → finalize update → re-fetch finalized.
	expectGetByID(mock, w.ManifestID(), pkgstore.LogArtifactStatusWriting, w.ObjectURI(), nil, "")
	mock.ExpectExec(`UPDATE agent_log_artifacts SET\s+byte_size`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	size := int64(len(payload))
	expectGetByID(mock, w.ManifestID(), pkgstore.LogArtifactStatusFinalized, w.ObjectURI(), &size, wantHex)

	manifest, err := store.Finalize(ctx, w)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if manifest.ByteSize != size {
		t.Errorf("ByteSize = %d, want %d", manifest.ByteSize, size)
	}
	if manifest.Checksum != wantHex {
		t.Errorf("Checksum = %q, want %q", manifest.Checksum, wantHex)
	}

	// Verify GCS holds the bytes. Read directly via the underlying
	// client so we test the upload path, not Get's manifest lookup.
	objKey := "logs/awell-test/spi-b986in/spi-attempt/run-001/wizard-spi-b986in/wizard/implement/claude/transcript.jsonl"
	rc, err := srv.Client().Bucket(testGCSBucket).Object(objKey).NewReader(ctx)
	if err != nil {
		t.Fatalf("open uploaded object: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read uploaded object: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("uploaded bytes = %q, want %q", got, payload)
	}
}

// TestGCSStore_GetReturnsBytesAndManifest exercises the Get path: the
// gateway / `spire logs pretty` flow uses Stat or Get to fetch existing
// artifacts.
func TestGCSStore_GetReturnsBytesAndManifest(t *testing.T) {
	store, mock, _, srv := newGCSForTest(t, "logs")
	ctx := context.Background()

	// Plant an object directly in the fake bucket.
	objKey := "logs/awell-test/spi-b986in/spi-attempt/run-001/wizard-spi-b986in/wizard/implement/claude/transcript.jsonl"
	srv.CreateObject(fakestorage.Object{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName: testGCSBucket,
			Name:       objKey,
		},
		Content: []byte("planted-bytes"),
	})

	uri := "gs://" + testGCSBucket + "/" + objKey
	size := int64(len("planted-bytes"))
	expectGetByID(mock, "log-getme", pkgstore.LogArtifactStatusFinalized, uri, &size, "sha256:abc")

	rc, manifest, err := store.Get(ctx, ManifestRef{ID: "log-getme"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "planted-bytes" {
		t.Errorf("Get bytes = %q, want planted-bytes", got)
	}
	if manifest.ObjectURI != uri {
		t.Errorf("ObjectURI = %q, want %q", manifest.ObjectURI, uri)
	}
}

// TestGCSStore_GetMissingObjectReturnsErrNotFound exercises the
// missing-object path. The manifest row exists, but the bucket has no
// matching object — a possible state if the bucket's lifecycle policy
// reaped the bytes before the manifest was cleaned up.
func TestGCSStore_GetMissingObjectReturnsErrNotFound(t *testing.T) {
	store, mock, _, _ := newGCSForTest(t, "logs")
	ctx := context.Background()

	uri := "gs://" + testGCSBucket + "/logs/awell-test/spi-b986in/spi-attempt/run-001/wizard-spi-b986in/wizard/implement/claude/transcript.jsonl"
	expectGetByID(mock, "log-x", pkgstore.LogArtifactStatusFinalized, uri, nil, "")

	_, _, err := store.Get(ctx, ManifestRef{ID: "log-x"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestGCSStore_GetMissingManifestReturnsErrNotFound covers the
// manifest-side failure path: ID resolves to no row.
func TestGCSStore_GetMissingManifestReturnsErrNotFound(t *testing.T) {
	store, mock, _, _ := newGCSForTest(t, "logs")
	ctx := context.Background()

	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts WHERE id = \?`).
		WithArgs("log-missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "tower", "bead_id", "attempt_id", "run_id", "agent_name",
			"role", "phase", "provider", "stream", "sequence", "object_uri",
			"byte_size", "checksum", "status", "started_at", "ended_at",
			"created_at", "updated_at", "redaction_version", "summary", "tail",
		}))

	_, _, err := store.Get(ctx, ManifestRef{ID: "log-missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestGCSStore_GetRejectsCrossBucketURI guards against a manifest row
// pointing at a different bucket than the store was constructed with.
// This shouldn't happen in production (the operator pins the bucket),
// but failing loud beats silently reading from the wrong bucket.
func TestGCSStore_GetRejectsCrossBucketURI(t *testing.T) {
	store, mock, _, _ := newGCSForTest(t, "logs")
	ctx := context.Background()

	expectGetByID(mock, "log-cross", pkgstore.LogArtifactStatusFinalized, "gs://other-bucket/logs/x.jsonl", nil, "")

	_, _, err := store.Get(ctx, ManifestRef{ID: "log-cross"})
	if err == nil {
		t.Fatal("expected error for cross-bucket URI")
	}
	if !strings.Contains(err.Error(), "other-bucket") {
		t.Errorf("error %q should name the other bucket", err)
	}
}

// TestGCSStore_EmptyPrefix stores objects at bucket root, no prefix.
func TestGCSStore_EmptyPrefix(t *testing.T) {
	store, mock, _, _ := newGCSForTest(t, "")
	ctx := context.Background()

	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w, err := store.Put(ctx, validIdentity(), 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasPrefix(w.ObjectURI(), "gs://"+testGCSBucket+"/awell-test/") {
		t.Errorf("ObjectURI = %q, want bucket-root prefix", w.ObjectURI())
	}
	_ = w.Close()
}

// TestParseGCSURI exercises the URI splitter directly.
func TestParseGCSURI(t *testing.T) {
	cases := []struct {
		in      string
		bucket  string
		key     string
		wantErr bool
	}{
		{"gs://b/k.jsonl", "b", "k.jsonl", false},
		{"gs://b/path/to/k.jsonl", "b", "path/to/k.jsonl", false},
		{"file:///x", "", "", true},
		{"gs://", "", "", true},
		{"not-a-uri", "", "", true},
	}
	for _, tc := range cases {
		bucket, key, err := parseGCSURI(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseGCSURI(%q) expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseGCSURI(%q): %v", tc.in, err)
			continue
		}
		if bucket != tc.bucket || key != tc.key {
			t.Errorf("parseGCSURI(%q) = (%q, %q), want (%q, %q)",
				tc.in, bucket, key, tc.bucket, tc.key)
		}
	}
}

// TestGCSStore_ListDelegatesToManifest verifies List queries the
// manifest table — never the bucket. The design forecloses bucket
// LIST as a discovery path; the manifest is the index of record.
func TestGCSStore_ListDelegatesToManifest(t *testing.T) {
	store, mock, _, srv := newGCSForTest(t, "logs")
	ctx := context.Background()

	// Plant a foreign object in the bucket. List must NOT see it.
	srv.CreateObject(fakestorage.Object{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName: testGCSBucket,
			Name:       "logs/foreign/object.jsonl",
		},
		Content: []byte("not in manifest"),
	})

	rows := sqlmock.NewRows([]string{
		"id", "tower", "bead_id", "attempt_id", "run_id", "agent_name",
		"role", "phase", "provider", "stream", "sequence", "object_uri",
		"byte_size", "checksum", "status", "started_at", "ended_at",
		"created_at", "updated_at", "redaction_version", "summary", "tail",
	})
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts\s+WHERE bead_id = \?`).
		WithArgs("spi-b986in").
		WillReturnRows(rows)

	got, err := store.List(ctx, Filter{BeadID: "spi-b986in"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %d, want 0 (bucket should not be scanned)", len(got))
	}
}
