package logstream

import (
	"os"
	"strings"
	"testing"
)

func TestClaudeAdapter_Parse_Fixture(t *testing.T) {
	data, err := os.ReadFile("testdata/claude-sample.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	events, ok := (&claudeAdapter{}).Parse(string(data))
	if !ok {
		t.Fatal("Parse returned ok=false for valid Claude fixture")
	}

	wantKinds := []EventKind{
		KindSessionStart,
		KindPrompt,
		KindAssistantText,
		KindToolCall,
		KindToolResult,
		KindToolCall,
		KindToolResult,
		KindUnknown,
		KindUsage,
		KindFinal,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("got %d events, want %d; events=%v", len(events), len(wantKinds), kindsOf(events))
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Errorf("events[%d].Kind = %v, want %v", i, events[i].Kind, want)
		}
	}

	// Exactly one KindUnknown, and it's the malformed line verbatim.
	var unknownCount int
	for _, e := range events {
		if e.Kind == KindUnknown {
			unknownCount++
			if e.Raw != "{this line is deliberately malformed" {
				t.Errorf("unknown event Raw = %q, want malformed line", e.Raw)
			}
		}
	}
	if unknownCount != 1 {
		t.Errorf("unknown event count = %d, want 1", unknownCount)
	}

	// No event should reference stream_event in its Title (they must have
	// been dropped, not routed to unknown).
	for _, e := range events {
		if strings.Contains(e.Title, "stream_event") {
			t.Errorf("stream_event leaked into Title: %q", e.Title)
		}
	}

	// Tool-result error flag: second KindToolResult is the errored one.
	var toolResults []LogEvent
	for _, e := range events {
		if e.Kind == KindToolResult {
			toolResults = append(toolResults, e)
		}
	}
	if len(toolResults) != 2 {
		t.Fatalf("got %d tool_result events, want 2", len(toolResults))
	}
	if toolResults[0].Error {
		t.Error("first tool_result should have Error=false")
	}
	if !toolResults[1].Error {
		t.Error("second tool_result should have Error=true")
	}

	// Tool calls carry name + id in Meta.
	var toolCalls []LogEvent
	for _, e := range events {
		if e.Kind == KindToolCall {
			toolCalls = append(toolCalls, e)
		}
	}
	if len(toolCalls) != 2 {
		t.Fatalf("got %d tool_call events, want 2", len(toolCalls))
	}
	for i, tc := range toolCalls {
		if tc.Meta["tool_name"] != "Bash" {
			t.Errorf("toolCalls[%d].Meta[tool_name] = %q, want Bash", i, tc.Meta["tool_name"])
		}
		if tc.Meta["tool_use_id"] == "" {
			t.Errorf("toolCalls[%d].Meta[tool_use_id] is empty", i)
		}
	}

	// Session start carries session_id.
	if events[0].Meta["session_id"] != "sess_01" {
		t.Errorf("session_start.Meta[session_id] = %q, want sess_01", events[0].Meta["session_id"])
	}

	// Usage carries input/output token counts.
	var usage LogEvent
	for _, e := range events {
		if e.Kind == KindUsage {
			usage = e
			break
		}
	}
	if usage.Meta["input_tokens"] != "100" {
		t.Errorf("usage.Meta[input_tokens] = %q, want 100", usage.Meta["input_tokens"])
	}
	if usage.Meta["output_tokens"] != "50" {
		t.Errorf("usage.Meta[output_tokens] = %q, want 50", usage.Meta["output_tokens"])
	}
}

func TestClaudeAdapter_Parse_NotClaude(t *testing.T) {
	events, ok := (&claudeAdapter{}).Parse("not json at all\nanother line\n")
	if ok {
		t.Errorf("Parse returned ok=true for non-Claude input, events=%v", events)
	}
}

func TestClaudeAdapter_Parse_EmptyLinesSkipped(t *testing.T) {
	raw := "\n\n" + `{"type":"system","subtype":"init","session_id":"s","cwd":"/tmp"}` + "\n\n"
	events, ok := (&claudeAdapter{}).Parse(raw)
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Kind != KindSessionStart {
		t.Errorf("Kind = %v, want KindSessionStart", events[0].Kind)
	}
}

func TestClaudeAdapter_Render_Collapses(t *testing.T) {
	ev := LogEvent{Kind: KindAssistantText, Body: "l1\nl2\nl3\nl4\nl5"}

	lines := (&claudeAdapter{}).Render(ev, 80, false)
	if len(lines) == 0 {
		t.Fatal("Render returned no lines")
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, "2 more") {
		t.Errorf("collapsed last line %q does not contain '2 more'", last)
	}
	if !strings.Contains(last, "expand") {
		t.Errorf("collapsed last line %q does not contain 'expand'", last)
	}

	lines = (&claudeAdapter{}).Render(ev, 80, true)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "more") {
		t.Errorf("expanded render still contains 'more' marker: %q", joined)
	}
	for _, want := range []string{"l1", "l2", "l3", "l4", "l5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expanded render missing %q: %q", want, joined)
		}
	}
}

func TestClaudeAdapter_Render_ToolResultError(t *testing.T) {
	ev := LogEvent{Kind: KindToolResult, Title: "← tool result (error)", Error: true}
	lines := (&claudeAdapter{}).Render(ev, 80, false)
	if len(lines) == 0 {
		t.Fatal("Render returned no lines")
	}
	// Assert some styling was applied — either the warning color code or
	// at least an ANSI escape sequence. Exact bytes depend on lipgloss.
	if !strings.Contains(lines[0], "214") && !strings.Contains(lines[0], "\x1b[") {
		t.Errorf("rendered first line %q has no detectable styling", lines[0])
	}
}

// kindsOf is a tiny helper for diagnostic output.
func kindsOf(events []LogEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Kind.String()
	}
	return out
}
