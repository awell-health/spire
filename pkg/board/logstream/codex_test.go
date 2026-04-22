package logstream

import (
	"strings"
	"testing"
)

func TestCodexAdapter_Parse_NoToolHappyPath(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"thread.started","thread_id":"th_01"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"it_1","type":"agent_message","text":"hello"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3}}`,
	}, "\n")

	events, ok := (&codexAdapter{}).Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false for valid Codex fixture")
	}

	wantKinds := []EventKind{
		KindSessionStart,
		KindTurnStart,
		KindAssistantText,
		KindUsage,
		KindFinal,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("got %d events, want %d; kinds=%v", len(events), len(wantKinds), kindsOf(events))
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Errorf("events[%d].Kind = %v, want %v", i, events[i].Kind, want)
		}
	}

	if events[0].Title != "codex session" {
		t.Errorf("session.Title = %q, want %q", events[0].Title, "codex session")
	}
	if events[0].Meta["thread_id"] != "th_01" {
		t.Errorf("session.Meta[thread_id] = %q, want th_01", events[0].Meta["thread_id"])
	}
	if events[1].Title != "turn started" {
		t.Errorf("turn-start.Title = %q, want %q", events[1].Title, "turn started")
	}
	if events[2].Body != "hello" {
		t.Errorf("assistant.Body = %q, want %q", events[2].Body, "hello")
	}

	usage := events[3]
	if usage.Meta["input_tokens"] != "10" {
		t.Errorf("usage.Meta[input_tokens] = %q, want 10", usage.Meta["input_tokens"])
	}
	if usage.Meta["cached_input_tokens"] != "4" {
		t.Errorf("usage.Meta[cached_input_tokens] = %q, want 4", usage.Meta["cached_input_tokens"])
	}
	if usage.Meta["output_tokens"] != "3" {
		t.Errorf("usage.Meta[output_tokens] = %q, want 3", usage.Meta["output_tokens"])
	}
	if !strings.Contains(usage.Body, "input=10") || !strings.Contains(usage.Body, "cached=4") || !strings.Contains(usage.Body, "output=3") {
		t.Errorf("usage.Body = %q, want to contain input=10, cached=4, output=3", usage.Body)
	}
	if events[4].Title != "turn complete" {
		t.Errorf("final.Title = %q, want %q", events[4].Title, "turn complete")
	}
}

func TestCodexAdapter_Parse_ToolUsingRun(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"thread.started","thread_id":"th_02"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"it_1","type":"command_execution","command":"pwd","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"it_1","type":"command_execution","command":"pwd","aggregated_output":"/private/tmp\n","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"it_2","type":"agent_message","text":"Done."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":42,"cached_input_tokens":0,"output_tokens":8}}`,
	}, "\n")

	events, ok := (&codexAdapter{}).Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false for valid Codex tool fixture")
	}

	wantKinds := []EventKind{
		KindSessionStart,
		KindTurnStart,
		KindToolCall,
		KindToolResult,
		KindAssistantText,
		KindUsage,
		KindFinal,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("got %d events, want %d; kinds=%v", len(events), len(wantKinds), kindsOf(events))
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Errorf("events[%d].Kind = %v, want %v", i, events[i].Kind, want)
		}
	}

	call := events[2]
	if !strings.HasPrefix(call.Title, "⚙ $ ") {
		t.Errorf("tool-call.Title = %q, want prefix %q", call.Title, "⚙ $ ")
	}
	if !strings.Contains(call.Title, "pwd") {
		t.Errorf("tool-call.Title = %q, want to contain pwd", call.Title)
	}
	if call.Meta["item_id"] != "it_1" {
		t.Errorf("tool-call.Meta[item_id] = %q, want it_1", call.Meta["item_id"])
	}
	if call.Meta["status"] != "in_progress" {
		t.Errorf("tool-call.Meta[status] = %q, want in_progress", call.Meta["status"])
	}

	result := events[3]
	if result.Error {
		t.Error("tool-result.Error = true, want false for exit 0")
	}
	if result.Body != "/private/tmp\n" {
		t.Errorf("tool-result.Body = %q, want %q", result.Body, "/private/tmp\n")
	}
	if result.Meta["exit_code"] != "0" {
		t.Errorf("tool-result.Meta[exit_code] = %q, want 0", result.Meta["exit_code"])
	}
	if result.Meta["item_id"] != "it_1" {
		t.Errorf("tool-result.Meta[item_id] = %q, want it_1", result.Meta["item_id"])
	}
	if !strings.Contains(result.Title, "exit 0") {
		t.Errorf("tool-result.Title = %q, want to contain 'exit 0'", result.Title)
	}
}

