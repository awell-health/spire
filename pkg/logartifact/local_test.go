package logartifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	pkgstore "github.com/awell-health/spire/pkg/store"
)

// newLocalForTest builds a LocalStore over t.TempDir() backed by a
// sqlmock connection. The mock controller is returned so tests can
// register the SQL expectations that follow.
func newLocalForTest(t *testing.T) (*LocalStore, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewLocal(t.TempDir(), db)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return store, mock, db
}

// expectInsertWriting registers the INSERT issued by Put.
func expectInsertWriting(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

// expectGetByID registers the SELECT issued by Finalize/Get/Stat to
// look up a row by primary key. status controls the row's status.
func expectGetByID(mock sqlmock.Sqlmock, id string, status, objectURI string, byteSize *int64, checksum string) {
	expectGetByIDWithVisibility(mock, id, status, objectURI, byteSize, checksum, "engineer_only", 0)
}

// expectGetByIDWithVisibility extends expectGetByID with visibility and
// redaction_version controls. Most tests stick with engineer_only / 0;
// redaction round-trip tests pass desktop_safe / a non-zero version.
func expectGetByIDWithVisibility(mock sqlmock.Sqlmock, id string, status, objectURI string, byteSize *int64, checksum, visibility string, redactionVersion int) {
	rows := sqlmock.NewRows([]string{
		"id", "tower", "bead_id", "attempt_id", "run_id", "agent_name",
		"role", "phase", "provider", "stream", "sequence", "object_uri",
		"byte_size", "checksum", "status", "started_at", "ended_at",
		"created_at", "updated_at", "redaction_version", "visibility",
		"summary", "tail",
	})
	var sizeArg, checksumArg interface{}
	if byteSize != nil {
		sizeArg = *byteSize
	}
	if checksum != "" {
		checksumArg = checksum
	}
	rows.AddRow(
		id, "awell-test", "spi-b986in", "spi-attempt", "run-001",
		"wizard-spi-b986in", "wizard", "implement", "claude", "transcript",
		0, objectURI,
		sizeArg, checksumArg,
		status, nil, nil,
		"2026-04-28 01:00:00", "2026-04-28 01:01:00",
		redactionVersion, visibility, nil, nil,
	)
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts WHERE id = \?`).
		WithArgs(id).
		WillReturnRows(rows)
}

// expectFinalize registers the UPDATE issued by FinalizeLogArtifact.
func expectFinalize(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`UPDATE agent_log_artifacts SET\s+byte_size`).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestNewLocal_RejectsBadInputs(t *testing.T) {
	if _, err := NewLocal("", nil); err == nil {
		t.Error("expected error for empty rootDir + nil db")
	}
	if _, err := NewLocal("relative/path", &sql.DB{}); err == nil {
		t.Error("expected error for relative rootDir")
	}
	if _, err := NewLocal(t.TempDir(), nil); err == nil {
		t.Error("expected error for nil db")
	}
}

// TestLocalStore_PutFinalizeRoundTrip is the core acceptance test:
// write bytes through Put, Finalize, and verify the canonical file
// exists with the right contents and checksum.
func TestLocalStore_PutFinalizeRoundTrip(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	identity := validIdentity()
	payload := []byte("transcript line 1\ntranscript line 2\n")
	wantChecksum := sha256.Sum256(payload)
	wantHex := "sha256:" + hex.EncodeToString(wantChecksum[:])

	expectInsertWriting(mock)

	w, err := store.Put(ctx, identity, 0, VisibilityEngineerOnly)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if w.ManifestID() == "" {
		t.Error("Writer.ManifestID is empty")
	}
	if !strings.HasPrefix(w.ObjectURI(), "file://") {
		t.Errorf("ObjectURI = %q, want file:// prefix", w.ObjectURI())
	}

	n, err := w.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	if w.Size() != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", w.Size(), len(payload))
	}

	// Finalize: lookup row (writing) → finalize update → re-fetch (finalized).
	expectGetByID(mock, w.ManifestID(), pkgstore.LogArtifactStatusWriting, w.ObjectURI(), nil, "")
	expectFinalize(mock)
	size := int64(len(payload))
	expectGetByID(mock, w.ManifestID(), pkgstore.LogArtifactStatusFinalized, w.ObjectURI(), &size, wantHex)

	manifest, err := store.Finalize(ctx, w)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if manifest.Status != StatusFinalized {
		t.Errorf("manifest.Status = %q, want %q", manifest.Status, StatusFinalized)
	}
	if manifest.ByteSize != size {
		t.Errorf("manifest.ByteSize = %d, want %d", manifest.ByteSize, size)
	}
	if manifest.Checksum != wantHex {
		t.Errorf("manifest.Checksum = %q, want %q", manifest.Checksum, wantHex)
	}

	// Verify the canonical file exists and matches the payload bytes.
	relKey, _ := BuildObjectKey("", identity, 0)
	finalPath := filepath.Join(store.Root(), filepath.FromSlash(relKey))
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read canonical file: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("canonical file = %q, want %q", got, payload)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestLocalStore_GetReturnsBytesAndManifest exercises the Get path
// after a successful round-trip: caller can re-read the artifact and
// see the manifest row.
func TestLocalStore_GetReturnsBytesAndManifest(t *testing.T) {
	store, mock, db := newLocalForTest(t)
	ctx := context.Background()

	identity := validIdentity()
	payload := []byte("get-roundtrip")

	// Write a raw file at the canonical path so we don't have to mock
	// the full Put/Finalize flow just to test Get.
	relKey, _ := BuildObjectKey("", identity, 0)
	finalPath := filepath.Join(store.Root(), filepath.FromSlash(relKey))
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(finalPath, payload, 0o644); err != nil {
		t.Fatalf("write canonical file: %v", err)
	}
	uri := "file://" + filepath.ToSlash(finalPath)

	size := int64(len(payload))
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
	if string(got) != string(payload) {
		t.Errorf("Get bytes = %q, want %q", got, payload)
	}
	if manifest.Status != StatusFinalized {
		t.Errorf("manifest.Status = %q", manifest.Status)
	}
	_ = db
}

// TestLocalStore_GetMissingRowReturnsErrNotFound covers the failure
// path: a manifest ID that doesn't exist returns ErrNotFound rather
// than a generic SQL error.
func TestLocalStore_GetMissingRowReturnsErrNotFound(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts WHERE id = \?`).
		WithArgs("log-missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "tower", "bead_id", "attempt_id", "run_id", "agent_name",
			"role", "phase", "provider", "stream", "sequence", "object_uri",
			"byte_size", "checksum", "status", "started_at", "ended_at",
			"created_at", "updated_at", "redaction_version", "visibility",
			"summary", "tail",
		}))

	_, _, err := store.Get(ctx, ManifestRef{ID: "log-missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestLocalStore_FinalizeIdempotent proves a second Finalize call on
// the same writer is a no-op that returns the existing manifest, not a
// duplicate row insert.
func TestLocalStore_FinalizeIdempotent(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	expectInsertWriting(mock)
	w, err := store.Put(ctx, validIdentity(), 0, VisibilityEngineerOnly)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// First Finalize: sees writing → updates → re-reads as finalized.
	size := int64(4)
	expectGetByID(mock, w.ManifestID(), pkgstore.LogArtifactStatusWriting, w.ObjectURI(), nil, "")
	expectFinalize(mock)
	expectGetByID(mock, w.ManifestID(), pkgstore.LogArtifactStatusFinalized, w.ObjectURI(), &size, "sha256:abc")
	if _, err := store.Finalize(ctx, w); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Second Finalize: Lookup sees finalized → return without rewriting.
	expectGetByID(mock, w.ManifestID(), pkgstore.LogArtifactStatusFinalized, w.ObjectURI(), &size, "sha256:abc")
	if _, err := store.Finalize(ctx, w); err != nil {
		t.Fatalf("second Finalize: %v", err)
	}
}

// TestLocalStore_StatNoBytes verifies Stat returns the manifest row
// without opening the artifact file. Useful for callers that just
// want to enumerate metadata.
func TestLocalStore_StatNoBytes(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	size := int64(123)
	expectGetByID(mock, "log-stat", pkgstore.LogArtifactStatusFinalized, "file:///dev/null", &size, "sha256:cafe")

	manifest, err := store.Stat(ctx, ManifestRef{ID: "log-stat"})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if manifest.ByteSize != size {
		t.Errorf("ByteSize = %d, want %d", manifest.ByteSize, size)
	}
}

// TestLocalStore_ListByBead delegates to pkg/store; verify the filter
// reaches the right query.
func TestLocalStore_ListByBead(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	rows := sqlmock.NewRows([]string{
		"id", "tower", "bead_id", "attempt_id", "run_id", "agent_name",
		"role", "phase", "provider", "stream", "sequence", "object_uri",
		"byte_size", "checksum", "status", "started_at", "ended_at",
		"created_at", "updated_at", "redaction_version", "visibility",
		"summary", "tail",
	}).AddRow(
		"log-a", "awell-test", "spi-b986in", "spi-att", "run-1",
		"wizard-spi-b986in", "wizard", "implement", "claude", "transcript",
		0, "file:///x.jsonl", nil, nil,
		pkgstore.LogArtifactStatusWriting, nil, nil,
		"2026-04-28 01:00:00", "2026-04-28 01:00:00",
		0, "engineer_only", nil, nil,
	)
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts\s+WHERE bead_id = \?`).
		WithArgs("spi-b986in").
		WillReturnRows(rows)

	got, err := store.List(ctx, Filter{BeadID: "spi-b986in"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "log-a" {
		t.Errorf("got %v, want [log-a]", got)
	}
}

// TestLocalStore_ListRequiresFilter rejects an empty filter so we
// don't accidentally full-table-scan agent_log_artifacts.
func TestLocalStore_ListRequiresFilter(t *testing.T) {
	store, _, _ := newLocalForTest(t)
	if _, err := store.List(context.Background(), Filter{}); err == nil {
		t.Error("expected error for empty filter")
	}
}

// TestLocalStore_PutDuplicateFinalizedReturnsError exercises the
// idempotency path for a finalized identity: Put must surface
// pkgstore.ErrLogArtifactExists rather than overwriting.
func TestLocalStore_PutDuplicateFinalizedReturnsError(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	// First INSERT trips the unique constraint.
	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WillReturnError(errors.New("Error 1062: Duplicate entry 'spi-b986in-...' for key 'uniq_log_artifact_identity'"))

	// Lookup returns a finalized row.
	size := int64(99)
	expectGetByIdentity(mock, pkgstore.LogArtifactStatusFinalized, &size, "sha256:cafe")

	_, err := store.Put(ctx, validIdentity(), 0, VisibilityEngineerOnly)
	if !errors.Is(err, pkgstore.ErrLogArtifactExists) {
		t.Errorf("Put err = %v, want ErrLogArtifactExists", err)
	}
}

// TestLocalStore_PutDuplicateWritingReturnsExistingWriter covers the
// resume path: an in-flight artifact (writing status) lets a new Put
// re-attach via the existing manifest ID.
func TestLocalStore_PutDuplicateWritingReturnsExistingWriter(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WillReturnError(errors.New("Error 1062: Duplicate entry"))
	expectGetByIdentity(mock, pkgstore.LogArtifactStatusWriting, nil, "")

	w, err := store.Put(ctx, validIdentity(), 0, VisibilityEngineerOnly)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if w.ManifestID() == "" {
		t.Error("expected re-attached writer to inherit existing manifest id")
	}
}

// expectGetByIdentity registers the identity-tuple SELECT.
func expectGetByIdentity(mock sqlmock.Sqlmock, status string, byteSize *int64, checksum string) {
	rows := sqlmock.NewRows([]string{
		"id", "tower", "bead_id", "attempt_id", "run_id", "agent_name",
		"role", "phase", "provider", "stream", "sequence", "object_uri",
		"byte_size", "checksum", "status", "started_at", "ended_at",
		"created_at", "updated_at", "redaction_version", "visibility",
		"summary", "tail",
	})
	var sizeArg, checksumArg interface{}
	if byteSize != nil {
		sizeArg = *byteSize
	}
	if checksum != "" {
		checksumArg = checksum
	}
	rows.AddRow(
		"log-existing", "awell-test", "spi-b986in", "spi-attempt", "run-001",
		"wizard-spi-b986in", "wizard", "implement", "claude", "transcript",
		0, "file:///stub.jsonl", sizeArg, checksumArg,
		status, nil, nil,
		"2026-04-28 01:00:00", "2026-04-28 01:00:00",
		0, "engineer_only", nil, nil,
	)
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts\s+WHERE bead_id = \?`).
		WillReturnRows(rows)
}

// TestParseLocalPath is an inverse-shape test: the path produced by
// BuildObjectKey decodes back into the same Identity.
func TestParseLocalPath(t *testing.T) {
	id := validIdentity()
	relKey, err := BuildObjectKey("", id, 0)
	if err != nil {
		t.Fatalf("BuildObjectKey: %v", err)
	}
	gotID, gotSeq, ok := parseLocalPath(relKey, "")
	if !ok {
		t.Fatalf("parseLocalPath rejected canonical key %q", relKey)
	}
	if gotSeq != 0 {
		t.Errorf("sequence = %d, want 0", gotSeq)
	}
	if gotID != id {
		t.Errorf("identity round-trip:\ngot  %+v\nwant %+v", gotID, id)
	}

	// Sequenced.
	relKey2, _ := BuildObjectKey("", id, 5)
	gotID2, gotSeq2, ok := parseLocalPath(relKey2, "")
	if !ok {
		t.Fatalf("parseLocalPath rejected sequenced key %q", relKey2)
	}
	if gotSeq2 != 5 {
		t.Errorf("sequence = %d, want 5", gotSeq2)
	}
	if gotID2 != id {
		t.Errorf("identity round-trip (seq=5): got %+v want %+v", gotID2, id)
	}

	// No-provider variant.
	id3 := id
	id3.Provider = ""
	id3.Stream = StreamStdout
	relKey3, _ := BuildObjectKey("", id3, 0)
	gotID3, _, ok := parseLocalPath(relKey3, "")
	if !ok {
		t.Fatalf("parseLocalPath rejected no-provider key %q", relKey3)
	}
	if gotID3 != id3 {
		t.Errorf("no-provider round-trip:\ngot  %+v\nwant %+v", gotID3, id3)
	}

	// Non-canonical paths are rejected.
	if _, _, ok := parseLocalPath("garbage", ""); ok {
		t.Error("expected reject on garbage path")
	}
	if _, _, ok := parseLocalPath("a/b/c.jsonl", ""); ok {
		t.Error("expected reject on too-short path")
	}
}

// TestParseLocalPath_WizardLayout pins the legacy wizard log layout
// shapes that pre-date the substrate. parseLocalPath must accept these
// when defaultTower is non-empty so Reconcile can pick up
// already-on-disk wizard/apprentice transcripts and surface them
// through the bead-logs API.
func TestParseLocalPath_WizardLayout(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		wantBead  string
		wantPhase string
		wantProv  string
		wantRole  Role
		wantStrm  Stream
	}{
		{
			name:      "orchestrator log",
			path:      "wizard-spi-tsodj3.log",
			wantBead:  "spi-tsodj3",
			wantPhase: "orchestrator",
			wantRole:  RoleWizard,
			wantStrm:  StreamStdout,
		},
		{
			name:      "spawn log",
			path:      "wizard-spi-tsodj3-implement-1.log",
			wantBead:  "spi-tsodj3",
			wantPhase: "implement-1",
			wantRole:  RoleWizard,
			wantStrm:  StreamStdout,
		},
		{
			name:      "claude transcript under wizard dir",
			path:      "wizard-spi-tsodj3/claude/implement-20260422-184843.jsonl",
			wantBead:  "spi-tsodj3",
			wantPhase: "implement",
			wantProv:  "claude",
			wantRole:  RoleWizard,
			wantStrm:  StreamTranscript,
		},
		{
			name:      "claude transcript under spawn dir",
			path:      "wizard-spi-tsodj3-implement-1/claude/implement-20260422-184843.jsonl",
			wantBead:  "spi-tsodj3",
			wantPhase: "implement",
			wantProv:  "claude",
			wantRole:  RoleWizard,
			wantStrm:  StreamTranscript,
		},
		{
			name:      "legacy claude .log transcript",
			path:      "wizard-spi-tsodj3/claude/implement-20260422-184843.log",
			wantBead:  "spi-tsodj3",
			wantPhase: "implement",
			wantProv:  "claude",
			wantRole:  RoleWizard,
			wantStrm:  StreamTranscript,
		},
		{
			name:      "multi-segment bead prefix",
			path:      "wizard-xserver-0hy/claude/plan-20260422-184843.jsonl",
			wantBead:  "xserver-0hy",
			wantPhase: "plan",
			wantProv:  "claude",
			wantRole:  RoleWizard,
			wantStrm:  StreamTranscript,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, _, ok := parseLocalPath(tc.path, "awell-test")
			if !ok {
				t.Fatalf("parseLocalPath rejected %q", tc.path)
			}
			if id.Tower != "awell-test" {
				t.Errorf("Tower = %q, want awell-test", id.Tower)
			}
			if id.BeadID != tc.wantBead {
				t.Errorf("BeadID = %q, want %q", id.BeadID, tc.wantBead)
			}
			if id.Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", id.Phase, tc.wantPhase)
			}
			if id.Provider != tc.wantProv {
				t.Errorf("Provider = %q, want %q", id.Provider, tc.wantProv)
			}
			if id.Role != tc.wantRole {
				t.Errorf("Role = %q, want %q", id.Role, tc.wantRole)
			}
			if id.Stream != tc.wantStrm {
				t.Errorf("Stream = %q, want %q", id.Stream, tc.wantStrm)
			}
			if id.AttemptID != LegacyAttemptID {
				t.Errorf("AttemptID = %q, want %q", id.AttemptID, LegacyAttemptID)
			}
			if id.AgentName == "" {
				t.Errorf("AgentName must be set")
			}
		})
	}
}

