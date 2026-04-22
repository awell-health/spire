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

func TestClaudeAdapter_Parse_HookStarted(t *testing.T) {
	line := `{"type":"system","subtype":"hook_started","hook_id":"h_01","hook_name":"SessionStart:startup","hook_event":"SessionStart"}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	if ev.Kind != KindHook {
		t.Errorf("Kind = %v, want KindHook", ev.Kind)
	}
	if ev.Title != "⚙ hook:SessionStart:startup (hook_started)" {
		t.Errorf("Title = %q", ev.Title)
	}
	if ev.Meta["hook_id"] != "h_01" {
		t.Errorf("Meta[hook_id] = %q", ev.Meta["hook_id"])
	}
	if ev.Meta["hook_event"] != "SessionStart" {
		t.Errorf("Meta[hook_event] = %q", ev.Meta["hook_event"])
	}
	if ev.Body != "" {
		t.Errorf("Body = %q, want empty", ev.Body)
	}
	if ev.Raw != line {
		t.Errorf("Raw did not preserve original line")
	}
}

func TestClaudeAdapter_Parse_HookResponse(t *testing.T) {
	line := `{"type":"system","subtype":"hook_response","hook_id":"h_01","hook_name":"PreToolUse:Bash","output":"blocked: reason\n"}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	if ev.Kind != KindHook {
		t.Errorf("Kind = %v, want KindHook", ev.Kind)
	}
	if ev.Title != "⚙ hook:PreToolUse:Bash (hook_response)" {
		t.Errorf("Title = %q", ev.Title)
	}
	if ev.Body != "blocked: reason\n" {
		t.Errorf("Body = %q, want full output", ev.Body)
	}
	if ev.Meta["hook_id"] != "h_01" {
		t.Errorf("Meta[hook_id] = %q", ev.Meta["hook_id"])
	}
}

func TestClaudeAdapter_Parse_HookMissingName(t *testing.T) {
	line := `{"type":"system","subtype":"hook_started","hook_id":"h_02"}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	if !strings.Contains(events[0].Title, "<unknown>") {
		t.Errorf("Title = %q, want fallback for missing hook_name", events[0].Title)
	}
}

func TestClaudeAdapter_Parse_RateLimitEvent_WithWaitAndReset(t *testing.T) {
	line := `{"type":"rate_limit_event","limit_type":"tokens","wait_seconds":60,"reset_at":"2026-04-22T15:00:00Z"}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	if ev.Kind != KindRateLimit {
		t.Errorf("Kind = %v, want KindRateLimit", ev.Kind)
	}
	want := "⏱ rate limit: tokens (wait 60s, reset 2026-04-22T15:00:00Z)"
	if ev.Title != want {
		t.Errorf("Title = %q, want %q", ev.Title, want)
	}
	if ev.Meta["limit_type"] != "tokens" {
		t.Errorf("Meta[limit_type] = %q", ev.Meta["limit_type"])
	}
	if ev.Meta["wait_seconds"] != "60" {
		t.Errorf("Meta[wait_seconds] = %q", ev.Meta["wait_seconds"])
	}
	if ev.Raw != line {
		t.Errorf("Raw did not preserve original line")
	}
}

func TestClaudeAdapter_Parse_RateLimitEvent_Minimal(t *testing.T) {
	line := `{"type":"rate_limit_event"}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	if ev.Kind != KindRateLimit {
		t.Errorf("Kind = %v, want KindRateLimit", ev.Kind)
	}
	if ev.Title != "⏱ rate limit" {
		t.Errorf("Title = %q, want bare fallback", ev.Title)
	}
}

func TestClaudeAdapter_Parse_ToolResult_StringSuccess(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"hello world","is_error":false}]}}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	if ev.Kind != KindToolResult {
		t.Fatalf("Kind = %v, want KindToolResult", ev.Kind)
	}
	if ev.Title != "↳ ok (11 bytes)" {
		t.Errorf("Title = %q, want '↳ ok (11 bytes)'", ev.Title)
	}
	if ev.Body != "hello world" {
		t.Errorf("Body = %q, want full content", ev.Body)
	}
	if ev.Error {
		t.Errorf("Error = true, want false")
	}
}

func TestClaudeAdapter_Parse_ToolResult_MultilineSuccess(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"line1\nline2\nline3\n","is_error":false}]}}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	if ev.Title != "↳ ok (3 lines)" {
		t.Errorf("Title = %q, want '↳ ok (3 lines)'", ev.Title)
	}
	if ev.Body != "line1\nline2\nline3\n" {
		t.Errorf("Body did not preserve full content: %q", ev.Body)
	}
}

func TestClaudeAdapter_Parse_ToolResult_Error(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"boom: exit 1\ntrailing context","is_error":true}]}}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	if !ev.Error {
		t.Errorf("Error = false, want true")
	}
	if ev.Title != "↳ err: boom: exit 1" {
		t.Errorf("Title = %q, want '↳ err: boom: exit 1'", ev.Title)
	}
	if !strings.Contains(ev.Body, "trailing context") {
		t.Errorf("Body did not preserve full content: %q", ev.Body)
	}
}

func TestClaudeAdapter_Parse_ToolResult_ErrorTruncation(t *testing.T) {
	long := strings.Repeat("x", 200)
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + long + `","is_error":true}]}}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	// Prefix "↳ err: " + 119 chars of content + "…" = 7 + 119 + len("…")
	const maxLine = 120
	// After "↳ err: " the error portion is capped at maxLine chars
	errSuffix := strings.TrimPrefix(ev.Title, "↳ err: ")
	if len([]rune(errSuffix)) > maxLine {
		t.Errorf("error title not truncated: len=%d, want <= %d", len([]rune(errSuffix)), maxLine)
	}
	if !strings.HasSuffix(ev.Title, "…") {
		t.Errorf("truncated title should end with ellipsis: %q", ev.Title)
	}
	if ev.Body != long {
		t.Errorf("Body should preserve full content despite truncated title")
	}
}

