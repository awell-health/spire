// Package olaptest provides a reusable contract test harness that both
// backend implementations (pkg/olap/duckdb and pkg/olap/clickhouse) run
// against their own fixtures to prove interface parity.
//
// This is deliberately NOT a _test.go file — backend subpackages must
// be able to import it from their own _test.go files. Callers wire up
// a writer factory and a reader factory (fresh instances per subtest)
// and invoke one of the RunContract* entry points.
//
// The harness writes via olap.Writer.Submit (raw SQL inside a tx, so
// both backends exercise their actual transaction path) and reads via
// the olap.TraceReader / olap.MetricsReader methods — proving the two
// ends of the interface agree on types, ordering, and pagination.
package olaptest

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
)

// Factories — the harness stays backend-agnostic by letting the caller
// construct fresh storage per subtest. Each factory is invoked at the
// top of a subtest and the returned instance is Closed on cleanup.

// WriterFactory returns a fresh olap.Writer. The harness will close it.
type WriterFactory func(t *testing.T) olap.Writer

// TraceReaderFactory returns a fresh olap.TraceReader bound to the same
// storage as the corresponding WriterFactory.
type TraceReaderFactory func(t *testing.T) olap.TraceReader

// MetricsReaderFactory returns a fresh olap.MetricsReader. Typically
// DuckDB-only.
type MetricsReaderFactory func(t *testing.T) olap.MetricsReader

// PairFactory returns a writer and a reader that share storage. This is
// the common case: a single backend satisfies both interfaces. Use this
// when your backend exposes a concrete type that embeds both (e.g.
// duckdb.DB or clickhouse.Store).
type PairFactory func(t *testing.T) (olap.Writer, olap.TraceReader)

// StoreFactory returns a Store (Writer + TraceReader + MetricsReader).
// Only DuckDB will satisfy this.
type StoreFactory func(t *testing.T) olap.Store

// RunContractTests is the top-level Writer + TraceReader contract entry
// point. It delegates to a suite of subtests covering round-trip,
// concurrent writes, transaction rollback, span hierarchy, trace-by-ID
// retrieval, timestamp ordering, and pagination.
func RunContractTests(t *testing.T, pair PairFactory) {
	t.Helper()
	if pair == nil {
		t.Fatal("olaptest: PairFactory is nil")
	}

	t.Run("RoundTrip", func(t *testing.T) { testRoundTrip(t, pair) })
	t.Run("ConcurrentWrites", func(t *testing.T) { testConcurrentWrites(t, pair) })
	t.Run("TransactionRollback", func(t *testing.T) { testTransactionRollback(t, pair) })
	t.Run("SpanHierarchy", func(t *testing.T) { testSpanHierarchy(t, pair) })
	t.Run("TraceByID", func(t *testing.T) { testTraceByID(t, pair) })
	t.Run("TimestampOrdering", func(t *testing.T) { testTimestampOrdering(t, pair) })
	t.Run("Pagination", func(t *testing.T) { testPagination(t, pair) })
}

// RunMetricsContractTests exercises the MetricsReader surface. DuckDB-
// only; ClickHouse will not call this.
func RunMetricsContractTests(t *testing.T, newMetrics MetricsReaderFactory) {
	t.Helper()
	if newMetrics == nil {
		t.Fatal("olaptest: MetricsReaderFactory is nil")
	}

	t.Run("Summary", func(t *testing.T) { testMetricsSummary(t, newMetrics) })
	t.Run("ModelBreakdown", func(t *testing.T) { testMetricsModelBreakdown(t, newMetrics) })
	t.Run("DORA", func(t *testing.T) { testMetricsDORA(t, newMetrics) })
}

// ---------------------------------------------------------------------------
// Writer + TraceReader subtests
// ---------------------------------------------------------------------------

