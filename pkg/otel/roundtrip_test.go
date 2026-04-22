//go:build cgo

package otel

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/observability/obsvocab"
	"github.com/awell-health/spire/pkg/olap"
	_ "github.com/awell-health/spire/pkg/olap/duckdb" // register duckdb factory for OpenStore
	"github.com/awell-health/spire/pkg/olap/olaptest"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// This file holds the layered end-to-end regression: emit a synthetic
// OTLP batch carrying canonical runtime-contract resource attributes,
// route it through the same ParseResourceSpans / ParseLogRecords
// transforms the production receiver uses, persist the resulting typed
// rows through the real olap.Writer, and prove the bead-scoped
// olap.TraceReader queries return the same rows. Above pkg/olap the
// test layer is barred from touching raw SQL or the analytics.db path,
// so the round-trip assertions here are the only place where the full
// ingest→store→reader contract lives under one `go test` invocation.

// canonicalAttrsFromVocab returns the canonical OTLP resource attributes
// keyed on the shared obsvocab constants. A rename in the vocabulary
// package without a corresponding update in the receiver's extractor
// breaks this test first, before the emitter and receiver disagree in
// production. The slice order mirrors obsvocab.CanonicalAttrKeys so a
// receiver that only reads the first few keys fails loudly.
func canonicalAttrsFromVocab(beadID, step string) []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		strKV(obsvocab.AttrBeadID, beadID),
		strKV(obsvocab.AttrAttemptID, beadID+"-attempt-1"),
		strKV(obsvocab.AttrRunID, "run-1"),
		strKV(obsvocab.AttrTower, "round-trip-tower"),
		strKV(obsvocab.AttrPrefix, "spi"),
		strKV(obsvocab.AttrRole, "apprentice"),
		strKV(obsvocab.AttrFormulaStep, step),
		strKV(obsvocab.AttrBackend, "process"),
		strKV(obsvocab.AttrWorkspaceKind, "owned_worktree"),
		strKV(obsvocab.AttrWorkspaceName, "feat"),
		strKV(obsvocab.AttrWorkspaceOrigin, "local-bind"),
		strKV(obsvocab.AttrHandoffMode, "bundle"),
		strKV(obsvocab.AttrAgentName, "apprentice-"+beadID+"-0"),
	}
}

// persistParseResultFromSpans writes ParseResourceSpans output via the
// Writer interface — the same path the production receiver uses — and
// returns after commit. Tests can then query via olap.TraceReader to
// prove round-trip. No raw analytics.db access leaks into the test
// code because the Writer is obtained from olaptest's StoreFactory.
func persistParseResultFromSpans(t *testing.T, w olap.Writer, result TraceParseResult) {
	t.Helper()
	if len(result.ToolEvents) == 0 && len(result.ToolSpans) == 0 {
		return
	}
	if err := w.Submit(func(tx *sql.Tx) error {
		if err := InsertBatchTx(tx, result.ToolEvents); err != nil {
			return err
		}
		return InsertToolSpansTx(tx, result.ToolSpans)
	}); err != nil {
		t.Fatalf("persist span result: %v", err)
	}
}

// persistParseResultFromLogs writes ParseLogRecords output via the
// Writer interface, mirroring the receiver's log-ingestion path.
func persistParseResultFromLogs(t *testing.T, w olap.Writer, result LogParseResult) {
	t.Helper()
	if len(result.ToolEvents) == 0 && len(result.APIEvents) == 0 {
		return
	}
	if err := w.Submit(func(tx *sql.Tx) error {
		if err := InsertBatchTx(tx, result.ToolEvents); err != nil {
			return err
		}
		return InsertAPIEventsTx(tx, result.APIEvents)
	}); err != nil {
		t.Fatalf("persist log result: %v", err)
	}
}

