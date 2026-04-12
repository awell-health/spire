package otel

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestSpanToToolEvent_KnownTool(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	span := &tracepb.Span{
		Name:               "Read",
		TraceId:            []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		StartTimeUnixNano:  now,
		EndTimeUnixNano:    now + 50_000_000, // 50ms
		Status:             &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
	}

	res := resourceAttrs{
		BeadID:    "spi-abc",
		AgentName: "apprentice-spi-abc-0",
		Step:      "implement",
		Tower:     "test-tower",
	}

	event, ok := spanToToolEvent(span, res, "default-tower")
	if !ok {
		t.Fatal("expected span to be recognized as tool event")
	}
	if event.ToolName != "Read" {
		t.Errorf("tool name = %q, want Read", event.ToolName)
	}
	if event.DurationMs != 50 {
		t.Errorf("duration = %d, want 50", event.DurationMs)
	}
	if !event.Success {
		t.Error("expected success=true")
	}
	if event.BeadID != "spi-abc" {
		t.Errorf("bead_id = %q, want spi-abc", event.BeadID)
	}
	if event.Tower != "test-tower" {
		t.Errorf("tower = %q, want test-tower (from resource)", event.Tower)
	}
}

func TestSpanToToolEvent_UnknownSpanDiscarded(t *testing.T) {
	span := &tracepb.Span{
		Name: "http.request",
	}
	_, ok := spanToToolEvent(span, resourceAttrs{}, "tower")
	if ok {
		t.Error("expected unknown span to be discarded")
	}
}

func TestSpanToToolEvent_ToolNameAttribute(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	span := &tracepb.Span{
		Name:              "tool_result",
		TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		StartTimeUnixNano: now,
		EndTimeUnixNano:   now + 100_000_000,
		Attributes: []*commonpb.KeyValue{
			{Key: "tool_name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "Bash"}}},
		},
		Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
	}

	event, ok := spanToToolEvent(span, resourceAttrs{Step: "review"}, "tower")
	if !ok {
		t.Fatal("expected tool_result span with tool_name=Bash to be recognized")
	}
	if event.ToolName != "Bash" {
		t.Errorf("tool name = %q, want Bash", event.ToolName)
	}
	if event.Success {
		t.Error("expected success=false for error status")
	}
	if event.DurationMs != 100 {
		t.Errorf("duration = %d, want 100", event.DurationMs)
	}
}

func TestSpanToToolEvent_FallbackTower(t *testing.T) {
	span := &tracepb.Span{
		Name: "Edit",
		StartTimeUnixNano: uint64(time.Now().UnixNano()),
		EndTimeUnixNano:   uint64(time.Now().UnixNano()) + 10_000_000,
	}
	event, ok := spanToToolEvent(span, resourceAttrs{}, "fallback-tower")
	if !ok {
		t.Fatal("expected Edit span to be recognized")
	}
	if event.Tower != "fallback-tower" {
		t.Errorf("tower = %q, want fallback-tower", event.Tower)
	}
}

func TestExtractResourceAttrs(t *testing.T) {
	rs := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				{Key: "bead.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "spi-xyz"}}},
				{Key: "agent.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "wizard-spi-xyz"}}},
				{Key: "step", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "plan"}}},
				{Key: "tower", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "my-tower"}}},
			},
		},
	}

	attrs := extractResourceAttrs(rs)
	if attrs.BeadID != "spi-xyz" {
		t.Errorf("bead_id = %q, want spi-xyz", attrs.BeadID)
	}
	if attrs.AgentName != "wizard-spi-xyz" {
		t.Errorf("agent_name = %q, want wizard-spi-xyz", attrs.AgentName)
	}
	if attrs.Step != "plan" {
		t.Errorf("step = %q, want plan", attrs.Step)
	}
	if attrs.Tower != "my-tower" {
		t.Errorf("tower = %q, want my-tower", attrs.Tower)
	}
}

// --- spanToToolSpan ---

