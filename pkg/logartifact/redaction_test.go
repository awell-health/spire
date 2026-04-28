package logartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/awell-health/spire/pkg/logartifact/redact"
	pkgstore "github.com/awell-health/spire/pkg/store"
)

// TestVisibility_DefaultIsEngineerOnly pins the safe-default contract.
// A zero-value Visibility must marshal as engineer_only; any code path
// that forgets to set visibility falls through to the strictest class.
func TestVisibility_DefaultIsEngineerOnly(t *testing.T) {
	var v Visibility
	if v != "" {
		t.Errorf("zero value = %q, want empty (coerced at boundary)", v)
	}
	if v.RedactsAtUpload() {
		t.Error("zero-value visibility should not redact at upload")
	}
	if !VisibilityEngineerOnly.Valid() {
		t.Error("VisibilityEngineerOnly should be Valid")
	}

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `"engineer_only"` {
		t.Errorf("marshal zero = %q, want %q", b, `"engineer_only"`)
	}
}

// TestVisibility_RoundTripJSON checks that all valid values survive a
// marshal/unmarshal round-trip and that an unknown value rejects.
func TestVisibility_RoundTripJSON(t *testing.T) {
	for _, want := range []Visibility{VisibilityEngineerOnly, VisibilityDesktopSafe, VisibilityPublic} {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %q: %v", want, err)
		}
		var got Visibility
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %q: %v", b, err)
		}
		if got != want {
			t.Errorf("round-trip %q != %q", got, want)
		}
	}
	var bogus Visibility
	if err := json.Unmarshal([]byte(`"hilarious-leak"`), &bogus); err == nil {
		t.Error("expected unmarshal of unknown visibility to fail")
	}
}

// TestLocalStore_PutRequiresVisibility codifies the design's "deliberate
// choice" rule: a caller that forgets visibility fails fast at runtime.
// Compile-time enforcement handled by the type system; this guards the
// runtime side.
func TestLocalStore_PutRequiresVisibility(t *testing.T) {
	store, _, _ := newLocalForTest(t)
	ctx := context.Background()

	if _, err := store.Put(ctx, validIdentity(), 0, ""); err == nil {
		t.Error("expected error for empty visibility")
	}
	if _, err := store.Put(ctx, validIdentity(), 0, Visibility("nope")); err == nil {
		t.Error("expected error for invalid visibility")
	}
}

// TestLocalStore_DesktopSafeRedactsAtUpload is the round-trip
// acceptance: a token-bearing payload uploaded as desktop_safe is
// masked on disk AND the manifest carries redaction_version > 0.
func TestLocalStore_DesktopSafeRedactsAtUpload(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	identity := validIdentity()
	payload := []byte("Authorization: Bearer eyJabcdefABCDEF1234567890ABC.payload.SIGNATURE\nname=value\n")
	if !bytes.Contains(payload, []byte("Bearer eyJ")) {
		t.Fatalf("test fixture lost Bearer line: %q", payload)
	}

	expectInsertWriting(mock)
	w, err := store.Put(ctx, identity, 0, VisibilityDesktopSafe)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	expectGetByIDWithVisibility(mock, w.ManifestID(),
		pkgstore.LogArtifactStatusWriting, w.ObjectURI(),
		nil, "", "desktop_safe", 0)
	expectFinalize(mock)
	mock.ExpectExec(`UPDATE agent_log_artifacts SET redaction_version = \?, updated_at = \? WHERE id = \?`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Compute redacted bytes for the size+checksum that the
	// post-finalize SELECT must return — Finalize stamps the on-disk
	// (redacted) checksum, not the input checksum.
	redacted, redactionVersion := redact.New().Redact(payload)
	if !bytes.Contains(redacted, redact.MaskToken) {
		t.Fatalf("redactor failed to mask the test fixture")
	}
	wantSum := sha256.Sum256(redacted)
	wantHex := "sha256:" + hex.EncodeToString(wantSum[:])
	wantSize := int64(len(redacted))

	expectGetByIDWithVisibility(mock, w.ManifestID(),
		pkgstore.LogArtifactStatusFinalized, w.ObjectURI(),
		&wantSize, wantHex, "desktop_safe", redactionVersion)

	manifest, err := store.Finalize(ctx, w)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if manifest.Visibility != VisibilityDesktopSafe {
		t.Errorf("manifest.Visibility = %q, want desktop_safe", manifest.Visibility)
	}
	if manifest.RedactionVersion != redactionVersion {
		t.Errorf("manifest.RedactionVersion = %d, want %d", manifest.RedactionVersion, redactionVersion)
	}

	// Verify the canonical file holds the redacted bytes — NOT the
	// input bytes. This is the whole point of upload-time redaction.
	relKey, _ := BuildObjectKey("", identity, 0)
	finalPath := filepath.Join(store.Root(), filepath.FromSlash(relKey))
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read canonical file: %v", err)
	}
	if !bytes.Contains(got, redact.MaskToken) {
		t.Errorf("canonical file should be redacted, got %q", got)
	}
	if bytes.Contains(got, []byte("Bearer eyJabcdefABCDEF1234567890ABC.payload.SIGNATURE")) {
		t.Errorf("canonical file leaks original token: %q", got)
	}
}