// TestRoundTrip_CanonicalAttrsThroughOLAPReader is the layered
// regression for the "OTEL emit → OLAP write → reader query" chain.
// Each assertion narrows a specific failure mode:
//
//  1. Canonical resource attrs must survive extraction.
//  2. Typed rows must carry bead_id / step into the storage layer.
//  3. The reader-side bead filter must return the same rows.
//
// Exactly one of these steps can regress at a time; the test fails on
// the specific boundary that broke, rather than on a downstream
// rendering surprise in the CLI.
func TestRoundTrip_CanonicalAttrsThroughOLAPReader(t *testing.T) {
	db := openInMemoryStore(t)
	t.Cleanup(func() { _ = db.Close() })

	now := uint64(time.Now().UnixNano())
	beadID := "spi-rt-canon"
	step := "implement"

	spansReq := []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{
			Attributes: canonicalAttrsFromVocab(beadID, step),
		},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{
				{
					Name:              "Read",
					TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
					SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
					StartTimeUnixNano: now,
					EndTimeUnixNano:   now + 40_000_000,
				},
				{
					Name:              "Edit",
					TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
					SpanId:            []byte{9, 10, 11, 12, 13, 14, 15, 16},
					StartTimeUnixNano: now + 50_000_000,
					EndTimeUnixNano:   now + 130_000_000,
				},
			},
		}},
	}}

	logsReq := []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{
			Attributes: append(canonicalAttrsFromVocab(beadID, step),
				strKV(obsvocab.AttrSessionID, "sess-rt")),
		},
		ScopeLogs: []*logspb.ScopeLogs{{
			LogRecords: []*logspb.LogRecord{
				{
					TimeUnixNano: now,
					Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.tool_result"}},
					Attributes: []*commonpb.KeyValue{
						strKV("tool_name", "Bash"),
						intKV("duration_ms", 200),
						boolKV("success", true),
					},
				},
				{
					TimeUnixNano: now + 1_000_000,
					Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.api_request"}},
					Attributes: []*commonpb.KeyValue{
						strKV("model", "claude-opus-4-7"),
						intKV("input_tokens", 1200),
						intKV("output_tokens", 500),
					},
				},
			},
		}},
	}}

	spanResult := ParseResourceSpans(spansReq, "default-tower")
	logResult := ParseLogRecords(logsReq, "default-tower")

	// Parse-stage assertions first so the failure message pinpoints the
	// boundary: extractor → typed-row correlation.
	if len(spanResult.ToolSpans) != 2 {
		t.Fatalf("ParseResourceSpans: got %d tool spans, want 2", len(spanResult.ToolSpans))
	}
	for _, s := range spanResult.ToolSpans {
		if s.BeadID != beadID {
			t.Errorf("ParseResourceSpans span BeadID = %q, want %q — canonical bead_id not persisted on typed row", s.BeadID, beadID)
		}
		if s.Step != step {
			t.Errorf("ParseResourceSpans span Step = %q, want %q — canonical formula_step not persisted on typed row", s.Step, step)
		}
	}
	if len(logResult.APIEvents) != 1 {
		t.Fatalf("ParseLogRecords: got %d api events, want 1", len(logResult.APIEvents))
	}
	if logResult.APIEvents[0].BeadID != beadID {
		t.Errorf("ParseLogRecords API event BeadID = %q, want %q", logResult.APIEvents[0].BeadID, beadID)
	}

	// Storage stage: persist through the real Writer.
	persistParseResultFromSpans(t, db, spanResult)
	persistParseResultFromLogs(t, db, logResult)

	// Reader stage: bead-scoped query must return the same rows. This
	// is the assertion that locks in the "emitter → receiver → olap →
	// reader" chain on canonical vocabulary. The reader is typed as
	// olap.TraceReader so the CLI layer above can depend on the same
	// interface without opening analytics.db.
	var reader olap.TraceReader = db

	gotSpans, err := reader.QueryToolSpansByBead(beadID)
	if err != nil {
		t.Fatalf("QueryToolSpansByBead: %v", err)
	}
	if len(gotSpans) != 2 {
		t.Fatalf("QueryToolSpansByBead(%q): got %d spans, want 2 — reader did not match bead_id of persisted rows", beadID, len(gotSpans))
	}

	gotAPIs, err := reader.QueryAPIEventsByBead(beadID)
	if err != nil {
		t.Fatalf("QueryAPIEventsByBead: %v", err)
	}
	if len(gotAPIs) != 1 {
		t.Fatalf("QueryAPIEventsByBead(%q): got %d, want 1", beadID, len(gotAPIs))
	}
	if gotAPIs[0].Model != "claude-opus-4-7" {
		t.Errorf("QueryAPIEventsByBead model = %q, want claude-opus-4-7", gotAPIs[0].Model)
	}

	// Control: a bead that never emitted must return zero rows — proves
	// the filter is actually scoping by bead_id, not returning
	// everything.
	if got, _ := reader.QueryToolSpansByBead("spi-nonexistent"); len(got) != 0 {
		t.Errorf("QueryToolSpansByBead(spi-nonexistent): got %d rows, want 0 — reader filter leaked", len(got))
	}
}

