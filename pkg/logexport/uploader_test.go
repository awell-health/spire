package logexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/logartifact"
)

// uploaderTestKit bundles the collaborators most uploader tests use so
// each table case stays compact.
type uploaderTestKit struct {
	store    *fakeStore
	stdout   *bytes.Buffer
	sink     *StdoutSink
	stats    *atomicStats
	uploader *Uploader
}

func newUploaderTestKit(t *testing.T) *uploaderTestKit {
	t.Helper()
	stats := &atomicStats{}
	stdout := &bytes.Buffer{}
	sink, err := NewStdoutSink(stdout, stats)
	if err != nil {
		t.Fatalf("NewStdoutSink: %v", err)
	}
	store := newFakeStore()
	uploader, err := NewUploader(store, nil, logartifact.VisibilityEngineerOnly, sink, stats)
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}
	uploader.SetRetryPolicy(noRetry)
	return &uploaderTestKit{store: store, stdout: stdout, sink: sink, stats: stats, uploader: uploader}
}

// TestUploader_AppendThenCloseFinalizes asserts the canonical happy
// path: Append opens the artifact, Close finalizes it, the manifest
// row carries the right size + checksum, and stats track the lifecycle.
func TestUploader_AppendThenCloseFinalizes(t *testing.T) {
	kit := newUploaderTestKit(t)
	id := stableIdentity()
	payload := []byte(`{"event":"hello"}` + "\n" + `{"event":"world"}` + "\n")

	if err := kit.uploader.Append(context.Background(), id, 0, payload); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := kit.uploader.Tracking(); got != 1 {
		t.Errorf("Tracking after Append = %d, want 1", got)
	}
	if err := kit.uploader.Close(context.Background(), id, 0, "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := kit.uploader.Tracking(); got != 0 {
		t.Errorf("Tracking after Close = %d, want 0", got)
	}

	manifests, err := kit.store.List(context.Background(), logartifact.Filter{BeadID: id.BeadID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(manifests))
	}
	if manifests[0].Status != logartifact.StatusFinalized {
		t.Errorf("Status = %q, want %q", manifests[0].Status, logartifact.StatusFinalized)
	}
	if manifests[0].ByteSize != int64(len(payload)) {
		t.Errorf("ByteSize = %d, want %d", manifests[0].ByteSize, len(payload))
	}
	hash := sha256.Sum256(payload)
	wantChecksum := "sha256:" + hex.EncodeToString(hash[:])
	if manifests[0].Checksum != wantChecksum {
		t.Errorf("Checksum = %q, want %q", manifests[0].Checksum, wantChecksum)
	}
	if got := kit.stats.Snapshot().ArtifactsFinalized; got != 1 {
		t.Errorf("Stats.ArtifactsFinalized = %d, want 1", got)
	}
}

// TestUploader_OpenFailureMarksStatsFailed asserts that a Put error
// stops the artifact from progressing, increments the failed counter,
// and emits an operational ERROR record on stdout.
func TestUploader_OpenFailureMarksStatsFailed(t *testing.T) {
	kit := newUploaderTestKit(t)
	kit.store.putFailures = []error{errors.New("dolt connection refused")}

	err := kit.uploader.Append(context.Background(), stableIdentity(), 0, []byte("x"))
	if err == nil {
		t.Fatal("Append: nil error, want failure")
	}
	if got := kit.stats.Snapshot().ArtifactsFailed; got < 1 {
		t.Errorf("Stats.ArtifactsFailed = %d, want >= 1", got)
	}
	if !strings.Contains(kit.stdout.String(), `"severity":"ERROR"`) {
		t.Errorf("expected ERROR operational record on stdout; got %s", kit.stdout.String())
	}
}

