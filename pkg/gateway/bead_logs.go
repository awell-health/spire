package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/board/logstream"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/logartifact"
	"github.com/awell-health/spire/pkg/logartifact/redact"
	"github.com/awell-health/spire/pkg/store"
)

// LogArtifactReader is the read-side subset of logartifact.Store that the
// gateway uses to serve bead-scoped log endpoints. The interface is
// narrower than logartifact.Store (no Put/Finalize) because the gateway
// never writes artifacts — exporters and agents own that path.
//
// Both logartifact.LocalStore and logartifact.GCSStore satisfy this
// interface implicitly. Tests inject a fake to avoid spinning up a real
// SQL database or GCS client.
type LogArtifactReader interface {
	List(ctx context.Context, filter logartifact.Filter) ([]logartifact.Manifest, error)
	Get(ctx context.Context, ref logartifact.ManifestRef) (io.ReadCloser, logartifact.Manifest, error)
	Stat(ctx context.Context, ref logartifact.ManifestRef) (logartifact.Manifest, error)
}

// logArtifactReaderFunc resolves the LogArtifactReader to use for a
// request. Returning ok=false signals that the gateway has no log
// artifact backend wired (e.g. a tower without logStore configured); the
// list endpoint then degrades to an empty manifest list rather than 500.
//
// cmd/spire wires this to the chart-configured backend at boot.
var logArtifactReaderFunc = func() (LogArtifactReader, bool) { return nil, false }

// SetLogArtifactReader installs the gateway-wide log artifact reader.
// Call once at startup from cmd/spire after constructing the local or
// GCS-backed store. Passing nil is allowed and results in
// logArtifactReaderFunc returning ok=false (the empty-manifest mode).
func SetLogArtifactReader(r LogArtifactReader) {
	if r == nil {
		logArtifactReaderFunc = func() (LogArtifactReader, bool) { return nil, false }
		return
	}
	logArtifactReaderFunc = func() (LogArtifactReader, bool) { return r, true }
}

// logArtifactReconcileFunc is the optional bead-scoped reconcile hook.
// When set, the list handler invokes it before returning an empty
// manifest list so on-disk transcripts that pre-date the substrate's
// write-path registration become visible without requiring the operator
// to run `spire reconcile-logs` first.
//
// The contract is: a single call per request, scoped to the requested
// bead, swallowing errors (return nil on best-effort failure). Wiring
// is via SetLogArtifactReconciler.
var logArtifactReconcileFunc func(ctx context.Context, beadID string) error

// SetLogArtifactReconciler installs the lazy reconcile hook. Pass nil
// to disable. The hook fires when reader.List returns an empty list,
// before the response is written, so a single retry surfaces newly
// inserted rows. cmd/spire wires this in local-native mode where the
// backend is a logartifact.LocalStore.
func SetLogArtifactReconciler(fn func(ctx context.Context, beadID string) error) {
	logArtifactReconcileFunc = fn
}

// Test seams: bead resolution and store-ensure are mocked through these
// vars so handler tests don't need to spin up a real Dolt store. The
// production wiring delegates to pkg/store.
var (
	logsStoreEnsureFunc = func(dir string) error {
		_, err := store.Ensure(dir)
		return err
	}
	logsGetBeadFunc = store.GetBead
)

// logArtifactRow is the JSON projection of one manifest row served on
// the list endpoint. It carries the identity tuple plus the small set of
// metadata desktop and CLI clients need to decide what to fetch next.
type logArtifactRow struct {
	ID               string                 `json:"id"`
	BeadID           string                 `json:"bead_id"`
	AttemptID        string                 `json:"attempt_id"`
	RunID            string                 `json:"run_id"`
	AgentName        string                 `json:"agent_name"`
	Role             string                 `json:"role"`
	Phase            string                 `json:"phase"`
	Provider         string                 `json:"provider,omitempty"`
	Stream           string                 `json:"stream"`
	Sequence         int                    `json:"sequence"`
	ByteSize         int64                  `json:"byte_size"`
	Checksum         string                 `json:"checksum,omitempty"`
	Status           string                 `json:"status"`
	StartedAt        string                 `json:"started_at,omitempty"`
	EndedAt          string                 `json:"ended_at,omitempty"`
	CreatedAt        string                 `json:"created_at"`
	UpdatedAt        string                 `json:"updated_at"`
	RedactionVersion int                    `json:"redaction_version"`
	Visibility       logartifact.Visibility `json:"visibility"`
	Summary          string                 `json:"summary,omitempty"`
	Tail             string                 `json:"tail,omitempty"`
	Links            logArtifactLinks       `json:"links"`
}