// TestParseLocalPath_WizardLayout_Rejects covers paths the wizard
// layout intentionally skips: stderr sidecars, tmpfiles, missing
// `wizard-` prefix, and non-bead-shaped names.
func TestParseLocalPath_WizardLayout_Rejects(t *testing.T) {
	cases := []string{
		"wizard-spi-tsodj3/claude/implement-20260422-184843.stderr.log",
		"wizard-spi-tsodj3/claude/implement-20260422-184843.jsonl.tmp",
		"orphan-file.log",
		"wizard-/claude/x.jsonl",
		"wizard-NOT_A_BEAD.log",
		"wizard-spi-tsodj3/claude/extra/path.jsonl",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			if _, _, ok := parseLocalPath(p, "awell-test"); ok {
				t.Errorf("parseLocalPath accepted bad path %q", p)
			}
		})
	}
}

// TestParseLocalPath_WizardLayoutDisabledWithoutTower verifies that the
// wizard fallback only activates when the caller passes a non-empty
// default tower. Tests calling parseLocalPath without a tower must not
// observe the wizard layout (preserves the prior contract).
func TestParseLocalPath_WizardLayoutDisabledWithoutTower(t *testing.T) {
	if _, _, ok := parseLocalPath("wizard-spi-tsodj3.log", ""); ok {
		t.Error("expected reject when defaultTower is empty")
	}
}

