package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
	"github.com/awell-health/spire/pkg/logartifact/redact"
	"github.com/awell-health/spire/pkg/store"
)

// fakeLogReader is an in-memory LogArtifactReader for handler tests. It
// holds a deterministic ordered list of manifests and a parallel byte
// map keyed by manifest ID. Methods walk the manifest list rather than
// relying on map iteration order so cursor and pagination assertions
// stay stable.
type fakeLogReader struct {
	manifests []logartifact.Manifest
	bytes     map[string][]byte
	listErr   error
	getErr    error
	statErr   error
	// notFound is the set of artifact IDs that should surface as
	// ErrNotFound from Get/Stat — tests use this to simulate "manifest
	// row exists but bytes missing" (a 410 case).
	notFound map[string]bool
	// statErrFor returns a custom error for a specific manifest ID; used
	// to test the manifest-row-but-no-bytes path independently.
	statErrFor map[string]error
	// getErrFor mirrors statErrFor for the Get path.
	getErrFor map[string]error
}

func (f *fakeLogReader) List(_ context.Context, filter logartifact.Filter) ([]logartifact.Manifest, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]logartifact.Manifest, 0, len(f.manifests))
	for _, m := range f.manifests {
		if filter.BeadID != "" && m.Identity.BeadID != filter.BeadID {
			continue
		}
		if filter.AttemptID != "" && m.Identity.AttemptID != filter.AttemptID {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeLogReader) Stat(_ context.Context, ref logartifact.ManifestRef) (logartifact.Manifest, error) {
	if f.statErr != nil {
		return logartifact.Manifest{}, f.statErr
	}
	if err, ok := f.statErrFor[ref.ID]; ok {
		return logartifact.Manifest{}, err
	}
	for _, m := range f.manifests {
		if m.ID == ref.ID {
			return m, nil
		}
	}
	return logartifact.Manifest{}, logartifact.ErrNotFound
}

func (f *fakeLogReader) Get(_ context.Context, ref logartifact.ManifestRef) (io.ReadCloser, logartifact.Manifest, error) {
	if f.getErr != nil {
		return nil, logartifact.Manifest{}, f.getErr
	}
	if err, ok := f.getErrFor[ref.ID]; ok {
		return nil, logartifact.Manifest{}, err
	}
	if f.notFound[ref.ID] {
		return nil, logartifact.Manifest{}, logartifact.ErrNotFound
	}
	for _, m := range f.manifests {
		if m.ID == ref.ID {
			b, ok := f.bytes[m.ID]
			if !ok {
				return nil, logartifact.Manifest{}, logartifact.ErrNotFound
			}
			return io.NopCloser(bytes.NewReader(b)), m, nil
		}
	}
	return nil, logartifact.Manifest{}, logartifact.ErrNotFound
}

// withLogStubs installs the in-memory reader plus stubbed bead/store
// resolvers and restores them on test cleanup. Test authors only call
// this once per test.
func withLogStubs(t *testing.T, reader LogArtifactReader, bead store.Bead, beadErr error) {
	t.Helper()
	prevReaderFunc := logArtifactReaderFunc
	prevEnsure := logsStoreEnsureFunc
	prevGetBead := logsGetBeadFunc

	logsStoreEnsureFunc = func(string) error { return nil }
	if reader == nil {
		logArtifactReaderFunc = func() (LogArtifactReader, bool) { return nil, false }
	} else {
		logArtifactReaderFunc = func() (LogArtifactReader, bool) { return reader, true }
	}
	logsGetBeadFunc = func(id string) (store.Bead, error) {
		if beadErr != nil {
			return store.Bead{}, beadErr
		}
		b := bead
		b.ID = id
		return b, nil
	}
	t.Cleanup(func() {
		logArtifactReaderFunc = prevReaderFunc
		logsStoreEnsureFunc = prevEnsure
		logsGetBeadFunc = prevGetBead
	})
}

// makeManifest is a populated fixture. Tests override fields rather
// than reconstructing from scratch so per-row variations stay easy to
// read.
func makeManifest(id string, opts ...func(*logartifact.Manifest)) logartifact.Manifest {
	m := logartifact.Manifest{
		ID: id,
		Identity: logartifact.Identity{
			Tower:     "awell-test",
			BeadID:    "spi-test1",
			AttemptID: "spi-att1",
			RunID:     "run-001",
			AgentName: "wizard-spi-test1",
			Role:      logartifact.RoleWizard,
			Phase:     "implement",
			Provider:  "claude",
			Stream:    logartifact.StreamTranscript,
		},
		Sequence:   0,
		ObjectURI:  "file:///tmp/" + id + ".jsonl",
		ByteSize:   42,
		Checksum:   "sha256:dead",
		Status:     logartifact.StatusFinalized,
		CreatedAt:  time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC),
		Visibility: logartifact.VisibilityEngineerOnly,
	}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// newLogTestServer mirrors newTestServer but takes an explicit bearer
// token so auth tests don't need to rebuild the value.
func newLogTestServer(token string) *Server {
	return &Server{addr: ":0", log: log.New(io.Discard, "", 0), apiToken: token}
}

func TestHandleBeadLogsRoute_DispatchesByTail(t *testing.T) {
	withLogStubs(t, &fakeLogReader{}, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	cases := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "list", path: "/api/v1/beads/spi-test1/logs", wantStatus: http.StatusOK},
		{name: "summary", path: "/api/v1/beads/spi-test1/logs/summary", wantStatus: http.StatusOK},
		{name: "unknown sub-route", path: "/api/v1/beads/spi-test1/logs/junk", wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			s.handleBeadByID(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d want %d body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestListBeadLogs_BeadMissingReturns404(t *testing.T) {
	withLogStubs(t, &fakeLogReader{}, store.Bead{}, errors.New("bead spi-nope not found"))
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-nope/logs", nil)
	rec := httptest.NewRecorder()
	s.listBeadLogs(rec, req, "spi-nope")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404 body=%q", rec.Code, rec.Body.String())
	}
}

func TestListBeadLogs_NoBackendConfiguredReturnsEmpty(t *testing.T) {
	withLogStubs(t, nil, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs", nil)
	rec := httptest.NewRecorder()
	s.listBeadLogs(rec, req, "spi-test1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	var resp logsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Artifacts) != 0 {
		t.Fatalf("artifacts = %d want 0", len(resp.Artifacts))
	}
	if resp.NextCursor != "" {
		t.Fatalf("cursor = %q want empty", resp.NextCursor)
	}
}

func TestListBeadLogs_EmptyManifestRows(t *testing.T) {
	withLogStubs(t, &fakeLogReader{}, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs", nil)
	rec := httptest.NewRecorder()
	s.listBeadLogs(rec, req, "spi-test1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	var resp logsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Artifacts) != 0 {
		t.Fatalf("artifacts = %d want 0", len(resp.Artifacts))
	}
}

func TestListBeadLogs_ListsManifestRows(t *testing.T) {
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{
			makeManifest("log-a"),
			makeManifest("log-b", func(m *logartifact.Manifest) {
				m.Sequence = 1
			}),
		},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs", nil)
	rec := httptest.NewRecorder()
	s.listBeadLogs(rec, req, "spi-test1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	var resp logsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Artifacts) != 2 {
		t.Fatalf("artifacts = %d want 2", len(resp.Artifacts))
	}
	if resp.Artifacts[0].ID != "log-a" || resp.Artifacts[1].ID != "log-b" {
		t.Fatalf("ids = %q,%q want log-a,log-b", resp.Artifacts[0].ID, resp.Artifacts[1].ID)
	}
	// Each row carries a links block pointing at /raw and (since these
	// are transcripts) /pretty so clients don't have to format URLs.
	if resp.Artifacts[0].Links.Raw == "" || !strings.Contains(resp.Artifacts[0].Links.Raw, "log-a/raw") {
		t.Fatalf("raw link = %q", resp.Artifacts[0].Links.Raw)
	}
	if resp.Artifacts[0].Links.Pretty == "" || !strings.Contains(resp.Artifacts[0].Links.Pretty, "log-a/pretty") {
		t.Fatalf("pretty link = %q", resp.Artifacts[0].Links.Pretty)
	}
}

func TestListBeadLogs_ManifestRowsWithoutBytes(t *testing.T) {
	// Manifest row exists but the artifact has not been finalized —
	// list must return the row with status=writing rather than error.
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{
			makeManifest("log-a", func(m *logartifact.Manifest) {
				m.Status = logartifact.StatusWriting
			}),
		},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs", nil)
	rec := httptest.NewRecorder()
	s.listBeadLogs(rec, req, "spi-test1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200", rec.Code)
	}
	var resp logsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Artifacts) != 1 || resp.Artifacts[0].Status != string(logartifact.StatusWriting) {
		t.Fatalf("artifacts = %+v want one with status=writing", resp.Artifacts)
	}
}

func TestListBeadLogs_PaginationCursorRoundTrip(t *testing.T) {
	manifests := make([]logartifact.Manifest, 5)
	for i := range manifests {
		id := []string{"log-a", "log-b", "log-c", "log-d", "log-e"}[i]
		manifests[i] = makeManifest(id, func(m *logartifact.Manifest) {
			m.Sequence = i
		})
	}
	reader := &fakeLogReader{manifests: manifests}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	// Page 1: limit=2 → first two rows + cursor.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?limit=2", nil)
	rec := httptest.NewRecorder()
	s.listBeadLogs(rec, req, "spi-test1")
	if rec.Code != http.StatusOK {
		t.Fatalf("page1 status = %d want 200", rec.Code)
	}
	var page1 logsListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &page1)
	if len(page1.Artifacts) != 2 || page1.Artifacts[0].ID != "log-a" || page1.Artifacts[1].ID != "log-b" {
		t.Fatalf("page1 = %+v", page1.Artifacts)
	}
	if page1.NextCursor == "" {
		t.Fatalf("page1 cursor empty, want set")
	}

	// Page 2: same limit, follow cursor → next two rows.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?limit=2&cursor="+page1.NextCursor, nil)
	rec2 := httptest.NewRecorder()
	s.listBeadLogs(rec2, req2, "spi-test1")
	if rec2.Code != http.StatusOK {
		t.Fatalf("page2 status = %d want 200", rec2.Code)
	}
	var page2 logsListResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &page2)
	if len(page2.Artifacts) != 2 || page2.Artifacts[0].ID != "log-c" || page2.Artifacts[1].ID != "log-d" {
		t.Fatalf("page2 = %+v", page2.Artifacts)
	}
	if page2.NextCursor == "" {
		t.Fatalf("page2 cursor empty, want set (one row remains)")
	}

	// Page 3: final row, cursor must be empty.
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?limit=2&cursor="+page2.NextCursor, nil)
	rec3 := httptest.NewRecorder()
	s.listBeadLogs(rec3, req3, "spi-test1")
	var page3 logsListResponse
	_ = json.Unmarshal(rec3.Body.Bytes(), &page3)
	if len(page3.Artifacts) != 1 || page3.Artifacts[0].ID != "log-e" {
		t.Fatalf("page3 = %+v", page3.Artifacts)
	}
	if page3.NextCursor != "" {
		t.Fatalf("page3 cursor = %q want empty", page3.NextCursor)
	}
}

func TestListBeadLogs_BadCursorReturns400(t *testing.T) {
	reader := &fakeLogReader{}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?cursor=not-base64!@#", nil)
	rec := httptest.NewRecorder()
	s.listBeadLogs(rec, req, "spi-test1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400 body=%q", rec.Code, rec.Body.String())
	}
}

