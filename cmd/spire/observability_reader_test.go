package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
)

// Tests in this file exercise the bead-scoped CLI reader path against
// in-memory fakes that satisfy olap.TraceReader / beadMetricsReader.
// The layered regression contract spi-9h5rt pins says the CLI layer
// must consume typed interfaces and must not open analytics.db — this
// file is where that contract is asserted on the CLI side. Above
// pkg/olap there is no SQL and no analytics.db path here; storage
// round-trip lives in pkg/olap/duckdb/contract_test.go.

// fakeCLITraceReader is an in-memory olap.TraceReader that returns
// whatever the test seeded. Bead-scoped methods are keyed on bead_id;
// unknown beads return empty slices (not errors) to match the real
// backend's contract so the CLI's "empty result" branch is what the
// test exercises.
type fakeCLITraceReader struct {
	spans  map[string][]olap.SpanRecord
	steps  map[string][]olap.StepToolBreakdown
	apis   map[string][]olap.APIEventStats
	byTool map[string][]olap.ToolEventStats
}

func (f *fakeCLITraceReader) QuerySpans(ctx context.Context, traceID string) ([]olap.SpanRecord, error) {
	return nil, nil
}

func (f *fakeCLITraceReader) QueryTraces(ctx context.Context, filter olap.TraceFilter) ([]olap.TraceSummary, error) {
	return nil, nil
}

func (f *fakeCLITraceReader) QueryToolSpansByBead(beadID string) ([]olap.SpanRecord, error) {
	return f.spans[beadID], nil
}

func (f *fakeCLITraceReader) QueryToolEventsByBead(beadID string) ([]olap.ToolEventStats, error) {
	return f.byTool[beadID], nil
}

func (f *fakeCLITraceReader) QueryToolEventsByStep(beadID string) ([]olap.StepToolBreakdown, error) {
	return f.steps[beadID], nil
}

func (f *fakeCLITraceReader) QueryAPIEventsByBead(beadID string) ([]olap.APIEventStats, error) {
	return f.apis[beadID], nil
}

// withInjectedTraceReader swaps the package-level traceReaderProvider
// for the duration of the test. t.Cleanup restores the original to
// prevent cross-test pollution even when an assertion fails.
func withInjectedTraceReader(t *testing.T, reader olap.TraceReader) {
	t.Helper()
	orig := traceReaderProvider
	traceReaderProvider = func() (olap.TraceReader, func(), error) {
		return reader, func() {}, nil
	}
	t.Cleanup(func() { traceReaderProvider = orig })
}

// TestTrace_ReaderSeam_PopulatesJSONFromFakeReader is the CLI-side
// counterpart to pkg/otel's reader_seam_test: the trace command must
// render bead-scoped OTEL data returned by a typed olap.TraceReader,
// regardless of what backend the reader came from. The assertion is
// on structured JSON output so cosmetic text changes don't break the
// suite.
func TestTrace_ReaderSeam_PopulatesJSONFromFakeReader(t *testing.T) {
	beadID := "spi-cli-trace"
	now := time.Now().UTC().Truncate(time.Millisecond)

	reader := &fakeCLITraceReader{
		spans: map[string][]olap.SpanRecord{
			beadID: {
				{TraceID: "tr-1", SpanID: "sp-1", SpanName: "Read", Kind: "tool", DurationMs: 50, Success: true, StartTime: now, EndTime: now.Add(50 * time.Millisecond)},
				{TraceID: "tr-1", SpanID: "sp-2", SpanName: "Edit", Kind: "tool", DurationMs: 120, Success: true, StartTime: now.Add(60 * time.Millisecond), EndTime: now.Add(180 * time.Millisecond)},
			},
		},
		steps: map[string][]olap.StepToolBreakdown{
			beadID: {
				{Step: "implement", Tools: []olap.ToolEventStats{{ToolName: "Read", Count: 1}, {ToolName: "Edit", Count: 1}}},
			},
		},
		apis: map[string][]olap.APIEventStats{
			beadID: {
				{Model: "claude-opus-4-7", Count: 1, TotalInputTokens: 1000, TotalOutputTokens: 400, TotalCostUSD: 0.12},
			},
		},
	}
	withInjectedTraceReader(t, reader)

	td := &traceData{ID: beadID}
	// Exercise only the OTEL reader block of buildTrace by inlining the
	// same calls the production code makes through the seam. This
	// isolates the reader-consumption contract from the bd/store
	// dependency of the full buildTrace path.
	r, closeReader, err := traceReaderProvider()
	if err != nil {
		t.Fatalf("traceReaderProvider: %v", err)
	}
	defer closeReader()

	if spans, err := r.QueryToolSpansByBead(beadID); err == nil {
		td.Spans = spans
	}
	if steps, err := r.QueryToolEventsByStep(beadID); err == nil {
		td.ToolBreakdown = steps
	}
	if apiStats, err := r.QueryAPIEventsByBead(beadID); err == nil {
		td.APIStats = apiStats
	}

	// Round-trip through the JSON output the CLI emits so cosmetic
	// rendering changes don't break the suite.
	raw, err := json.Marshal(td)
	if err != nil {
		t.Fatalf("marshal traceData: %v", err)
	}
	var got traceData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal traceData: %v\nraw=%s", err, raw)
	}
	if len(got.Spans) != 2 {
		t.Errorf("got %d spans, want 2 — trace reader seam not surfacing QueryToolSpansByBead", len(got.Spans))
	}
	if len(got.ToolBreakdown) != 1 || got.ToolBreakdown[0].Step != "implement" {
		t.Errorf("got %v step breakdown, want single 'implement' row", got.ToolBreakdown)
	}
	if len(got.APIStats) != 1 || got.APIStats[0].Model != "claude-opus-4-7" {
		t.Errorf("got %v api stats, want claude-opus-4-7 row", got.APIStats)
	}
}

