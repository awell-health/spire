package gateway

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/observability"
	"github.com/awell-health/spire/pkg/olap"
)

// listAttemptToolCallsFunc is the seam through which handleAttemptToolCalls
// queries the OLAP store. Mirrors traceCollect / graphCollect — tests
// install a fake here to exercise the handler without spinning a real
// DuckDB.
var listAttemptToolCallsFunc = observability.ListAttemptToolCalls

// handleAttemptByID routes /api/v1/attempts/{id}/tool_calls (the only
// attempt-scoped sub-resource today). The bare /api/v1/attempts/{id}
// path returns 404 — there is no general attempt detail endpoint yet,
// and squatting on it now would either return inconsistent data or
// require a second round of refactor when the detail endpoint lands.
func (s *Server) handleAttemptByID(w http.ResponseWriter, r *http.Request) {
	rest := pathSuffix(r.URL.Path, "/api/v1/attempts/")
	if rest == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	id, sub, _ := strings.Cut(rest, "/")
	if id == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	switch sub {
	case "tool_calls":
		s.getAttemptToolCalls(w, r, id)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// AttemptToolCallsResponse is the JSON shape returned by
// GET /api/v1/attempts/{id}/tool_calls. Page indexing is one-based;
// tool_calls is the slice for the requested page (possibly empty).
type AttemptToolCallsResponse struct {
	AttemptID string                `json:"attempt_id"`
	Page      int                   `json:"page"`
	PageSize  int                   `json:"page_size"`
	ToolCalls []olap.ToolCallRecord `json:"tool_calls"`
}

// getAttemptToolCalls answers GET /api/v1/attempts/{id}/tool_calls with
// the per-invocation tool calls captured during the attempt. Page and
// page_size are query params (defaults: page=1, page_size=200; cap
// page_size=1000 — over-large pages risk shipping multiple kilobytes
// of args JSON per row).
//
// Status codes:
//   - 200 with an empty tool_calls array when the attempt exists but
//     has no tool calls (e.g. crashed before any tool ran, or pre-
//     migration attempt with no session_id stamp).
//   - 405 for non-GET methods.
//   - 500 for unexpected store/OLAP failures.
func (s *Server) getAttemptToolCalls(w http.ResponseWriter, r *http.Request, attemptID string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	pageSize := observability.DefaultToolCallPageSize
	if v := r.URL.Query().Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	}
	// Clamp once here so the value passed to the store, the value echoed
	// back to the client, and the value the store sees are all the same.
	// (ListAttemptToolCalls clamps defensively too, but routing both
	// values through one clamp keeps this handler the source of truth.)
	if pageSize > observability.MaxToolCallPageSize {
		pageSize = observability.MaxToolCallPageSize
	}

	rows, err := listAttemptToolCallsFunc(attemptID, page, pageSize)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []olap.ToolCallRecord{}
	}

	writeJSON(w, http.StatusOK, AttemptToolCallsResponse{
		AttemptID: attemptID,
		Page:      page,
		PageSize:  pageSize,
		ToolCalls: rows,
	})
}
