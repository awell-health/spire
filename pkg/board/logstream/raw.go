package logstream

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// rawAdapter is the terminal fallback adapter. Parse always succeeds and
// returns a single KindUnknown event containing the whole blob in Raw.
// Render width-wraps each line without applying color.
type rawAdapter struct{}

func (rawAdapter) Name() string { return "raw" }

func (rawAdapter) Parse(raw string) ([]LogEvent, bool) {
	return []LogEvent{{Kind: KindUnknown, Raw: raw}}, true
}

func (rawAdapter) Render(ev LogEvent, width int, _ bool) []string {
	lines := strings.Split(ev.Raw, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	style := lipgloss.NewStyle().Width(width)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = style.Render(l)
	}
	return out
}
