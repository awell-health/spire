package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/awell-health/spire/pkg/graph"
)

// withFakeGraph swaps graphCollect for the duration of a test and restores
// it in cleanup. Tests can't use t.Parallel because graphCollect is a
// package-global seam — same shape as withFakeCollect for trace.
func withFakeGraph(t *testing.T, fake func(string, graph.Options) (*graph.GraphResponse, error)) {
	t.Helper()
	orig := graphCollect
	graphCollect = fake
	t.Cleanup(func() { graphCollect = orig })
}

func TestGetBeadGraph_NotFound(t *testing.T) {
	withFakeGraph(t, func(id string, _ graph.Options) (*graph.GraphResponse, error) {
		return nil, &graph.NotFoundError{ID: id}
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-missing/graph", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetBeadGraph_MaxDepthExceeded(t *testing.T) {
	withFakeGraph(t, func(string, graph.Options) (*graph.GraphResponse, error) {
		return nil, graph.ErrMaxDepthExceeded
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-x/graph?max_depth=99", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetBeadGraph_InternalError(t *testing.T) {
	withFakeGraph(t, func(string, graph.Options) (*graph.GraphResponse, error) {
		return nil, errors.New("dolt down")
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-x/graph", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestGetBeadGraph_Populated(t *testing.T) {
	withFakeGraph(t, func(id string, opts graph.Options) (*graph.GraphResponse, error) {
		if opts.MaxDepth != graph.DefaultMaxDepth {
			t.Errorf("MaxDepth = %d, want %d", opts.MaxDepth, graph.DefaultMaxDepth)
		}
		return &graph.GraphResponse{
			RootID: id,
			Nodes: map[string]graph.Node{
				id: {
					ID:        id,
					Title:     "Epic",
					Status:    "in_progress",
					Type:      "epic",
					Priority:  1,
					Labels:    []string{"epic"},
					UpdatedAt: "2026-04-25T15:00:00Z",
				},
				"spi-task1": {
					ID:        "spi-task1",
					Title:     "Task 1",
					Status:    "closed",
					Type:      "task",
					Priority:  1,
					Parent:    id,
					Labels:    []string{},
					UpdatedAt: "2026-04-25T15:00:00Z",
					Depth:     1,
					Metrics: &graph.Metrics{
						DurationMs: 60000,
						CostUSD:    0.1,
						RunCount:   1,
					},
				},
			},
			Edges: []graph.Edge{
				{From: id, To: "spi-task1", Type: "parent"},
			},
			Totals: graph.Totals{
				DurationMs: 60000,
				CostUSD:    0.1,
				RunCount:   1,
				ByStatus:   map[string]int{"in_progress": 1, "closed": 1},
				ByType:     map[string]int{"epic": 1, "task": 1},
			},
			ActiveAgents: []graph.ActiveAgent{
				{BeadID: id, Name: "wizard-x", ElapsedMs: 5000, Model: "claude-opus-4-7", Branch: "feat/x"},
			},
		}, nil
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-epic/graph", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got graph.GraphResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v, body=%s", err, rec.Body.String())
	}
	if got.RootID != "spi-epic" {
		t.Errorf("RootID = %q", got.RootID)
	}
	if len(got.Nodes) != 2 {
		t.Errorf("Nodes = %d, want 2", len(got.Nodes))
	}
	if len(got.Edges) != 1 || got.Edges[0].Type != "parent" {
		t.Errorf("Edges = %+v", got.Edges)
	}
	if got.Totals.ByStatus["in_progress"] != 1 {
		t.Errorf("Totals.ByStatus = %v", got.Totals.ByStatus)
	}
	if len(got.ActiveAgents) != 1 || got.ActiveAgents[0].Model != "claude-opus-4-7" {
		t.Errorf("ActiveAgents = %+v", got.ActiveAgents)
	}
}

func TestGetBeadGraph_MaxDepthParam(t *testing.T) {
	tests := []struct {
		query    string
		wantDepth int
	}{
		{"?max_depth=5", 5},
		{"?max_depth=1", 1},
		{"", graph.DefaultMaxDepth},
		{"?max_depth=abc", graph.DefaultMaxDepth},  // bad input clamps to default
		{"?max_depth=0", graph.DefaultMaxDepth},    // zero clamps to default
		{"?max_depth=-3", graph.DefaultMaxDepth},   // negative clamps to default
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			got := -1
			withFakeGraph(t, func(_ string, opts graph.Options) (*graph.GraphResponse, error) {
				got = opts.MaxDepth
				return &graph.GraphResponse{
					RootID: "spi-x",
					Nodes:  map[string]graph.Node{"spi-x": {ID: "spi-x"}},
					Edges:  []graph.Edge{},
				}, nil
			})

			s := newTestServer(&fakeTrigger{})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-x/graph"+tc.query, nil)
			rec := httptest.NewRecorder()
			s.handleBeadByID(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
			if got != tc.wantDepth {
				t.Errorf("Options.MaxDepth = %d, want %d", got, tc.wantDepth)
			}
		})
	}
}

func TestGetBeadGraph_MethodNotAllowed(t *testing.T) {
	withFakeGraph(t, func(string, graph.Options) (*graph.GraphResponse, error) {
		t.Fatal("collector should not be called on non-GET")
		return nil, nil
	})

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-x/graph", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
