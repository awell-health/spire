package logstream

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
)

// codexAdapter parses `codex exec --json` JSONL transcripts into canonical
// LogEvents. Each JSONL line is a typed envelope: thread.started,
// turn.started, turn.completed, item.started, item.completed.
type codexAdapter struct{}

func (codexAdapter) Name() string { return "codex" }

// codexEnvelope is the top-level shape of a single JSONL line.
type codexEnvelope struct {
	Type     string      `json:"type"`
	ThreadID string      `json:"thread_id,omitempty"`
	Item     *codexItem  `json:"item,omitempty"`
	Usage    *codexUsage `json:"usage,omitempty"`
}

// codexItem is the payload inside item.started / item.completed envelopes.
// ExitCode is a pointer so zero (success) is distinguishable from absent.
type codexItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Text             string `json:"text,omitempty"`
	Command          string `json:"command,omitempty"`
	AggregatedOutput string `json:"aggregated_output,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	Status           string `json:"status,omitempty"`
}

type codexUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

// knownCodexTypes are the top-level envelope types Parse will accept as
// evidence that the input really is a Codex transcript. At least one line
// must match one of these for Parse to return ok=true.
var knownCodexTypes = map[string]bool{
	"thread.started":  true,
	"turn.started":    true,
	"turn.completed":  true,
	"item.started":    true,
	"item.completed":  true,
}

func (codexAdapter) Parse(raw string) ([]LogEvent, bool) {
	var out []LogEvent
	recognized := false

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var env codexEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			out = append(out, LogEvent{Kind: KindUnknown, Raw: line})
			continue
		}

		if knownCodexTypes[env.Type] {
			recognized = true
		}

		switch env.Type {
		case "thread.started":
			meta := map[string]string{}
			if env.ThreadID != "" {
				meta["thread_id"] = env.ThreadID
			}
			out = append(out, LogEvent{
				Kind:  KindSessionStart,
				Title: "codex session",
				Meta:  meta,
				Raw:   line,
			})

		case "turn.started":
			out = append(out, LogEvent{
				Kind:  KindTurnStart,
				Title: "turn started",
				Raw:   line,
			})

		case "turn.completed":
			if env.Usage != nil {
				out = append(out, LogEvent{
					Kind:  KindUsage,
					Title: "usage",
					Body: fmt.Sprintf("input=%d cached=%d output=%d",
						env.Usage.InputTokens, env.Usage.CachedInputTokens, env.Usage.OutputTokens),
					Meta: map[string]string{
						"input_tokens":        fmt.Sprint(env.Usage.InputTokens),
						"cached_input_tokens": fmt.Sprint(env.Usage.CachedInputTokens),
						"output_tokens":       fmt.Sprint(env.Usage.OutputTokens),
					},
					Raw: line,
				})
			}
			out = append(out, LogEvent{
				Kind:  KindFinal,
				Title: "turn complete",
				Raw:   line,
			})

		case "item.started":
			if ev, ok := codexItemStarted(env.Item, line); ok {
				out = append(out, ev)
			} else {
				out = append(out, LogEvent{Kind: KindUnknown, Raw: line})
			}

		case "item.completed":
			if ev, ok := codexItemCompleted(env.Item, line); ok {
				out = append(out, ev)
			} else {
				out = append(out, LogEvent{Kind: KindUnknown, Raw: line})
			}

		default:
			out = append(out, LogEvent{
				Kind:  KindUnknown,
				Title: "unknown type: " + env.Type,
				Raw:   line,
			})
		}
	}

	if !recognized {
		return nil, false
	}
	return out, true
}

// codexItemStarted produces a KindToolCall for command_execution items.
// agent_message has no observed item.started, so other item types are
// rejected (caller emits KindUnknown).
func codexItemStarted(item *codexItem, line string) (LogEvent, bool) {
	if item == nil {
		return LogEvent{}, false
	}
	switch item.Type {
	case "command_execution":
		meta := map[string]string{}
		if item.ID != "" {
			meta["item_id"] = item.ID
		}
		if item.Status != "" {
			meta["status"] = item.Status
		}
		return LogEvent{
			Kind:  KindToolCall,
			Title: "⚙ $ " + item.Command,
			Meta:  meta,
			Raw:   line,
		}, true
	default:
		return LogEvent{}, false
	}
}

// codexItemCompleted produces KindAssistantText for agent_message items and
// KindToolResult for command_execution items. Other item types are
// rejected (caller emits KindUnknown).
func codexItemCompleted(item *codexItem, line string) (LogEvent, bool) {
	if item == nil {
		return LogEvent{}, false
	}
	switch item.Type {
	case "agent_message":
		meta := map[string]string{}
		if item.ID != "" {
			meta["item_id"] = item.ID
		}
		return LogEvent{
			Kind: KindAssistantText,
			Body: item.Text,
			Meta: meta,
			Raw:  line,
		}, true
	case "command_execution":
		meta := map[string]string{}
		if item.ID != "" {
			meta["item_id"] = item.ID
		}
		var (
			title  string
			errorF bool
		)
		if item.ExitCode != nil {
			title = fmt.Sprintf("↳ exit %d", *item.ExitCode)
			meta["exit_code"] = fmt.Sprint(*item.ExitCode)
			errorF = *item.ExitCode != 0
		} else {
			title = "↳ exit ?"
		}
		return LogEvent{
			Kind:  KindToolResult,
			Title: title,
			Body:  item.AggregatedOutput,
			Meta:  meta,
			Error: errorF,
			Raw:   line,
		}, true
	default:
		return LogEvent{}, false
	}
}

// codexStyles returns the per-kind lipgloss style palette used by Render.
// Mirrors claudeStyles so the two providers render consistently.
func codexStyles() map[EventKind]lipgloss.Style {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	primary := lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	accent2 := lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
	success := lipgloss.NewStyle().Foreground(lipgloss.Color("86"))
	warning := lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	return map[EventKind]lipgloss.Style{
		KindUnknown:       dim,
		KindSessionStart:  dim,
		KindPrompt:        accent,
		KindAssistantText: primary,
		KindToolCall:      accent2,
		KindToolResult:    dim,
		KindTurnStart:     dim,
		KindTurnEnd:       dim,
		KindUsage:         success,
		KindStderr:        warning,
		KindFinal:         success,
	}
}

func (codexAdapter) Render(ev LogEvent, width int, expanded bool) []string {
	styles := codexStyles()
	wrap := lipgloss.NewStyle().Width(width)
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	warning := lipgloss.NewStyle().Foreground(lipgloss.Color("214"))

	titleStyle := styles[ev.Kind]
	if ev.Kind == KindToolResult && ev.Error {
		titleStyle = warning
	}

	title := ev.Title
	if title == "" {
		title = ev.Kind.String()
	}
	if ev.Kind == KindUnknown && !strings.HasPrefix(title, "? ") {
		title = "? " + title
	}

	var out []string
	out = append(out, wrap.Render(titleStyle.Render(title)))

	bodyLines, hidden := collapseBody(ev.Body, expanded)
	bodyStyle := styles[ev.Kind]
	if ev.Kind == KindToolResult && ev.Error {
		bodyStyle = warning
	}
	for _, l := range bodyLines {
		out = append(out, wrap.Render(bodyStyle.Render(l)))
	}
	if hidden > 0 {
		out = append(out, wrap.Render(dim.Render(fmt.Sprintf("… %d more (x to expand)", hidden))))
	}

	if len(ev.Meta) > 0 {
		keys := make([]string, 0, len(ev.Meta))
		for k := range ev.Meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+ev.Meta[k])
		}
		out = append(out, wrap.Render(dim.Render(strings.Join(parts, "  "))))
	}

	return out
}
