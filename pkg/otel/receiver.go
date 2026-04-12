package otel

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
)

// DefaultPort is the standard OTLP gRPC port.
const DefaultPort = 4317

// knownTools is the set of tool names emitted by Claude Code and Codex that
// we record. Spans with other names are discarded (framework/HTTP internals).
var knownTools = map[string]bool{
	"Read":         true,
	"Edit":         true,
	"Write":        true,
	"Bash":         true,
	"Grep":         true,
	"Glob":         true,
	"Agent":        true,
	"WebFetch":     true,
	"WebSearch":    true,
	"NotebookEdit": true,
	"TodoWrite":    true,
	"LSP":          true,
	// Codex equivalents
	"read_file":  true,
	"write_file": true,
	"shell":      true,
	"search":     true,
}

// Receiver is a dual-signal OTLP gRPC receiver that accepts both traces and
// logs. Logs are the primary signal (both Claude and Codex), traces provide
// enrichment (Claude-only beta) with hierarchical span data.
type Receiver struct {
	collectorpb.UnimplementedTraceServiceServer
	writeFn func(fn func(*sql.Tx) error) error
	tower   string
	server  *grpc.Server
	lis     net.Listener
	port    int
}

// logsReceiver wraps the Receiver to implement the LogsServiceServer interface.
// Separate from Receiver because both TraceService and LogsService define an
// Export method with different signatures.
type logsReceiver struct {
	collogspb.UnimplementedLogsServiceServer
	r *Receiver
}

// NewReceiver creates a new OTLP receiver. writeFn is called for each batch of
// tool events to persist them to DuckDB. In daemon mode this is typically
// DuckWriter.Submit; standalone callers can use a closure over olap.WriteFunc.
// Call Start() to begin listening.
func NewReceiver(writeFn func(fn func(*sql.Tx) error) error, port int, tower string) *Receiver {
	if port <= 0 {
		port = DefaultPort
	}
	return &Receiver{
		writeFn: writeFn,
		tower:   tower,
		port:    port,
	}
}

// Start binds the gRPC server and begins accepting connections.
// Both TraceService/Export and LogsService/Export are registered.
func (r *Receiver) Start() error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", r.port))
	if err != nil {
		return fmt.Errorf("otel receiver: listen :%d: %w", r.port, err)
	}
	r.lis = lis
	r.server = grpc.NewServer()
	collectorpb.RegisterTraceServiceServer(r.server, r)
	collogspb.RegisterLogsServiceServer(r.server, &logsReceiver{r: r})

	go func() {
		if err := r.server.Serve(lis); err != nil {
			log.Printf("[otel] receiver stopped: %v", err)
		}
	}()

	log.Printf("[otel] OTLP receiver listening on :%d (traces + logs)", r.port)
	return nil
}

// Stop gracefully shuts down the gRPC server.
func (r *Receiver) Stop() {
	if r.server != nil {
		r.server.GracefulStop()
		log.Printf("[otel] receiver stopped")
	}
}

// Export implements the OTLP TraceService/Export RPC. It extracts tool-related
// spans from the request, maps them to ToolEvents (for backward compat) and
// ToolSpans (for the hierarchical waterfall view), and writes both to DuckDB.
func (r *Receiver) Export(_ context.Context, req *collectorpb.ExportTraceServiceRequest) (*collectorpb.ExportTraceServiceResponse, error) {
	var events []ToolEvent
	var spans []ToolSpan

	for _, rs := range req.GetResourceSpans() {
		resAttrs := extractResourceAttrs(rs)

		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				// Write all spans to tool_spans for the waterfall view.
				if ts, ok := spanToToolSpan(span, resAttrs, r.tower); ok {
					spans = append(spans, ts)
				}

				// Also write tool-filtered events to tool_events for backward compat.
				if event, ok := spanToToolEvent(span, resAttrs, r.tower); ok {
					events = append(events, event)
				}
			}
		}
	}

	if len(events) > 0 || len(spans) > 0 {
		if err := r.writeFn(func(tx *sql.Tx) error {
			if err := InsertBatchTx(tx, events); err != nil {
				return err
			}
			return InsertToolSpansTx(tx, spans)
		}); err != nil {
			log.Printf("[otel] write trace batch (%d events, %d spans): %v", len(events), len(spans), err)
		}
	}

	return &collectorpb.ExportTraceServiceResponse{}, nil
}

