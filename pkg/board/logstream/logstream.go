// Package logstream provides a provider-agnostic log event surface for the
// Spire board inspector. Each provider (Claude, Codex, etc.) implements an
// Adapter that knows how to Parse its native transcript format into a
// canonical []LogEvent stream and Render those events back to styled lines
// for display.
//
// The inspector calls Get(providerName) once per transcript file; if the
// name is not registered, Get returns a terminal rawAdapter fallback that
// treats the bytes as opaque.
package logstream

import "time"

// EventKind enumerates the canonical event types an adapter may emit.
// The iota order is load-bearing — tests and future debug output rely on
// String() producing stable short labels in this order.
type EventKind int

const (
	KindUnknown EventKind = iota
	KindSessionStart
	KindPrompt
	KindAssistantText
	KindToolCall
	KindToolResult
	KindTurnStart
	KindTurnEnd
	KindUsage
	KindStderr
	KindFinal
)

// String returns a short lowercase label for the kind.
func (k EventKind) String() string {
	switch k {
	case KindSessionStart:
		return "session-start"
	case KindPrompt:
		return "prompt"
	case KindAssistantText:
		return "assistant-text"
	case KindToolCall:
		return "tool-call"
	case KindToolResult:
		return "tool-result"
	case KindTurnStart:
		return "turn-start"
	case KindTurnEnd:
		return "turn-end"
	case KindUsage:
		return "usage"
	case KindStderr:
		return "stderr"
	case KindFinal:
		return "final"
	default:
		return "unknown"
	}
}

// LogEvent is the canonical event representation passed between Parse and
// Render. Adapters fill whichever fields are meaningful for their format;
// the renderer decides how to display each kind.
type LogEvent struct {
	Kind  EventKind
	Time  time.Time
	Title string
	Body  string
	Meta  map[string]string
	Error bool
	Raw   string
}

// Adapter converts a provider-specific transcript blob into canonical
// LogEvents and renders each event as styled display lines.
//
// Parse runs once per transcript file; it may be moderately expensive and
// is not streaming. If the blob does not look like this adapter's format,
// Parse returns (nil, false) so the caller can try another adapter or fall
// back to raw.
//
// Render runs per-event per-frame and must be fast and side-effect free.
// Each returned line is fully styled and width-wrapped, ready to be
// concatenated into a scroll region.
type Adapter interface {
	Name() string
	Parse(raw string) ([]LogEvent, bool)
	Render(ev LogEvent, width int, expanded bool) []string
}

// registry maps provider name to adapter. Additional adapters (e.g.
// "codex") are added in their own subtasks; keep the map minimal here.
var registry = map[string]Adapter{
	"claude": &claudeAdapter{},
}

// Get returns the adapter for the named provider, or a terminal rawAdapter
// fallback if the name is unknown. Never returns nil.
func Get(name string) Adapter {
	if a, ok := registry[name]; ok {
		return a
	}
	return rawAdapter{}
}
