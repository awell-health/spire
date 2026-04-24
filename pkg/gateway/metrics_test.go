package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/metrics/report"
)

// withMetricsStubs replaces the reader-factory + build seams for the
// life of the test. Returns a captured-opts pointer the caller can
// assert on.
type metricsStubs struct {
	lastOpts *report.Options
	buildErr error
}

func withMetricsStubs(t *testing.T, factoryErr error) *metricsStubs {
	t.Helper()
	stubs := &metricsStubs{}
	prevFactory := metricsReaderFactory
	prevBuild := metricsBuild

	metricsReaderFactory = func() (report.Reader, func(), error) {
		if factoryErr != nil {
			return nil, nil, factoryErr
		}
		return &nopReader{}, func() {}, nil
	}
	metricsBuild = func(ctx context.Context, r report.Reader, opts report.Options) (*report.MetricsResponse, error) {
		stubs.lastOpts = &opts
		if stubs.buildErr != nil {
			return nil, stubs.buildErr
		}
		return &report.MetricsResponse{
			Scope:       opts.Scope.String(),
			Window:      opts.Window.Range,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Aspirational: report.AspirationalBlock{},
		}, nil
	}
	t.Cleanup(func() {
		metricsReaderFactory = prevFactory
		metricsBuild = prevBuild
	})
	return stubs
}

// nopReader returns empty slices for every Reader method — sufficient
// for handler tests since we stub Build() to not call it.
type nopReader struct{}

func (nopReader) QueryThroughputWeekly(context.Context, report.Scope, time.Time, time.Time) ([]report.ThroughputWeek, error) {
	return nil, nil
}
func (nopReader) QueryHeroActiveAgents(context.Context, report.Scope, time.Time, time.Time) (int, int, error) {
	return 0, 0, nil
}
func (nopReader) QueryHeroCostByWeek(context.Context, report.Scope, time.Time, time.Time) (float64, float64, error) {
	return 0, 0, nil
}
func (nopReader) QueryHeroMTTR(context.Context, report.Scope, time.Time, time.Time) (float64, float64, error) {
	return 0, 0, nil
}
func (nopReader) QueryLifecycleByType(context.Context, report.Scope, time.Time, time.Time) ([]report.LifecycleByType, error) {
	return nil, nil
}
func (nopReader) QueryBugAttachmentWeekly(context.Context, report.Scope, time.Time, time.Time) ([]report.BugAttachmentWeek, error) {
	return nil, nil
}
func (nopReader) QueryFormulas(context.Context, report.Scope, time.Time, time.Time) ([]report.FormulaRow, error) {
	return nil, nil
}
func (nopReader) QueryCostDaily(context.Context, report.Scope, time.Time, time.Time) ([]report.CostDay, error) {
	return nil, nil
}
func (nopReader) QueryPhases(context.Context, report.Scope, time.Time, time.Time) ([]report.PhaseRow, error) {
	return nil, nil
}
func (nopReader) QueryFailures(context.Context, report.Scope, time.Time, time.Time) (report.FailuresBlock, error) {
	return report.FailuresBlock{}, nil
}
func (nopReader) QueryModels(context.Context, report.Scope, time.Time, time.Time) ([]report.ModelRow, error) {
	return nil, nil
}
func (nopReader) QueryTools(context.Context, report.Scope, time.Time, time.Time) ([]report.ToolRow, error) {
	return nil, nil
}

func TestHandleMetrics_RejectsNonGET(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withMetricsStubs(t, nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/metrics", nil)
		rec := httptest.NewRecorder()
		s.handleMetrics(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

func TestHandleMetrics_DefaultsScopeAndWindow(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	stubs := withMetricsStubs(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if stubs.lastOpts == nil {
		t.Fatal("Build was not called")
	}
	if stubs.lastOpts.Scope.String() != "all" {
		t.Errorf("scope = %q, want all", stubs.lastOpts.Scope.String())
	}
	if stubs.lastOpts.Window.Range != "7d" {
		t.Errorf("window = %q, want 7d", stubs.lastOpts.Window.Range)
	}
	if stubs.lastOpts.Aspirational {
		t.Error("aspirational should default to false")
	}
}

func TestHandleMetrics_ParsesQueryParams(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	stubs := withMetricsStubs(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics?scope=spi&window=30d&aspirational=true", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stubs.lastOpts.Scope.Prefix != "spi" {
		t.Errorf("scope prefix = %q, want spi", stubs.lastOpts.Scope.Prefix)
	}
	if stubs.lastOpts.Window.Range != "30d" {
		t.Errorf("window range = %q, want 30d", stubs.lastOpts.Window.Range)
	}
	if !stubs.lastOpts.Aspirational {
		t.Error("aspirational should be true")
	}
}

func TestHandleMetrics_BadWindowReturns400(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withMetricsStubs(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics?window=bogus", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid window range") {
		t.Errorf("body = %q, want mention of invalid window", rec.Body.String())
	}
}

func TestHandleMetrics_CustomWindowRequiresSinceUntil(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withMetricsStubs(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics?window=custom", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleMetrics_OLAPUnavailableReturns503(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withMetricsStubs(t, errors.New("no tower configured"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "OLAP unavailable") {
		t.Errorf("body = %q, want mention of OLAP unavailable", rec.Body.String())
	}
}

func TestHandleMetrics_BuildErrorReturns500(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	stubs := withMetricsStubs(t, nil)
	stubs.buildErr = errors.New("query failed")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHandleMetrics_ReturnsJSONWithCamelCaseFields(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withMetricsStubs(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Assert camelCase field names match TS (not snake_case).
	for _, want := range []string{"scope", "window", "generatedAt", "hero", "throughput", "lifecycle", "bugAttachment", "formulas", "costDaily", "phases", "failures", "models", "tools", "aspirational"} {
		if _, ok := resp[want]; !ok {
			t.Errorf("missing top-level field %q", want)
		}
	}
	// Reject snake_case field names that must NOT be present.
	for _, bad := range []string{"bug_attachment", "cost_daily", "deploy_frequency", "lead_time"} {
		if _, ok := resp[bad]; ok {
			t.Errorf("unexpected snake_case field %q present", bad)
		}
	}
}