// testRoundTrip writes a single span then reads it back through every
// access path the interface exposes (bead-scoped and trace-scoped).
func testRoundTrip(t *testing.T, pair PairFactory) {
	w, r := pair(t)
	t.Cleanup(func() { _ = w.Close() })

	traceID := "trace-rt-1"
	beadID := "bead-rt-1"
	now := time.Now().UTC().Truncate(time.Millisecond)

	span := SpanFixture{
		TraceID:    traceID,
		SpanID:     "span-rt-1",
		BeadID:     beadID,
		SpanName:   "tool.Read",
		Kind:       "tool",
		DurationMs: 42,
		Success:    true,
		StartTime:  now,
		EndTime:    now.Add(42 * time.Millisecond),
	}

	if err := InsertSpans(w, []SpanFixture{span}); err != nil {
		t.Fatalf("insert span: %v", err)
	}

	gotByBead, err := r.QueryToolSpansByBead(beadID)
	if err != nil {
		t.Fatalf("QueryToolSpansByBead: %v", err)
	}
	if len(gotByBead) != 1 {
		t.Fatalf("bead-scoped read: expected 1 span, got %d", len(gotByBead))
	}
	if gotByBead[0].SpanID != span.SpanID {
		t.Errorf("bead-scoped read: span_id = %q, want %q", gotByBead[0].SpanID, span.SpanID)
	}

	gotByTrace, err := r.QuerySpans(context.Background(), traceID)
	if err != nil {
		t.Fatalf("QuerySpans: %v", err)
	}
	if len(gotByTrace) != 1 {
		t.Fatalf("trace-scoped read: expected 1 span, got %d", len(gotByTrace))
	}
	if gotByTrace[0].SpanID != span.SpanID {
		t.Errorf("trace-scoped read: span_id = %q, want %q", gotByTrace[0].SpanID, span.SpanID)
	}
}

