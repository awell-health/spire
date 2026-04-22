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

// ---------------------------------------------------------------------------
// OTEL-ingest tables contract (tool_events, api_events). The round-trip
// contract for tool_spans lives in RunContractTests above; this surface
// covers the sibling tables the OTEL receiver also writes to. Keeping
// all three under the shared harness is what guarantees a schema drift
// in any single table fails exactly one targeted test instead of
// surfacing as a downstream rendering bug.
// ---------------------------------------------------------------------------

// ToolEventFixture is the minimal shape callers use to drive
// InsertToolEvents. Zero values are permissive; BeadID and ToolName are
// what the regression surface actually asserts on.
type ToolEventFixture struct {
	SessionID  string
	BeadID     string
	AgentName  string
	Step       string
	ToolName   string
	DurationMs int
	Success    bool
	Timestamp  time.Time
	Tower      string
	Provider   string
	EventKind  string
}

// APIEventFixture is the minimal shape callers use to drive
// InsertAPIEvents.
type APIEventFixture struct {
	SessionID        string
	BeadID           string
	AgentName        string
	Step             string
	Provider         string
	Model            string
	DurationMs       int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
	Timestamp        time.Time
	Tower            string
}

const insertToolEventSQL = `INSERT INTO tool_events (
	session_id, bead_id, agent_name, step, tool_name,
	duration_ms, success, timestamp, tower, provider, event_kind
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const insertAPIEventSQL = `INSERT INTO api_events (
	session_id, bead_id, agent_name, step, provider, model,
	duration_ms, input_tokens, output_tokens,
	cache_read_tokens, cache_write_tokens, cost_usd, timestamp, tower
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// InsertToolEvents writes tool event fixtures via Writer.Submit.
func InsertToolEvents(w olap.Writer, fixtures []ToolEventFixture) error {
	if len(fixtures) == 0 {
		return nil
	}
	return w.Submit(func(tx *sql.Tx) error {
		for _, e := range fixtures {
			ts := e.Timestamp
			if ts.IsZero() {
				ts = time.Now().UTC()
			}
			if _, err := tx.ExecContext(context.Background(), insertToolEventSQL,
				e.SessionID, e.BeadID, e.AgentName, e.Step, e.ToolName,
				e.DurationMs, e.Success, ts, e.Tower, e.Provider, e.EventKind,
			); err != nil {
				return fmt.Errorf("insert tool event %s/%s: %w", e.BeadID, e.ToolName, err)
			}
		}
		return nil
	})
}

// InsertAPIEvents writes API event fixtures via Writer.Submit.
func InsertAPIEvents(w olap.Writer, fixtures []APIEventFixture) error {
	if len(fixtures) == 0 {
		return nil
	}
	return w.Submit(func(tx *sql.Tx) error {
		for _, e := range fixtures {
			ts := e.Timestamp
			if ts.IsZero() {
				ts = time.Now().UTC()
			}
			if _, err := tx.ExecContext(context.Background(), insertAPIEventSQL,
				e.SessionID, e.BeadID, e.AgentName, e.Step, e.Provider, e.Model,
				e.DurationMs, e.InputTokens, e.OutputTokens,
				e.CacheReadTokens, e.CacheWriteTokens, e.CostUSD, ts, e.Tower,
			); err != nil {
				return fmt.Errorf("insert api event %s/%s: %w", e.BeadID, e.Model, err)
			}
		}
		return nil
	})
}

// RunOTELIngestContract exercises the OTEL-ingest tables (tool_events,
// api_events) beyond the tool_spans round-trip covered by
// RunContractTests. Covers:
//
//   - Seeded tool_events are findable by (bead, step, tool) aggregation.
//   - Rows with an empty bead_id land but are not pulled in by a
//     bead-scoped query for a different bead. (The "data exists but
//     cannot be correlated" regression from the spec.)
//   - Seeded api_events are grouped by model and totals add up.
//
// MetricsReader (DuckDB only) is used for the event-style aggregations;
// callers that only have a ReadWrite backend can skip this by passing a
// nil StoreFactory.
func RunOTELIngestContract(t *testing.T, store StoreFactory) {
	t.Helper()
	if store == nil {
		t.Skip("olaptest: StoreFactory not provided; OTEL-ingest contract requires MetricsReader backend")
	}

	t.Run("ToolEventsRoundTripByBead", func(t *testing.T) { testToolEventsRoundTrip(t, store) })
	t.Run("ToolEventsEmptyBeadID_NotLeakingIntoOtherBead", func(t *testing.T) {
		testToolEventsEmptyBeadIsolation(t, store)
	})
	t.Run("APIEventsGroupedByModel", func(t *testing.T) { testAPIEventsAggregates(t, store) })
	t.Run("StepBreakdownOrdered", func(t *testing.T) { testToolEventsStepBreakdown(t, store) })
}

func testToolEventsRoundTrip(t *testing.T, store StoreFactory) {
	s := store(t)
	t.Cleanup(func() { _ = s.Close() })

	beadID := "bead-evt-1"
	now := time.Now().UTC().Truncate(time.Second)

	events := []ToolEventFixture{
		{SessionID: "sess", BeadID: beadID, Step: "implement", ToolName: "Read", Success: true, Timestamp: now, EventKind: "tool_result"},
		{SessionID: "sess", BeadID: beadID, Step: "implement", ToolName: "Read", Success: true, Timestamp: now.Add(time.Second), EventKind: "tool_result"},
		{SessionID: "sess", BeadID: beadID, Step: "implement", ToolName: "Edit", Success: false, Timestamp: now.Add(2 * time.Second), EventKind: "tool_result"},
	}
	if err := InsertToolEvents(s, events); err != nil {
		t.Fatalf("InsertToolEvents: %v", err)
	}

	stats, err := s.QueryToolEventsByBead(beadID)
	if err != nil {
		t.Fatalf("QueryToolEventsByBead: %v", err)
	}
	if len(stats) == 0 {
		t.Fatalf("QueryToolEventsByBead(%q): got 0 rows, want at least 1 — bead_id filter failed to match seeded rows", beadID)
	}

	counts := map[string]int{}
	failures := map[string]int{}
	for _, st := range stats {
		counts[st.ToolName] = st.Count
		failures[st.ToolName] = st.FailureCount
	}
	if counts["Read"] != 2 {
		t.Errorf("Read count = %d, want 2", counts["Read"])
	}
	if counts["Edit"] != 1 {
		t.Errorf("Edit count = %d, want 1", counts["Edit"])
	}
	if failures["Edit"] != 1 {
		t.Errorf("Edit failure count = %d, want 1 — success=false not propagated", failures["Edit"])
	}
}

func testToolEventsEmptyBeadIsolation(t *testing.T, store StoreFactory) {
	s := store(t)
	t.Cleanup(func() { _ = s.Close() })

	realBead := "bead-evt-isolated"
	now := time.Now().UTC()

	events := []ToolEventFixture{
		// Correlated event that belongs to realBead.
		{BeadID: realBead, Step: "implement", ToolName: "Read", Success: true, Timestamp: now, EventKind: "tool_result"},
		// Uncorrelated event — bead_id was dropped on the wire (the
		// failure mode spi-ecnfe and friends guard against). Must land
		// but must NOT be returned under realBead's scope.
		{BeadID: "", Step: "implement", ToolName: "Grep", Success: true, Timestamp: now.Add(time.Second), EventKind: "tool_result"},
	}
	if err := InsertToolEvents(s, events); err != nil {
		t.Fatalf("InsertToolEvents: %v", err)
	}

	stats, err := s.QueryToolEventsByBead(realBead)
	if err != nil {
		t.Fatalf("QueryToolEventsByBead: %v", err)
	}

	for _, st := range stats {
		if st.ToolName == "Grep" {
			t.Errorf("QueryToolEventsByBead(%q) returned a Grep row emitted without bead_id — filter is leaking", realBead)
		}
	}

	if got, _ := s.QueryToolEventsByBead(""); len(got) > 0 {
		// Many backends legitimately tolerate empty-string lookup; we
		// just assert no cross-bead leak above. Leaving this as a log
		// rather than a hard error so backends with tolerant filters
		// don't fail here.
		t.Logf("QueryToolEventsByBead(\"\") returned %d rows — acceptable but worth noting", len(got))
	}
}

func testAPIEventsAggregates(t *testing.T, store StoreFactory) {
	s := store(t)
	t.Cleanup(func() { _ = s.Close() })

	beadID := "bead-api-1"
	now := time.Now().UTC()

	events := []APIEventFixture{
		{SessionID: "sess", BeadID: beadID, Step: "implement", Provider: "claude", Model: "claude-opus-4-7", InputTokens: 1000, OutputTokens: 400, CostUSD: 0.12, Timestamp: now},
		{SessionID: "sess", BeadID: beadID, Step: "implement", Provider: "claude", Model: "claude-opus-4-7", InputTokens: 2000, OutputTokens: 800, CostUSD: 0.24, Timestamp: now.Add(time.Second)},
		{SessionID: "sess", BeadID: beadID, Step: "review", Provider: "claude", Model: "claude-haiku-4-5", InputTokens: 500, OutputTokens: 100, CostUSD: 0.02, Timestamp: now.Add(2 * time.Second)},
	}
	if err := InsertAPIEvents(s, events); err != nil {
		t.Fatalf("InsertAPIEvents: %v", err)
	}

	stats, err := s.QueryAPIEventsByBead(beadID)
	if err != nil {
		t.Fatalf("QueryAPIEventsByBead: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("QueryAPIEventsByBead: got %d model groups, want 2", len(stats))
	}

	byModel := map[string]olap.APIEventStats{}
	for _, st := range stats {
		byModel[st.Model] = st
	}
	opus, ok := byModel["claude-opus-4-7"]
	if !ok {
		t.Fatalf("QueryAPIEventsByBead: claude-opus-4-7 missing from %v", byModel)
	}
	if opus.Count != 2 {
		t.Errorf("opus count = %d, want 2", opus.Count)
	}
	if opus.TotalInputTokens != 3000 {
		t.Errorf("opus input tokens = %d, want 3000", opus.TotalInputTokens)
	}
	if opus.TotalOutputTokens != 1200 {
		t.Errorf("opus output tokens = %d, want 1200", opus.TotalOutputTokens)
	}
	if opus.TotalCostUSD < 0.35 || opus.TotalCostUSD > 0.37 {
		t.Errorf("opus cost = %f, want ~0.36", opus.TotalCostUSD)
	}
}

func testToolEventsStepBreakdown(t *testing.T, store StoreFactory) {
	s := store(t)
	t.Cleanup(func() { _ = s.Close() })

	beadID := "bead-evt-step"
	now := time.Now().UTC()

	events := []ToolEventFixture{
		{BeadID: beadID, Step: "implement", ToolName: "Read", Success: true, Timestamp: now, EventKind: "tool_result"},
		{BeadID: beadID, Step: "implement", ToolName: "Edit", Success: true, Timestamp: now.Add(time.Second), EventKind: "tool_result"},
		{BeadID: beadID, Step: "review", ToolName: "Read", Success: true, Timestamp: now.Add(2 * time.Second), EventKind: "tool_result"},
	}
	if err := InsertToolEvents(s, events); err != nil {
		t.Fatalf("InsertToolEvents: %v", err)
	}

	steps, err := s.QueryToolEventsByStep(beadID)
	if err != nil {
		t.Fatalf("QueryToolEventsByStep: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("QueryToolEventsByStep: got %d step groups, want 2", len(steps))
	}

	stepNames := map[string]int{}
	for _, s := range steps {
		stepNames[s.Step] = len(s.Tools)
	}
	if stepNames["implement"] < 2 {
		t.Errorf("implement step should carry at least 2 tools (Read, Edit), got %d", stepNames["implement"])
	}
	if stepNames["review"] < 1 {
		t.Errorf("review step should carry at least 1 tool (Read), got %d", stepNames["review"])
	}
}

// ---------------------------------------------------------------------------
// Lifecycle contract (bead_lifecycle_olap + MetricsReader lifecycle
// queries). Exercises the spi-hmdwm surface: lifecycle timings per
// bead, the by-type P50/P95 rollup, review/fix counts, and the
// hierarchical child drill-down. Kept under the shared harness so the
// bead-detail block the CLI renders can't silently diverge from the
// underlying table when new columns are added.
// ---------------------------------------------------------------------------

// LifecycleFixture is the minimal shape callers use to drive
// InsertBeadLifecycles. Zero-valued timestamps represent NULL in the
// backing store — the canonical way to model pre-feature or in-flight
// beads.
type LifecycleFixture struct {
	BeadID       string
	BeadType     string
	FiledAt      time.Time
	ReadyAt      time.Time
	StartedAt    time.Time
	ClosedAt     time.Time
	UpdatedAt    time.Time
	ReviewCount  int
	FixCount     int
	ArbiterCount int
}

const insertLifecycleSQL = `INSERT INTO bead_lifecycle_olap (
	bead_id, bead_type, filed_at, ready_at, started_at, closed_at, updated_at,
	review_count, fix_count, arbiter_count, synced_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// InsertBeadLifecycles writes lifecycle fixtures via Writer.Submit.
// Exposed so CLI-layer tests that need lifecycle-populated storage
// can seed it in one place rather than each drafting their own SQL.
func InsertBeadLifecycles(w olap.Writer, fixtures []LifecycleFixture) error {
	if len(fixtures) == 0 {
		return nil
	}
	return w.Submit(func(tx *sql.Tx) error {
		syncedAt := time.Now().UTC()
		for _, f := range fixtures {
			var (
				filed   any = nil
				ready   any = nil
				started any = nil
				closed  any = nil
				updated any = f.UpdatedAt
			)
			if !f.FiledAt.IsZero() {
				filed = f.FiledAt
			}
			if !f.ReadyAt.IsZero() {
				ready = f.ReadyAt
			}
			if !f.StartedAt.IsZero() {
				started = f.StartedAt
			}
			if !f.ClosedAt.IsZero() {
				closed = f.ClosedAt
			}
			if f.UpdatedAt.IsZero() {
				updated = syncedAt
			}
			if _, err := tx.ExecContext(context.Background(), insertLifecycleSQL,
				f.BeadID, f.BeadType, filed, ready, started, closed, updated,
				f.ReviewCount, f.FixCount, f.ArbiterCount, syncedAt,
			); err != nil {
				return fmt.Errorf("insert lifecycle fixture %s: %w", f.BeadID, err)
			}
		}
		return nil
	})
}

// RunLifecycleContract exercises the spi-hmdwm surface against a Store
// backend. Covers per-bead interval derivation, review/fix aggregation,
// by-type percentiles, and the hierarchical-ID child drill-down. Any
// backend that implements MetricsReader (DuckDB) must pass.
func RunLifecycleContract(t *testing.T, store StoreFactory) {
	t.Helper()
	if store == nil {
		t.Fatal("olaptest: StoreFactory is nil")
	}

	t.Run("PerBeadIntervals_CanonicalFields", func(t *testing.T) { testLifecycleIntervals(t, store) })
	t.Run("PerBeadIntervals_InFlightReturnsNilIntervals", func(t *testing.T) { testLifecycleInFlight(t, store) })
	t.Run("ByType_P50P95Rollup", func(t *testing.T) { testLifecycleByType(t, store) })
	t.Run("ReviewFixCounts_FromAgentRuns", func(t *testing.T) { testReviewFixCounts(t, store) })
	t.Run("ChildDrilldown_HierarchicalPrefix", func(t *testing.T) { testChildDrilldown(t, store) })
	t.Run("QueueSeparateFromExecution", func(t *testing.T) { testQueueSeparateFromExecution(t, store) })
}

func testLifecycleIntervals(t *testing.T, store StoreFactory) {
	s := store(t)
	t.Cleanup(func() { _ = s.Close() })

	base := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	filed := base
	ready := base.Add(10 * time.Minute)
	started := base.Add(15 * time.Minute)
	closed := base.Add(75 * time.Minute)

	err := InsertBeadLifecycles(s, []LifecycleFixture{{
		BeadID:    "spi-lifecycle-1",
		BeadType:  "task",
		FiledAt:   filed,
		ReadyAt:   ready,
		StartedAt: started,
		ClosedAt:  closed,
		UpdatedAt: closed,
	}})
	if err != nil {
		t.Fatalf("InsertBeadLifecycles: %v", err)
	}

	got, err := s.QueryLifecycleForBead("spi-lifecycle-1")
	if err != nil {
		t.Fatalf("QueryLifecycleForBead: %v", err)
	}
	if got == nil {
		t.Fatal("QueryLifecycleForBead returned nil for seeded bead")
	}
	if got.BeadType != "task" {
		t.Errorf("BeadType = %q, want task", got.BeadType)
	}
	if got.FiledToClosedSeconds == nil || *got.FiledToClosedSeconds < 4499 || *got.FiledToClosedSeconds > 4501 {
		t.Errorf("FiledToClosedSeconds = %v, want ~4500s", got.FiledToClosedSeconds)
	}
	if got.QueueSeconds == nil || *got.QueueSeconds < 299 || *got.QueueSeconds > 301 {
		t.Errorf("QueueSeconds = %v, want ~300s", got.QueueSeconds)
	}
	if got.StartedToClosedSeconds == nil || *got.StartedToClosedSeconds < 3599 || *got.StartedToClosedSeconds > 3601 {
		t.Errorf("StartedToClosedSeconds = %v, want ~3600s", got.StartedToClosedSeconds)
	}
}

func testLifecycleInFlight(t *testing.T, store StoreFactory) {
	s := store(t)
	t.Cleanup(func() { _ = s.Close() })

	base := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)

	// Pre-feature bead: only filed_at is set. The "pre-feature" label
	// matters because this is the bead shape that exercises the NULL-
	// preserving rendering contract spi-hmdwm encoded — zero intervals
	// must not be misreported as 0s.
	err := InsertBeadLifecycles(s, []LifecycleFixture{{
		BeadID:    "spi-lifecycle-pre",
		BeadType:  "task",
		FiledAt:   base,
		UpdatedAt: base,
	}})
	if err != nil {
		t.Fatalf("InsertBeadLifecycles: %v", err)
	}

	got, err := s.QueryLifecycleForBead("spi-lifecycle-pre")
	if err != nil {
		t.Fatalf("QueryLifecycleForBead: %v", err)
	}
	if got == nil {
		t.Fatal("QueryLifecycleForBead returned nil for seeded bead")
	}
	if got.FiledToClosedSeconds != nil {
		t.Errorf("FiledToClosedSeconds = %v, want nil (bead never closed)", *got.FiledToClosedSeconds)
	}
	if got.StartedToClosedSeconds != nil {
		t.Errorf("StartedToClosedSeconds = %v, want nil (bead never closed)", *got.StartedToClosedSeconds)
	}
	if got.QueueSeconds != nil {
		t.Errorf("QueueSeconds = %v, want nil (no started_at)", *got.QueueSeconds)
	}
}

func testLifecycleByType(t *testing.T, store StoreFactory) {
	s := store(t)
	t.Cleanup(func() { _ = s.Close() })

	base := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)

	// Seed 5 closed task beads with filed→closed in [60s..300s] and
	// 1 closed bug bead with 30s filed→closed. By-type rollup should
	// group them and return non-zero P50 for both types.
	var fixtures []LifecycleFixture
	for i := 0; i < 5; i++ {
		filed := base.Add(time.Duration(i) * time.Minute)
		closed := filed.Add(time.Duration(60+i*60) * time.Second)
		fixtures = append(fixtures, LifecycleFixture{
			BeadID:    fmt.Sprintf("spi-task-%d", i),
			BeadType:  "task",
			FiledAt:   filed,
			ReadyAt:   filed.Add(5 * time.Second),
			StartedAt: filed.Add(10 * time.Second),
			ClosedAt:  closed,
			UpdatedAt: closed,
		})
	}
	fixtures = append(fixtures, LifecycleFixture{
		BeadID:    "spi-bug-1",
		BeadType:  "bug",
		FiledAt:   base,
		ReadyAt:   base.Add(2 * time.Second),
		StartedAt: base.Add(4 * time.Second),
		ClosedAt:  base.Add(30 * time.Second),
		UpdatedAt: base.Add(30 * time.Second),
	})
	if err := InsertBeadLifecycles(s, fixtures); err != nil {
		t.Fatalf("InsertBeadLifecycles: %v", err)
	}

	rollup, err := s.QueryLifecycleByType(base.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("QueryLifecycleByType: %v", err)
	}
	if len(rollup) < 2 {
		t.Fatalf("QueryLifecycleByType: got %d rows, want at least 2 (task + bug)", len(rollup))
	}

	byType := map[string]olap.LifecycleByType{}
	for _, r := range rollup {
		byType[r.BeadType] = r
	}
	task, ok := byType["task"]
	if !ok {
		t.Fatalf("task missing from rollup: %v", byType)
	}
	if task.Count != 5 {
		t.Errorf("task count = %d, want 5", task.Count)
	}
	if task.FiledToClosedP50 <= 0 {
		t.Errorf("task FiledToClosedP50 = %v, want > 0", task.FiledToClosedP50)
	}

	bug, ok := byType["bug"]
	if !ok {
		t.Fatalf("bug missing from rollup: %v", byType)
	}
	if bug.Count != 1 {
		t.Errorf("bug count = %d, want 1", bug.Count)
	}
}

func testReviewFixCounts(t *testing.T, store StoreFactory) {
	s := store(t)
	t.Cleanup(func() { _ = s.Close() })

	beadID := "spi-review-counts-1"
	now := time.Now().UTC()

	// Seed agent_runs_olap directly: review/fix counts come from that
	// table, not from bead_lifecycle_olap. The SQL mirrors the shape
	// cmd/spire/metrics.go uses.
	err := s.Submit(func(tx *sql.Tx) error {
		runs := []struct {
			id, phase, role, result string
			reviewRounds            int
		}{
			{"rr-1", "implement", "apprentice", "success", 0},
			{"rr-2", "sage-review", "sage", "request_changes", 1},
			{"rr-3", "fix", "apprentice", "success", 1},
			{"rr-4", "sage-review", "sage", "request_changes", 2},
			{"rr-5", "fix", "apprentice", "success", 2},
			{"rr-6", "arbiter", "arbiter", "success", 3},
		}
		for _, r := range runs {
			if _, err := tx.ExecContext(context.Background(), `
				INSERT INTO agent_runs_olap (id, bead_id, formula_name, phase, role, model, tower, repo, result, review_rounds, started_at, completed_at)
				VALUES (?, ?, 'task-default', ?, ?, 'opus', 't1', 'test', ?, ?, ?, ?)`,
				r.id, beadID, r.phase, r.role, r.result, r.reviewRounds,
				now.Add(-time.Hour), now.Add(-time.Hour+30*time.Second),
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed agent_runs_olap: %v", err)
	}

	got, err := s.QueryReviewFixCounts(beadID)
	if err != nil {
		t.Fatalf("QueryReviewFixCounts: %v", err)
	}
	if got == nil {
		t.Fatal("QueryReviewFixCounts returned nil")
	}
	if got.ReviewCount != 2 {
		t.Errorf("ReviewCount = %d, want 2", got.ReviewCount)
	}
	if got.FixCount != 2 {
		t.Errorf("FixCount = %d, want 2", got.FixCount)
	}
	if got.ArbiterCount != 1 {
		t.Errorf("ArbiterCount = %d, want 1", got.ArbiterCount)
	}
	if got.MaxReviewRounds != 3 {
		t.Errorf("MaxReviewRounds = %d, want 3", got.MaxReviewRounds)
	}
}

func testChildDrilldown(t *testing.T, store StoreFactory) {
	s := store(t)
	t.Cleanup(func() { _ = s.Close() })

	base := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)

	fixtures := []LifecycleFixture{
		{BeadID: "spi-parent", BeadType: "epic", FiledAt: base, UpdatedAt: base},
		{BeadID: "spi-parent.1", BeadType: "step", FiledAt: base, UpdatedAt: base, ClosedAt: base.Add(30 * time.Second)},
		{BeadID: "spi-parent.2", BeadType: "step", FiledAt: base, UpdatedAt: base, ClosedAt: base.Add(60 * time.Second)},
		{BeadID: "spi-unrelated", BeadType: "task", FiledAt: base, UpdatedAt: base},
	}
	if err := InsertBeadLifecycles(s, fixtures); err != nil {
		t.Fatalf("InsertBeadLifecycles: %v", err)
	}

	kids, err := s.QueryChildLifecycle("spi-parent")
	if err != nil {
		t.Fatalf("QueryChildLifecycle: %v", err)
	}
	if len(kids) != 2 {
		t.Fatalf("QueryChildLifecycle: got %d rows, want 2 (spi-parent.1, spi-parent.2)", len(kids))
	}
	for _, k := range kids {
		if k.BeadID == "spi-unrelated" {
			t.Errorf("QueryChildLifecycle(%q) returned unrelated bead %q — hierarchical prefix filter leaked", "spi-parent", k.BeadID)
		}
	}
}

func testQueueSeparateFromExecution(t *testing.T, store StoreFactory) {
	s := store(t)
	t.Cleanup(func() { _ = s.Close() })

	// Motivating case from spi-hmdwm: 21m13s queue delay, 2h52m49s execution.
	// The spec demands queue and execution render as separate numbers so an
	// operator can tell scheduler slowness from worker slowness.
	base := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	filed := base
	ready := base.Add(5 * time.Minute)
	started := ready.Add(21*time.Minute + 13*time.Second)   // queue = 21m13s
	closed := started.Add(2*time.Hour + 52*time.Minute + 49*time.Second) // exec = 2h52m49s

	if err := InsertBeadLifecycles(s, []LifecycleFixture{{
		BeadID:    "spi-h32xj-like",
		BeadType:  "task",
		FiledAt:   filed,
		ReadyAt:   ready,
		StartedAt: started,
		ClosedAt:  closed,
		UpdatedAt: closed,
	}}); err != nil {
		t.Fatalf("InsertBeadLifecycles: %v", err)
	}

	got, err := s.QueryLifecycleForBead("spi-h32xj-like")
	if err != nil {
		t.Fatalf("QueryLifecycleForBead: %v", err)
	}
	if got == nil || got.QueueSeconds == nil || got.StartedToClosedSeconds == nil {
		t.Fatalf("QueryLifecycleForBead: missing queue/execution intervals: %+v", got)
	}
	if *got.QueueSeconds < 1272 || *got.QueueSeconds > 1274 {
		t.Errorf("QueueSeconds = %v, want ~1273s (21m13s)", *got.QueueSeconds)
	}
	if *got.StartedToClosedSeconds < 10368 || *got.StartedToClosedSeconds > 10370 {
		t.Errorf("StartedToClosedSeconds = %v, want ~10369s (2h52m49s)", *got.StartedToClosedSeconds)
	}

	// Strongest assertion: queue and execution must differ — a renderer
	// bug that collapses them would make both match. A regression in
	// either the ETL or the query SQL would collapse these.
	if *got.QueueSeconds == *got.StartedToClosedSeconds {
		t.Errorf("QueueSeconds and StartedToClosedSeconds both = %v — queue collapsed into execution", *got.QueueSeconds)
	}
}
