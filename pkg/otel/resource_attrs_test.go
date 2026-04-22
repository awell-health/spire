package otel

import (
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// TestExtractRunContext_CanonicalOnly asserts the receiver's extractor
// recognizes the full canonical runtime-contract vocabulary emitted by
// the worker spawn paths (pkg/agent.applyProcessEnv and the k8s
// backend). These are the keys that spi-xplwy / spi-zm3b1 stamp onto
// OTEL_RESOURCE_ATTRIBUTES. A miss here means trace/log rows land
// without bead_id+formula_step correlation even though the worker
// emitted the contract correctly.
func TestExtractRunContext_CanonicalOnly(t *testing.T) {
	res := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			strKV("bead_id", "spi-canonical"),
			strKV("attempt_id", "spi-canonical-attempt-1"),
			strKV("run_id", "run-42"),
			strKV("tower", "canonical-tower"),
			strKV("prefix", "spi"),
			strKV("role", "apprentice"),
			strKV("formula_step", "implement"),
			strKV("backend", "process"),
			strKV("workspace_kind", "owned_worktree"),
			strKV("workspace_name", "feat"),
			strKV("workspace_origin", "local-bind"),
			strKV("handoff_mode", "bundle"),
			strKV("agent.name", "apprentice-spi-canonical-0"),
			strKV("session.id", "sess-canonical"),
		},
	}

	rc := ExtractRunContext(res)

	if rc.BeadID != "spi-canonical" {
		t.Errorf("BeadID = %q, want spi-canonical", rc.BeadID)
	}
	if rc.AttemptID != "spi-canonical-attempt-1" {
		t.Errorf("AttemptID = %q, want spi-canonical-attempt-1", rc.AttemptID)
	}
	if rc.RunID != "run-42" {
		t.Errorf("RunID = %q, want run-42", rc.RunID)
	}
	if rc.Tower != "canonical-tower" {
		t.Errorf("Tower = %q, want canonical-tower", rc.Tower)
	}
	if rc.Prefix != "spi" {
		t.Errorf("Prefix = %q, want spi", rc.Prefix)
	}
	if rc.Role != "apprentice" {
		t.Errorf("Role = %q, want apprentice", rc.Role)
	}
	if rc.FormulaStep != "implement" {
		t.Errorf("FormulaStep = %q, want implement", rc.FormulaStep)
	}
	if rc.Backend != "process" {
		t.Errorf("Backend = %q, want process", rc.Backend)
	}
	if rc.WorkspaceKind != "owned_worktree" {
		t.Errorf("WorkspaceKind = %q, want owned_worktree", rc.WorkspaceKind)
	}
	if rc.WorkspaceName != "feat" {
		t.Errorf("WorkspaceName = %q, want feat", rc.WorkspaceName)
	}
	if rc.WorkspaceOrigin != "local-bind" {
		t.Errorf("WorkspaceOrigin = %q, want local-bind", rc.WorkspaceOrigin)
	}
	if rc.HandoffMode != "bundle" {
		t.Errorf("HandoffMode = %q, want bundle", rc.HandoffMode)
	}
	if rc.AgentName != "apprentice-spi-canonical-0" {
		t.Errorf("AgentName = %q, want apprentice-spi-canonical-0", rc.AgentName)
	}
	if rc.SessionID != "sess-canonical" {
		t.Errorf("SessionID = %q, want sess-canonical", rc.SessionID)
	}
}

// TestExtractRunContext_LegacyOnly asserts legacy keys (bead.id, step)
// still populate BeadID and FormulaStep when canonical keys are absent.
// This is the migration fallback — pre-spi-xplwy worker binaries (or
// manual OTLP fixtures) must still correlate to a bead.
func TestExtractRunContext_LegacyOnly(t *testing.T) {
	res := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			strKV("bead.id", "spi-legacy"),
			strKV("step", "plan"),
			strKV("agent.name", "wizard-legacy"),
			strKV("tower", "legacy-tower"),
		},
	}

	rc := ExtractRunContext(res)

	if rc.BeadID != "spi-legacy" {
		t.Errorf("BeadID = %q, want spi-legacy (legacy fallback)", rc.BeadID)
	}
	if rc.FormulaStep != "plan" {
		t.Errorf("FormulaStep = %q, want plan (legacy fallback)", rc.FormulaStep)
	}
	if rc.AgentName != "wizard-legacy" {
		t.Errorf("AgentName = %q, want wizard-legacy", rc.AgentName)
	}
	if rc.Tower != "legacy-tower" {
		t.Errorf("Tower = %q, want legacy-tower", rc.Tower)
	}
}

