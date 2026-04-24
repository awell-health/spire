package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/awell-health/spire/pkg/trace"
)

// withFakeCollect swaps traceCollect for the duration of a test and
// restores it in the cleanup. Tests can't use t.Parallel because
// traceCollect is a package-global seam.
func withFakeCollect(t *testing.T, fake func(string, trace.Options) (*trace.Data, error)) {
	t.Helper()
	orig := traceCollect
	traceCollect = fake
	t.Cleanup(func() { traceCollect = orig })
}

func TestGetBeadTrace_NotFound(t *testing.T) {
	withFakeCollect(t, func(id string, _ trace.Options) (*trace.Data, error) {
		return nil, &trace.NotFoundError{ID: id}
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-missing/trace", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetBeadTrace_EmptyShape(t *testing.T) {
	withFakeCollect(t, func(id string, _ trace.Options) (*trace.Data, error) {
		return &trace.Data{
			Pipeline: []trace.PipelineStep{},
			Totals:   trace.Totals{},
			LogTail:  []trace.LogLine{},
		}, nil
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-nevrun/trace", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got trace.Data
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v, body=%s", err, rec.Body.String())
	}
	if got.Pipeline == nil {
		t.Error("pipeline is nil, want []")
	}
	if len(got.Pipeline) != 0 {
		t.Errorf("pipeline len = %d, want 0", len(got.Pipeline))
	}
	if got.ActiveAgent != nil {
		t.Errorf("active_agent = %+v, want nil", got.ActiveAgent)
	}
	if got.LogTail == nil {
		t.Error("log_tail is nil, want []")
	}
	if got.Totals.DurationMs != 0 || got.Totals.CostUSD != 0 {
		t.Errorf("totals non-zero = %+v", got.Totals)
	}
	// active_agent must serialize as JSON null, not {}.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw unmarshal: %v", err)
	}
	if string(raw["active_agent"]) != "null" {
		t.Errorf("active_agent raw = %s, want null", raw["active_agent"])
	}
}

func TestGetBeadTrace_Populated(t *testing.T) {
	withFakeCollect(t, func(id string, opts trace.Options) (*trace.Data, error) {
		if opts.Tail != trace.DefaultTailLines {
			t.Errorf("default tail = %d, want %d", opts.Tail, trace.DefaultTailLines)
		}
		return &trace.Data{
			Pipeline: []trace.PipelineStep{
				{Step: "plan", Status: "closed", DurationMs: 75000, CostUSD: 0.10, Reads: 3, Writes: 1},
				{Step: "implement", Status: "in_progress", DurationMs: 120000, CostUSD: 0.50, Reads: 10, Writes: 5, Retries: 1},
				{Step: "review", Status: "open"},
			},
			Totals: trace.Totals{DurationMs: 195000, CostUSD: 0.60, Reads: 13, Writes: 6, Retries: 1},
			ActiveAgent: &trace.ActiveAgent{
				Name:      "wizard-spi-abc",
				ElapsedMs: 45000,
				Model:     "claude-opus-4-7",
				Branch:    "feat/spi-abc",
			},
			LogTail: []trace.LogLine{
				{TS: "2026-04-24T16:49:12Z", Line: "starting implement phase"},
			},
		}, nil
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-abc/trace", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got trace.Data
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Pipeline) != 3 {
		t.Fatalf("pipeline len = %d, want 3", len(got.Pipeline))
	}
	if got.Pipeline[1].Step != "implement" || got.Pipeline[1].Status != "in_progress" {
		t.Errorf("pipeline[1] = %+v", got.Pipeline[1])
	}
	if got.Pipeline[1].Retries != 1 {
		t.Errorf("pipeline[1].Retries = %d, want 1", got.Pipeline[1].Retries)
	}
	if got.Totals.DurationMs != 195000 {
		t.Errorf("totals.duration_ms = %d, want 195000", got.Totals.DurationMs)
	}
	if got.ActiveAgent == nil || got.ActiveAgent.Name != "wizard-spi-abc" {
		t.Errorf("active_agent = %+v", got.ActiveAgent)
	}
	if got.ActiveAgent.ElapsedMs != 45000 {
		t.Errorf("active_agent.elapsed_ms = %d, want 45000", got.ActiveAgent.ElapsedMs)
	}
	if len(got.LogTail) != 1 || got.LogTail[0].Line != "starting implement phase" {
		t.Errorf("log_tail = %+v", got.LogTail)
	}
}

func TestGetBeadTrace_TailParam(t *testing.T) {
	tests := []struct {
		query    string
		wantTail int
	}{
		{"?tail=50", 50},
		{"?tail=0", 0},
		{"", trace.DefaultTailLines},
		{"?tail=abc", trace.DefaultTailLines},       // bad input clamps to default
		{"?tail=-10", trace.DefaultTailLines},       // negative clamps to default
		{"?tail=999999", trace.MaxTailLines},        // huge clamps to max
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			gotTail := -1
			withFakeCollect(t, func(_ string, opts trace.Options) (*trace.Data, error) {
				gotTail = opts.Tail
				return &trace.Data{Pipeline: []trace.PipelineStep{}, LogTail: []trace.LogLine{}}, nil
			})

			s := newTestServer(&fakeTrigger{})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-abc/trace"+tc.query, nil)
			rec := httptest.NewRecorder()
			s.handleBeadByID(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if gotTail != tc.wantTail {
				t.Errorf("Options.Tail = %d, want %d", gotTail, tc.wantTail)
			}
		})
	}
}

func TestGetBeadTrace_MethodNotAllowed(t *testing.T) {
	withFakeCollect(t, func(string, trace.Options) (*trace.Data, error) {
		t.Fatal("collector should not be called on non-GET")
		return nil, nil
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/trace", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