func TestGetBeadLogRaw_StreamsBytesForEngineerScope(t *testing.T) {
	transcript := []byte(`{"type":"system","subtype":"init","session_id":"abc","cwd":"/x"}` + "\n")
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{
			makeManifest("log-a", func(m *logartifact.Manifest) {
				m.ByteSize = int64(len(transcript))
			}),
		},
		bytes: map[string][]byte{"log-a": transcript},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs/log-a/raw", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.getBeadLogRaw(rec, req, "spi-test1", "log-a")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), transcript) {
		t.Fatalf("body=%q want %q", rec.Body.String(), transcript)
	}
	if rec.Header().Get("X-Spire-Render-Redaction-Version") != "0" {
		t.Fatalf("redaction header = %q want 0", rec.Header().Get("X-Spire-Render-Redaction-Version"))
	}
	if rec.Header().Get("Content-Type") != "application/x-ndjson; charset=utf-8" {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestGetBeadLogRaw_RedactsForDesktopScope(t *testing.T) {
	// Build a payload with a token shape the redactor masks. We pick
	// "Authorization: Bearer ..." because the pattern set in
	// pkg/logartifact/redact catches it; the masked payload must NOT
	// contain the secret value.
	secret := "Authorization: Bearer abcdef0123456789abcdef0123456789"
	payload := []byte("line1\n" + secret + "\nline3\n")
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{
			makeManifest("log-a", func(m *logartifact.Manifest) {
				m.Visibility = logartifact.VisibilityDesktopSafe
				m.ByteSize = int64(len(payload))
			}),
		},
		bytes: map[string][]byte{"log-a": payload},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs/log-a/raw", nil)
	// Default scope (desktop) — engineer-only material would 403 here,
	// but the manifest is desktop_safe so the read proceeds and the
	// redactor strips the token.
	rec := httptest.NewRecorder()
	s.getBeadLogRaw(rec, req, "spi-test1", "log-a")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("abcdef0123456789abcdef0123456789")) {
		t.Fatalf("body still contains secret bytes: %q", rec.Body.String())
	}
	if !redact.Contains(rec.Body.Bytes()) {
		t.Fatalf("body lacks redaction mask: %q", rec.Body.String())
	}
	if rec.Header().Get("X-Spire-Render-Redaction-Version") == "0" {
		t.Fatalf("redaction header = 0, want >=1")
	}
}