// TestExtractRunContext_CanonicalWinsOverLegacy is the core bug fix:
// when a Resource carries BOTH canonical and legacy keys for the same
// identity, canonical must win silently. This guards against the
// receiver being wedged on legacy keys while workers emit canonical.
func TestExtractRunContext_CanonicalWinsOverLegacy(t *testing.T) {
	// Canonical before legacy in slice order.
	t.Run("canonical_before_legacy", func(t *testing.T) {
		res := &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				strKV("bead_id", "spi-canonical"),
				strKV("formula_step", "implement"),
				strKV("bead.id", "spi-legacy"),
				strKV("step", "plan"),
			},
		}
		rc := ExtractRunContext(res)
		if rc.BeadID != "spi-canonical" {
			t.Errorf("BeadID = %q, want spi-canonical (canonical must win)", rc.BeadID)
		}
		if rc.FormulaStep != "implement" {
			t.Errorf("FormulaStep = %q, want implement (canonical must win)", rc.FormulaStep)
		}
	})

	// Legacy before canonical in slice order. OTLP does not guarantee
	// attribute ordering, so the extractor must handle both.
	t.Run("legacy_before_canonical", func(t *testing.T) {
		res := &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				strKV("bead.id", "spi-legacy"),
				strKV("step", "plan"),
				strKV("bead_id", "spi-canonical"),
				strKV("formula_step", "implement"),
			},
		}
		rc := ExtractRunContext(res)
		if rc.BeadID != "spi-canonical" {
			t.Errorf("BeadID = %q, want spi-canonical (canonical must win regardless of order)", rc.BeadID)
		}
		if rc.FormulaStep != "implement" {
			t.Errorf("FormulaStep = %q, want implement (canonical must win regardless of order)", rc.FormulaStep)
		}
	})
}

// TestExtractRunContext_MissingBoth confirms the zero value is returned
// when neither canonical nor legacy keys are present — the receiver
// must degrade gracefully for unrelated telemetry (framework spans,
// http internals, etc.) rather than misattribute to an empty bead.
func TestExtractRunContext_MissingBoth(t *testing.T) {
	res := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			strKV("http.method", "GET"),
			strKV("http.url", "https://example.com"),
		},
	}
	rc := ExtractRunContext(res)
	if rc.BeadID != "" {
		t.Errorf("BeadID = %q, want empty", rc.BeadID)
	}
	if rc.FormulaStep != "" {
		t.Errorf("FormulaStep = %q, want empty", rc.FormulaStep)
	}
	if rc.AgentName != "" {
		t.Errorf("AgentName = %q, want empty", rc.AgentName)
	}
}

// TestExtractRunContext_PartialCanonicalLegacyFallback confirms the
// canonical-wins rule is per-field: a Resource can have canonical
// formula_step + legacy bead.id, and each field falls through correctly.
func TestExtractRunContext_PartialCanonicalLegacyFallback(t *testing.T) {
	res := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			// Canonical step, legacy bead.
			strKV("formula_step", "review"),
			strKV("bead.id", "spi-partial"),
		},
	}
	rc := ExtractRunContext(res)
	if rc.BeadID != "spi-partial" {
		t.Errorf("BeadID = %q, want spi-partial (legacy fallback for bead_id)", rc.BeadID)
	}
	if rc.FormulaStep != "review" {
		t.Errorf("FormulaStep = %q, want review (canonical)", rc.FormulaStep)
	}
}

