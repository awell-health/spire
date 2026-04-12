package otel

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Test constants for reuse across integration tests.
const (
	testTower   = "test-tower"
	testRepo    = "test"
	testFormula = "task-default"
)

var testBeadIDs = []string{"test-a001", "test-a002", "test-a003", "test-a004", "test-a005"}

// TestOTelIntegration_GRPCRoundTrip starts the gRPC receiver on a random port,
// sends mock OTLP trace and log export requests, and verifies both traces and
// logs are written to DuckDB tool_events and tool_spans tables with correct
// event_kind values.
func TestOTelIntegration_GRPCRoundTrip(t *testing.T) {
	// Set up in-memory DuckDB.
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open DuckDB: %v", err)
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

	// Find a free port by briefly binding to :0, then release it.
	tmpLis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	freePort := tmpLis.Addr().(*net.TCPAddr).Port
	tmpLis.Close()

	// Start receiver on the free port.
	recv := NewReceiver(writeFn, freePort, testTower)
	if err := recv.Start(); err != nil {
		t.Fatalf("Start receiver: %v", err)
	}
	defer recv.Stop()

	addr := fmt.Sprintf("localhost:%d", freePort)

	// Connect gRPC client.
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	traceClient := collectorpb.NewTraceServiceClient(conn)
	logsClient := collogspb.NewLogsServiceClient(conn)

	ctx := context.Background()
	now := uint64(time.Now().UnixNano())

	// --- Send trace export request with tool spans (Read, Edit, Bash) ---
	traceReq := &collectorpb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{Key: "bead.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test-grpc-trace"}}},
						{Key: "agent.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "apprentice-grpc-0"}}},
						{Key: "step", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "implement"}}},
						{Key: "tower", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: testTower}}},
					},
				},
				ScopeSpans: []*tracepb.ScopeSpans{
					{
						Spans: []*tracepb.Span{
							{
								Name:              "Read",
								TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
								SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
								StartTimeUnixNano: now,
								EndTimeUnixNano:   now + 50_000_000, // 50ms
								Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
							},
							{
								Name:              "Edit",
								TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
								SpanId:            []byte{2, 3, 4, 5, 6, 7, 8, 9},
								StartTimeUnixNano: now + 50_000_000,
								EndTimeUnixNano:   now + 150_000_000, // 100ms
								Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
							},
							{
								Name:              "Bash",
								TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
								SpanId:            []byte{3, 4, 5, 6, 7, 8, 9, 10},
								StartTimeUnixNano: now + 150_000_000,
								EndTimeUnixNano:   now + 450_000_000, // 300ms
								Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
							},
						},
					},
				},
			},
		},
	}

	traceResp, err := traceClient.Export(ctx, traceReq)
	if err != nil {
		t.Fatalf("TraceService.Export: %v", err)
	}
	if traceResp == nil {
		t.Fatal("expected non-nil trace response")
	}

	// --- Send log export request with tool_result and api_request events ---
	logReq := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{Key: "bead.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test-grpc-logs"}}},
						{Key: "agent.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "apprentice-grpc-1"}}},
						{Key: "step", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "implement"}}},
						{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "sess-grpc-logs"}}},
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
									{Key: "duration_ms", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 55}}},
									{Key: "success", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
								},
							},
							{
								TimeUnixNano: now + 1_000_000,
								Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.tool_result"}},
								Attributes: []*commonpb.KeyValue{
									{Key: "tool_name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "Edit"}}},
									{Key: "duration_ms", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 120}}},
									{Key: "success", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
								},
							},
							{
								TimeUnixNano: now + 2_000_000,
								Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.api_request"}},
								Attributes: []*commonpb.KeyValue{
									{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude-opus-4-6"}}},
									{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 3000}}},
									{Key: "output_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1200}}},
									{Key: "cost_usd", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.08}}},
								},
							},
						},
					},
				},
			},
		},
	}

	logResp, err := logsClient.Export(ctx, logReq)
	if err != nil {
		t.Fatalf("LogsService.Export: %v", err)
	}
	if logResp == nil {
		t.Fatal("expected non-nil logs response")
	}

	// --- Verify trace data in tool_spans ---
	var spanCount int
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_spans WHERE bead_id = 'test-grpc-trace'",
	).Scan(&spanCount)
	if err != nil {
		t.Fatalf("count tool_spans: %v", err)
	}
	if spanCount != 3 {
		t.Errorf("expected 3 tool_spans, got %d", spanCount)
	}

	// Verify specific span names from traces.
	rows, err := db.SqlDB().QueryContext(ctx,
		"SELECT span_name, kind, duration_ms, success FROM tool_spans WHERE bead_id = 'test-grpc-trace' ORDER BY start_time",
	)
	if err != nil {
		t.Fatalf("query tool_spans: %v", err)
	}
	defer rows.Close()

	expectedSpans := []struct {
		name     string
		kind     string
		duration int
		success  bool
	}{
		{"Read", "tool", 50, true},
		{"Edit", "tool", 100, true},
		{"Bash", "tool", 300, false},
	}

	i := 0
	for rows.Next() {
		var name, kind string
		var duration int
		var success bool
		if err := rows.Scan(&name, &kind, &duration, &success); err != nil {
			t.Fatalf("scan span: %v", err)
		}
		if i >= len(expectedSpans) {
			t.Fatalf("unexpected extra span row")
		}
		exp := expectedSpans[i]
		if name != exp.name {
			t.Errorf("span %d: name=%q, want %q", i, name, exp.name)
		}
		if kind != exp.kind {
			t.Errorf("span %d: kind=%q, want %q", i, kind, exp.kind)
		}
		if duration != exp.duration {
			t.Errorf("span %d: duration=%d, want %d", i, duration, exp.duration)
		}
		if success != exp.success {
			t.Errorf("span %d: success=%v, want %v", i, success, exp.success)
		}
		i++
	}

	// Verify trace-derived tool_events (backward-compat path).
	var traceEventCount int
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_events WHERE bead_id = 'test-grpc-trace'",
	).Scan(&traceEventCount)
	if err != nil {
		t.Fatalf("count trace tool_events: %v", err)
	}
	if traceEventCount != 3 {
		t.Errorf("expected 3 trace-derived tool_events, got %d", traceEventCount)
	}

	// --- Verify log data in tool_events ---
	var logToolCount int
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_events WHERE bead_id = 'test-grpc-logs'",
	).Scan(&logToolCount)
	if err != nil {
		t.Fatalf("count log tool_events: %v", err)
	}
	if logToolCount != 2 {
		t.Errorf("expected 2 log-derived tool_events, got %d", logToolCount)
	}

	// Verify event_kind is 'tool_result' for log-derived events.
	logRows, err := db.SqlDB().QueryContext(ctx,
		"SELECT tool_name, provider, event_kind FROM tool_events WHERE bead_id = 'test-grpc-logs' ORDER BY timestamp",
	)
	if err != nil {
		t.Fatalf("query log tool_events: %v", err)
	}
	defer logRows.Close()

	expectedLogEvents := []struct {
		toolName  string
		provider  string
		eventKind string
	}{
		{"Read", "claude", "tool_result"},
		{"Edit", "claude", "tool_result"},
	}

	j := 0
	for logRows.Next() {
		var toolName, provider, eventKind string
		if err := logRows.Scan(&toolName, &provider, &eventKind); err != nil {
			t.Fatalf("scan log event: %v", err)
		}
		if j >= len(expectedLogEvents) {
			t.Fatalf("unexpected extra log event row")
		}
		exp := expectedLogEvents[j]
		if toolName != exp.toolName {
			t.Errorf("log event %d: tool_name=%q, want %q", j, toolName, exp.toolName)
		}
		if provider != exp.provider {
			t.Errorf("log event %d: provider=%q, want %q", j, provider, exp.provider)
		}
		if eventKind != exp.eventKind {
			t.Errorf("log event %d: event_kind=%q, want %q", j, eventKind, exp.eventKind)
		}
		j++
	}

	// --- Verify api_events from logs ---
	var apiCount int
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM api_events WHERE bead_id = 'test-grpc-logs'",
	).Scan(&apiCount)
	if err != nil {
		t.Fatalf("count api_events: %v", err)
	}
	if apiCount != 1 {
		t.Errorf("expected 1 api_event, got %d", apiCount)
	}

	var model string
	var inputTokens int64
	var costUSD float64
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT model, input_tokens, cost_usd FROM api_events WHERE bead_id = 'test-grpc-logs'",
	).Scan(&model, &inputTokens, &costUSD)
	if err != nil {
		t.Fatalf("scan api_event: %v", err)
	}
	if model != "claude-opus-4-6" {
		t.Errorf("api model=%q, want claude-opus-4-6", model)
	}
	if inputTokens != 3000 {
		t.Errorf("api input_tokens=%d, want 3000", inputTokens)
	}
	if costUSD != 0.08 {
		t.Errorf("api cost_usd=%f, want 0.08", costUSD)
	}

	// --- Verify event_kind distinction: traces write 'span', logs write 'tool_result' ---
	// Trace-derived events in tool_events have event_kind='' (not set from trace path).
	// Log-derived events have event_kind='tool_result'.
	var logKindCount int
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_events WHERE bead_id = 'test-grpc-logs' AND event_kind = 'tool_result'",
	).Scan(&logKindCount)
	if err != nil {
		t.Fatalf("count log event_kind: %v", err)
	}
	if logKindCount != 2 {
		t.Errorf("expected 2 events with event_kind='tool_result', got %d", logKindCount)
	}
}
