package steward

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func newTestMetricsServer(t *testing.T, opts ...MetricsServerOption) (*MetricsServer, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewMetricsServer(9999, db, opts...), mock
}

func TestNewMetricsServer_NoOptions(t *testing.T) {
	m, _ := newTestMetricsServer(t)
	if m.cycleStats != nil {
		t.Error("expected nil cycleStats without option")
	}
	if m.mergeQueue != nil {
		t.Error("expected nil mergeQueue without option")
	}
}

func TestNewMetricsServer_WithOptions(t *testing.T) {
	cs := NewCycleStats()
	mq := NewMergeQueue()
	m, _ := newTestMetricsServer(t, WithCycleStats(cs), WithMergeQueue(mq))
	if m.cycleStats != cs {
		t.Error("expected cycleStats to be set")
	}
	if m.mergeQueue != mq {
		t.Error("expected mergeQueue to be set")
	}
}

func TestHandleLivez(t *testing.T) {
	m, _ := newTestMetricsServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	m.handleLivez(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("livez: got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "ok" {
		t.Errorf("livez body: got %q, want %q", body, "ok")
	}
}

func TestHandleReadyz_Healthy(t *testing.T) {
	m, mock := newTestMetricsServer(t)
	mock.ExpectPing()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	m.handleReadyz(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("readyz: got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "ok" {
		t.Errorf("readyz body: got %q, want %q", body, "ok")
	}
}

func TestHandleReadyz_Unhealthy(t *testing.T) {
	m, mock := newTestMetricsServer(t)
	mock.ExpectPing().WillReturnError(sqlmock.ErrCancelled)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	m.handleReadyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz: got %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.HasPrefix(body, "not ready:") {
		t.Errorf("readyz body: got %q, want prefix 'not ready:'", body)
	}
}

func TestHandleDetailedHealth_Basic(t *testing.T) {
	m, mock := newTestMetricsServer(t)
	mock.ExpectPing()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/detailed", nil)
	m.handleDetailedHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("detailed health: got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q, want application/json", ct)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status: got %v, want ok", resp["status"])
	}
	if resp["dolt"] != "connected" {
		t.Errorf("dolt: got %v, want connected", resp["dolt"])
	}
}

func TestHandleDetailedHealth_DoltDown(t *testing.T) {
	m, mock := newTestMetricsServer(t)
	mock.ExpectPing().WillReturnError(sqlmock.ErrCancelled)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/detailed", nil)
	m.handleDetailedHealth(rec, req)

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "degraded" {
		t.Errorf("status: got %v, want degraded", resp["status"])
	}
}

func TestHandleDetailedHealth_WithCycleStats(t *testing.T) {
	cs := NewCycleStats()
	cs.Record(CycleStatsSnapshot{
		LastCycleAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		CycleDuration:    1200 * time.Millisecond,
		ActiveAgents:     3,
		SchedulableWork:  5,
		SpawnedThisCycle: 2,
	})
	m, mock := newTestMetricsServer(t, WithCycleStats(cs))
	mock.ExpectPing()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/detailed", nil)
	m.handleDetailedHealth(rec, req)

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := resp["active_agents"].(float64); !ok || int(v) != 3 {
		t.Errorf("active_agents: got %v, want 3", resp["active_agents"])
	}
	if v, ok := resp["schedulable_work"].(float64); !ok || int(v) != 5 {
		t.Errorf("schedulable_work: got %v, want 5", resp["schedulable_work"])
	}
	if v, ok := resp["cycle_duration_ms"].(float64); !ok || int(v) != 1200 {
		t.Errorf("cycle_duration_ms: got %v, want 1200", resp["cycle_duration_ms"])
	}
	if v, ok := resp["spawned_last_cycle"].(float64); !ok || int(v) != 2 {
		t.Errorf("spawned_last_cycle: got %v, want 2", resp["spawned_last_cycle"])
	}
}

func TestHandleDetailedHealth_WithMergeQueue(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "spi-abc"})
	mq.Enqueue(MergeRequest{BeadID: "spi-def"})

	m, mock := newTestMetricsServer(t, WithMergeQueue(mq))
	mock.ExpectPing()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/detailed", nil)
	m.handleDetailedHealth(rec, req)

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := resp["merge_queue_depth"].(float64); !ok || int(v) != 2 {
		t.Errorf("merge_queue_depth: got %v, want 2", resp["merge_queue_depth"])
	}
	// No active merge, so merge_active should not be present.
	if _, ok := resp["merge_active"]; ok {
		t.Errorf("merge_active should not be present when no merge is active")
	}
}

func TestHandleMetrics_CycleStatsGauges(t *testing.T) {
	cs := NewCycleStats()
	cs.Record(CycleStatsSnapshot{
		ActiveAgents:    7,
		SchedulableWork: 12,
		CycleDuration:   500 * time.Millisecond,
		QueueDepth:      3,
	})

	m, mock := newTestMetricsServer(t, WithCycleStats(cs))
	// CollectMetrics runs 3 queries: aggregates, merge frequency, per-formula.
	mock.ExpectQuery("SELECT").WillReturnRows(
		sqlmock.NewRows([]string{"total", "successful", "failed", "active", "tokens", "cost"}).
			AddRow(0, 0, 0, 0, 0, 0.0),
	)
	mock.ExpectQuery("SELECT").WillReturnRows(
		sqlmock.NewRows([]string{"merges_7d", "merges_30d"}).AddRow(0, 0),
	)
	mock.ExpectQuery("SELECT").WillReturnRows(
		sqlmock.NewRows([]string{"formula_name", "formula_version", "run_count", "success_count", "avg_cost", "avg_duration"}),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.handleMetrics(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "spire_steward_active_agents 7") {
		t.Errorf("missing spire_steward_active_agents gauge in metrics output")
	}
	if !strings.Contains(body, "spire_steward_schedulable_work 12") {
		t.Errorf("missing spire_steward_schedulable_work gauge in metrics output")
	}
	if !strings.Contains(body, "spire_steward_merge_queue_depth 3") {
		t.Errorf("missing spire_steward_merge_queue_depth gauge in metrics output")
	}
	if !strings.Contains(body, "spire_steward_cycle_duration_seconds") {
		t.Errorf("missing spire_steward_cycle_duration_seconds gauge in metrics output")
	}
}
