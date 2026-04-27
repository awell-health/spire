package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/observability"
	"github.com/awell-health/spire/pkg/olap"
)

// withFakeListAttempt swaps listAttemptToolCallsFunc for the duration of
// a test and restores it on cleanup. Mirrors withFakeCollect in the trace
// tests — the same pattern, applied to the attempt seam.
func withFakeListAttempt(t *testing.T, fake func(string, int, int) ([]olap.ToolCallRecord, error)) {
	t.Helper()
	orig := listAttemptToolCallsFunc
	listAttemptToolCallsFunc = fake
	t.Cleanup(func() { listAttemptToolCallsFunc = orig })
}

// --- handleAttemptByID routing ---

func TestHandleAttemptByID_BareCollectionIs404(t *testing.T) {
	withFakeListAttempt(t, func(string, int, int) ([]olap.ToolCallRecord, error) {
		t.Fatal("listAttemptToolCallsFunc should not be invoked on the bare collection path")
		return nil, nil
	})
	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/attempts/", nil)
	rec := httptest.NewRecorder()
	s.handleAttemptByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("bare collection: status = %d, want 404", rec.Code)
	}
}

func TestHandleAttemptByID_AttemptWithoutSubresourceIs404(t *testing.T) {
	withFakeListAttempt(t, func(string, int, int) ([]olap.ToolCallRecord, error) {
		t.Fatal("listAttemptToolCallsFunc should not be invoked without a subresource")
		return nil, nil
	})
	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/attempts/att-x", nil)
	rec := httptest.NewRecorder()
	s.handleAttemptByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("plain detail: status = %d, want 404", rec.Code)
	}
}

func TestHandleAttemptByID_UnknownSubresourceIs404(t *testing.T) {
	withFakeListAttempt(t, func(string, int, int) ([]olap.ToolCallRecord, error) {
		t.Fatal("listAttemptToolCallsFunc should not be invoked on unknown subresource")
		return nil, nil
	})
	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/attempts/att-x/something_else", nil)
	rec := httptest.NewRecorder()
	s.handleAttemptByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown sub: status = %d, want 404", rec.Code)
	}
}

func TestHandleAttemptByID_ToolCallsRoutesToHandler(t *testing.T) {
	called := false
	withFakeListAttempt(t, func(id string, page, pageSize int) ([]olap.ToolCallRecord, error) {
		called = true
		if id != "att-x" {
			t.Errorf("attempt id = %q, want att-x", id)
		}
		return []olap.ToolCallRecord{}, nil
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/attempts/att-x/tool_calls", nil)
	rec := httptest.NewRecorder()
	s.handleAttemptByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Error("listAttemptToolCallsFunc was not invoked")
	}
}

// --- getAttemptToolCalls method enforcement ---

func TestGetAttemptToolCalls_RejectsNonGet(t *testing.T) {
	withFakeListAttempt(t, func(string, int, int) ([]olap.ToolCallRecord, error) {
		t.Fatal("seam must not be invoked on non-GET")
		return nil, nil
	})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			s := newTestServer(&fakeTrigger{})
			req := httptest.NewRequest(method, "/api/v1/attempts/att-x/tool_calls", nil)
			rec := httptest.NewRecorder()
			s.handleAttemptByID(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s: status = %d, want 405", method, rec.Code)
			}
		})
	}
}

// --- query-param parsing and defaults ---

