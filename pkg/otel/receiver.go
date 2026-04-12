package otel

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

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

// Receiver is a minimal OTLP gRPC trace receiver that extracts tool invocation
// spans and writes them to DuckDB.
type Receiver struct {
	collectorpb.UnimplementedTraceServiceServer
	writeFn func(fn func(*sql.Tx) error) error
	tower   string
	server  *grpc.Server
	lis     net.Listener
	port    int
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
func (r *Receiver) Start() error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", r.port))
	if err != nil {
		return fmt.Errorf("otel receiver: listen :%d: %w", r.port, err)
	}
	r.lis = lis
	r.server = grpc.NewServer()
	collectorpb.RegisterTraceServiceServer(r.server, r)

	go func() {
		if err := r.server.Serve(lis); err != nil {
			log.Printf("[otel] receiver stopped: %v", err)
		}
	}()

	log.Printf("[otel] OTLP trace receiver listening on :%d", r.port)
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
// spans from the request, maps them to ToolEvents, and writes them to DuckDB.
func (r *Receiver) Export(_ context.Context, req *collectorpb.ExportTraceServiceRequest) (*collectorpb.ExportTraceServiceResponse, error) {
	var events []ToolEvent

	for _, rs := range req.GetResourceSpans() {
		// Extract resource-level attributes (bead.id, agent.name, step, tower).
		resAttrs := extractResourceAttrs(rs)

		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				event, ok := spanToToolEvent(span, resAttrs, r.tower)
				if !ok {
					continue
				}
				events = append(events, event)
			}
		}
	}

	if len(events) > 0 {
		if err := r.writeFn(func(tx *sql.Tx) error {
			return InsertBatchTx(tx, events)
		}); err != nil {
			log.Printf("[otel] write batch (%d events): %v", len(events), err)
			// Return success anyway — don't backpressure the provider
		}
	}

	return &collectorpb.ExportTraceServiceResponse{}, nil
}

// resourceAttrs holds resource-level attributes extracted from OTEL_RESOURCE_ATTRIBUTES.
type resourceAttrs struct {
	BeadID    string
	AgentName string
	Step      string
	Tower     string
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

// kvStringValue extracts a string value from an OTel KeyValue.
func kvStringValue(kv *commonpb.KeyValue) string {
	if kv.GetValue() == nil {
		return ""
	}
	return kv.GetValue().GetStringValue()
}