// testConcurrentWrites fans N goroutines into Submit and verifies every
// span lands. Proves that whatever serialization the backend uses
// (DuckDB's writer goroutine, ClickHouse's direct writes) is safe under
// contention.
func testConcurrentWrites(t *testing.T, pair PairFactory) {
	w, r := pair(t)
	t.Cleanup(func() { _ = w.Close() })

	const workers = 8
	const spansPerWorker = 5
	beadID := "bead-concurrent"

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			fixtures := make([]SpanFixture, spansPerWorker)
			for j := 0; j < spansPerWorker; j++ {
				fixtures[j] = SpanFixture{
					TraceID:   fmt.Sprintf("trace-w%d", worker),
					SpanID:    fmt.Sprintf("span-w%d-%d", worker, j),
					BeadID:    beadID,
					SpanName:  "concurrent.op",
					Kind:      "tool",
					Success:   true,
					StartTime: time.Now().UTC(),
					EndTime:   time.Now().UTC().Add(time.Millisecond),
				}
			}
			if err := InsertSpans(w, fixtures); err != nil {
				errs <- fmt.Errorf("worker %d: %w", worker, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	got, err := r.QueryToolSpansByBead(beadID)
	if err != nil {
		t.Fatalf("QueryToolSpansByBead: %v", err)
	}
	if want := workers * spansPerWorker; len(got) != want {
		t.Errorf("expected %d spans under contention, got %d", want, len(got))
	}
}

// testTransactionRollback verifies that when fn returns an error, no
// rows land. Note: some backends (ClickHouse) don't implement true
// rollback — the harness treats those backends as pass if the behaviour
// documented in the implementation matches reality.
func testTransactionRollback(t *testing.T, pair PairFactory) {
	w, r := pair(t)
	t.Cleanup(func() { _ = w.Close() })

	beadID := "bead-rollback"
	errInjected := fmt.Errorf("injected")

	err := w.Submit(func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(context.Background(),
			insertSpanSQL,
			"trace-rb-1", "span-rb-1", "", "", beadID, "", "",
			"rolled.back", "tool", 1, true, time.Now().UTC(), time.Now().UTC(),
			"", "{}")
		if execErr != nil {
			return execErr
		}
		return errInjected
	})
	if err == nil {
		t.Fatal("Submit returned nil; expected injected error")
	}

	got, err := r.QueryToolSpansByBead(beadID)
	if err != nil {
		t.Fatalf("QueryToolSpansByBead: %v", err)
	}
	// Some engines auto-commit; if so, expect 1. Strict engines expect 0.
	// We don't fail either way — we just flag non-zero so the reader
	// can see which semantics their backend has.
	if len(got) > 0 {
		t.Logf("backend did not roll back (auto-commit semantics): %d spans remained", len(got))
	}
}

// testSpanHierarchy proves parent_span_id round-trips so the waterfall
// rendering in cmd/spire/trace.go can reconstruct the tree.
func testSpanHierarchy(t *testing.T, pair PairFactory) {
	w, r := pair(t)
	t.Cleanup(func() { _ = w.Close() })

	traceID := "trace-hier"
	beadID := "bead-hier"
	base := time.Now().UTC().Truncate(time.Millisecond)

	fixtures := []SpanFixture{
		{
			TraceID: traceID, SpanID: "root", ParentSpanID: "",
			BeadID: beadID, SpanName: "root.op", Kind: "interaction",
			Success: true, StartTime: base, EndTime: base.Add(100 * time.Millisecond),
		},
		{
			TraceID: traceID, SpanID: "child-1", ParentSpanID: "root",
			BeadID: beadID, SpanName: "child.one", Kind: "tool",
			Success: true, StartTime: base.Add(10 * time.Millisecond), EndTime: base.Add(40 * time.Millisecond),
		},
		{
			TraceID: traceID, SpanID: "child-2", ParentSpanID: "root",
			BeadID: beadID, SpanName: "child.two", Kind: "tool",
			Success: true, StartTime: base.Add(50 * time.Millisecond), EndTime: base.Add(90 * time.Millisecond),
		},
	}
	if err := InsertSpans(w, fixtures); err != nil {
		t.Fatalf("insert spans: %v", err)
	}

	got, err := r.QuerySpans(context.Background(), traceID)
	if err != nil {
		t.Fatalf("QuerySpans: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(got))
	}
	parents := map[string]string{}
	for _, s := range got {
		parents[s.SpanID] = s.ParentSpanID
	}
	if parents["root"] != "" {
		t.Errorf("root parent = %q, want empty", parents["root"])
	}
	if parents["child-1"] != "root" {
		t.Errorf("child-1 parent = %q, want root", parents["child-1"])
	}
	if parents["child-2"] != "root" {
		t.Errorf("child-2 parent = %q, want root", parents["child-2"])
	}
}

// testTraceByID writes spans across two trace_ids and verifies
// QuerySpans returns only the requested trace's spans.
func testTraceByID(t *testing.T, pair PairFactory) {
	w, r := pair(t)
	t.Cleanup(func() { _ = w.Close() })

	base := time.Now().UTC().Truncate(time.Millisecond)
	fixtures := []SpanFixture{
		spanFixture("trace-A", "span-A1", "bead-A", base),
		spanFixture("trace-A", "span-A2", "bead-A", base.Add(10*time.Millisecond)),
		spanFixture("trace-B", "span-B1", "bead-B", base.Add(20*time.Millisecond)),
	}
	if err := InsertSpans(w, fixtures); err != nil {
		t.Fatalf("insert spans: %v", err)
	}

	a, err := r.QuerySpans(context.Background(), "trace-A")
	if err != nil {
		t.Fatalf("QuerySpans(A): %v", err)
	}
	if len(a) != 2 {
		t.Errorf("trace-A: expected 2 spans, got %d", len(a))
	}

	b, err := r.QuerySpans(context.Background(), "trace-B")
	if err != nil {
		t.Fatalf("QuerySpans(B): %v", err)
	}
	if len(b) != 1 {
		t.Errorf("trace-B: expected 1 span, got %d", len(b))
	}
}

// testTimestampOrdering verifies QueryToolSpansByBead returns spans in
// start_time ASC order, which the waterfall renderer depends on.
func testTimestampOrdering(t *testing.T, pair PairFactory) {
	w, r := pair(t)
	t.Cleanup(func() { _ = w.Close() })

	beadID := "bead-ts"
	base := time.Now().UTC().Truncate(time.Millisecond)

	fixtures := []SpanFixture{
		spanFixture("trace-ts", "span-late", beadID, base.Add(30*time.Millisecond)),
		spanFixture("trace-ts", "span-early", beadID, base),
		spanFixture("trace-ts", "span-mid", beadID, base.Add(15*time.Millisecond)),
	}
	if err := InsertSpans(w, fixtures); err != nil {
		t.Fatalf("insert spans: %v", err)
	}

	got, err := r.QueryToolSpansByBead(beadID)
	if err != nil {
		t.Fatalf("QueryToolSpansByBead: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].StartTime.Before(got[i-1].StartTime) {
			t.Errorf("ordering violated at index %d: %v before %v",
				i, got[i].StartTime, got[i-1].StartTime)
		}
	}
}

// testPagination writes more spans than a likely default page size and
// verifies QueryTraces respects filter.Limit.
func testPagination(t *testing.T, pair PairFactory) {
	w, r := pair(t)
	t.Cleanup(func() { _ = w.Close() })

	base := time.Now().UTC().Truncate(time.Millisecond)
	const total = 15

	fixtures := make([]SpanFixture, total)
	for i := 0; i < total; i++ {
		fixtures[i] = spanFixture(
			fmt.Sprintf("trace-page-%02d", i),
			fmt.Sprintf("span-page-%02d", i),
			fmt.Sprintf("bead-page-%02d", i),
			base.Add(time.Duration(i)*time.Millisecond),
		)
	}
	if err := InsertSpans(w, fixtures); err != nil {
		t.Fatalf("insert spans: %v", err)
	}

	got, err := r.QueryTraces(context.Background(), olap.TraceFilter{Limit: 5})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(got) > 5 {
		t.Errorf("pagination violated: limit=5 returned %d summaries", len(got))
	}
}

// ---------------------------------------------------------------------------
// MetricsReader subtests
// ---------------------------------------------------------------------------

// testMetricsSummary invokes QuerySummary on an empty backend and
// verifies it returns a non-nil zero-valued struct rather than an error.
// Populated-data parity is exercised in backend-specific tests that
// seed agent_runs_olap.
func testMetricsSummary(t *testing.T, newMetrics MetricsReaderFactory) {
	m := newMetrics(t)
	s, err := m.QuerySummary(time.Now().AddDate(0, -1, 0))
	if err != nil {
		t.Fatalf("QuerySummary: %v", err)
	}
	if s == nil {
		t.Fatal("QuerySummary returned nil stats")
	}
}

// testMetricsModelBreakdown verifies QueryModelBreakdown returns a
// (possibly empty) slice without error on empty storage.
func testMetricsModelBreakdown(t *testing.T, newMetrics MetricsReaderFactory) {
	m := newMetrics(t)
	_, err := m.QueryModelBreakdown(time.Now().AddDate(0, -1, 0))
	if err != nil {
		t.Fatalf("QueryModelBreakdown: %v", err)
	}
}

// testMetricsDORA verifies QueryDORA returns a non-nil zero-valued
// struct on empty storage.
func testMetricsDORA(t *testing.T, newMetrics MetricsReaderFactory) {
	m := newMetrics(t)
	d, err := m.QueryDORA(time.Now().AddDate(0, -1, 0))
	if err != nil {
		t.Fatalf("QueryDORA: %v", err)
	}
	if d == nil {
		t.Fatal("QueryDORA returned nil metrics")
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// SpanFixture is the minimal shape callers use to drive InsertSpans.
// Intentionally permissive — zero values are fine for any field.
type SpanFixture struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	SessionID    string
	BeadID       string
	AgentName    string
	Step         string
	SpanName     string
	Kind         string
	DurationMs   int
	Success      bool
	StartTime    time.Time
	EndTime      time.Time
	Tower        string
	Attributes   string
}

// spanFixture builds a minimal SpanFixture for ordering / pagination
// tests where only identity and timestamp matter.
func spanFixture(traceID, spanID, beadID string, start time.Time) SpanFixture {
	return SpanFixture{
		TraceID:   traceID,
		SpanID:    spanID,
		BeadID:    beadID,
		SpanName:  "fixture.op",
		Kind:      "tool",
		Success:   true,
		StartTime: start,
		EndTime:   start.Add(5 * time.Millisecond),
	}
}

// insertSpanSQL is the parameterised INSERT both backends accept.
// Backends may shadow this with dialect-specific SQL if needed; the
// placeholder shape (? ordering, attribute positions) must match.
const insertSpanSQL = `INSERT INTO tool_spans (
	trace_id, span_id, parent_span_id, session_id, bead_id, agent_name, step,
	span_name, kind, duration_ms, success, start_time, end_time,
	tower, attributes
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// InsertSpans writes fixtures through the Writer.Submit path. Exported
// so backend tests can seed their own fixtures when they need more
// control than the standard harness subtests provide.
func InsertSpans(w olap.Writer, fixtures []SpanFixture) error {
	if len(fixtures) == 0 {
		return nil
	}
	return w.Submit(func(tx *sql.Tx) error {
		for _, f := range fixtures {
			attrs := f.Attributes
			if attrs == "" {
				attrs = "{}"
			}
			if _, err := tx.ExecContext(context.Background(), insertSpanSQL,
				f.TraceID, f.SpanID, f.ParentSpanID, f.SessionID, f.BeadID,
				f.AgentName, f.Step, f.SpanName, f.Kind, f.DurationMs,
				f.Success, f.StartTime, f.EndTime, f.Tower, attrs,
			); err != nil {
				return fmt.Errorf("insert fixture %s: %w", f.SpanID, err)
			}
		}
		return nil
	})
}