// logArtifactLinks bundles the per-artifact sub-route URLs callers can
// follow without re-deriving them from the bead path.
type logArtifactLinks struct {
	Raw    string `json:"raw"`
	Pretty string `json:"pretty,omitempty"`
}

// logsListResponse is the envelope for GET /api/v1/beads/{id}/logs.
// next_cursor is empty when no more rows are available; the cursor is
// opaque base64 — callers must not parse it.
type logsListResponse struct {
	Artifacts  []logArtifactRow `json:"artifacts"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// logsCursor is the in-memory cursor shape. byte_offset is reserved for
// the live-follow work in spi-bkha5x; today it is always 0 on outbound
// cursors. Encoding both fields now means the wire format does not need
// to change when live-follow lands.
type logsCursor struct {
	ArtifactSeq int   `json:"a"`
	ByteOffset  int64 `json:"b"`
}

// defaultLogsListLimit is the page size used when the caller does not
// pass ?limit. Picked to keep payloads under ~64KB even with the
// summary/tail fields populated.
const defaultLogsListLimit = 50

// maxLogsListLimit clamps caller-supplied ?limit values. Large values
// would defeat the cursor; a smaller cap costs an extra round-trip but
// keeps the response bounded.
const maxLogsListLimit = 200

// handleBeadLogsRoute dispatches the /api/v1/beads/{id}/logs* family of
// routes to the right handler based on the path tail. Tail values:
//
//   - ""                      → list
//   - "/summary"              → summary
//   - "/{artifact_id}/raw"    → raw
//   - "/{artifact_id}/pretty" → pretty
//
// Anything else is a 404 — the surface is small and stable so undefined
// suffixes are likely client typos rather than future API growth.
func (s *Server) handleBeadLogsRoute(w http.ResponseWriter, r *http.Request, beadID, tail string) {
	if beadID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bead ID required"})
		return
	}
	switch {
	case tail == "" || tail == "/":
		s.listBeadLogs(w, r, beadID)
		return
	case tail == "/summary":
		s.getBeadLogSummary(w, r, beadID)
		return
	}
	if strings.HasSuffix(tail, "/raw") {
		artifactID := strings.TrimSuffix(strings.TrimPrefix(tail, "/"), "/raw")
		if artifactID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "artifact ID required"})
			return
		}
		s.getBeadLogRaw(w, r, beadID, artifactID)
		return
	}
	if strings.HasSuffix(tail, "/pretty") {
		artifactID := strings.TrimSuffix(strings.TrimPrefix(tail, "/"), "/pretty")
		if artifactID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "artifact ID required"})
			return
		}
		s.getBeadLogPretty(w, r, beadID, artifactID)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// listBeadLogs answers GET /api/v1/beads/{id}/logs with the manifest
// rows for a bead. The response is an envelope so cursor metadata can
// land alongside the rows without a header-only contract. Empty bead
// returns 200 with an empty list — callers distinguish "no logs yet"
// from "bead missing" via the bead-resolve 404.
//
// When the request carries ?follow=true, control is handed to the
// follow handler which serves a different envelope (events instead of
// manifest rows). The shared path keeps URL surface tight so clients
// only need to remember /logs.
func (s *Server) listBeadLogs(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if isFollowRequest(r) {
		s.followBeadLogs(w, r, id)
		return
	}
	if err := logsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if _, err := logsGetBeadFunc(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	limit := parseLogsListLimit(r.URL.Query().Get("limit"))
	cursor, err := decodeLogsCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
		return
	}

	reader, ok := logArtifactReaderFunc()
	if !ok {
		// No backend wired: the bead exists but there are no manifest
		// rows the gateway can resolve. Return an empty list — clients
		// treat this as "no logs available", which is correct.
		writeJSON(w, http.StatusOK, logsListResponse{Artifacts: []logArtifactRow{}})
		return
	}

	manifests, err := reader.List(r.Context(), logartifact.Filter{BeadID: id})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(manifests) == 0 && logArtifactReconcileFunc != nil {
		// Transition fallback: the manifest is empty but the on-disk
		// wizard log directory may have transcripts for this bead that
		// were never registered through Put/Finalize. Reconcile once
		// and re-list. Best-effort — a reconcile error is swallowed so
		// a flaky filesystem walk never escalates a list to 500.
		_ = logArtifactReconcileFunc(r.Context(), id)
		manifests, err = reader.List(r.Context(), logartifact.Filter{BeadID: id})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	// Cursor advance: skip rows whose ordinal position is below the
	// recorded artifact_seq. The list is already deterministic by
	// (attempt, run, sequence, created_at) — see ListLogArtifactsForBead.
	start := cursor.ArtifactSeq
	if start < 0 {
		start = 0
	}
	if start > len(manifests) {
		start = len(manifests)
	}
	end := start + limit
	if end > len(manifests) {
		end = len(manifests)
	}

	rows := make([]logArtifactRow, 0, end-start)
	for _, m := range manifests[start:end] {
		rows = append(rows, manifestToRow(id, m))
	}

	resp := logsListResponse{Artifacts: rows}
	if end < len(manifests) {
		resp.NextCursor = encodeLogsCursor(logsCursor{ArtifactSeq: end})
	}
	writeJSON(w, http.StatusOK, resp)
}

// getBeadLogSummary answers GET /api/v1/beads/{id}/logs/summary with
// the bounded summary/tail rows already stored on each manifest. Cheap
// because no artifact bytes are fetched; the board uses this for the
// Logs tab header.
func (s *Server) getBeadLogSummary(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if err := logsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if _, err := logsGetBeadFunc(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	reader, ok := logArtifactReaderFunc()
	if !ok {
		writeJSON(w, http.StatusOK, logsListResponse{Artifacts: []logArtifactRow{}})
		return
	}
	manifests, err := reader.List(r.Context(), logartifact.Filter{BeadID: id})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rows := make([]logArtifactRow, 0, len(manifests))
	for _, m := range manifests {
		rows = append(rows, manifestToRow(id, m))
	}
	writeJSON(w, http.StatusOK, logsListResponse{Artifacts: rows})
}

// getBeadLogRaw answers GET /api/v1/beads/{id}/logs/{artifact_id}/raw —
// streams the artifact's stored bytes through the gateway. Engineer
// callers reading engineer_only artifacts get the bytes verbatim;
// every other path passes the bytes through the runtime redactor. The
// response is streamed with io.Copy so a large transcript never gets
// fully buffered in gateway memory.
func (s *Server) getBeadLogRaw(w http.ResponseWriter, r *http.Request, beadID, artifactID string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if err := logsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	reader, ok := logArtifactReaderFunc()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "log artifact backend not configured"})
		return
	}

	manifest, err := reader.Stat(r.Context(), logartifact.ManifestRef{ID: artifactID})
	if err != nil {
		if errors.Is(err, logartifact.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "log artifact not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if manifest.Identity.BeadID != beadID {
		// Refuse to serve an artifact under the wrong bead path even
		// if the ID resolves; the bead segment in the URL is part of
		// the access contract.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "log artifact not found for bead"})
		return
	}
	if manifest.Status != logartifact.StatusFinalized {
		// Manifest-only rows (writing) and failed rows have no readable
		// bytes through this path. Surface 409 with the current status
		// so clients can decide whether to retry or give up.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":  "artifact not yet available",
			"status": string(manifest.Status),
		})
		return
	}
	scope := scopeFromRequest(r)
	if !canReadVisibility(scope, manifest.Visibility) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":      "access denied for caller scope",
			"visibility": string(manifest.Visibility),
		})
		return
	}

	rc, m2, err := reader.Get(r.Context(), logartifact.ManifestRef{ID: artifactID})
	if err != nil {
		if errors.Is(err, logartifact.ErrNotFound) {
			// Manifest exists but the byte store has lost the object —
			// most often a GCS lifecycle policy removed the artifact
			// while the manifest persists. Distinct from 404 on a
			// missing manifest.
			writeJSON(w, http.StatusGone, map[string]string{"error": "artifact bytes unavailable"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", contentTypeForStream(m2))
	w.Header().Set("X-Spire-Manifest-ID", m2.ID)
	w.Header().Set("X-Spire-Visibility", string(m2.Visibility))
	w.Header().Set("X-Spire-Stored-Redaction-Version", strconv.Itoa(m2.RedactionVersion))

	// Engineer-scope reads of engineer-only artifacts pass through
	// unmodified — defense-in-depth at the storage layer is a no-op on
	// raw bytes the operator has already authorized for this scope.
	if scope == logartifact.ScopeEngineer && m2.Visibility == logartifact.VisibilityEngineerOnly {
		w.Header().Set("X-Spire-Render-Redaction-Version", "0")
		// Trust the manifest's recorded byte size when redaction is a
		// no-op; mismatches here would surface as truncated downloads.
		if m2.ByteSize > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(m2.ByteSize, 10))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, rc)
		return
	}

	// Redacted path. The redactor's regex set must see token
	// boundaries, so we read into memory before applying it. For
	// cap-bounded provider transcripts (single MB at most under the
	// design's intended use) this is acceptable; if/when the cap
	// changes a streaming filter can replace the buffer here without
	// touching the contract.
	raw, err := io.ReadAll(rc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read artifact: " + err.Error()})
		return
	}
	redacted, version := redact.New().Redact(raw)
	w.Header().Set("X-Spire-Render-Redaction-Version", strconv.Itoa(version))
	w.Header().Set("Content-Length", strconv.Itoa(len(redacted)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(redacted)
}

// prettyResponse is the JSON envelope for the pretty endpoint. Events
// are returned in canonical logstream order; clients render styling
// locally so the gateway stays presentation-free.
type prettyResponse struct {
	ManifestID             string                 `json:"manifest_id"`
	BeadID                 string                 `json:"bead_id"`
	Provider               string                 `json:"provider"`
	Visibility             logartifact.Visibility `json:"visibility"`
	StoredRedactionVersion int                    `json:"stored_redaction_version"`
	RedactionVersion       int                    `json:"redaction_version"`
	Events                 []prettyEvent          `json:"events"`
}

// prettyEvent is the JSON shape of one parsed log event. It mirrors
// logstream.LogEvent but ships time as RFC3339 (or empty) so JSON
// consumers don't need a logstream-specific decoder.
type prettyEvent struct {
	Kind  string            `json:"kind"`
	Time  string            `json:"time,omitempty"`
	Title string            `json:"title,omitempty"`
	Body  string            `json:"body,omitempty"`
	Meta  map[string]string `json:"meta,omitempty"`
	Error bool              `json:"error,omitempty"`
}

// getBeadLogPretty answers GET /api/v1/beads/{id}/logs/{artifact_id}/pretty
// — fetches the artifact via the store, runs it through the provider's
// logstream adapter, and returns the parsed events as JSON. Stream type
// must be transcript (only transcript adapters know how to parse);
// stdout/stderr would be a 400.
func (s *Server) getBeadLogPretty(w http.ResponseWriter, r *http.Request, beadID, artifactID string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if err := logsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	reader, ok := logArtifactReaderFunc()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "log artifact backend not configured"})
		return
	}

	manifest, err := reader.Stat(r.Context(), logartifact.ManifestRef{ID: artifactID})
	if err != nil {
		if errors.Is(err, logartifact.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "log artifact not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if manifest.Identity.BeadID != beadID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "log artifact not found for bead"})
		return
	}
	if manifest.Identity.Stream != logartifact.StreamTranscript {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "pretty rendering is only available for transcript streams",
			"stream": string(manifest.Identity.Stream),
		})
		return
	}
	if manifest.Status != logartifact.StatusFinalized {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":  "artifact not yet available",
			"status": string(manifest.Status),
		})
		return
	}
	scope := scopeFromRequest(r)
	if !canReadVisibility(scope, manifest.Visibility) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":      "access denied for caller scope",
			"visibility": string(manifest.Visibility),
		})
		return
	}

	rc, m2, err := reader.Get(r.Context(), logartifact.ManifestRef{ID: artifactID})
	if err != nil {
		if errors.Is(err, logartifact.ErrNotFound) {
			writeJSON(w, http.StatusGone, map[string]string{"error": "artifact bytes unavailable"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rc.Close()

	raw, err := io.ReadAll(rc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read artifact: " + err.Error()})
		return
	}

	// Apply the redactor unless engineer scope is reading engineer-only
	// content. The byte stream feeds the adapter, not the response, so
	// the parsed events reflect the redacted shape.
	renderedRedactionVersion := 0
	if !(scope == logartifact.ScopeEngineer && m2.Visibility == logartifact.VisibilityEngineerOnly) {
		raw, renderedRedactionVersion = redact.New().Redact(raw)
	}

	adapter := logstream.Get(m2.Identity.Provider)
	events, _ := adapter.Parse(string(raw))
	out := make([]prettyEvent, 0, len(events))
	for _, ev := range events {
		pe := prettyEvent{
			Kind:  ev.Kind.String(),
			Title: ev.Title,
			Body:  ev.Body,
			Meta:  ev.Meta,
			Error: ev.Error,
		}
		if !ev.Time.IsZero() {
			pe.Time = ev.Time.UTC().Format("2006-01-02T15:04:05Z")
		}
		out = append(out, pe)
	}

	writeJSON(w, http.StatusOK, prettyResponse{
		ManifestID:             m2.ID,
		BeadID:                 beadID,
		Provider:               m2.Identity.Provider,
		Visibility:             m2.Visibility,
		StoredRedactionVersion: m2.RedactionVersion,
		RedactionVersion:       renderedRedactionVersion,
		Events:                 out,
	})
}

// manifestToRow converts a substrate manifest into the JSON row shape.
// The links block carries the canonical URLs for raw/pretty so clients
// don't need to format them.
func manifestToRow(beadID string, m logartifact.Manifest) logArtifactRow {
	row := logArtifactRow{
		ID:               m.ID,
		BeadID:           m.Identity.BeadID,
		AttemptID:        m.Identity.AttemptID,
		RunID:            m.Identity.RunID,
		AgentName:        m.Identity.AgentName,
		Role:             string(m.Identity.Role),
		Phase:            m.Identity.Phase,
		Provider:         m.Identity.Provider,
		Stream:           string(m.Identity.Stream),
		Sequence:         m.Sequence,
		ByteSize:         m.ByteSize,
		Checksum:         m.Checksum,
		Status:           string(m.Status),
		RedactionVersion: m.RedactionVersion,
		Visibility:       m.Visibility,
		Summary:          m.Summary,
		Tail:             m.Tail,
	}
	if !m.StartedAt.IsZero() {
		row.StartedAt = m.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if !m.EndedAt.IsZero() {
		row.EndedAt = m.EndedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if !m.CreatedAt.IsZero() {
		row.CreatedAt = m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if !m.UpdatedAt.IsZero() {
		row.UpdatedAt = m.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	row.Links.Raw = fmt.Sprintf("/api/v1/beads/%s/logs/%s/raw", beadID, m.ID)
	if m.Identity.Stream == logartifact.StreamTranscript {
		row.Links.Pretty = fmt.Sprintf("/api/v1/beads/%s/logs/%s/pretty", beadID, m.ID)
	}
	return row
}

// parseLogsListLimit clamps and defaults the ?limit query parameter. A
// missing or unparseable value falls back to defaultLogsListLimit; a
// value above maxLogsListLimit is clamped down rather than 400'd
// because URL hand-edits are common.
func parseLogsListLimit(s string) int {
	if s == "" {
		return defaultLogsListLimit
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return defaultLogsListLimit
	}
	if n > maxLogsListLimit {
		return maxLogsListLimit
	}
	return n
}

// encodeLogsCursor returns a base64(JSON) representation of c. The
// cursor is opaque on the wire so clients never parse its internals.
func encodeLogsCursor(c logsCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeLogsCursor parses a cursor returned by encodeLogsCursor. An
// empty input yields a zero cursor; a malformed input is a 400 to the
// caller, distinct from "no cursor sent".
func decodeLogsCursor(s string) (logsCursor, error) {
	if s == "" {
		return logsCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return logsCursor{}, err
	}
	var c logsCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return logsCursor{}, err
	}
	return c, nil
}

// scopeFromRequest derives the caller scope from request headers. Until
// the gateway carries identity-aware RBAC, callers can pass
// X-Spire-Scope with a known value to override the default.
//
// Default behavior depends on the gateway's deployment mode (spi-dhqv40):
//   - local-native: defaults to ScopeEngineer. The gateway is bound to
//     localhost and the only caller is the operator's own desktop/CLI;
//     gating the operator from their own engineer-only artifacts breaks
//     the bead-detail Logs tab without serving any threat model.
//   - cluster-native / cluster-attached / unknown: defaults to
//     ScopeDesktop. Foreign desktop callers exist in those topologies and
//     engineer-only artifacts must not leak by default. Internal/CLI
//     callers that need raw bytes set the header explicitly.
//
// Explicit headers always win over the default — operators can voluntarily
// downgrade to ScopeDesktop or ScopePublic in any mode.
func scopeFromRequest(r *http.Request) logartifact.CallerScope {
	v := strings.TrimSpace(r.Header.Get("X-Spire-Scope"))
	switch logartifact.CallerScope(v) {
	case logartifact.ScopeEngineer:
		return logartifact.ScopeEngineer
	case logartifact.ScopeDesktop:
		return logartifact.ScopeDesktop
	case logartifact.ScopePublic:
		return logartifact.ScopePublic
	}
	return defaultScopeForMode(scopeDeploymentModeFunc())
}

// defaultScopeForMode picks the no-header default scope for a deployment
// mode. Pulled out as a pure function so tests can drive every mode
// without standing up a real tower config.
func defaultScopeForMode(mode config.DeploymentMode) logartifact.CallerScope {
	if mode == config.DeploymentModeLocalNative {
		return logartifact.ScopeEngineer
	}
	return logartifact.ScopeDesktop
}

// scopeDeploymentModeFunc resolves the active tower's deployment mode for
// scopeFromRequest. Wrapped in a func so tests can stub it without
// touching the real config dir; production callers use the package-level
// resolver which mirrors handleRoster's resolveTowerForRosterFunc.
//
// On any resolution error, returns DeploymentModeUnknown so the
// fall-through default is the safer ScopeDesktop, matching pre-spi-dhqv40
// behavior.
var scopeDeploymentModeFunc = func() config.DeploymentMode {
	tower, err := config.ResolveTowerConfig()
	if err != nil || tower == nil {
		return config.DeploymentModeUnknown
	}
	return tower.EffectiveDeploymentMode()
}

// canReadVisibility mirrors the access matrix in pkg/logartifact.Render.
// Duplicated rather than imported so the gateway can decide without
// loading bytes — Render-side gating still applies if a caller routes
// through that path, but on the gateway hot path we want the cheapest
// possible refusal.
func canReadVisibility(scope logartifact.CallerScope, vis logartifact.Visibility) bool {
	switch scope {
	case logartifact.ScopeEngineer:
		return true
	case logartifact.ScopeDesktop:
		return vis == logartifact.VisibilityDesktopSafe || vis == logartifact.VisibilityPublic
	case logartifact.ScopePublic:
		return vis == logartifact.VisibilityPublic
	default:
		return false
	}
}

// contentTypeForStream picks a reasonable Content-Type based on the
// manifest's stream. Transcripts are emitted as JSONL; stdout/stderr
// are plain text. The body itself is unchanged — this only sets the
// header so curl/desktop pick the right viewer.
func contentTypeForStream(m logartifact.Manifest) string {
	switch m.Identity.Stream {
	case logartifact.StreamTranscript:
		return "application/x-ndjson; charset=utf-8"
	default:
		return "text/plain; charset=utf-8"
	}
}