func TestGetBeadLogRaw_EngineerOnlyDeniesDesktopScope(t *testing.T) {
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{makeManifest("log-a")},
		bytes:     map[string][]byte{"log-a": []byte("hello\n")},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs/log-a/raw", nil)
	// Default scope (no header) → desktop, which cannot read engineer_only.
	rec := httptest.NewRecorder()
	s.getBeadLogRaw(rec, req, "spi-test1", "log-a")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d want 403 body=%q", rec.Code, rec.Body.String())
	}
}

func TestGetBeadLogRaw_NotYetFinalized(t *testing.T) {
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{
			makeManifest("log-a", func(m *logartifact.Manifest) {
				m.Status = logartifact.StatusWriting
			}),
		},
		bytes: map[string][]byte{"log-a": []byte("hello\n")},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs/log-a/raw", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.getBeadLogRaw(rec, req, "spi-test1", "log-a")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d want 409 body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not yet available") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestGetBeadLogRaw_ManifestNotFound(t *testing.T) {
	reader := &fakeLogReader{}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs/log-missing/raw", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.getBeadLogRaw(rec, req, "spi-test1", "log-missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404 body=%q", rec.Code, rec.Body.String())
	}
}

func TestGetBeadLogRaw_BeadIDMismatch(t *testing.T) {
	// An attacker-controlled bead segment must not be enough to fetch
	// any manifest by ID — the bead in the URL has to match the
	// manifest's bead.
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{makeManifest("log-a")},
		bytes:     map[string][]byte{"log-a": []byte("hello\n")},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-other/logs/log-a/raw", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.getBeadLogRaw(rec, req, "spi-other", "log-a")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404 body=%q", rec.Code, rec.Body.String())
	}
}