// Export implements the OTLP LogsService/Export RPC on the logsReceiver.
// This is the primary signal path — both Claude Code and Codex emit tool
// events via logs.
func (l *logsReceiver) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	result := ParseLogRecords(req.GetResourceLogs(), l.r.tower)

	if len(result.ToolEvents) > 0 || len(result.APIEvents) > 0 {
		if err := l.r.writeFn(func(tx *sql.Tx) error {
			if err := InsertBatchTx(tx, result.ToolEvents); err != nil {
				return err
			}
			return InsertAPIEventsTx(tx, result.APIEvents)
		}); err != nil {
			log.Printf("[otel] write log batch (%d tool events, %d api events): %v",
				len(result.ToolEvents), len(result.APIEvents), err)
		}
	}

	return &collogspb.ExportLogsServiceResponse{}, nil
}

// resourceAttrs holds resource-level attributes extracted from OTEL_RESOURCE_ATTRIBUTES.
type resourceAttrs struct {
	BeadID    string
	AgentName string
	Step      string
	Tower     string
	SessionID string
}

// extractResourceAttrs reads bead.id, agent.name, step, tower from the
// resource attributes that Spire injects via OTEL_RESOURCE_ATTRIBUTES at spawn.
func extractResourceAttrs(rs *tracepb.ResourceSpans) resourceAttrs {
	var attrs resourceAttrs
	res := rs.GetResource()
	if res == nil {
		return attrs
	}
	for _, kv := range res.GetAttributes() {
		val := kvStringValue(kv)
		switch kv.GetKey() {
		case "bead.id":
			attrs.BeadID = val
		case "agent.name":
			attrs.AgentName = val
		case "step":
			attrs.Step = val
		case "tower":
			attrs.Tower = val
		}
	}
	return attrs
}

// spanToToolEvent converts an OTel span to a ToolEvent if it represents a
// known tool invocation. Returns false if the span should be discarded.
func spanToToolEvent(span *tracepb.Span, res resourceAttrs, defaultTower string) (ToolEvent, bool) {
	name := span.GetName()

	// Check if the span name itself is a known tool.
	toolName := ""
	if knownTools[name] {
		toolName = name
	}

	// Check span attributes for tool_name (some providers set it as an attribute).
	if toolName == "" {
		for _, kv := range span.GetAttributes() {
			if kv.GetKey() == "tool_name" || kv.GetKey() == "tool.name" {
				candidate := kvStringValue(kv)
				if knownTools[candidate] {
					toolName = candidate
				}
				break
			}
		}
	}

	// Also match "tool_result" or "tool_call" span names with a tool_name attribute.
	if toolName == "" && (strings.HasPrefix(name, "tool_") || strings.HasPrefix(name, "tool.")) {
		for _, kv := range span.GetAttributes() {
			if kv.GetKey() == "tool_name" || kv.GetKey() == "tool.name" {
				candidate := kvStringValue(kv)
				if candidate != "" {
					toolName = candidate
				}
				break
			}
		}
	}

	if toolName == "" {
		return ToolEvent{}, false
	}

	// Compute duration from start/end timestamps (nanoseconds).
	startNano := span.GetStartTimeUnixNano()
	endNano := span.GetEndTimeUnixNano()
	var durationMs int
	if endNano > startNano {
		durationMs = int((endNano - startNano) / 1_000_000)
	}

	// Determine success from span status.
	success := true
	if st := span.GetStatus(); st != nil {
		if st.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
			success = false
		}
	}

	// Session ID from trace ID.
	sessionID := hex.EncodeToString(span.GetTraceId())

	// Timestamp from span start.
	ts := time.Unix(0, int64(startNano)).UTC()
	if startNano == 0 {
		ts = time.Now().UTC()
	}

	tower := res.Tower
	if tower == "" {
		tower = defaultTower
	}

	return ToolEvent{
		SessionID:  sessionID,
		BeadID:     res.BeadID,
		AgentName:  res.AgentName,
		Step:       res.Step,
		ToolName:   toolName,
		DurationMs: durationMs,
		Success:    success,
		Timestamp:  ts,
		Tower:      tower,
	}, true
}