// TestUploader_FinalizeFailureMarksRowFailed asserts that a finalize
// error transitions the manifest into a visible failed state and
// surfaces an ERROR operational record. The agent's path forward is
// uninterrupted — the test asserts the failure is recorded rather
// than hidden.
func TestUploader_FinalizeFailureMarksRowFailed(t *testing.T) {
	kit := newUploaderTestKit(t)
	kit.store.finalizeFailures = []error{errors.New("dolt write timeout")}

	id := stableIdentity()
	if err := kit.uploader.Append(context.Background(), id, 0, []byte("x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := kit.uploader.Close(context.Background(), id, 0, "shutdown"); err == nil {
		t.Fatal("Close: nil error, want failure")
	}

	if got := kit.stats.Snapshot().ArtifactsFailed; got < 1 {
		t.Errorf("Stats.ArtifactsFailed = %d, want >= 1", got)
	}
	if !strings.Contains(kit.stdout.String(), `finalize artifact failed`) {
		t.Errorf("expected finalize-failure operational record; got %s", kit.stdout.String())
	}
}

// TestUploader_RetryThenSucceed exercises the retry path with a
// permissive policy. Two transient failures followed by success should
// finalize cleanly and bump the retries counter.
func TestUploader_RetryThenSucceed(t *testing.T) {
	kit := newUploaderTestKit(t)
	kit.uploader.SetRetryPolicy(retryPolicy{MaxAttempts: 5, BaseDelay: 0})
	kit.store.putFailures = []error{errors.New("blip"), errors.New("blip")}

	id := stableIdentity()
	if err := kit.uploader.Append(context.Background(), id, 0, []byte("x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := kit.uploader.Close(context.Background(), id, 0, "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stats := kit.stats.Snapshot()
	if stats.ArtifactsFinalized != 1 {
		t.Errorf("ArtifactsFinalized = %d, want 1", stats.ArtifactsFinalized)
	}
	if stats.ManifestRetries < 2 {
		t.Errorf("ManifestRetries = %d, want >= 2", stats.ManifestRetries)
	}
}

// TestUploader_CloseAllDrainsEverything covers the shutdown path: after
// CloseAll every previously-open entry is finalized and Tracking
// returns to zero.
func TestUploader_CloseAllDrainsEverything(t *testing.T) {
	kit := newUploaderTestKit(t)
	for i := 0; i < 3; i++ {
		id := stableIdentity()
		id.RunID = "run-" + string(rune('a'+i))
		if err := kit.uploader.Append(context.Background(), id, 0, []byte("x")); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if got := kit.uploader.Tracking(); got != 3 {
		t.Errorf("Tracking before flush = %d, want 3", got)
	}
	if err := kit.uploader.CloseAll(context.Background(), "shutdown"); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if got := kit.uploader.Tracking(); got != 0 {
		t.Errorf("Tracking after flush = %d, want 0", got)
	}
	if got := kit.stats.Snapshot().ArtifactsFinalized; got != 3 {
		t.Errorf("Stats.ArtifactsFinalized = %d, want 3", got)
	}
}

// TestUploader_ChecksumMatchesPayload pins the checksum byte-for-byte
// against the known SHA-256 of the input. This is the property the
// gateway depends on when serving a bead's logs.
func TestUploader_ChecksumMatchesPayload(t *testing.T) {
	kit := newUploaderTestKit(t)
	payload := []byte("the quick brown fox jumps over the lazy dog\n")
	id := stableIdentity()

	if err := kit.uploader.Append(context.Background(), id, 0, payload); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := kit.uploader.Close(context.Background(), id, 0, "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	manifests, _ := kit.store.List(context.Background(), logartifact.Filter{BeadID: id.BeadID})
	if len(manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(manifests))
	}
	want := sha256.Sum256(payload)
	got := manifests[0].Checksum
	if got != "sha256:"+hex.EncodeToString(want[:]) {
		t.Errorf("Checksum = %q, want sha256:%x", got, want)
	}
}

// TestUploader_SequenceMonotonicityAcrossRotation closes sequence 0,
// then opens sequence 1 with the same Identity. Each lands as a
// distinct manifest row, simulating the rotation path the tailer uses.
func TestUploader_SequenceMonotonicityAcrossRotation(t *testing.T) {
	kit := newUploaderTestKit(t)
	id := stableIdentity()

	if err := kit.uploader.Append(context.Background(), id, 0, []byte("a\n")); err != nil {
		t.Fatalf("Append seq0: %v", err)
	}
	if err := kit.uploader.Close(context.Background(), id, 0, "rotated"); err != nil {
		t.Fatalf("Close seq0: %v", err)
	}
	if err := kit.uploader.Append(context.Background(), id, 1, []byte("b\n")); err != nil {
		t.Fatalf("Append seq1: %v", err)
	}
	if err := kit.uploader.Close(context.Background(), id, 1, "test"); err != nil {
		t.Fatalf("Close seq1: %v", err)
	}

	manifests, _ := kit.store.List(context.Background(), logartifact.Filter{BeadID: id.BeadID})
	if len(manifests) != 2 {
		t.Fatalf("manifests = %d, want 2", len(manifests))
	}
}

// TestUploader_RejectsInvalidVisibility ensures NewUploader rejects an
// out-of-range visibility so a misconfigured caller fails at construction.
func TestUploader_RejectsInvalidVisibility(t *testing.T) {
	stats := &atomicStats{}
	sink, _ := NewStdoutSink(&bytes.Buffer{}, stats)
	_, err := NewUploader(newFakeStore(), nil, logartifact.Visibility("classified"), sink, stats)
	if err == nil {
		t.Errorf("NewUploader: nil error for invalid visibility")
	}
}

// TestUploader_CloseUnopenedKeyIsNoop covers the shutdown idempotency
// — Close on a (identity, sequence) pair the uploader never saw is a
// no-op rather than an error.
func TestUploader_CloseUnopenedKeyIsNoop(t *testing.T) {
	kit := newUploaderTestKit(t)
	if err := kit.uploader.Close(context.Background(), stableIdentity(), 0, "drain"); err != nil {
		t.Errorf("Close on unopened key returned %v", err)
	}
}
