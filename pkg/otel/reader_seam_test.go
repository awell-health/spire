package otel

import (
	"context"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// fakeTraceReader is an in-memory olap.TraceReader stub used by the
// reader-seam regression test. It is deliberately narrow — only the
// bead-scoped query methods that cmd/spire/trace.go exercises are
// populated. Other TraceReader methods return empty results so the
// interface is satisfied but not relied on for this test.
//
// This lets the test assert the end-to-end contract — canonical OTLP
// resource attrs → typed rows with bead_id → findable via TraceReader
// — without opening DuckDB or booting a tower, matching the archmage's
// DB-free regression guardrail for spi-ecnfe.
type fakeTraceReader struct {
	spans  map[string][]olap.SpanRecord        // bead_id → spans
	steps  map[string][]olap.StepToolBreakdown // bead_id → step breakdowns
	apis   map[string][]olap.APIEventStats     // bead_id → api stats
	byTool map[string][]olap.ToolEventStats    // bead_id → tool stats
}

func (f *fakeTraceReader) QuerySpans(ctx context.Context, traceID string) ([]olap.SpanRecord, error) {
	// Not needed for the bead-scoped lookup under test.
	return nil, nil
}

func (f *fakeTraceReader) QueryTraces(ctx context.Context, filter olap.TraceFilter) ([]olap.TraceSummary, error) {
	return nil, nil
}

func (f *fakeTraceReader) QueryToolSpansByBead(beadID string) ([]olap.SpanRecord, error) {
	return f.spans[beadID], nil
}

func (f *fakeTraceReader) QueryToolEventsByBead(beadID string) ([]olap.ToolEventStats, error) {
	return f.byTool[beadID], nil
}

func (f *fakeTraceReader) QueryToolEventsByStep(beadID string) ([]olap.StepToolBreakdown, error) {
	return f.steps[beadID], nil
}

func (f *fakeTraceReader) QueryAPIEventsByBead(beadID string) ([]olap.APIEventStats, error) {
	return f.apis[beadID], nil
}

// ingestAndIndex mimics what a real backend would do: take the typed
// rows ParseResourceSpans / ParseLogRecords produce and make them
// findable by bead_id. A real SQL backend does this through a
// WHERE bead_id = ? filter; this fake does it by building a map keyed
// on the same field. If the ingestion path forgets to populate bead_id,
// the rows land under the empty-string key and the by-bead query misses.
func (f *fakeTraceReader) ingestToolSpans(spans []ToolSpan) {
	if f.spans == nil {
		f.spans = make(map[string][]olap.SpanRecord)
	}
	for _, s := range spans {
		f.spans[s.BeadID] = append(f.spans[s.BeadID], olap.SpanRecord{
			TraceID:      s.TraceID,
			SpanID:       s.SpanID,
			ParentSpanID: s.ParentSpanID,
			SpanName:     s.SpanName,
			Kind:         s.Kind,
			DurationMs:   s.DurationMs,
			Success:      s.Success,
			StartTime:    s.StartTime,
			EndTime:      s.EndTime,
			Attributes:   s.Attributes,
		})
	}
}

func (f *fakeTraceReader) ingestToolEvents(events []ToolEvent) {
	if f.steps == nil {
		f.steps = make(map[string][]olap.StepToolBreakdown)
	}
	if f.byTool == nil {
		f.byTool = make(map[string][]olap.ToolEventStats)
	}
	// Group by bead → step → tool stats, mimicking the SQL aggregation the
	// real backend performs. For the purposes of this test we only need
	// the bead-scoped index to be populated.
	perBead := make(map[string]map[string]map[string]*olap.ToolEventStats)
	for _, e := range events {
		beadSteps, ok := perBead[e.BeadID]
		if !ok {
			beadSteps = make(map[string]map[string]*olap.ToolEventStats)
			perBead[e.BeadID] = beadSteps
		}
		stepTools, ok := beadSteps[e.Step]
		if !ok {
			stepTools = make(map[string]*olap.ToolEventStats)
			beadSteps[e.Step] = stepTools
		}
		s, ok := stepTools[e.ToolName]
		if !ok {
			s = &olap.ToolEventStats{ToolName: e.ToolName, Step: e.Step}
			stepTools[e.ToolName] = s
		}
		s.Count++
		if !e.Success {
			s.FailureCount++
		}
	}
	for beadID, steps := range perBead {
		for step, tools := range steps {
			br := olap.StepToolBreakdown{Step: step}
			for _, s := range tools {
				br.Tools = append(br.Tools, *s)
				f.byTool[beadID] = append(f.byTool[beadID], *s)
			}
			f.steps[beadID] = append(f.steps[beadID], br)
		}
	}
}

func (f *fakeTraceReader) ingestAPIEvents(events []APIEvent) {
	if f.apis == nil {
		f.apis = make(map[string][]olap.APIEventStats)
	}
	// Aggregate per bead_id+model like the real backend.
	type key struct {
		bead, model string
	}
	agg := map[key]*olap.APIEventStats{}
	for _, e := range events {
		k := key{e.BeadID, e.Model}
		s, ok := agg[k]
		if !ok {
			s = &olap.APIEventStats{Model: e.Model}
			agg[k] = s
		}
		s.Count++
		s.TotalCostUSD += e.CostUSD
		s.TotalInputTokens += e.InputTokens
		s.TotalOutputTokens += e.OutputTokens
	}
	for k, s := range agg {
		f.apis[k.bead] = append(f.apis[k.bead], *s)
	}
}

// TestReaderSeam_CanonicalIngestionIsFindableByBead is the spi-ecnfe
// reader-side regression: wire the full ingestion → reader contract
// together with only in-memory fakes, and prove that a bead emitted
// with canonical OTEL resource attributes (bead_id, formula_step) is
// locatable by bead-scoped reader queries. A miss here means either
// the receiver dropped bead_id on the typed row (regressed extractor)
// or the reader-side filter is keying on the wrong column.
func TestReaderSeam_CanonicalIngestionIsFindableByBead(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	beadID := "spi-reader-seam"
	step := "implement"

	spansReq := []*tracepb.ResourceSpans{
		{
			Resource: &resourcepb.Resource{
				Attributes: canonicalResourceAttrs(beadID, step),
			},
			ScopeSpans: []*tracepb.ScopeSpans{
				{
					Spans: []*tracepb.Span{
						{
							Name:              "Read",
							TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
							SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
							StartTimeUnixNano: now,
							EndTimeUnixNano:   now + 20_000_000,
						},
						{
							Name:              "Edit",
							TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
							SpanId:            []byte{9, 10, 11, 12, 13, 14, 15, 16},
							StartTimeUnixNano: now + 30_000_000,
							EndTimeUnixNano:   now + 80_000_000,
						},
					},
				},
			},
		},
	}

	logsReq := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{
				Attributes: append(canonicalResourceAttrs(beadID, step),
					strKV("session.id", "sess-reader-seam")),
			},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.api_request"}},
							Attributes: []*commonpb.KeyValue{
								strKV("model", "claude-opus-4-7"),
								intKV("input_tokens", 1200),
								intKV("output_tokens", 400),
							},
						},
					},
				},
			},
		},
	}

	spanRes := ParseResourceSpans(spansReq, "default-tower")
	logRes := ParseLogRecords(logsReq, "default-tower")

	reader := &fakeTraceReader{}
	reader.ingestToolSpans(spanRes.ToolSpans)
	reader.ingestToolEvents(spanRes.ToolEvents)
	reader.ingestToolEvents(logRes.ToolEvents) // log-driven tool events would also land here
	reader.ingestAPIEvents(logRes.APIEvents)

	// Interface assertion: the fake must satisfy olap.TraceReader so the
	// test exercises the real contract surface cmd/spire/trace.go depends
	// on.
	var _ olap.TraceReader = reader

	gotSpans, err := reader.QueryToolSpansByBead(beadID)
	if err != nil {
		t.Fatalf("QueryToolSpansByBead: %v", err)
	}
	if len(gotSpans) != 2 {
		t.Fatalf("QueryToolSpansByBead(%q): got %d spans, want 2 — canonical attrs should have populated BeadID on every span", beadID, len(gotSpans))
	}

	gotSteps, err := reader.QueryToolEventsByStep(beadID)
	if err != nil {
		t.Fatalf("QueryToolEventsByStep: %v", err)
	}
	if len(gotSteps) == 0 {
		t.Fatalf("QueryToolEventsByStep(%q): no step breakdown rows — ingestion dropped bead_id", beadID)
	}
	for _, s := range gotSteps {
		if s.Step != step {
			t.Errorf("step breakdown Step = %q, want %q — canonical formula_step not propagated", s.Step, step)
		}
	}

	gotAPIs, err := reader.QueryAPIEventsByBead(beadID)
	if err != nil {
		t.Fatalf("QueryAPIEventsByBead: %v", err)
	}
	if len(gotAPIs) != 1 {
		t.Fatalf("QueryAPIEventsByBead(%q): got %d, want 1", beadID, len(gotAPIs))
	}

	// Control: a bead that never emitted telemetry returns empty rather
	// than misattributed rows — proves the filter is actually scoping.
	if got, _ := reader.QueryToolSpansByBead("spi-nonexistent"); len(got) != 0 {
		t.Errorf("QueryToolSpansByBead(spi-nonexistent): got %d rows, want 0 — filter leaked", len(got))
	}
}