func TestGetAttemptToolCalls_DefaultPagination(t *testing.T) {
	var gotPage, gotPageSize int
	withFakeListAttempt(t, func(_ string, page, pageSize int) ([]olap.ToolCallRecord, error) {
		gotPage, gotPageSize = page, pageSize
		return nil, nil
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/attempts/att-x/tool_calls", nil)
	rec := httptest.NewRecorder()
	s.handleAttemptByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotPage != 1 {
		t.Errorf("default page = %d, want 1", gotPage)
	}
	if gotPageSize != observability.DefaultToolCallPageSize {
		t.Errorf("default page_size = %d, want %d", gotPageSize, observability.DefaultToolCallPageSize)
	}
}

func TestGetAttemptToolCalls_CustomPagination(t *testing.T) {
	var gotPage, gotPageSize int
	withFakeListAttempt(t, func(_ string, page, pageSize int) ([]olap.ToolCallRecord, error) {
		gotPage, gotPageSize = page, pageSize
		return nil, nil
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/attempts/att-x/tool_calls?page=3&page_size=50", nil)
	rec := httptest.NewRecorder()
	s.handleAttemptByID(rec, req)

	if gotPage != 3 || gotPageSize != 50 {
		t.Errorf("got page=%d size=%d, want page=3 size=50", gotPage, gotPageSize)
	}
}

func TestGetAttemptToolCalls_BadParamsFallToDefaults(t *testing.T) {
	cases := []struct {
		query        string
		wantPage     int
		wantPageSize int
	}{
		{"?page=abc", 1, observability.DefaultToolCallPageSize},
		{"?page=0", 1, observability.DefaultToolCallPageSize},
		{"?page=-5", 1, observability.DefaultToolCallPageSize},
		{"?page_size=junk", 1, observability.DefaultToolCallPageSize},
		{"?page_size=0", 1, observability.DefaultToolCallPageSize},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			var gotPage, gotPageSize int
			withFakeListAttempt(t, func(_ string, page, pageSize int) ([]olap.ToolCallRecord, error) {
				gotPage, gotPageSize = page, pageSize
				return nil, nil
			})

			s := newTestServer(&fakeTrigger{})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/attempts/att-x/tool_calls"+tc.query, nil)
			rec := httptest.NewRecorder()
			s.handleAttemptByID(rec, req)

			if gotPage != tc.wantPage {
				t.Errorf("page = %d, want %d", gotPage, tc.wantPage)
			}
			if gotPageSize != tc.wantPageSize {
				t.Errorf("pageSize = %d, want %d", gotPageSize, tc.wantPageSize)
			}
		})
	}
}

func TestGetAttemptToolCalls_PageSizeClampedOnceBeforeCall(t *testing.T) {
	// The handler clamps before calling the seam: the seam must see
	// MaxToolCallPageSize, not the user's larger value. Confirms the
	// "clamp once" fix from sage feedback (was previously clamped only
	// on the way back).
	var gotPageSize int
	withFakeListAttempt(t, func(_ string, _, pageSize int) ([]olap.ToolCallRecord, error) {
		gotPageSize = pageSize
		return nil, nil
	})

	s := newTestServer(&fakeTrigger{})
	url := "/api/v1/attempts/att-x/tool_calls?page_size=99999"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	s.handleAttemptByID(rec, req)

	if gotPageSize != observability.MaxToolCallPageSize {
		t.Errorf("seam saw page_size=%d, want clamped to %d",
			gotPageSize, observability.MaxToolCallPageSize)
	}

	// And the response echoes the clamped value too.
	var resp AttemptToolCallsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.PageSize != observability.MaxToolCallPageSize {
		t.Errorf("response page_size = %d, want %d",
			resp.PageSize, observability.MaxToolCallPageSize)
	}
}

// --- response shape ---

func TestGetAttemptToolCalls_NilRowsBecomeEmptySlice(t *testing.T) {
	// The store may return nil for a pre-migration / never-stamped attempt.
	// We must serialize as `[]`, not `null`, so JS clients can iterate.
	withFakeListAttempt(t, func(string, int, int) ([]olap.ToolCallRecord, error) {
		return nil, nil
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/attempts/att-x/tool_calls", nil)
	rec := httptest.NewRecorder()
	s.handleAttemptByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw unmarshal: %v", err)
	}
	if string(raw["tool_calls"]) != "[]" {
		t.Errorf("tool_calls = %s, want []", string(raw["tool_calls"]))
	}
}

func TestGetAttemptToolCalls_PopulatedRows(t *testing.T) {
	rows := []olap.ToolCallRecord{
		{
			ToolName: "Bash", Source: "span", Success: true, DurationMs: 250,
			Timestamp: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
			Attributes: `{"command":"ls"}`,
		},
		{
			ToolName: "Read", Source: "span", Success: true, DurationMs: 5,
			Attributes: `{"file_path":"/tmp/x"}`,
		},
	}
	withFakeListAttempt(t, func(string, int, int) ([]olap.ToolCallRecord, error) {
		return rows, nil
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/attempts/att-x/tool_calls", nil)
	rec := httptest.NewRecorder()
	s.handleAttemptByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp AttemptToolCallsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AttemptID != "att-x" {
		t.Errorf("attempt_id = %q, want att-x", resp.AttemptID)
	}
	if resp.Page != 1 {
		t.Errorf("page = %d, want 1", resp.Page)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("len(tool_calls) = %d, want 2", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ToolName != "Bash" {
		t.Errorf("tool_calls[0].tool_name = %q, want Bash", resp.ToolCalls[0].ToolName)
	}
}

func TestGetAttemptToolCalls_StoreErrorIs500(t *testing.T) {
	withFakeListAttempt(t, func(string, int, int) ([]olap.ToolCallRecord, error) {
		return nil, &olapErr{msg: "olap connect failed"}
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/attempts/att-x/tool_calls", nil)
	rec := httptest.NewRecorder()
	s.handleAttemptByID(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "olap connect failed") {
		t.Errorf("body should surface store error: %s", rec.Body.String())
	}
}

// olapErr is a trivial error implementation to avoid pulling in a stdlib
// errors import for one literal. fmt.Errorf would also work, but this
// keeps the test cohesive.
type olapErr struct{ msg string }

func (e *olapErr) Error() string { return e.msg }
