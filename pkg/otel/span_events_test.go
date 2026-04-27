package otel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// TestSpanAttrsAndEventsToJSON_EmptyAndAttrsOnly verifies the attribute-only
// path keeps the historical shape: top-level keys, no `events` array, and
// preserves identity context (`session.id`, `user.email`, etc).
func TestSpanAttrsAndEventsToJSON_EmptyAndAttrsOnly(t *testing.T) {
	got := spanAttrsAndEventsToJSON(nil, nil)
	if got != "{}" {
		t.Errorf("nil/nil = %q, want {}", got)
	}

	attrs := []*commonpb.KeyValue{
		strKV("session.id", "sess-x"),
		strKV("user.email", "alice@example.com"),
		strKV("tool_name", "Bash"),
	}
	out := spanAttrsAndEventsToJSON(attrs, nil)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["session.id"] != "sess-x" {
		t.Errorf("session.id = %v, want sess-x", m["session.id"])
	}
	if m["user.email"] != "alice@example.com" {
		t.Errorf("user.email = %v, want alice@example.com", m["user.email"])
	}
	if m["tool_name"] != "Bash" {
		t.Errorf("tool_name = %v, want Bash", m["tool_name"])
	}
	if _, has := m["events"]; has {
		t.Error("attrs-only path should not emit an events key")
	}
}

// TestSpanAttrsAndEventsToJSON_MergesEvents confirms span events surface
// inside attributes JSON under a top-level "events" array, with the
// per-event attribute set preserved. This is the regression hook for
// the bead's acceptance criterion: emitters that attach args to span
// events (rather than top-level attributes) must not have their data
// dropped.
func TestSpanAttrsAndEventsToJSON_MergesEvents(t *testing.T) {
	attrs := []*commonpb.KeyValue{
		strKV("session.id", "sess-y"),
		strKV("tool_name", "Bash"),
	}
	events := []*tracepb.Span_Event{
		{
			Name:         "tool_call",
			TimeUnixNano: uint64(time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC).UnixNano()),
			Attributes: []*commonpb.KeyValue{
				strKV("command", "spire graph spi-XYZ"),
				strKV("input_value", "spire graph spi-XYZ"),
			},
		},
		{
			Name:         "tool_result",
			TimeUnixNano: uint64(time.Date(2026, 4, 27, 12, 0, 1, 0, time.UTC).UnixNano()),
			Attributes: []*commonpb.KeyValue{
				strKV("output_value", "graph rendered"),
			},
		},
	}

	out := spanAttrsAndEventsToJSON(attrs, events)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, out)
	}

	// Identity context preserved at top level.
	if m["session.id"] != "sess-y" {
		t.Errorf("session.id should survive event merge; got %v", m["session.id"])
	}

	// Events array emitted.
	rawEvents, ok := m["events"]
	if !ok {
		t.Fatalf("missing events key; output=%s", out)
	}
	evs, ok := rawEvents.([]any)
	if !ok {
		t.Fatalf("events should be an array; got %T", rawEvents)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}

	first, ok := evs[0].(map[string]any)
	if !ok {
		t.Fatalf("first event should be a map; got %T", evs[0])
	}
	if first["name"] != "tool_call" {
		t.Errorf("first event name=%v, want tool_call", first["name"])
	}
	firstAttrs, ok := first["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("first.attributes should be a map; got %T", first["attributes"])
	}
	if firstAttrs["command"] != "spire graph spi-XYZ" {
		t.Errorf("event command not surfaced: got %v", firstAttrs["command"])
	}
	if firstAttrs["input_value"] != "spire graph spi-XYZ" {
		t.Errorf("event input_value not surfaced: got %v", firstAttrs["input_value"])
	}

	// Timestamp emitted as RFC3339 — readable without protobuf.
	if ts, ok := first["timestamp"].(string); !ok || !strings.Contains(ts, "2026-04-27T12:00:00") {
		t.Errorf("timestamp = %v, want 2026-04-27T12:00:00 prefix", first["timestamp"])
	}
}

// TestSpanToToolSpan_AttributesIncludeEvents exercises the receiver-level
// integration: a Span with both attributes and Span_Events ends up with
// a single attributes JSON blob containing both surfaces. Mirrors the
// real ingest path through Receiver.Export.
func TestSpanToToolSpan_AttributesIncludeEvents(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	span := &tracepb.Span{
		Name:              "Bash",
		TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
		StartTimeUnixNano: now,
		EndTimeUnixNano:   now + 200_000_000, // 200ms
		Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
		Attributes: []*commonpb.KeyValue{
			strKV("session.id", "sess-z"),
			strKV("tool_name", "Bash"),
		},
		Events: []*tracepb.Span_Event{
			{
				Name:         "tool_call",
				TimeUnixNano: now,
				Attributes: []*commonpb.KeyValue{
					strKV("command", "go test ./..."),
				},
			},
		},
	}

	ts, ok := spanToToolSpan(span, RunContext{BeadID: "spi-x"}, "tower")
	if !ok {
		t.Fatal("expected span to be accepted")
	}
	if !strings.Contains(ts.Attributes, `"events"`) {
		t.Errorf("attributes JSON missing events array: %s", ts.Attributes)
	}
	if !strings.Contains(ts.Attributes, `"command":"go test ./..."`) {
		t.Errorf("attributes JSON missing event command: %s", ts.Attributes)
	}
	// Identity context still present.
	if !strings.Contains(ts.Attributes, `"session.id":"sess-z"`) {
		t.Errorf("attributes JSON dropped session.id: %s", ts.Attributes)
	}
}
