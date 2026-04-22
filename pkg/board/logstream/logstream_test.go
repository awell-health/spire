package logstream

import "testing"

func TestGet_KnownReturnsClaude(t *testing.T) {
	a := Get("claude")
	if a == nil {
		t.Fatal("Get(\"claude\") returned nil")
	}
	if got := a.Name(); got != "claude" {
		t.Errorf("Name() = %q, want %q", got, "claude")
	}
}

func TestGet_UnknownReturnsRaw(t *testing.T) {
	// "codex" is not yet registered — it's added in a later subtask. Until
	// then, it should fall back to raw like any unknown name.
	for _, name := range []string{"codex", "", "anything"} {
		t.Run(name, func(t *testing.T) {
			a := Get(name)
			if a == nil {
				t.Fatalf("Get(%q) returned nil", name)
			}
			if got := a.Name(); got != "raw" {
				t.Errorf("Name() = %q, want %q", got, "raw")
			}
		})
	}
}

func TestRawAdapter_ParsePassthrough(t *testing.T) {
	events, ok := rawAdapter{}.Parse("hello\nworld")
	if !ok {
		t.Fatal("Parse returned ok=false")
	}
	if len(events) != 1 {
		t.Fatalf("Parse returned %d events, want 1", len(events))
	}
	if events[0].Kind != KindUnknown {
		t.Errorf("Kind = %v, want KindUnknown", events[0].Kind)
	}
	if events[0].Raw != "hello\nworld" {
		t.Errorf("Raw = %q, want %q", events[0].Raw, "hello\nworld")
	}
}

func TestEventKind_String(t *testing.T) {
	cases := map[EventKind]string{
		KindUnknown:       "unknown",
		KindSessionStart:  "session-start",
		KindPrompt:        "prompt",
		KindAssistantText: "assistant-text",
		KindToolCall:      "tool-call",
		KindToolResult:    "tool-result",
		KindTurnStart:     "turn-start",
		KindTurnEnd:       "turn-end",
		KindUsage:         "usage",
		KindStderr:        "stderr",
		KindFinal:         "final",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("EventKind(%d).String() = %q, want %q", k, got, want)
		}
	}
}