// TestLocalStore_EngineerOnlyPreservesRawBytes is the symmetric
// round-trip: engineer_only artifacts hit disk unmodified and the
// manifest's redaction_version stays at 0 (the reserved "no redactor
// applied" value).
func TestLocalStore_EngineerOnlyPreservesRawBytes(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	identity := validIdentity()
	payload := []byte("Authorization: Bearer eyJabcdefABCDEF1234567890ABC.payload.SIGNATURE\nplain text\n")
	wantSum := sha256.Sum256(payload)
	wantHex := "sha256:" + hex.EncodeToString(wantSum[:])
	wantSize := int64(len(payload))

	expectInsertWriting(mock)
	w, err := store.Put(ctx, identity, 0, VisibilityEngineerOnly)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Lookup → finalize → re-fetch. No redaction-stamp UPDATE in this
	// path (engineer_only never invokes SetLogArtifactRedaction).
	expectGetByID(mock, w.ManifestID(), pkgstore.LogArtifactStatusWriting, w.ObjectURI(), nil, "")
	expectFinalize(mock)
	expectGetByID(mock, w.ManifestID(), pkgstore.LogArtifactStatusFinalized, w.ObjectURI(), &wantSize, wantHex)

	manifest, err := store.Finalize(ctx, w)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if manifest.Visibility != VisibilityEngineerOnly {
		t.Errorf("manifest.Visibility = %q, want engineer_only", manifest.Visibility)
	}
	if manifest.RedactionVersion != 0 {
		t.Errorf("manifest.RedactionVersion = %d, want 0 (no redactor applied)", manifest.RedactionVersion)
	}

	// On-disk bytes match the input exactly.
	relKey, _ := BuildObjectKey("", identity, 0)
	finalPath := filepath.Join(store.Root(), filepath.FromSlash(relKey))
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read canonical file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("canonical file = %q, want %q", got, payload)
	}
}

// TestRender_EngineerScopeSeesRawEngineerOnly: the only path where the
// rendered bytes are NOT re-redacted. Operator audits depend on this.
func TestRender_EngineerScopeSeesRawEngineerOnly(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	identity := validIdentity()
	payload := []byte("Authorization: Bearer eyJabcdefABCDEF1234567890ABC.payload.SIGNATURE")
	relKey, _ := BuildObjectKey("", identity, 0)
	finalPath := filepath.Join(store.Root(), filepath.FromSlash(relKey))
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(finalPath, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	uri := "file://" + filepath.ToSlash(finalPath)
	size := int64(len(payload))

	// Stat → Get → re-Stat (Render reads ID via Stat first, then Get).
	expectGetByIDWithVisibility(mock, "log-engonly",
		pkgstore.LogArtifactStatusFinalized, uri,
		&size, "sha256:abc", "engineer_only", 0)
	expectGetByIDWithVisibility(mock, "log-engonly",
		pkgstore.LogArtifactStatusFinalized, uri,
		&size, "sha256:abc", "engineer_only", 0)

	out, meta, err := Render(ctx, store, ManifestRef{ID: "log-engonly"}, ScopeEngineer)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Errorf("Render bytes = %q, want raw %q", out, payload)
	}
	if meta.Visibility != VisibilityEngineerOnly {
		t.Errorf("meta.Visibility = %q, want engineer_only", meta.Visibility)
	}
	if meta.RedactionVersion != 0 {
		t.Errorf("meta.RedactionVersion = %d, want 0", meta.RedactionVersion)
	}
	if meta.ManifestID != "log-engonly" {
		t.Errorf("meta.ManifestID = %q", meta.ManifestID)
	}
}

