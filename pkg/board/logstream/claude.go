package logstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// claudeAdapter parses Claude's stream-json transcript format (one JSON
// object per line) into canonical LogEvents and renders them with a
// per-kind color palette.
type claudeAdapter struct{}

func (claudeAdapter) Name() string { return "claude" }

// knownClaudeTypes lists the top-level "type" values we recognize as
// Claude-shape. Encountering any of these sets sawClaudeShape, which
// guards Parse against accidentally "claiming" non-Claude JSONL.
var knownClaudeTypes = map[string]bool{
	"system":           true,
	"user":             true,
	"assistant":        true,
	"result":           true,
	"stream_event":     true,
	"rate_limit_event": true,
}

func (claudeAdapter) Parse(raw string) ([]LogEvent, bool) {
	var (
		events         []LogEvent
		sawClaudeShape bool
		onlyUnknown    = true
	)

	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			events = append(events, LogEvent{Kind: KindUnknown, Raw: line})
			continue
		}

		var typ string
		if err := json.Unmarshal(obj["type"], &typ); err != nil {
			events = append(events, LogEvent{Kind: KindUnknown, Raw: line, Title: "unknown type: " + string(obj["type"])})
			continue
		}

		if knownClaudeTypes[typ] {
			sawClaudeShape = true
		}

		switch typ {
		case "system":
			var subtype string
			_ = json.Unmarshal(obj["subtype"], &subtype)
			switch subtype {
			case "init":
				meta := map[string]string{}
				var sid, cwd string
				if err := json.Unmarshal(obj["session_id"], &sid); err == nil && sid != "" {
					meta["session_id"] = sid
				}
				if err := json.Unmarshal(obj["cwd"], &cwd); err == nil && cwd != "" {
					meta["cwd"] = cwd
				}
				ev := LogEvent{
					Kind:  KindSessionStart,
					Title: "session started",
					Meta:  meta,
					Raw:   line,
				}
				events = append(events, ev)
				onlyUnknown = false
			case "hook_started", "hook_response":
				events = append(events, buildHookEvent(obj, line, subtype))
				onlyUnknown = false
			}

		case "user":
			emitted := emitUserEvents(obj, line)
			if len(emitted) > 0 {
				events = append(events, emitted...)
				onlyUnknown = false
			}

		case "assistant":
			emitted := emitAssistantEvents(obj, line)
			if len(emitted) > 0 {
				events = append(events, emitted...)
				onlyUnknown = false
			}

		case "result":
			events = append(events, buildUsageEvent(obj, line), buildFinalEvent(obj, line))
			onlyUnknown = false

		case "rate_limit_event":
			events = append(events, buildRateLimitEvent(obj, line))
			onlyUnknown = false

		case "stream_event":
			// Drop: stream_event deltas are not useful after replay.

		default:
			events = append(events, LogEvent{
				Kind:  KindUnknown,
				Title: "unknown type: " + typ,
				Raw:   line,
			})
		}
	}

	if !sawClaudeShape && onlyUnknown {
		return nil, false
	}
	return events, true
}

// emitUserEvents handles a "user" line. message.content can be either a
// JSON string (treated as a prompt) or an array of content blocks.
func emitUserEvents(obj map[string]json.RawMessage, line string) []LogEvent {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(obj["message"], &msg); err != nil {
		return nil
	}
	content, ok := msg["content"]
	if !ok {
		return nil
	}

	var asString string
	if err := json.Unmarshal(content, &asString); err == nil {
		return []LogEvent{{
			Kind:  KindPrompt,
			Title: "» prompt",
			Body:  asString,
			Raw:   line,
		}}
	}

	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}

	var out []LogEvent
	for _, b := range blocks {
		var btyp string
		_ = json.Unmarshal(b["type"], &btyp)
		switch btyp {
		case "text":
			var txt string
			_ = json.Unmarshal(b["text"], &txt)
			out = append(out, LogEvent{
				Kind:  KindPrompt,
				Title: "» prompt",
				Body:  txt,
				Raw:   line,
			})
		case "tool_result":
			var (
				id      string
				isError bool
			)
			_ = json.Unmarshal(b["tool_use_id"], &id)
			_ = json.Unmarshal(b["is_error"], &isError)
			title, body := summarizeToolResultContent(b["content"], isError)
			meta := map[string]string{}
			if id != "" {
				meta["tool_use_id"] = id
			}
			out = append(out, LogEvent{
				Kind:  KindToolResult,
				Title: title,
				Body:  body,
				Meta:  meta,
				Error: isError,
				Raw:   line,
			})
		}
	}
	return out
}