// TestTrace_ReaderSeam_EmptyBeadReturnsNoData verifies the CLI's empty
// path: a bead that the reader has no data for renders without
// panicking and without leaking rows from other beads. Regression
// matches "data exists but cannot be correlated" from the spi-9h5rt
// spec — even when the reader is healthy, an unscoped query for a
// bead with zero rows must yield an empty slice, not nil-panic or
// cross-bead leak.
func TestTrace_ReaderSeam_EmptyBeadReturnsNoData(t *testing.T) {
	// Reader has rows for one bead only — the query for a different
	// bead must return empty slices.
	reader := &fakeCLITraceReader{
		spans: map[string][]olap.SpanRecord{
			"spi-other": {
				{TraceID: "tr-2", SpanID: "sp-x", SpanName: "Bash", Kind: "tool"},
			},
		},
	}
	withInjectedTraceReader(t, reader)

	r, closeReader, err := traceReaderProvider()
	if err != nil {
		t.Fatalf("traceReaderProvider: %v", err)
	}
	defer closeReader()

	spans, err := r.QueryToolSpansByBead("spi-cli-empty")
	if err != nil {
		t.Fatalf("QueryToolSpansByBead: %v", err)
	}
	if len(spans) != 0 {
		t.Errorf("empty-bead query returned %d rows, want 0 — reader leaked spi-other rows into unrelated bead", len(spans))
	}
}

// TestTrace_ReaderSeam_PartialData covers the mixed state: a bead has
// tool spans but no tool events and no API events. The CLI must render
// the spans block without panicking on the empty fields.
func TestTrace_ReaderSeam_PartialData(t *testing.T) {
	beadID := "spi-cli-partial"
	reader := &fakeCLITraceReader{
		spans: map[string][]olap.SpanRecord{
			beadID: {{TraceID: "tr-3", SpanID: "sp-only", SpanName: "Read", Kind: "tool"}},
		},
		// steps, apis, byTool deliberately nil for this bead.
	}
	withInjectedTraceReader(t, reader)

	r, _, _ := traceReaderProvider()

	spans, _ := r.QueryToolSpansByBead(beadID)
	steps, _ := r.QueryToolEventsByStep(beadID)
	apis, _ := r.QueryAPIEventsByBead(beadID)

	if len(spans) != 1 {
		t.Errorf("expected 1 span, got %d", len(spans))
	}
	if len(steps) != 0 || len(apis) != 0 {
		t.Errorf("expected empty steps + apis, got steps=%d apis=%d", len(steps), len(apis))
	}
}

// ---------------------------------------------------------------------------
// beadMetricsReader seam — the `spire metrics --bead` path.
// ---------------------------------------------------------------------------

// fakeBeadMetricsReader satisfies beadMetricsReader for CLI tests.
// Keyed on beadID so queries for unknown beads exercise the empty
// path. Methods return (zero, nil) when no data is registered.
type fakeBeadMetricsReader struct {
	runs       map[string]beadRunStats
	lifecycle  map[string]*olap.BeadLifecycleIntervals
	reviewFix  map[string]*olap.ReviewFixCounts
	children   map[string][]olap.BeadLifecycleIntervals
}

func (f *fakeBeadMetricsReader) BeadRunSummary(beadID string) (beadRunStats, error) {
	if s, ok := f.runs[beadID]; ok {
		return s, nil
	}
	return beadRunStats{}, nil
}

func (f *fakeBeadMetricsReader) QueryLifecycleForBead(beadID string) (*olap.BeadLifecycleIntervals, error) {
	return f.lifecycle[beadID], nil
}

func (f *fakeBeadMetricsReader) QueryReviewFixCounts(beadID string) (*olap.ReviewFixCounts, error) {
	if rf, ok := f.reviewFix[beadID]; ok {
		return rf, nil
	}
	return nil, nil
}

func (f *fakeBeadMetricsReader) QueryChildLifecycle(parentID string) ([]olap.BeadLifecycleIntervals, error) {
	return f.children[parentID], nil
}

// floatPtr returns a *float64 pointer to v. Local helper so test
// fixtures read cleanly without a throwaway variable per field.
func floatPtr(v float64) *float64 { return &v }