// TestExtractRunContext_DotVsUnderscore asserts the extractor treats
// canonical `bead_id` and legacy `bead.id` as distinct keys — a naive
// lowercase/normalize would collapse them and reintroduce the bug.
func TestExtractRunContext_DotVsUnderscore(t *testing.T) {
	// Only canonical with underscore — must match.
	res := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			strKV("bead_id", "spi-under"),
		},
	}
	rc := ExtractRunContext(res)
	if rc.BeadID != "spi-under" {
		t.Errorf("underscore form not recognized: BeadID = %q", rc.BeadID)
	}

	// Only legacy with dot — must match via fallback.
	res2 := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			strKV("bead.id", "spi-dot"),
		},
	}
	rc2 := ExtractRunContext(res2)
	if rc2.BeadID != "spi-dot" {
		t.Errorf("dot form not recognized as legacy fallback: BeadID = %q", rc2.BeadID)
	}
}

// canonicalResourceAttrs returns the OTLP resource attribute set that
// matches what pkg/agent spawn paths inject via OTEL_RESOURCE_ATTRIBUTES
// for the canonical runtime contract. Used by ingestion regression tests
// to prove receiver-side recognition of the exact emitter surface.
func canonicalResourceAttrs(beadID, step string) []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		strKV("bead_id", beadID),
		strKV("attempt_id", beadID+"-attempt-1"),
		strKV("run_id", "run-1"),
		strKV("tower", "ingestion-tower"),
		strKV("prefix", "spi"),
		strKV("role", "apprentice"),
		strKV("formula_step", step),
		strKV("backend", "process"),
		strKV("workspace_kind", "owned_worktree"),
		strKV("workspace_name", "feat"),
		strKV("workspace_origin", "local-bind"),
		strKV("handoff_mode", "bundle"),
		strKV("agent.name", "apprentice-"+beadID+"-0"),
	}
}

// TestParseResourceSpans_CanonicalAttrsProduceCorrelatedRows is the
// end-to-end receiver-ingestion regression for spi-ecnfe. A single
// ResourceSpans batch carrying canonical runtime-contract attrs must
// yield ToolEvent and ToolSpan rows whose BeadID and Step fields match
// the canonical attrs — the observability pipeline's silent-correlation
// failure mode (rows landing with empty bead_id despite the worker
// emitting it) regresses here, not in DuckDB.
func TestParseResourceSpans_CanonicalAttrsProduceCorrelatedRows(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	beadID := "spi-ingest-canon"
	step := "implement"

	req := []*tracepb.ResourceSpans{
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
							EndTimeUnixNano:   now + 50_000_000,
							Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
						},
					},
				},
			},
		},
	}

	result := ParseResourceSpans(req, "default-tower")

	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	if len(result.ToolSpans) != 1 {
		t.Fatalf("expected 1 tool span, got %d", len(result.ToolSpans))
	}

	ev := result.ToolEvents[0]
	if ev.BeadID != beadID {
		t.Errorf("ToolEvent.BeadID = %q, want %q — canonical bead_id not recognized", ev.BeadID, beadID)
	}
	if ev.Step != step {
		t.Errorf("ToolEvent.Step = %q, want %q — canonical formula_step not recognized", ev.Step, step)
	}
	if ev.Tower != "ingestion-tower" {
		t.Errorf("ToolEvent.Tower = %q, want ingestion-tower", ev.Tower)
	}

	ts := result.ToolSpans[0]
	if ts.BeadID != beadID {
		t.Errorf("ToolSpan.BeadID = %q, want %q — canonical bead_id not recognized", ts.BeadID, beadID)
	}
	if ts.Step != step {
		t.Errorf("ToolSpan.Step = %q, want %q — canonical formula_step not recognized", ts.Step, step)
	}
}