func TestCodexAdapter_Parse_NonZeroExit(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"thread.started","thread_id":"th_03"}`,
		`{"type":"item.completed","item":{"id":"it_1","type":"command_execution","command":"false","aggregated_output":"","exit_code":2,"status":"completed"}}`,
	}, "\n")

	events, ok := (&codexAdapter{}).Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false")
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}

	result := events[1]
	if result.Kind != KindToolResult {
		t.Fatalf("events[1].Kind = %v, want KindToolResult", result.Kind)
	}
	if !result.Error {
		t.Error("tool-result.Error = false, want true for exit 2")
	}
	if !strings.Contains(result.Title, "exit 2") {
		t.Errorf("tool-result.Title = %q, want to contain 'exit 2'", result.Title)
	}
	if result.Meta["exit_code"] != "2" {
		t.Errorf("tool-result.Meta[exit_code] = %q, want 2", result.Meta["exit_code"])
	}
}

func TestCodexAdapter_Parse_NilExitCode(t *testing.T) {
	raw := `{"type":"thread.started","thread_id":"th_04"}` + "\n" +
		`{"type":"item.completed","item":{"id":"it_1","type":"command_execution","command":"sleep","aggregated_output":"","status":"timeout"}}`

	events, ok := (&codexAdapter{}).Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false")
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	result := events[1]
	if result.Kind != KindToolResult {
		t.Fatalf("events[1].Kind = %v, want KindToolResult", result.Kind)
	}
	if result.Error {
		t.Error("tool-result.Error = true, want false when ExitCode is nil")
	}
	if !strings.Contains(result.Title, "exit ?") {
		t.Errorf("tool-result.Title = %q, want to contain 'exit ?'", result.Title)
	}
	if _, present := result.Meta["exit_code"]; present {
		t.Errorf("tool-result.Meta[exit_code] should be absent when ExitCode is nil, got %q", result.Meta["exit_code"])
	}
}

func TestCodexAdapter_Parse_MalformedLineBetweenValid(t *testing.T) {
	badLine := `{this is not json`
	raw := strings.Join([]string{
		`{"type":"thread.started","thread_id":"th_05"}`,
		badLine,
		`{"type":"turn.started"}`,
	}, "\n")

	events, ok := (&codexAdapter{}).Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false; surrounding events should make it recognized")
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3; kinds=%v", len(events), kindsOf(events))
	}
	if events[0].Kind != KindSessionStart {
		t.Errorf("events[0].Kind = %v, want KindSessionStart", events[0].Kind)
	}
	if events[1].Kind != KindUnknown {
		t.Errorf("events[1].Kind = %v, want KindUnknown", events[1].Kind)
	}
	if events[1].Raw != badLine {
		t.Errorf("events[1].Raw = %q, want %q", events[1].Raw, badLine)
	}
	if events[2].Kind != KindTurnStart {
		t.Errorf("events[2].Kind = %v, want KindTurnStart", events[2].Kind)
	}
}

func TestCodexAdapter_Parse_UnknownTopLevelType(t *testing.T) {
	raw := `{"type":"thread.started","thread_id":"th_06"}` + "\n" +
		`{"type":"mystery","foo":"bar"}`

	events, ok := (&codexAdapter{}).Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false; earlier line recognized the stream as Codex")
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2; kinds=%v", len(events), kindsOf(events))
	}
	if events[1].Kind != KindUnknown {
		t.Errorf("events[1].Kind = %v, want KindUnknown", events[1].Kind)
	}
	if events[1].Raw != `{"type":"mystery","foo":"bar"}` {
		t.Errorf("events[1].Raw = %q, want the mystery line", events[1].Raw)
	}
}

func TestCodexAdapter_Parse_UnknownItemType(t *testing.T) {
	// agent_reasoning is not handled; it must surface as KindUnknown rather
	// than being silently dropped.
	raw := `{"type":"thread.started","thread_id":"th_07"}` + "\n" +
		`{"type":"item.completed","item":{"id":"it_1","type":"agent_reasoning","text":"thought"}}`

	events, ok := (&codexAdapter{}).Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false")
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2; kinds=%v", len(events), kindsOf(events))
	}
	if events[1].Kind != KindUnknown {
		t.Errorf("events[1].Kind = %v, want KindUnknown", events[1].Kind)
	}
}

func TestCodexAdapter_Parse_UnknownItemTypeOnStarted(t *testing.T) {
	// item.started with a non-command_execution item should emit KindUnknown
	// — only command_execution is a real started event.
	raw := `{"type":"thread.started","thread_id":"th_08"}` + "\n" +
		`{"type":"item.started","item":{"id":"it_1","type":"agent_message"}}`

	events, ok := (&codexAdapter{}).Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false")
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[1].Kind != KindUnknown {
		t.Errorf("events[1].Kind = %v, want KindUnknown", events[1].Kind)
	}
}

func TestCodexAdapter_Parse_NonCodexPlaintext(t *testing.T) {
	events, ok := (&codexAdapter{}).Parse("hello world\nanother plaintext line\n")
	if ok {
		t.Errorf("Parse returned ok=true for non-Codex plaintext; events=%v", kindsOf(events))
	}
}

func TestCodexAdapter_Parse_EmptyString(t *testing.T) {
	events, ok := (&codexAdapter{}).Parse("")
	if ok {
		t.Errorf("Parse returned ok=true for empty string; events=%v", kindsOf(events))
	}
	if events != nil {
		t.Errorf("events = %v, want nil", events)
	}
}

func TestCodexAdapter_Parse_BlankLinesSkipped(t *testing.T) {
	raw := "\n\n" + `{"type":"thread.started","thread_id":"th_09"}` + "\n\n" +
		`{"type":"turn.started"}` + "\n\n"

	events, ok := (&codexAdapter{}).Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false")
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (blank lines must be skipped); kinds=%v", len(events), kindsOf(events))
	}
	for _, e := range events {
		if e.Kind == KindUnknown {
			t.Errorf("unexpected KindUnknown from blank-line input: Raw=%q", e.Raw)
		}
	}
}

func TestCodexAdapter_Parse_NonCodexJSON(t *testing.T) {
	// Valid JSON but no recognized Codex type — should return (nil, false).
	raw := `{"foo":"bar"}` + "\n" + `{"baz":1}`
	events, ok := (&codexAdapter{}).Parse(raw)
	if ok {
		t.Errorf("Parse returned ok=true for non-Codex JSON; events=%v", kindsOf(events))
	}
	if events != nil {
		t.Errorf("events = %v, want nil when no Codex type recognized", events)
	}
}

func TestCodexAdapter_Render_AllKinds(t *testing.T) {
	cases := []struct {
		name string
		ev   LogEvent
	}{
		{"session-start", LogEvent{Kind: KindSessionStart, Title: "codex session", Meta: map[string]string{"thread_id": "th"}}},
		{"turn-start", LogEvent{Kind: KindTurnStart, Title: "turn started"}},
		{"assistant-text", LogEvent{Kind: KindAssistantText, Body: "hello"}},
		{"tool-call", LogEvent{Kind: KindToolCall, Title: "⚙ $ pwd"}},
		{"tool-result-ok", LogEvent{Kind: KindToolResult, Title: "↳ exit 0", Body: "/tmp\n"}},
		{"usage", LogEvent{Kind: KindUsage, Title: "usage", Body: "input=10 cached=0 output=5"}},
		{"final", LogEvent{Kind: KindFinal, Title: "turn complete"}},
		{"unknown", LogEvent{Kind: KindUnknown, Raw: "gibberish"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := (&codexAdapter{}).Render(tc.ev, 80, false)
			if len(lines) == 0 {
				t.Fatal("Render returned no lines")
			}
		})
	}
}

func TestCodexAdapter_Render_ToolResultError(t *testing.T) {
	ev := LogEvent{Kind: KindToolResult, Title: "↳ exit 2", Body: "boom", Error: true}
	lines := (&codexAdapter{}).Render(ev, 80, false)
	if len(lines) == 0 {
		t.Fatal("Render returned no lines")
	}
	// Either the warning color code "214" or an ANSI escape must appear —
	// same idiom as the Claude adapter test.
	if !strings.Contains(lines[0], "214") && !strings.Contains(lines[0], "\x1b[") {
		t.Errorf("rendered first line %q has no detectable styling", lines[0])
	}
}

func TestCodexAdapter_Render_Collapses(t *testing.T) {
	body := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10"
	ev := LogEvent{Kind: KindToolResult, Title: "↳ exit 0", Body: body}

	lines := (&codexAdapter{}).Render(ev, 120, false)
	// Count body-like lines: everything after the title. Bounded at ≤ 4
	// (3 lines + the "… N more" hint).
	if len(lines) < 2 {
		t.Fatalf("collapsed render too short: %d lines", len(lines))
	}
	bodyPart := lines[1:]
	if len(bodyPart) > 4 {
		t.Errorf("collapsed body has %d lines, want ≤ 4; lines=%v", len(bodyPart), bodyPart)
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, "7 more") {
		t.Errorf("collapsed hint %q does not contain '7 more'", last)
	}
	if !strings.Contains(last, "expand") {
		t.Errorf("collapsed hint %q does not contain 'expand'", last)
	}

	lines = (&codexAdapter{}).Render(ev, 120, true)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "more (x to expand)") {
		t.Errorf("expanded render still contains collapse marker: %q", joined)
	}
	for _, want := range []string{"l1", "l5", "l10"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expanded render missing %q: %q", want, joined)
		}
	}
}