// TestLocalStore_Reconcile_PicksUpExternallyWrittenFile is the
// "list existing wizard/provider transcripts" acceptance: a transcript
// file written by a non-Spire process can be brought into the manifest.
func TestLocalStore_Reconcile_PicksUpExternallyWrittenFile(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	// Drop a file at the canonical path corresponding to a known
	// identity. parseLocalPath must accept the layout for Reconcile to
	// pick it up.
	identity := validIdentity()
	relKey, _ := BuildObjectKey("", identity, 0)
	abs := filepath.Join(store.Root(), filepath.FromSlash(relKey))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	contents := []byte("orphan transcript bytes")
	if err := os.WriteFile(abs, contents, 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// Reconcile checks the identity tuple first: row not found.
	emptyRows := sqlmock.NewRows([]string{
		"id", "tower", "bead_id", "attempt_id", "run_id", "agent_name",
		"role", "phase", "provider", "stream", "sequence", "object_uri",
		"byte_size", "checksum", "status", "started_at", "ended_at",
		"created_at", "updated_at", "redaction_version", "visibility",
		"summary", "tail",
	})
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts\s+WHERE bead_id = \?`).
		WillReturnRows(emptyRows)
	// Insert returns OK.
	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	manifests, err := store.Reconcile(ctx, identity.Tower, identity.BeadID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(manifests))
	}
	got := manifests[0]
	if got.Identity != identity {
		t.Errorf("identity:\ngot  %+v\nwant %+v", got.Identity, identity)
	}
	if got.Status != StatusFinalized {
		t.Errorf("Status = %q, want %q", got.Status, StatusFinalized)
	}

	wantSum := sha256.Sum256(contents)
	wantHex := "sha256:" + hex.EncodeToString(wantSum[:])
	if got.Checksum != wantHex {
		t.Errorf("Checksum = %q, want %q", got.Checksum, wantHex)
	}
	if got.ByteSize != int64(len(contents)) {
		t.Errorf("ByteSize = %d, want %d", got.ByteSize, len(contents))
	}
}

// TestLocalStore_Reconcile_NoOpForExistingRow returns the existing
// manifest without re-hashing or re-inserting, proving Reconcile is
// idempotent.
func TestLocalStore_Reconcile_NoOpForExistingRow(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	identity := validIdentity()
	relKey, _ := BuildObjectKey("", identity, 0)
	abs := filepath.Join(store.Root(), filepath.FromSlash(relKey))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte("existing"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Identity lookup returns an already-finalized row → Reconcile
	// must NOT issue an INSERT.
	size := int64(8)
	expectGetByIdentity(mock, pkgstore.LogArtifactStatusFinalized, &size, "sha256:abc")

	manifests, err := store.Reconcile(ctx, identity.Tower, identity.BeadID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected 1 existing manifest, got %d", len(manifests))
	}
	if manifests[0].Status != StatusFinalized {
		t.Errorf("Status = %q, want finalized", manifests[0].Status)
	}
}

// TestLocalStore_Reconcile_TowerRequired guards the tower argument.
func TestLocalStore_Reconcile_TowerRequired(t *testing.T) {
	store, _, _ := newLocalForTest(t)
	if _, err := store.Reconcile(context.Background(), "", "spi-foo"); err == nil {
		t.Error("expected error for empty tower")
	}
}

// TestLocalStore_Reconcile_WizardLayout exercises the legacy on-disk
// wizard layout: an orchestrator .log plus a claude .jsonl transcript
// for the same bead must both surface as manifest rows after a single
// Reconcile call. This is the spi-tsodj3 acceptance — the gateway
// bead-logs API was returning empty because parseLocalPath couldn't
// decode this layout.
func TestLocalStore_Reconcile_WizardLayout(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	beadID := "spi-tsodj3"
	wizardName := "wizard-" + beadID
	root := store.Root()

	// Orchestrator log at root.
	wizardLogPath := filepath.Join(root, wizardName+".log")
	if err := os.WriteFile(wizardLogPath, []byte("wizard log line\n"), 0o644); err != nil {
		t.Fatalf("write wizard log: %v", err)
	}

	// Provider transcript under wizard dir.
	claudeDir := filepath.Join(root, wizardName, "claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir claude: %v", err)
	}
	transcriptPath := filepath.Join(claudeDir, "implement-20260422-184843.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"system"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// Two artifacts → two identity-tuple lookups (each "not found") and
	// two inserts. The walk order is filesystem-dependent (alphabetical
	// by directory entry) so we just allow either order.
	emptyRows := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{
			"id", "tower", "bead_id", "attempt_id", "run_id", "agent_name",
			"role", "phase", "provider", "stream", "sequence", "object_uri",
			"byte_size", "checksum", "status", "started_at", "ended_at",
			"created_at", "updated_at", "redaction_version", "visibility",
			"summary", "tail",
		})
	}
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts\s+WHERE bead_id = \?`).
		WillReturnRows(emptyRows())
	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts\s+WHERE bead_id = \?`).
		WillReturnRows(emptyRows())
	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	manifests, err := store.Reconcile(ctx, "awell-test", beadID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(manifests) != 2 {
		t.Fatalf("expected 2 manifests, got %d", len(manifests))
	}

	// One should be the wizard log (stdout) and one the claude transcript.
	var sawWizardLog, sawTranscript bool
	for _, m := range manifests {
		if m.Identity.BeadID != beadID {
			t.Errorf("BeadID = %q, want %q", m.Identity.BeadID, beadID)
		}
		if m.Identity.Tower != "awell-test" {
			t.Errorf("Tower = %q, want awell-test", m.Identity.Tower)
		}
		if m.Identity.Stream == StreamStdout {
			sawWizardLog = true
			if m.Identity.Phase != "orchestrator" {
				t.Errorf("orchestrator log Phase = %q, want orchestrator", m.Identity.Phase)
			}
		}
		if m.Identity.Stream == StreamTranscript {
			sawTranscript = true
			if m.Identity.Provider != "claude" {
				t.Errorf("transcript Provider = %q, want claude", m.Identity.Provider)
			}
			if m.Identity.Phase != "implement" {
				t.Errorf("transcript Phase = %q, want implement", m.Identity.Phase)
			}
		}
	}
	if !sawWizardLog {
		t.Errorf("Reconcile did not register the wizard .log file")
	}
	if !sawTranscript {
		t.Errorf("Reconcile did not register the claude .jsonl transcript")
	}
}

// TestLocalStore_Reconcile_NoTowerDirIsEmpty proves Reconcile against a
// freshly-built store (no files yet) returns no manifests and no
// error. This is the common case after first `spire up`.
func TestLocalStore_Reconcile_NoTowerDirIsEmpty(t *testing.T) {
	store, _, _ := newLocalForTest(t)
	manifests, err := store.Reconcile(context.Background(), "awell-test", "spi-foo")
	if err != nil {
		t.Errorf("Reconcile: %v", err)
	}
	if len(manifests) != 0 {
		t.Errorf("expected 0 manifests, got %d", len(manifests))
	}
}

// TestLocalPathFromURI_RejectsNonFileURI guards the URI parser.
func TestLocalPathFromURI_RejectsNonFileURI(t *testing.T) {
	if _, err := localPathFromURI("gs://bucket/key"); err == nil {
		t.Error("expected error for gs:// URI on local backend")
	}
}

// TestLocalWriter_CloseIsSafeWithoutFinalize: closing without
// finalizing must remove the tmpfile so subsequent Put calls don't
// trip on stale state. Sanity check for crash recovery semantics.
func TestLocalWriter_CloseIsSafeWithoutFinalize(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	expectInsertWriting(mock)
	w, err := store.Put(ctx, validIdentity(), 0, VisibilityEngineerOnly)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	lw := w.(*localWriter)
	tmpPath := lw.tmpPath
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("tmpfile should exist before close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmpfile should be removed after Close, got: %v", err)
	}
	// Final canonical path should not exist.
	if _, err := os.Stat(lw.finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("finalPath should not exist without Finalize, got: %v", err)
	}
}

// TestLocalStore_PutSetsChecksumIncrementally proves the running
// hasher matches a single-pass SHA-256 over the same byte sequence.
// Catches regressions where a write path forgets to fan into the
// hasher.
func TestLocalStore_PutSetsChecksumIncrementally(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	expectInsertWriting(mock)
	w, err := store.Put(ctx, validIdentity(), 0, VisibilityEngineerOnly)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	chunks := [][]byte{[]byte("alpha "), []byte("bravo "), []byte("charlie")}
	for _, c := range chunks {
		if _, err := w.Write(c); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	full := []byte("alpha bravo charlie")
	wantSum := sha256.Sum256(full)
	wantHex := hex.EncodeToString(wantSum[:])
	if w.ChecksumHex() != wantHex {
		t.Errorf("ChecksumHex = %q, want %q", w.ChecksumHex(), wantHex)
	}
	_ = time.Now // keep time import for any future temporal asserts
	_ = w.Close()
}
