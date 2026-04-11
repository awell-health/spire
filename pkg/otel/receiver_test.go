package otel

import (
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
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