// TestParseResourceSpans_LegacyAttrsStillCorrelate asserts legacy
// (bead.id, step) attrs keep working on the ingestion path while the
// fallback is retained. This is the migration-window compatibility
// guard — pre-spi-xplwy telemetry must still land bead-correlated.
func TestParseResourceSpans_LegacyAttrsStillCorrelate(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	req := []*tracepb.ResourceSpans{
		{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					strKV("bead.id", "spi-legacy-ingest"),
					strKV("step", "plan"),
					strKV("agent.name", "wizard-legacy"),
				},
			},
			ScopeSpans: []*tracepb.ScopeSpans{
				{
					Spans: []*tracepb.Span{
						{
							Name:              "Bash",
							TraceId:           []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a},
							SpanId:            []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88},
							StartTimeUnixNano: now,
							EndTimeUnixNano:   now + 10_000_000,
						},
					},
				},
			},
		},
	}

	result := ParseResourceSpans(req, "default-tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	ev := result.ToolEvents[0]
	if ev.BeadID != "spi-legacy-ingest" {
		t.Errorf("ToolEvent.BeadID = %q, want spi-legacy-ingest (legacy fallback)", ev.BeadID)
	}
	if ev.Step != "plan" {
		t.Errorf("ToolEvent.Step = %q, want plan (legacy fallback)", ev.Step)
	}
}

// TestParseLogRecords_CanonicalAttrsProduceCorrelatedRows is the log
// ingestion twin of the span test. The primary observability signal
// (logs) must also correlate with bead_id / formula_step when the
// worker emits canonical keys.
func TestParseLogRecords_CanonicalAttrsProduceCorrelatedRows(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	beadID := "spi-ingest-logs"
	step := "implement"

	rls := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{
				Attributes: append(canonicalResourceAttrs(beadID, step),
					strKV("session.id", "sess-ingest-logs")),
			},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.tool_result"}},
							Attributes: []*commonpb.KeyValue{
								strKV("tool_name", "Edit"),
								intKV("duration_ms", 150),
								boolKV("success", true),
							},
						},
						{
							TimeUnixNano: now + 1_000_000,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.api_request"}},
							Attributes: []*commonpb.KeyValue{
								strKV("model", "claude-opus-4-7"),
								intKV("input_tokens", 3000),
							},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(rls, "default-tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	if len(result.APIEvents) != 1 {
		t.Fatalf("expected 1 API event, got %d", len(result.APIEvents))
	}

	ev := result.ToolEvents[0]
	if ev.BeadID != beadID {
		t.Errorf("ToolEvent.BeadID = %q, want %q — canonical bead_id not recognized on log path", ev.BeadID, beadID)
	}
	if ev.Step != step {
		t.Errorf("ToolEvent.Step = %q, want %q — canonical formula_step not recognized on log path", ev.Step, step)
	}
	if ev.SessionID != "sess-ingest-logs" {
		t.Errorf("ToolEvent.SessionID = %q, want sess-ingest-logs", ev.SessionID)
	}

	api := result.APIEvents[0]
	if api.BeadID != beadID {
		t.Errorf("APIEvent.BeadID = %q, want %q", api.BeadID, beadID)
	}
	if api.Step != step {
		t.Errorf("APIEvent.Step = %q, want %q", api.Step, step)
	}
}

// TestParseLogRecords_CanonicalWinsOverLegacyOnIngestion is the log
// twin of the canonical-wins extractor test, exercised through the
// ParseLogRecords pipeline so emitter/receiver disagreement cannot
// hide behind a helper boundary.
func TestParseLogRecords_CanonicalWinsOverLegacyOnIngestion(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	rls := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					// Both forms present. Canonical must win.
					strKV("bead_id", "spi-canonical-wins"),
					strKV("formula_step", "review"),
					strKV("bead.id", "spi-legacy-loses"),
					strKV("step", "plan"),
				},
			},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.tool_result"}},
							Attributes: []*commonpb.KeyValue{
								strKV("tool_name", "Read"),
							},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(rls, "tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	ev := result.ToolEvents[0]
	if ev.BeadID != "spi-canonical-wins" {
		t.Errorf("BeadID = %q, want spi-canonical-wins (canonical must win over legacy)", ev.BeadID)
	}
	if ev.Step != "review" {
		t.Errorf("Step = %q, want review (canonical must win over legacy)", ev.Step)
	}
}