func TestSpanToToolSpan_Basic(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	span := &tracepb.Span{
		Name:              "Read",
		TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
		ParentSpanId:      []byte{8, 7, 6, 5, 4, 3, 2, 1},
		StartTimeUnixNano: now,
		EndTimeUnixNano:   now + 75_000_000, // 75ms
		Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
		Attributes: []*commonpb.KeyValue{
			{Key: "file.path", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "/foo/bar.go"}}},
		},
	}

	res := resourceAttrs{
		BeadID:    "spi-test",
		AgentName: "apprentice-0",
		Step:      "implement",
		Tower:     "test-tower",
	}

	ts, ok := spanToToolSpan(span, res, "default-tower")
	if !ok {
		t.Fatal("expected span to be accepted")
	}
	if ts.SpanName != "Read" {
		t.Errorf("SpanName = %q, want Read", ts.SpanName)
	}
	if ts.Kind != "tool" {
		t.Errorf("Kind = %q, want tool", ts.Kind)
	}
	if ts.DurationMs != 75 {
		t.Errorf("DurationMs = %d, want 75", ts.DurationMs)
	}
	if !ts.Success {
		t.Error("expected Success=true")
	}
	if ts.BeadID != "spi-test" {
		t.Errorf("BeadID = %q, want spi-test", ts.BeadID)
	}
	if ts.Tower != "test-tower" {
		t.Errorf("Tower = %q, want test-tower (from resource)", ts.Tower)
	}
	if ts.ParentSpanID == "" {
		t.Error("expected non-empty ParentSpanID")
	}
	// Verify attributes JSON contains the file path.
	if ts.Attributes == "{}" || ts.Attributes == "" {
		t.Error("expected non-empty attributes JSON")
	}
}

func TestSpanToToolSpan_EmptyNameDiscarded(t *testing.T) {
	span := &tracepb.Span{
		Name: "",
	}
	_, ok := spanToToolSpan(span, resourceAttrs{}, "tower")
	if ok {
		t.Error("expected empty-name span to be discarded")
	}
}

func TestSpanToToolSpan_ErrorStatus(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	span := &tracepb.Span{
		Name:              "Bash",
		TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
		StartTimeUnixNano: now,
		EndTimeUnixNano:   now + 200_000_000,
		Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
	}

	ts, ok := spanToToolSpan(span, resourceAttrs{}, "tower")
	if !ok {
		t.Fatal("expected span to be accepted")
	}
	if ts.Success {
		t.Error("expected Success=false for error status")
	}
	if ts.DurationMs != 200 {
		t.Errorf("DurationMs = %d, want 200", ts.DurationMs)
	}
}

func TestSpanToToolSpan_FallbackTower(t *testing.T) {
	span := &tracepb.Span{
		Name:              "interaction",
		TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
		StartTimeUnixNano: uint64(time.Now().UnixNano()),
		EndTimeUnixNano:   uint64(time.Now().UnixNano()) + 10_000_000,
	}
	ts, ok := spanToToolSpan(span, resourceAttrs{}, "fallback-tower")
	if !ok {
		t.Fatal("expected span to be accepted")
	}
	if ts.Tower != "fallback-tower" {
		t.Errorf("Tower = %q, want fallback-tower", ts.Tower)
	}
}

// --- classifySpanKind ---