// emitAssistantEvents handles an "assistant" line. message.content is an
// array of content blocks (text, tool_use, ...).
func emitAssistantEvents(obj map[string]json.RawMessage, line string) []LogEvent {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(obj["message"], &msg); err != nil {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(msg["content"], &blocks); err != nil {
		return nil
	}

	var out []LogEvent
	for _, b := range blocks {
		var btyp string
		_ = json.Unmarshal(b["type"], &btyp)
		switch btyp {
		case "text":
			var txt string
			_ = json.Unmarshal(b["text"], &txt)
			out = append(out, LogEvent{
				Kind: KindAssistantText,
				Body: txt,
				Raw:  line,
			})
		case "tool_use":
			var (
				name  string
				id    string
				input json.RawMessage
			)
			_ = json.Unmarshal(b["name"], &name)
			_ = json.Unmarshal(b["id"], &id)
			input = b["input"]

			meta := map[string]string{}
			if name != "" {
				meta["tool_name"] = name
			}
			if id != "" {
				meta["tool_use_id"] = id
			}
			out = append(out, LogEvent{
				Kind:  KindToolCall,
				Title: "→ " + name + "(" + shortInput(input) + ")",
				Body:  prettyJSON(input),
				Meta:  meta,
				Raw:   line,
			})
		}
	}
	return out
}

// buildUsageEvent extracts usage.input_tokens / output_tokens plus timing
// and cost from a "result" object and returns a KindUsage event.
func buildUsageEvent(obj map[string]json.RawMessage, line string) LogEvent {
	meta := map[string]string{}

	if u, ok := obj["usage"]; ok {
		var usage map[string]json.RawMessage
		if err := json.Unmarshal(u, &usage); err == nil {
			for k, v := range usage {
				meta[k] = rawToString(v)
			}
		}
	}

	for _, k := range []string{"duration_ms", "total_cost_usd"} {
		if v, ok := obj[k]; ok {
			meta[k] = rawToString(v)
		}
	}

	return LogEvent{
		Kind:  KindUsage,
		Title: "usage",
		Meta:  meta,
		Raw:   line,
	}
}

// buildFinalEvent extracts subtype, result body, and num_turns from a
// "result" object and returns a KindFinal event.
func buildFinalEvent(obj map[string]json.RawMessage, line string) LogEvent {
	var subtype, result string
	_ = json.Unmarshal(obj["subtype"], &subtype)
	_ = json.Unmarshal(obj["result"], &result)

	meta := map[string]string{}
	if v, ok := obj["num_turns"]; ok {
		meta["num_turns"] = rawToString(v)
	}

	return LogEvent{
		Kind:  KindFinal,
		Title: "final result (" + subtype + ")",
		Body:  result,
		Meta:  meta,
		Raw:   line,
	}
}

// stringifyToolResultContent renders a tool_result content field as a
// single string. The content can be a plain JSON string or an array of
// blocks (e.g. [{type:"text", text:"..."}]). Non-text blocks surface as
// "[<type>]" markers so expand-all has something to show.
func stringifyToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			var btyp string
			_ = json.Unmarshal(b["type"], &btyp)
			switch btyp {
			case "text":
				var txt string
				if err := json.Unmarshal(b["text"], &txt); err == nil {
					parts = append(parts, txt)
				}
			case "":
				// Missing type: fall back to reading "text" if present.
				var txt string
				if err := json.Unmarshal(b["text"], &txt); err == nil {
					parts = append(parts, txt)
				}
			default:
				parts = append(parts, "["+btyp+"]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// toolResultContentKinds returns the distinct block types present in a
// tool_result content array. Returns nil for string or malformed content.
func toolResultContentKinds(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var kinds []string
	for _, b := range blocks {
		var btyp string
		_ = json.Unmarshal(b["type"], &btyp)
		if btyp == "" {
			btyp = "text"
		}
		if !seen[btyp] {
			seen[btyp] = true
			kinds = append(kinds, btyp)
		}
	}
	return kinds
}

// summarizeToolResultContent returns a one-line summary title and the full
// body for a tool_result content field. The summary format is:
//   - on error: "↳ err: <first line, truncated to 120 chars>"
//   - on success, image-only content: "↳ ok (image)"
//   - on success, multi-line content: "↳ ok (<N> lines)"
//   - on success, single-line content: "↳ ok (<N> bytes)"
//
// The body preserves the full content (including non-text block markers)
// so expand-all renders it faithfully.
func summarizeToolResultContent(raw json.RawMessage, isError bool) (title, body string) {
	body = stringifyToolResultContent(raw)
	if isError {
		first := firstToolResultLine(body)
		if first == "" {
			first = "(empty)"
		}
		const maxErr = 120
		if len(first) > maxErr {
			first = first[:maxErr-1] + "…"
		}
		return "↳ err: " + first, body
	}
	kinds := toolResultContentKinds(raw)
	imageOnly := len(kinds) > 0
	for _, k := range kinds {
		if k != "image" {
			imageOnly = false
			break
		}
	}
	if imageOnly {
		return "↳ ok (image)", body
	}
	if body == "" {
		return "↳ ok (0 bytes)", body
	}
	trimmed := strings.TrimRight(body, "\n")
	if strings.Contains(trimmed, "\n") {
		lines := strings.Count(trimmed, "\n") + 1
		return fmt.Sprintf("↳ ok (%d lines)", lines), body
	}
	if trimmed != body {
		// Had a trailing newline but otherwise single-line. Treat as 1 line.
		return "↳ ok (1 line)", body
	}
	return fmt.Sprintf("↳ ok (%d bytes)", len(body)), body
}

// firstToolResultLine returns the first non-empty line of s, stripped of
// trailing whitespace. Used to build "err: <first line>" summaries.
func firstToolResultLine(s string) string {
	s = strings.TrimLeft(s, "\r\n\t ")
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimRight(s, "\r\t ")
}

// buildHookEvent produces a KindHook event from a system/hook_{started,response}
// line. Title format is "⚙ hook:<hook_name> (<subtype>)"; the hook's output
// (hook_response only) is stashed in Body for expand-all.
func buildHookEvent(obj map[string]json.RawMessage, line, subtype string) LogEvent {
	var hookName, hookID, hookEvent, output string
	_ = json.Unmarshal(obj["hook_name"], &hookName)
	_ = json.Unmarshal(obj["hook_id"], &hookID)
	_ = json.Unmarshal(obj["hook_event"], &hookEvent)
	_ = json.Unmarshal(obj["output"], &output)

	name := hookName
	if name == "" {
		name = "<unknown>"
	}
	title := fmt.Sprintf("⚙ hook:%s (%s)", name, subtype)

	meta := map[string]string{}
	if hookID != "" {
		meta["hook_id"] = hookID
	}
	if hookEvent != "" {
		meta["hook_event"] = hookEvent
	}

	return LogEvent{
		Kind:  KindHook,
		Title: title,
		Body:  output,
		Meta:  meta,
		Raw:   line,
	}
}

// buildRateLimitEvent produces a KindRateLimit event from a rate_limit_event
// line. Title summarizes limit_type, wait_seconds, and reset_at if present;
// falls back to a bare "⏱ rate limit" when the payload has no known fields.
// The raw line is preserved for expand-all.
func buildRateLimitEvent(obj map[string]json.RawMessage, line string) LogEvent {
	meta := map[string]string{}
	for _, k := range []string{"limit_type", "reset_at", "wait_seconds"} {
		if v, ok := obj[k]; ok && len(v) > 0 {
			meta[k] = rawToString(v)
		}
	}

	title := "⏱ rate limit"
	if v, ok := meta["limit_type"]; ok && v != "" {
		title = "⏱ rate limit: " + v
	}
	var extras []string
	if v, ok := meta["wait_seconds"]; ok && v != "" {
		extras = append(extras, "wait "+v+"s")
	}
	if v, ok := meta["reset_at"]; ok && v != "" {
		extras = append(extras, "reset "+v)
	}
	if len(extras) > 0 {
		title = title + " (" + strings.Join(extras, ", ") + ")"
	}

	return LogEvent{
		Kind:  KindRateLimit,
		Title: title,
		Meta:  meta,
		Raw:   line,
	}
}

// shortInput returns a single-line summary of a tool_use input object,
// truncated to ≤60 chars with an ellipsis.
func shortInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return ""
	}
	s := compact.String()
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	s = strings.ReplaceAll(s, "\n", " ")
	const max = 60
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}

// prettyJSON returns an indented form of raw, or the raw string on error.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// rawToString converts a json.RawMessage to a string representation
// without JSON quoting for primitives.
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b)
	}
	return strings.TrimSpace(string(raw))
}

// claudeStyles returns the per-kind lipgloss style palette used by Render.
// Called once per Render invocation — cheap, but not memoized to avoid
// any locking concerns on concurrent renders.
func claudeStyles() map[EventKind]lipgloss.Style {
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
		KindHook:          dim,
		KindRateLimit:     dim,
	}
}

func (claudeAdapter) Render(ev LogEvent, width int, expanded bool) []string {
	styles := claudeStyles()
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

	// tool_result body is collapsed entirely by default: the summary line
	// is already in Title, so we only show the body on expand-all.
	var bodyLines []string
	var hidden int
	if ev.Kind == KindToolResult && !expanded {
		if ev.Body != "" {
			out = append(out, wrap.Render(dim.Render("… (x to expand)")))
		}
	} else {
		bodyLines, hidden = collapseBody(ev.Body, expanded)
	}
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

// collapseBody returns the first 3 non-empty display lines of body plus
// the count of hidden trailing lines, unless expanded is true (in which
// case all lines are returned and hidden is 0).
func collapseBody(body string, expanded bool) ([]string, int) {
	if body == "" {
		return nil, 0
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if expanded || len(lines) <= 3 {
		return lines, 0
	}
	return lines[:3], len(lines) - 3
}