// TestRender_DesktopScopeRefusesEngineerOnly: scope-gate at the render
// boundary. A non-engineer caller cannot read engineer_only artifacts
// even if the bytes are accidentally clean.
func TestRender_DesktopScopeRefusesEngineerOnly(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	expectGetByIDWithVisibility(mock, "log-eng",
		pkgstore.LogArtifactStatusFinalized, "file:///dev/null",
		nil, "sha256:abc", "engineer_only", 0)

	_, meta, err := Render(ctx, store, ManifestRef{ID: "log-eng"}, ScopeDesktop)
	if !errors.Is(err, ErrAccessDenied) {
		t.Errorf("err = %v, want ErrAccessDenied", err)
	}
	if meta.ManifestID != "log-eng" {
		t.Errorf("denied response should still carry ManifestID, got %q", meta.ManifestID)
	}
	if meta.Visibility != VisibilityEngineerOnly {
		t.Errorf("denied meta.Visibility = %q, want engineer_only", meta.Visibility)
	}
}

// TestRender_DesktopScopeReRedactsDesktopSafe: defense-in-depth. Even
// when bytes were redacted at upload, the render layer re-applies the
// current redactor on every read. Old artifacts pick up new patterns
// without rewriting storage.
func TestRender_DesktopScopeReRedactsDesktopSafe(t *testing.T) {
	store, mock, _ := newLocalForTest(t)
	ctx := context.Background()

	identity := validIdentity()
	// Plant raw (un-redacted) bytes on disk to simulate a future
	// scenario: an artifact uploaded under an older redactor pattern
	// set didn't catch this credential, but the current redactor does.
	planted := []byte("api_key=ABCDEF1234567890XYZ\nplain\n")
	relKey, _ := BuildObjectKey("", identity, 0)
	finalPath := filepath.Join(store.Root(), filepath.FromSlash(relKey))
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(finalPath, planted, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	uri := "file://" + filepath.ToSlash(finalPath)
	size := int64(len(planted))

	// Stat (for scope check) + Stat-from-Get.
	expectGetByIDWithVisibility(mock, "log-ds",
		pkgstore.LogArtifactStatusFinalized, uri,
		&size, "sha256:zzz", "desktop_safe", 1)
	expectGetByIDWithVisibility(mock, "log-ds",
		pkgstore.LogArtifactStatusFinalized, uri,
		&size, "sha256:zzz", "desktop_safe", 1)

	out, meta, err := Render(ctx, store, ManifestRef{ID: "log-ds"}, ScopeDesktop)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Contains(out, redact.MaskToken) {
		t.Errorf("expected re-redaction in render output, got %q", out)
	}
	if bytes.Contains(out, []byte("ABCDEF1234567890XYZ")) {
		t.Errorf("render output leaks api_key value: %q", out)
	}
	if meta.RedactionVersion != redact.CurrentRedactionVersion {
		t.Errorf("meta.RedactionVersion = %d, want %d", meta.RedactionVersion, redact.CurrentRedactionVersion)
	}
	if meta.StoredRedactionVersion != 1 {
		t.Errorf("meta.StoredRedactionVersion = %d, want 1", meta.StoredRedactionVersion)
	}
}
