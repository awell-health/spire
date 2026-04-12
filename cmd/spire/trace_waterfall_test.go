package main

import (
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
)

func TestRenderSpanWaterfall_TreeStructure(t *testing.T) {
	now := time.Now().UTC()
	spans := []olap.SpanRecord{
		{
			TraceID:      "trace-1",
			SpanID:       "root",
			ParentSpanID: "",
			SpanName:     "interaction",
			Kind:         "interaction",
			DurationMs:   500,
			Success:      true,
			StartTime:    now,
			EndTime:      now.Add(500 * time.Millisecond),
		},
		{
			TraceID:      "trace-1",
			SpanID:       "child-1",
			ParentSpanID: "root",
			SpanName:     "Read",
			Kind:         "tool",
			DurationMs:   50,
			Success:      true,
			StartTime:    now.Add(10 * time.Millisecond),
			EndTime:      now.Add(60 * time.Millisecond),
		},
		{
			TraceID:      "trace-1",
			SpanID:       "child-2",
			ParentSpanID: "root",
			SpanName:     "Bash",
			Kind:         "tool",
			DurationMs:   100,
			Success:      false,
			StartTime:    now.Add(100 * time.Millisecond),
			EndTime:      now.Add(200 * time.Millisecond),
		},
		{
			TraceID:      "trace-1",
			SpanID:       "grandchild",
			ParentSpanID: "child-2",
			SpanName:     "llm_call",
			Kind:         "llm_request",
			DurationMs:   30,
			Success:      true,
			StartTime:    now.Add(110 * time.Millisecond),
			EndTime:      now.Add(140 * time.Millisecond),
		},
	}

	var s strings.Builder
	renderSpanWaterfall(&s, spans)
	output := s.String()

	// Verify all span names appear in output.
	for _, name := range []string{"interaction", "Read", "Bash", "llm_call"} {
		if !strings.Contains(output, name) {
			t.Errorf("output missing span name %q", name)
		}
	}

	// Verify durations appear.
	if !strings.Contains(output, "500ms") {
		t.Error("output missing 500ms duration for interaction")
	}
	if !strings.Contains(output, "50ms") {
		t.Error("output missing 50ms duration for Read")
	}
	if !strings.Contains(output, "100ms") {
		t.Error("output missing 100ms duration for Bash")
	}

	// Verify kind tags appear.
	if !strings.Contains(output, "[interaction]") {
		t.Error("output missing [interaction] kind tag")
	}
	if !strings.Contains(output, "[tool]") {
		t.Error("output missing [tool] kind tag")
	}
	if !strings.Contains(output, "[llm_request]") {
		t.Error("output missing [llm_request] kind tag")
	}

	// Verify tree indentation: grandchild should have more indentation than children.
	lines := strings.Split(output, "\n")
	var childIndent, grandchildIndent int
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if strings.Contains(line, "Read") {
			childIndent = indent
		}
		if strings.Contains(line, "llm_call") {
			grandchildIndent = indent
		}
	}
	if grandchildIndent <= childIndent {
		t.Errorf("grandchild indent (%d) should be > child indent (%d)", grandchildIndent, childIndent)
	}
}

func TestRenderSpanWaterfall_FlatFallback(t *testing.T) {
	// All spans have parent IDs that don't match any span in the set — no roots.
	// Should fall back to flat list.
	now := time.Now().UTC()
	spans := []olap.SpanRecord{
		{
			SpanID:       "orphan-1",
			ParentSpanID: "missing-parent",
			SpanName:     "Read",
			Kind:         "tool",
			DurationMs:   50,
			Success:      true,
			StartTime:    now,
			EndTime:      now.Add(50 * time.Millisecond),
		},
		{
			SpanID:       "orphan-2",
			ParentSpanID: "another-missing",
			SpanName:     "Edit",
			Kind:         "tool",
			DurationMs:   80,
			Success:      true,
			StartTime:    now.Add(100 * time.Millisecond),
			EndTime:      now.Add(180 * time.Millisecond),
		},
	}

	var s strings.Builder
	renderSpanWaterfall(&s, spans)
	output := s.String()

	if !strings.Contains(output, "Read") {
		t.Error("flat fallback should contain Read")
	}
	if !strings.Contains(output, "Edit") {
		t.Error("flat fallback should contain Edit")
	}
}

func TestRenderSpanWaterfall_RootWithZeroParent(t *testing.T) {
	// Parent span ID is all zeros — should be treated as root.
	now := time.Now().UTC()
	spans := []olap.SpanRecord{
		{
			SpanID:       "root-span",
			ParentSpanID: "0000000000000000",
			SpanName:     "root_op",
			Kind:         "other",
			DurationMs:   200,
			Success:      true,
			StartTime:    now,
			EndTime:      now.Add(200 * time.Millisecond),
		},
	}

	var s strings.Builder
	renderSpanWaterfall(&s, spans)
	output := s.String()

	if !strings.Contains(output, "root_op") {
		t.Error("zero-parent span should be treated as root and rendered")
	}
}

func TestRenderSpanWaterfall_Empty(t *testing.T) {
	var s strings.Builder
	renderSpanWaterfall(&s, nil)
	if s.Len() != 0 {
		t.Errorf("expected empty output for nil spans, got %q", s.String())
	}

	s.Reset()
	renderSpanWaterfall(&s, []olap.SpanRecord{})
	if s.Len() != 0 {
		t.Errorf("expected empty output for empty spans, got %q", s.String())
	}
}