// spanToToolSpan converts an OTel span to a ToolSpan for the waterfall view.
// Unlike spanToToolEvent, this keeps all spans (not just known tools) to
// preserve the full hierarchy for trace visualization.
func spanToToolSpan(span *tracepb.Span, res resourceAttrs, defaultTower string) (ToolSpan, bool) {
	name := span.GetName()
	if name == "" {
		return ToolSpan{}, false
	}

	startNano := span.GetStartTimeUnixNano()
	endNano := span.GetEndTimeUnixNano()
	var durationMs int
	if endNano > startNano {
		durationMs = int((endNano - startNano) / 1_000_000)
	}

	success := true
	if st := span.GetStatus(); st != nil {
		if st.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
			success = false
		}
	}

	startTime := time.Unix(0, int64(startNano)).UTC()
	endTime := time.Unix(0, int64(endNano)).UTC()
	if startNano == 0 {
		startTime = time.Now().UTC()
	}

	tower := res.Tower
	if tower == "" {
		tower = defaultTower
	}

	// Classify span kind from name patterns.
	kind := classifySpanKind(name, span.GetAttributes())

	// Serialize span attributes to JSON.
	attrsJSON := spanAttrsToJSON(span.GetAttributes())

	return ToolSpan{
		TraceID:      hex.EncodeToString(span.GetTraceId()),
		SpanID:       hex.EncodeToString(span.GetSpanId()),
		ParentSpanID: hex.EncodeToString(span.GetParentSpanId()),
		SessionID:    hex.EncodeToString(span.GetTraceId()),
		BeadID:       res.BeadID,
		AgentName:    res.AgentName,
		Step:         res.Step,
		SpanName:     name,
		Kind:         kind,
		DurationMs:   durationMs,
		Success:      success,
		StartTime:    startTime,
		EndTime:      endTime,
		Tower:        tower,
		Attributes:   attrsJSON,
	}, true
}

// classifySpanKind derives a span kind from the span name and attributes.
func classifySpanKind(name string, attrs []*commonpb.KeyValue) string {
	if knownTools[name] {
		return "tool"
	}
	// Check tool_name attribute.
	for _, kv := range attrs {
		if kv.GetKey() == "tool_name" || kv.GetKey() == "tool.name" {
			if kvStringValue(kv) != "" {
				return "tool"
			}
		}
	}
	if strings.HasPrefix(name, "tool_") || strings.HasPrefix(name, "tool.") {
		return "tool"
	}
	if strings.Contains(name, "llm") || strings.Contains(name, "api") ||
		strings.Contains(name, "LLM") || strings.Contains(name, "API") {
		return "llm_request"
	}
	if strings.Contains(name, "interaction") {
		return "interaction"
	}
	return "other"
}

// spanAttrsToJSON serializes span attributes to a JSON string.
func spanAttrsToJSON(attrs []*commonpb.KeyValue) string {
	if len(attrs) == 0 {
		return "{}"
	}
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		m[kv.GetKey()] = kvStringValue(kv)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// kvStringValue extracts a string value from an OTel KeyValue.
func kvStringValue(kv *commonpb.KeyValue) string {
	if kv.GetValue() == nil {
		return ""
	}
	return kv.GetValue().GetStringValue()
}