// TestReaderSeam_LegacyAttrsAreFindableByBead confirms the legacy
// fallback keeps working on the reader path: pre-migration emitters
// using `bead.id` / `step` still produce rows the reader can find.
// When the legacy fallback is eventually removed, this test is the
// deliberate delete.
func TestReaderSeam_LegacyAttrsAreFindableByBead(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	legacyBead := "spi-reader-seam-legacy"

	spansReq := []*tracepb.ResourceSpans{
		{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					strKV("bead.id", legacyBead),
					strKV("step", "plan"),
				},
			},
			ScopeSpans: []*tracepb.ScopeSpans{
				{
					Spans: []*tracepb.Span{
						{
							Name:              "Grep",
							TraceId:           []byte{0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
							SpanId:            []byte{0xca, 0xfe, 0xba, 0xbe, 1, 2, 3, 4},
							StartTimeUnixNano: now,
							EndTimeUnixNano:   now + 5_000_000,
						},
					},
				},
			},
		},
	}

	result := ParseResourceSpans(spansReq, "default-tower")
	reader := &fakeTraceReader{}
	reader.ingestToolSpans(result.ToolSpans)
	reader.ingestToolEvents(result.ToolEvents)

	gotSpans, err := reader.QueryToolSpansByBead(legacyBead)
	if err != nil {
		t.Fatalf("QueryToolSpansByBead: %v", err)
	}
	if len(gotSpans) != 1 {
		t.Errorf("legacy attrs produced %d findable spans for bead %q, want 1", len(gotSpans), legacyBead)
	}
}