func TestClaudeAdapter_Parse_ToolResult_ErrorEmptyContent(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"","is_error":true}]}}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	if events[0].Title != "↳ err: (empty)" {
		t.Errorf("Title = %q, want '↳ err: (empty)'", events[0].Title)
	}
}

func TestClaudeAdapter_Parse_ToolResult_ArrayTextBlocks(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}],"is_error":false}]}}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	// Concatenated with newline: "first\nsecond" → 2 lines
	if ev.Title != "↳ ok (2 lines)" {
		t.Errorf("Title = %q, want '↳ ok (2 lines)'", ev.Title)
	}
	if ev.Body != "first\nsecond" {
		t.Errorf("Body = %q, want concatenated text", ev.Body)
	}
}

func TestClaudeAdapter_Parse_ToolResult_ImageOnly(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","data":"iVBOR..."}}],"is_error":false}]}}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	if ev.Title != "↳ ok (image)" {
		t.Errorf("Title = %q, want '↳ ok (image)'", ev.Title)
	}
	// Body should at least mark that there was an image for expand-all.
	if !strings.Contains(ev.Body, "[image]") {
		t.Errorf("Body should contain '[image]' marker: %q", ev.Body)
	}
}

func TestClaudeAdapter_Parse_ToolResult_MissingIsError(t *testing.T) {
	// is_error absent → treat as success
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	events, ok := (&claudeAdapter{}).Parse(line)
	if !ok || len(events) != 1 {
		t.Fatalf("Parse returned ok=%v events=%d", ok, len(events))
	}
	ev := events[0]
	if ev.Error {
		t.Errorf("Error = true, want false when is_error absent")
	}
	if !strings.HasPrefix(ev.Title, "↳ ok") {
		t.Errorf("Title = %q, want ok-prefixed", ev.Title)
	}
}

func TestClaudeAdapter_Render_ToolResultCollapsed(t *testing.T) {
	ev := LogEvent{
		Kind:  KindToolResult,
		Title: "↳ ok (5 lines)",
		Body:  "l1\nl2\nl3\nl4\nl5",
	}
	lines := (&claudeAdapter{}).Render(ev, 80, false)
	joined := strings.Join(lines, "\n")
	for _, forbidden := range []string{"l1", "l2", "l3", "l4", "l5"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("collapsed render should hide body content, got %q containing %q", joined, forbidden)
		}
	}
	if !strings.Contains(joined, "x to expand") {
		t.Errorf("collapsed render missing expand hint: %q", joined)
	}

	// Expanded: full content visible.
	lines = (&claudeAdapter{}).Render(ev, 80, true)
	joined = strings.Join(lines, "\n")
	for _, want := range []string{"l1", "l2", "l3", "l4", "l5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expanded render missing %q: %q", want, joined)
		}
	}
}

func TestClaudeAdapter_Render_ToolResultCollapsedEmptyBody(t *testing.T) {
	// No body → don't emit an expand hint.
	ev := LogEvent{Kind: KindToolResult, Title: "↳ ok (0 bytes)"}
	lines := (&claudeAdapter{}).Render(ev, 80, false)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "x to expand") {
		t.Errorf("no-body render should omit expand hint: %q", joined)
	}
}

func TestClaudeAdapter_Render_HookDim(t *testing.T) {
	ev := LogEvent{Kind: KindHook, Title: "⚙ hook:Test (hook_started)"}
	lines := (&claudeAdapter{}).Render(ev, 80, false)
	if len(lines) == 0 {
		t.Fatal("Render returned no lines")
	}
	// Dim uses color 241 in the palette; assert the styling bytes appear.
	if !strings.Contains(lines[0], "241") && !strings.Contains(lines[0], "\x1b[") {
		t.Errorf("hook title should be dim-styled: %q", lines[0])
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