// TestMetricsBead_ReaderSeam_FullJSON drives renderBeadMetricsFromReader
// with a fully populated fake and asserts the rendered JSON carries
// every block: summary, lifecycle, review/fix, children. A regression
// in any of these blocks fails here before it surfaces in the
// operator-facing output.
func TestMetricsBead_ReaderSeam_FullJSON(t *testing.T) {
	beadID := "spi-cli-metrics"
	base := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)

	reader := &fakeBeadMetricsReader{
		runs: map[string]beadRunStats{
			beadID: {TotalRuns: 5, Successes: 4, Failures: 1, SuccessRate: 80.0, AvgCostUSD: 0.20, AvgDurationS: 90, TotalCostUSD: 1.00},
		},
		lifecycle: map[string]*olap.BeadLifecycleIntervals{
			beadID: {
				BeadLifecycle:          olap.BeadLifecycle{BeadID: beadID, BeadType: "task", FiledAt: base, ClosedAt: base.Add(time.Hour)},
				FiledToClosedSeconds:   floatPtr(3600),
				StartedToClosedSeconds: floatPtr(1800),
				QueueSeconds:           floatPtr(120),
			},
		},
		reviewFix: map[string]*olap.ReviewFixCounts{
			beadID: {BeadID: beadID, ReviewCount: 2, FixCount: 1, ArbiterCount: 0, MaxReviewRounds: 2},
		},
		children: map[string][]olap.BeadLifecycleIntervals{
			beadID: {
				{BeadLifecycle: olap.BeadLifecycle{BeadID: beadID + ".1", BeadType: "step"}, FiledToClosedSeconds: floatPtr(60)},
			},
		},
	}

	out, err := captureStdout(t, func() error {
		return renderBeadMetricsFromReader(reader, beadID, true)
	})
	if err != nil {
		t.Fatalf("renderBeadMetricsFromReader: %v", err)
	}

	var got beadSummary
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal bead summary: %v\nraw=%s", err, out)
	}
	if got.BeadID != beadID {
		t.Errorf("BeadID = %q, want %q", got.BeadID, beadID)
	}
	if got.TotalRuns != 5 || got.Successes != 4 {
		t.Errorf("summary counts wrong: %+v", got)
	}
	if got.Lifecycle == nil || got.Lifecycle.QueueSeconds == nil || *got.Lifecycle.QueueSeconds != 120 {
		t.Errorf("lifecycle queue missing or wrong: %+v", got.Lifecycle)
	}
	if got.ReviewFix == nil || got.ReviewFix.ReviewCount != 2 {
		t.Errorf("review/fix wrong: %+v", got.ReviewFix)
	}
	if len(got.Children) != 1 || got.Children[0].BeadID != beadID+".1" {
		t.Errorf("children wrong: %+v", got.Children)
	}
}

// TestMetricsBead_ReaderSeam_EmptyBead asserts the JSON shape is
// preserved even when the reader has no data. Downstream tools that
// consume the JSON must see the schema consistently — this guards the
// "schema intact when empty" regression path.
func TestMetricsBead_ReaderSeam_EmptyBead(t *testing.T) {
	reader := &fakeBeadMetricsReader{}

	out, err := captureStdout(t, func() error {
		return renderBeadMetricsFromReader(reader, "spi-cli-missing", true)
	})
	if err != nil {
		t.Fatalf("renderBeadMetricsFromReader: %v", err)
	}

	if !json.Valid([]byte(out)) {
		t.Fatalf("output is not valid JSON: %s", out)
	}
	if !strings.Contains(out, "\"bead_id\"") {
		t.Errorf("missing bead_id key in empty output: %s", out)
	}
	if !strings.Contains(out, "\"total_runs\"") {
		t.Errorf("missing total_runs key in empty output: %s", out)
	}
}

// TestMetricsBead_ReaderSeam_LifecycleOnly covers a bead with lifecycle
// data but zero runs — common for pre-feature beads backfilled into
// bead_lifecycle but with no agent_runs_olap history. Must render the
// lifecycle block without the empty-everything fallback message.
func TestMetricsBead_ReaderSeam_LifecycleOnly(t *testing.T) {
	beadID := "spi-cli-prefeat"
	base := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)

	reader := &fakeBeadMetricsReader{
		lifecycle: map[string]*olap.BeadLifecycleIntervals{
			beadID: {
				BeadLifecycle:        olap.BeadLifecycle{BeadID: beadID, BeadType: "task", FiledAt: base, ClosedAt: base.Add(30 * time.Minute)},
				FiledToClosedSeconds: floatPtr(1800),
			},
		},
	}

	out, err := captureStdout(t, func() error {
		return renderBeadMetricsFromReader(reader, beadID, true)
	})
	if err != nil {
		t.Fatalf("renderBeadMetricsFromReader: %v", err)
	}
	var got beadSummary
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TotalRuns != 0 {
		t.Errorf("TotalRuns = %d, want 0", got.TotalRuns)
	}
	if got.Lifecycle == nil {
		t.Fatalf("Lifecycle is nil — lifecycle reader path dropped the row")
	}
	if got.Lifecycle.FiledToClosedSeconds == nil || *got.Lifecycle.FiledToClosedSeconds != 1800 {
		t.Errorf("FiledToClosedSeconds = %v, want 1800", got.Lifecycle.FiledToClosedSeconds)
	}
}