func TestGetBeadLogRaw_ArtifactBytesMissing(t *testing.T) {
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{makeManifest("log-a")},
		bytes:     map[string][]byte{"log-a": []byte("hello\n")},
		notFound:  map[string]bool{"log-a": true}, // simulate GCS lifecycle deletion
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs/log-a/raw", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.getBeadLogRaw(rec, req, "spi-test1", "log-a")
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d want 410 body=%q", rec.Code, rec.Body.String())
	}
}

func TestGetBeadLogPretty_ParsesClaudeTranscript(t *testing.T) {
	// One init line + one user prompt — the claude adapter parses both
	// and emits two events.
	transcript := []byte(strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1","cwd":"/x"}`,
		`{"type":"user","message":{"content":"hello"}}`,
	}, "\n") + "\n")
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{
			makeManifest("log-a", func(m *logartifact.Manifest) {
				m.ByteSize = int64(len(transcript))
			}),
		},
		bytes: map[string][]byte{"log-a": transcript},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs/log-a/pretty", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.getBeadLogPretty(rec, req, "spi-test1", "log-a")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	var resp prettyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%q", err, rec.Body.String())
	}
	if resp.Provider != "claude" {
		t.Fatalf("provider = %q want claude", resp.Provider)
	}
	if len(resp.Events) < 1 {
		t.Fatalf("events = %d want >=1", len(resp.Events))
	}
	// The first event from a claude init line must have kind
	// session-start so the desktop renderer keys correctly.
	if resp.Events[0].Kind != "session-start" {
		t.Fatalf("first event kind = %q want session-start", resp.Events[0].Kind)
	}
}

