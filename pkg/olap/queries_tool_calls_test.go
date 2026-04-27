//go:build cgo

package olap

import (
	"testing"
	"time"
)

// openTestOLAP opens an in-memory DuckDB with the schema initialised.
// Mirrors the openMockDoltLifecycle helper next door but for the real
// OLAP DB shape — `Open("")` gives us a private memory DB per test.
func openTestOLAP(t *testing.T) *DB {
	t.Helper()
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open olap: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertSpan(t *testing.T, db *DB, sessionID, toolName string, ts time.Time, success bool, attrs string) {
	t.Helper()
	spanID := "sp-" + toolName + ts.Format("150405.000")
	_, err := db.db.Exec(`INSERT INTO tool_spans
		(trace_id, span_id, parent_span_id, session_id, bead_id, agent_name, step,
		 span_name, kind, duration_ms, success, start_time, end_time, tower, attributes)
		VALUES ('tr', ?, '', ?, 'spi-x', 'wizard', 'implement',
		 ?, 'tool', 100, ?, ?, ?, 'dev', ?)`,
		spanID, sessionID, toolName, success, ts, ts.Add(100*time.Millisecond), attrs)
	if err != nil {
		t.Fatalf("insert span: %v", err)
	}
}

func insertEvent(t *testing.T, db *DB, sessionID, toolName string, ts time.Time, success bool) {
	t.Helper()
	_, err := db.db.Exec(`INSERT INTO tool_events
		(session_id, bead_id, agent_name, step, tool_name, duration_ms, success, timestamp, tower, provider, event_kind)
		VALUES (?, 'spi-x', 'wizard', 'implement', ?, 100, ?, ?, 'dev', 'claude', 'tool')`,
		sessionID, toolName, success, ts)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

// --- empty / unknown session ---

func TestQueryToolCallsBySession_EmptyDB(t *testing.T) {
	db := openTestOLAP(t)
	rows, err := db.QueryToolCallsBySession("sess-missing", 100, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

// --- spans only ---

func TestQueryToolCallsBySession_SpansOnly(t *testing.T) {
	db := openTestOLAP(t)
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	insertSpan(t, db, "sess-1", "Bash", t0, true, `{"command":"ls"}`)
	insertSpan(t, db, "sess-1", "Read", t0.Add(time.Second), true, `{"file_path":"/tmp/x"}`)

	rows, err := db.QueryToolCallsBySession("sess-1", 100, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for i, r := range rows {
		if r.Source != "span" {
			t.Errorf("row[%d].Source = %q, want span", i, r.Source)
		}
		if r.SessionID != "sess-1" {
			t.Errorf("row[%d].SessionID = %q, want sess-1", i, r.SessionID)
		}
	}
	// Span rows must carry the attributes payload — that's why spans are
	// preferred over log events.
	if rows[0].Attributes == "" || rows[0].Attributes == "{}" {
		t.Errorf("row[0].Attributes empty, want non-empty: %q", rows[0].Attributes)
	}
}

// --- events only ---

func TestQueryToolCallsBySession_EventsOnly(t *testing.T) {
	db := openTestOLAP(t)
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	insertEvent(t, db, "sess-2", "Bash", t0, true)
	insertEvent(t, db, "sess-2", "Read", t0.Add(time.Second), false)

	rows, err := db.QueryToolCallsBySession("sess-2", 100, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for i, r := range rows {
		if r.Source != "log" {
			t.Errorf("row[%d].Source = %q, want log (no span available)", i, r.Source)
		}
	}
	if rows[1].Success {
		t.Errorf("row[1].Success = true, want false (insertEvent passed false)")
	}
}

// --- dedup at same-second + same tool ---

func TestQueryToolCallsBySession_DedupesSameSecondSameTool(t *testing.T) {
	// When a span and a log row both report Bash at the same second,
	// we keep the span (rich payload) and drop the log to avoid
	// double-counting Claude Code's dual-emit behaviour.
	db := openTestOLAP(t)
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	insertSpan(t, db, "sess-3", "Bash", t0.Add(250*time.Millisecond), true, `{"command":"ls"}`)
	insertEvent(t, db, "sess-3", "Bash", t0.Add(750*time.Millisecond), true)

	rows, err := db.QueryToolCallsBySession("sess-3", 100, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (dedup'd by tool+second)", len(rows))
	}
	if rows[0].Source != "span" {
		t.Errorf("row[0].Source = %q, want span (preferred)", rows[0].Source)
	}
}

// --- log row preserved when span is at a DIFFERENT second ---

func TestQueryToolCallsBySession_PreservesLogAtDifferentSecond(t *testing.T) {
	// Span at second N, log at second N+1 — both rows must surface.
	db := openTestOLAP(t)
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	insertSpan(t, db, "sess-4", "Bash", t0, true, `{"command":"ls"}`)
	insertEvent(t, db, "sess-4", "Bash", t0.Add(time.Second*2), true)

	rows, err := db.QueryToolCallsBySession("sess-4", 100, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (different seconds → no dedup)", len(rows))
	}
	if rows[0].Source != "span" || rows[1].Source != "log" {
		t.Errorf("sources = [%q,%q], want [span,log] (spans appended first, logs after)", rows[0].Source, rows[1].Source)
	}
}

// --- log row preserved when DIFFERENT tool same second ---

func TestQueryToolCallsBySession_PreservesLogDifferentTool(t *testing.T) {
	// Same second, different tool name — dedup MUST NOT fire.
	db := openTestOLAP(t)
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	insertSpan(t, db, "sess-5", "Bash", t0, true, `{}`)
	insertEvent(t, db, "sess-5", "Read", t0, true)

	rows, err := db.QueryToolCallsBySession("sess-5", 100, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (different tools → no dedup)", len(rows))
	}
}

// --- limit fallback / clamp ---

func TestQueryToolCallsBySession_LimitDefaultsTo200(t *testing.T) {
	db := openTestOLAP(t)
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		insertSpan(t, db, "sess-6", "Bash", t0.Add(time.Duration(i)*time.Second), true, "{}")
	}

	// limit=0 → falls back to 200 (well above the 3 we inserted)
	rows, err := db.QueryToolCallsBySession("sess-6", 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("got %d rows, want 3 (limit=0 should default, not zero out)", len(rows))
	}

	// negative limit → also falls back
	rows, err = db.QueryToolCallsBySession("sess-6", -10, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("got %d rows on negative limit, want 3 (negative → default)", len(rows))
	}
}

// --- pagination ---

func TestQueryToolCallsBySession_Pagination(t *testing.T) {
	db := openTestOLAP(t)
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	const total = 5
	for i := 0; i < total; i++ {
		insertSpan(t, db, "sess-page", "Bash", t0.Add(time.Duration(i)*time.Second), true, "{}")
	}

	page1, err := db.QueryToolCallsBySession("sess-page", 2, 0)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1) != 2 {
		t.Errorf("page 1 size = %d, want 2", len(page1))
	}

	page2, err := db.QueryToolCallsBySession("sess-page", 2, 2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("page 2 size = %d, want 2", len(page2))
	}

	page3, err := db.QueryToolCallsBySession("sess-page", 2, 4)
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if len(page3) != 1 {
		t.Errorf("page 3 size = %d, want 1 (last partial)", len(page3))
	}

	// All pages combined must equal the source set with no overlap.
	gotSpans := map[string]bool{}
	for _, r := range append(append(page1, page2...), page3...) {
		gotSpans[r.SpanID] = true
	}
	if len(gotSpans) != total {
		t.Errorf("union of pages had %d distinct span IDs, want %d (overlap or gap)", len(gotSpans), total)
	}
}

func TestQueryToolCallsBySession_OffsetPastEndIsEmpty(t *testing.T) {
	db := openTestOLAP(t)
	insertSpan(t, db, "sess-end", "Bash",
		time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC), true, "{}")

	rows, err := db.QueryToolCallsBySession("sess-end", 10, 100)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows on out-of-range offset, want 0", len(rows))
	}
}

func TestQueryToolCallsBySession_NegativeOffsetClampsToZero(t *testing.T) {
	db := openTestOLAP(t)
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	insertSpan(t, db, "sess-neg", "Bash", t0, true, "{}")
	insertSpan(t, db, "sess-neg", "Read", t0.Add(time.Second), true, "{}")

	// A negative offset is treated as 0 — still returns the full result set.
	rows, err := db.QueryToolCallsBySession("sess-neg", 100, -5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2 (negative offset → clamp to 0)", len(rows))
	}
}

// --- session isolation ---

func TestQueryToolCallsBySession_DoesNotLeakOtherSessions(t *testing.T) {
	db := openTestOLAP(t)
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	insertSpan(t, db, "sess-A", "Bash", t0, true, "{}")
	insertSpan(t, db, "sess-B", "Read", t0, true, "{}")
	insertEvent(t, db, "sess-B", "Grep", t0, true)

	rows, err := db.QueryToolCallsBySession("sess-A", 100, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (only sess-A)", len(rows))
	}
	if rows[0].SessionID != "sess-A" {
		t.Errorf("got session %q, want sess-A", rows[0].SessionID)
	}
}

// --- non-tool spans are filtered ---

func TestQueryToolCallsBySession_FiltersNonToolKindSpans(t *testing.T) {
	// Spans with kind != 'tool' (e.g. agent_run, request) must not surface.
	db := openTestOLAP(t)
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	// One real tool span, one agent_run span (same session).
	insertSpan(t, db, "sess-filter", "Bash", t0, true, "{}")
	if _, err := db.db.Exec(`INSERT INTO tool_spans
		(trace_id, span_id, parent_span_id, session_id, bead_id, agent_name, step,
		 span_name, kind, duration_ms, success, start_time, end_time, tower, attributes)
		VALUES ('tr', 'sp-other', '', 'sess-filter', 'spi-x', 'wizard', 'implement',
		 'agent_run', 'agent', 100, true, ?, ?, 'dev', '{}')`,
		t0, t0.Add(time.Second)); err != nil {
		t.Fatalf("insert non-tool span: %v", err)
	}

	rows, err := db.QueryToolCallsBySession("sess-filter", 100, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1 (kind != 'tool' must be filtered)", len(rows))
	}
}