func TestClassifySpanKind(t *testing.T) {
	tests := []struct {
		name  string
		attrs []*commonpb.KeyValue
		want  string
	}{
		// Known tool names
		{"Read", nil, "tool"},
		{"Edit", nil, "tool"},
		{"Bash", nil, "tool"},
		{"Grep", nil, "tool"},
		{"Glob", nil, "tool"},
		{"Write", nil, "tool"},
		{"Agent", nil, "tool"},
		{"read_file", nil, "tool"},
		{"shell", nil, "tool"},
		// tool_name attribute
		{"some_span", []*commonpb.KeyValue{
			{Key: "tool_name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "Read"}}},
		}, "tool"},
		{"some_span", []*commonpb.KeyValue{
			{Key: "tool.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "Edit"}}},
		}, "tool"},
		// tool_ prefix
		{"tool_result", nil, "tool"},
		{"tool.call", nil, "tool"},
		// LLM/API
		{"llm_request", nil, "llm_request"},
		{"api_call", nil, "llm_request"},
		{"send_LLM_request", nil, "llm_request"},
		{"call_API", nil, "llm_request"},
		// Interaction
		{"user_interaction", nil, "interaction"},
		{"claude_code.interaction", nil, "interaction"},
		// Other
		{"http.request", nil, "other"},
		{"some_internal_span", nil, "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifySpanKind(tt.name, tt.attrs)
			if got != tt.want {
				t.Errorf("classifySpanKind(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// --- logsReceiver.Export round-trip ---

func TestLogsReceiverExport_RoundTrip(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	writeFn := func(fn func(*sql.Tx) error) error {
		return db.WithWriteLock(func(sqlDB *sql.DB) error {
			tx, err := sqlDB.BeginTx(context.Background(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := fn(tx); err != nil {
				return err
			}
			return tx.Commit()
		})
	}

	recv := NewReceiver(writeFn, 0, "test-tower")
	lr := &logsReceiver{r: recv}

	now := uint64(time.Now().UnixNano())

	req := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{Key: "bead.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "spi-roundtrip"}}},
						{Key: "agent.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "apprentice-0"}}},
						{Key: "step", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "implement"}}},
						{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "sess-rt"}}},
					},
				},
				ScopeLogs: []*logspb.ScopeLogs{
					{
						LogRecords: []*logspb.LogRecord{
							{
								TimeUnixNano: now,
								Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.tool_result"}},
								Attributes: []*commonpb.KeyValue{
									{Key: "tool_name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "Read"}}},
									{Key: "duration_ms", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 42}}},
									{Key: "success", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
								},
							},
							{
								TimeUnixNano: now + 1_000_000,
								Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.api_request"}},
								Attributes: []*commonpb.KeyValue{
									{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude-opus-4-6"}}},
									{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 2000}}},
									{Key: "output_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 800}}},
									{Key: "cost_usd", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.05}}},
								},
							},
						},
					},
				},
			},
		},
	}

	resp, err := lr.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Verify tool_events row.
	ctx := context.Background()
	var toolName string
	var durationMs int
	var success bool
	var beadID string
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT tool_name, duration_ms, success, bead_id FROM tool_events WHERE bead_id = 'spi-roundtrip'",
	).Scan(&toolName, &durationMs, &success, &beadID)
	if err != nil {
		t.Fatalf("query tool_events: %v", err)
	}
	if toolName != "Read" {
		t.Errorf("tool_name = %q, want Read", toolName)
	}
	if durationMs != 42 {
		t.Errorf("duration_ms = %d, want 42", durationMs)
	}
	if !success {
		t.Error("expected success=true")
	}

	// Verify provider and event_kind columns on tool_events row.
	var provider, eventKind string
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT provider, event_kind FROM tool_events WHERE bead_id = 'spi-roundtrip'",
	).Scan(&provider, &eventKind)
	if err != nil {
		t.Fatalf("query tool_events provider/event_kind: %v", err)
	}
	if provider != "claude" {
		t.Errorf("provider = %q, want claude", provider)
	}
	if eventKind != "tool_result" {
		t.Errorf("event_kind = %q, want tool_result", eventKind)
	}

	// Verify api_events row.
	var model string
	var inputTokens int64
	var costUSD float64
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT model, input_tokens, cost_usd FROM api_events WHERE bead_id = 'spi-roundtrip'",
	).Scan(&model, &inputTokens, &costUSD)
	if err != nil {
		t.Fatalf("query api_events: %v", err)
	}
	if model != "claude-opus-4-6" {
		t.Errorf("model = %q, want claude-opus-4-6", model)
	}
	if inputTokens != 2000 {
		t.Errorf("input_tokens = %d, want 2000", inputTokens)
	}
	if costUSD != 0.05 {
		t.Errorf("cost_usd = %f, want 0.05", costUSD)
	}
}

// TestLogsReceiverExport_CodexRoundTrip verifies that Codex log events
// (read_file, write_file, shell, search) are correctly parsed and written
// to DuckDB through the full receiver pipeline.
func TestLogsReceiverExport_CodexRoundTrip(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	writeFn := func(fn func(*sql.Tx) error) error {
		return db.WithWriteLock(func(sqlDB *sql.DB) error {
			tx, err := sqlDB.BeginTx(context.Background(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := fn(tx); err != nil {
				return err
			}
			return tx.Commit()
		})
	}

	recv := NewReceiver(writeFn, 0, "test-tower")
	lr := &logsReceiver{r: recv}

	now := uint64(time.Now().UnixNano())
	codexTools := []string{"read_file", "write_file", "shell", "search"}
	var records []*logspb.LogRecord
	for i, tool := range codexTools {
		records = append(records, &logspb.LogRecord{
			TimeUnixNano: now + uint64(i)*1_000_000,
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.tool_result"}},
			Attributes: []*commonpb.KeyValue{
				{Key: "tool_name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: tool}}},
				{Key: "duration_ms", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64((i + 1) * 10)}}},
				{Key: "success", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
			},
		})
	}

	req := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{Key: "bead.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "spi-codex-rt"}}},
						{Key: "agent.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "apprentice-codex-0"}}},
						{Key: "step", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "implement"}}},
						{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "sess-codex-rt"}}},
					},
				},
				ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}},
			},
		},
	}

	resp, err := lr.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Verify all 4 Codex tool events were written.
	ctx := context.Background()
	var count int
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_events WHERE bead_id = 'spi-codex-rt'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 4 {
		t.Errorf("expected 4 tool events, got %d", count)
	}

	// Verify each tool was written with correct provider and event_kind.
	rows, err := db.SqlDB().QueryContext(ctx,
		"SELECT tool_name, provider, event_kind, step FROM tool_events WHERE bead_id = 'spi-codex-rt' ORDER BY timestamp",
	)
	if err != nil {
		t.Fatalf("query rows: %v", err)
	}
	defer rows.Close()

	var gotTools []string
	for rows.Next() {
		var toolName, provider, eventKind, step string
		if err := rows.Scan(&toolName, &provider, &eventKind, &step); err != nil {
			t.Fatalf("scan: %v", err)
		}
		gotTools = append(gotTools, toolName)
		if provider != "codex" {
			t.Errorf("tool %s: provider = %q, want codex", toolName, provider)
		}
		if eventKind != "tool_result" {
			t.Errorf("tool %s: event_kind = %q, want tool_result", toolName, eventKind)
		}
		if step != "implement" {
			t.Errorf("tool %s: step = %q, want implement", toolName, step)
		}
	}
	if len(gotTools) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(gotTools))
	}
	for i, want := range codexTools {
		if gotTools[i] != want {
			t.Errorf("row %d: tool_name = %q, want %q", i, gotTools[i], want)
		}
	}
}