func TestGetBeadLogPretty_RejectsNonTranscriptStream(t *testing.T) {
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{
			makeManifest("log-a", func(m *logartifact.Manifest) {
				m.Identity.Stream = logartifact.StreamStdout
			}),
		},
		bytes: map[string][]byte{"log-a": []byte("plain stdout\n")},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs/log-a/pretty", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.getBeadLogPretty(rec, req, "spi-test1", "log-a")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400 body=%q", rec.Code, rec.Body.String())
	}
}

func TestBearerAuth_RejectsUnauthenticatedLogsRequest(t *testing.T) {
	// Auth applies at the mux layer. We exercise the actual middleware
	// chain on a token-protected server so the test catches a
	// regression that loosens auth on the new routes.
	withLogStubs(t, &fakeLogReader{}, store.Bead{Status: "open"}, nil)
	s := newLogTestServer("test-token")

	mux := http.NewServeMux()
	mux.Handle("/api/v1/beads/", s.corsMiddleware(s.bearerAuth(s.handleBeadByID)))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/beads/spi-test1/logs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401", resp.StatusCode)
	}

	// Same request with a valid bearer should reach the handler and
	// return 200 (empty list, since reader has no manifests).
	authedReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/beads/spi-test1/logs", nil)
	authedReq.Header.Set("Authorization", "Bearer test-token")
	authedResp, err := http.DefaultClient.Do(authedReq)
	if err != nil {
		t.Fatalf("authed get: %v", err)
	}
	defer authedResp.Body.Close()
	if authedResp.StatusCode != http.StatusOK {
		t.Fatalf("authed status = %d want 200", authedResp.StatusCode)
	}
}

func TestParseLogsListLimit_DefaultsAndClamps(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", defaultLogsListLimit},
		{"abc", defaultLogsListLimit},
		{"-5", defaultLogsListLimit},
		{"0", defaultLogsListLimit},
		{"5", 5},
		{"99999", maxLogsListLimit},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := parseLogsListLimit(tc.in)
			if got != tc.want {
				t.Fatalf("parseLogsListLimit(%q) = %d want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestEncodeDecodeLogsCursor_RoundTrip(t *testing.T) {
	c := logsCursor{ArtifactSeq: 42, ByteOffset: 100}
	enc := encodeLogsCursor(c)
	dec, err := decodeLogsCursor(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec != c {
		t.Fatalf("round-trip = %+v want %+v", dec, c)
	}
}

func TestSetLogArtifactReader_NilUnwiresBackend(t *testing.T) {
	// SetLogArtifactReader(nil) returns ok=false from the resolver so
	// the gateway exposes the empty-manifest mode instead of a stale
	// pointer. This guards against a regression where a nil install
	// silently keeps the previous reader live.
	prev := logArtifactReaderFunc
	t.Cleanup(func() { logArtifactReaderFunc = prev })

	SetLogArtifactReader(&fakeLogReader{})
	if _, ok := logArtifactReaderFunc(); !ok {
		t.Fatalf("after SetLogArtifactReader(non-nil), ok must be true")
	}
	SetLogArtifactReader(nil)
	if _, ok := logArtifactReaderFunc(); ok {
		t.Fatalf("after SetLogArtifactReader(nil), ok must be false")
	}
}
