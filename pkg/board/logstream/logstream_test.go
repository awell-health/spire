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

func TestGet_KnownReturnsCodex(t *testing.T) {
	a := Get("codex")
	if a == nil {
		t.Fatal("Get(\"codex\") returned nil")
	}
	if got := a.Name(); got != "codex" {
		t.Errorf("Name() = %q, want %q", got, "codex")
	}
}

func TestGet_UnknownReturnsRaw(t *testing.T) {
	for _, name := range []string{"", "anything", "mystery"} {
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

func TestRegistered_ReturnsSortedKnownProviders(t *testing.T) {
	got := Registered()
	want := []string{"claude", "codex"}
	if len(got) != len(want) {
		t.Fatalf("Registered() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Registered()[%d] = %q, want %q", i, got[i], want[i])
		}
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
		KindHook:          "hook",
		KindRateLimit:     "rate-limit",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("EventKind(%d).String() = %q, want %q", k, got, want)
		}
	}
}