// TestInsertBatchTx_LogParsedEvents verifies that events parsed from log
// records (with provider and event_kind set) are correctly written to the
// tool_events DuckDB table.
func TestInsertBatchTx_LogParsedEvents(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	events := []ToolEvent{
		{
			SessionID:  "sess-log-1",
			BeadID:     "spi-logtest",
			AgentName:  "apprentice-0",
			Step:       "implement",
			ToolName:   "Read",
			DurationMs: 42,
			Success:    true,
			Timestamp:  now,
			Tower:      "test-tower",
			Provider:   "claude",
			EventKind:  "tool_result",
		},
		{
			SessionID:  "sess-log-1",
			BeadID:     "spi-logtest",
			AgentName:  "apprentice-0",
			Step:       "implement",
			ToolName:   "shell",
			DurationMs: 300,
			Success:    false,
			Timestamp:  now.Add(time.Second),
			Tower:      "test-tower",
			Provider:   "codex",
			EventKind:  "tool_result",
		},
		{
			SessionID:  "sess-log-1",
			BeadID:     "spi-logtest",
			AgentName:  "apprentice-0",
			Step:       "implement",
			ToolName:   "Bash",
			Timestamp:  now.Add(2 * time.Second),
			Tower:      "test-tower",
			Provider:   "claude",
			EventKind:  "tool_decision",
		},
	}

	err = db.WithWriteLock(func(sqlDB *sql.DB) error {
		tx, err := sqlDB.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := InsertBatchTx(tx, events); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		t.Fatalf("InsertBatchTx: %v", err)
	}

	// Verify all 3 events were inserted.
	ctx := context.Background()
	var count int
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_events WHERE bead_id = 'spi-logtest'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 rows, got %d", count)
	}

	// Verify provider and event_kind are stored correctly.
	var provider, eventKind string
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT provider, event_kind FROM tool_events WHERE bead_id = 'spi-logtest' AND tool_name = 'Read'",
	).Scan(&provider, &eventKind)
	if err != nil {
		t.Fatalf("scan Read: %v", err)
	}
	if provider != "claude" {
		t.Errorf("Read provider = %q, want claude", provider)
	}
	if eventKind != "tool_result" {
		t.Errorf("Read event_kind = %q, want tool_result", eventKind)
	}

	// Verify Codex event.
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT provider, event_kind FROM tool_events WHERE bead_id = 'spi-logtest' AND tool_name = 'shell'",
	).Scan(&provider, &eventKind)
	if err != nil {
		t.Fatalf("scan shell: %v", err)
	}
	if provider != "codex" {
		t.Errorf("shell provider = %q, want codex", provider)
	}
	if eventKind != "tool_result" {
		t.Errorf("shell event_kind = %q, want tool_result", eventKind)
	}

	// Verify tool_decision event kind.
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT provider, event_kind FROM tool_events WHERE bead_id = 'spi-logtest' AND tool_name = 'Bash'",
	).Scan(&provider, &eventKind)
	if err != nil {
		t.Fatalf("scan Bash: %v", err)
	}
	if provider != "claude" {
		t.Errorf("Bash provider = %q, want claude", provider)
	}
	if eventKind != "tool_decision" {
		t.Errorf("Bash event_kind = %q, want tool_decision", eventKind)
	}
}