// TestRoundTrip_MissingBeadID_IsolatesFromCorrelatedBead is the
// negative surface of the round-trip: a Resource with no bead_id
// emits rows under empty-string, and a bead-scoped reader query for a
// different bead must not return them. Locks in the regression where
// uncorrelated telemetry silently merges with a real bead.
func TestRoundTrip_MissingBeadID_IsolatesFromCorrelatedBead(t *testing.T) {
	db := openInMemoryStore(t)
	t.Cleanup(func() { _ = db.Close() })

	now := uint64(time.Now().UnixNano())
	realBead := "spi-rt-real"

	// Correlated batch for realBead.
	correlated := []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{
			Attributes: canonicalAttrsFromVocab(realBead, "implement"),
		},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{
				Name:              "Read",
				TraceId:           []byte{0xa, 0xb, 0xc, 0xd, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
				SpanId:            []byte{0x1a, 2, 3, 4, 5, 6, 7, 8},
				StartTimeUnixNano: now,
				EndTimeUnixNano:   now + 10_000_000,
			}},
		}},
	}}

	// Uncorrelated batch: carries a tower but no bead_id / step. The
	// receiver must not drop the rows (the event happened), but it
	// must not merge them into realBead's queries.
	uncorrelated := []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				strKV(obsvocab.AttrTower, "round-trip-tower"),
				strKV(obsvocab.AttrAgentName, "apprentice-orphan-0"),
			},
		},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{
				Name:              "Grep",
				TraceId:           []byte{0xbe, 0xef, 0xde, 0xad, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
				SpanId:            []byte{0x99, 0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22},
				StartTimeUnixNano: now + 5_000_000,
				EndTimeUnixNano:   now + 8_000_000,
			}},
		}},
	}}

	persistParseResultFromSpans(t, db, ParseResourceSpans(correlated, "default-tower"))
	persistParseResultFromSpans(t, db, ParseResourceSpans(uncorrelated, "default-tower"))

	var reader olap.TraceReader = db

	gotReal, err := reader.QueryToolSpansByBead(realBead)
	if err != nil {
		t.Fatalf("QueryToolSpansByBead(real): %v", err)
	}
	if len(gotReal) != 1 {
		t.Fatalf("QueryToolSpansByBead(%q): got %d rows, want 1 (correlated row only)", realBead, len(gotReal))
	}
	if gotReal[0].SpanName != "Read" {
		t.Errorf("QueryToolSpansByBead(%q): got span %q, want Read — uncorrelated Grep leaked into real bead's query", realBead, gotReal[0].SpanName)
	}
}

// TestRoundTrip_LegacyAttrsStillCorrelateViaReader proves the legacy
// fallback (bead.id / step) still survives the full emit → olap →
// reader chain. When the fallback is eventually removed — either via
// a deliberate spi-* bead or a rewrite of resource_attrs.go — this
// test is the planned delete.
func TestRoundTrip_LegacyAttrsStillCorrelateViaReader(t *testing.T) {
	db := openInMemoryStore(t)
	t.Cleanup(func() { _ = db.Close() })

	now := uint64(time.Now().UnixNano())
	legacyBead := "spi-rt-legacy"

	req := []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				strKV(obsvocab.LegacyAttrBeadID, legacyBead),
				strKV(obsvocab.LegacyAttrStep, "plan"),
				strKV(obsvocab.AttrTower, "round-trip-tower"),
			},
		},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{
				Name:              "Read",
				TraceId:           []byte{0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7, 0x8, 0x9, 0xa, 0xb, 0xc, 0xd, 0xe, 0xf, 0x10},
				SpanId:            []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88},
				StartTimeUnixNano: now,
				EndTimeUnixNano:   now + 10_000_000,
			}},
		}},
	}}

	persistParseResultFromSpans(t, db, ParseResourceSpans(req, "default-tower"))

	var reader olap.TraceReader = db
	got, err := reader.QueryToolSpansByBead(legacyBead)
	if err != nil {
		t.Fatalf("QueryToolSpansByBead(legacy): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("QueryToolSpansByBead(%q): got %d rows, want 1 — legacy fallback broken end-to-end", legacyBead, len(got))
	}
}

// openInMemoryStore opens a fresh in-memory DuckDB Store for each
// round-trip test. Must be cgo-only because DuckDB requires CGO; the
// file-level build tag at the top of this test file pins that.
func openInMemoryStore(t *testing.T) olap.Store {
	t.Helper()
	db, err := olap.OpenStore(olap.Config{Backend: olap.BackendDuckDB, Path: ""})
	if err != nil {
		t.Fatalf("OpenStore duckdb in-memory: %v", err)
	}
	return db
}

// Compile-time assertions — the helpers above must accept exactly what
// olaptest seeds and what the receiver emits, so a type drift surfaces
// here rather than as an opaque panic.
var (
	_ func(*testing.T, olap.Writer, TraceParseResult) = persistParseResultFromSpans
	_ func(*testing.T, olap.Writer, LogParseResult)   = persistParseResultFromLogs
	_ context.Context                                  = context.Background()
	_ olaptest.SpanFixture                             = olaptest.SpanFixture{}
)
