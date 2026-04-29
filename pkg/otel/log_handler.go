package otel

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// LogParseResult holds the parsed tool events and API events from a batch of
// OTLP log records.
type LogParseResult struct {
	ToolEvents []ToolEvent
	APIEvents  []APIEvent
}

// ParseLogRecords processes OTLP ResourceLogs and extracts tool events and API
// events from known event names. Unknown events are silently skipped.
func ParseLogRecords(resourceLogs []*logspb.ResourceLogs, defaultTower string) LogParseResult {
	var result LogParseResult

	for _, rl := range resourceLogs {
		res := ExtractRunContext(rl.GetResource())

		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				eventName := resolveEventName(lr)
				if eventName == "" {
					continue
				}

				ts := resolveLogTimestamp(lr)
				tower := res.Tower
				if tower == "" {
					tower = defaultTower
				}

				provider, kind := classifyEvent(eventName)

				switch kind {
				case "tool_result":
					result.ToolEvents = append(result.ToolEvents, parseToolEvent(lr, provider, kind, res, tower, ts))
				case "api_request":
					result.APIEvents = append(result.APIEvents, parseAPIEvent(lr, provider, res, tower, ts))
				case "rate_limit_event":
					result.APIEvents = append(result.APIEvents, parseRateLimitEvent(lr, provider, res, tower, ts))
				case "tool_decision", "user_prompt":
					result.ToolEvents = append(result.ToolEvents, parseGenericToolEvent(lr, provider, kind, res, tower, ts))
				default:
					// Don't silently discard the event body. claude_code (and
					// other providers) report internal_error / mcp_server_*
					// / hook_execution_* / compaction / etc. via OTLP — the
					// names are unfamiliar to spire's parser, but the bodies
					// carry the diagnostic payload (error messages, stack
					// traces, hook IDs). Log the full record so an operator
					// tailing daemon.error.log can see what the provider
					// actually reported, and so we have ground truth when an
					// apprentice dies and we need to correlate.
					var fields []string
					fields = append(fields, fmt.Sprintf("event=%s", eventName))
					fields = append(fields, fmt.Sprintf("provider=%s", provider))
					fields = append(fields, fmt.Sprintf("tower=%s", tower))
					fields = append(fields, fmt.Sprintf("ts=%s", ts.UTC().Format(time.RFC3339Nano)))
					if sev := lr.GetSeverityText(); sev != "" {
						fields = append(fields, fmt.Sprintf("severity=%s", sev))
					}
					if sevNum := lr.GetSeverityNumber(); sevNum != 0 {
						fields = append(fields, fmt.Sprintf("severity_number=%d", sevNum))
					}
					if tid := lr.GetTraceId(); len(tid) > 0 {
						fields = append(fields, fmt.Sprintf("trace_id=%x", tid))
					}
					if sid := lr.GetSpanId(); len(sid) > 0 {
						fields = append(fields, fmt.Sprintf("span_id=%x", sid))
					}
					if b := lr.GetBody(); b != nil {
						if sv := b.GetStringValue(); sv != "" {
							fields = append(fields, fmt.Sprintf("body=%q", sv))
						} else if kvl := b.GetKvlistValue(); kvl != nil {
							var bodyKVs []string
							for _, kv := range kvl.GetValues() {
								bodyKVs = append(bodyKVs, fmt.Sprintf("%s=%s", kv.GetKey(), kvStringValue(kv)))
							}
							fields = append(fields, fmt.Sprintf("body={%s}", strings.Join(bodyKVs, " ")))
						}
					}
					for _, kv := range lr.GetAttributes() {
						k := kv.GetKey()
						if k == "log.event.name" || k == "event.name" {
							continue
						}
						fields = append(fields, fmt.Sprintf("%s=%s", k, kvStringValue(kv)))
					}
					log.Printf("[otel] unparsed log event %s", strings.Join(fields, " "))
				}
			}
		}
	}

	return result
}

// resolveEventName extracts the event name from a log record.
// Checks EventName field first (OTLP 1.5+), then Body string value,
// then log.event.name attribute.
func resolveEventName(lr *logspb.LogRecord) string {
	if name := lr.GetEventName(); name != "" {
		return name
	}

	if body := lr.GetBody(); body != nil {
		if sv := body.GetStringValue(); sv != "" && isKnownEventPrefix(sv) {
			return sv
		}
	}

	for _, kv := range lr.GetAttributes() {
		if kv.GetKey() == "log.event.name" || kv.GetKey() == "event.name" {
			if v := kvStringValue(kv); v != "" {
				return v
			}
		}
	}

	return ""
}

// resolveLogTimestamp returns the event timestamp from the log record,
// preferring Timestamp over ObservedTimestamp.
func resolveLogTimestamp(lr *logspb.LogRecord) time.Time {
	if ts := lr.GetTimeUnixNano(); ts > 0 {
		return time.Unix(0, int64(ts)).UTC()
	}
	if ts := lr.GetObservedTimeUnixNano(); ts > 0 {
		return time.Unix(0, int64(ts)).UTC()
	}
	return time.Now().UTC()
}

// classifyEvent returns (provider, kind) from an event name.
func classifyEvent(eventName string) (string, string) {
	if strings.HasPrefix(eventName, "claude_code.") {
		return "claude", strings.TrimPrefix(eventName, "claude_code.")
	}
	if strings.HasPrefix(eventName, "codex.") {
		return "codex", strings.TrimPrefix(eventName, "codex.")
	}
	return "unknown", eventName
}

func isKnownEventPrefix(s string) bool {
	return strings.HasPrefix(s, "claude_code.") || strings.HasPrefix(s, "codex.")
}

func parseToolEvent(lr *logspb.LogRecord, provider, kind string, res RunContext, tower string, ts time.Time) ToolEvent {
	attrs := logAttrMap(lr.GetAttributes())
	return ToolEvent{
		SessionID:  res.SessionID,
		BeadID:     res.BeadID,
		AgentName:  res.AgentName,
		Step:       res.FormulaStep,
		ToolName:   attrStr(attrs, "tool_name", "tool.name"),
		DurationMs: attrInt(attrs, "duration_ms", "duration"),
		Success:    attrBool(attrs, "success", true),
		Timestamp:  ts,
		Tower:      tower,
		Provider:   provider,
		EventKind:  kind,
		Attributes: extractToolAttrs(attrs),
	}
}

// toolAttrFields lists the attribute keys (across Claude / Codex / generic
// OTel emitters) that carry per-call argument and result data. Multiple keys
// per concept exist because emitters disagree on naming: Claude Code uses
// `tool_input` / `tool_output`, Bash specifically often emits `command`,
// generic OTel uses `gen_ai.tool.input` / `input_value`. We capture every
// known surface so the downstream surfaces can render whichever one the
// emitter chose.
var toolAttrFields = []string{
	// Tool inputs.
	"command",
	"tool_input",
	"input_value",
	"file_path",
	"pattern",
	"prompt",
	"description",
	"old_string",
	"new_string",
	"path",
	"query",
	"url",
	"gen_ai.tool.input",
	// Tool results / outputs.
	"tool_output",
	"output_value",
	"result",
	"gen_ai.tool.output",
	// Error context.
	"error_message",
	"error",
	// Optional surrounding metadata.
	"tool_call_id",
	"step_id",
}

// extractToolAttrs lifts the rich-payload attribute keys (args, results,
// error context) out of a flat OTLP attribute map and returns a small map
// suitable for surfacing per-call. Empty entries are omitted. Returns nil
// if no relevant keys are present.
func extractToolAttrs(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string, len(toolAttrFields))
	for _, k := range toolAttrFields {
		if v, ok := attrs[k]; ok && v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseAPIEvent(lr *logspb.LogRecord, provider string, res RunContext, tower string, ts time.Time) APIEvent {
	attrs := logAttrMap(lr.GetAttributes())
	return APIEvent{
		SessionID:        res.SessionID,
		BeadID:           res.BeadID,
		AgentName:        res.AgentName,
		Step:             res.FormulaStep,
		Provider:         provider,
		Model:            attrStr(attrs, "model", "llm.model"),
		DurationMs:       attrInt(attrs, "duration_ms", "duration"),
		InputTokens:      attrInt64(attrs, "input_tokens", "llm.input_tokens"),
		OutputTokens:     attrInt64(attrs, "output_tokens", "llm.output_tokens"),
		CacheReadTokens:  attrInt64(attrs, "cache_read_tokens", "cache_read_input_tokens"),
		CacheWriteTokens: attrInt64(attrs, "cache_write_tokens", "cache_creation_input_tokens"),
		CostUSD:          attrFloat(attrs, "cost_usd", "cost"),
		Timestamp:        ts,
		Tower:            tower,
		EventType:        "api_request",
	}
}

// parseRateLimitEvent maps a claude_code.rate_limit_event (or the equivalent
// provider-prefixed variant) onto an APIEvent row with event_type='rate_limit'.
// Token / cost fields stay zero — a rate-limit row records the occurrence, not
// a completed call. RetryCount is populated when the provider reports it.
func parseRateLimitEvent(lr *logspb.LogRecord, provider string, res RunContext, tower string, ts time.Time) APIEvent {
	attrs := logAttrMap(lr.GetAttributes())
	return APIEvent{
		SessionID:  res.SessionID,
		BeadID:     res.BeadID,
		AgentName:  res.AgentName,
		Step:       res.FormulaStep,
		Provider:   provider,
		Model:      attrStr(attrs, "model", "llm.model"),
		Timestamp:  ts,
		Tower:      tower,
		EventType:  "rate_limit",
		RetryCount: attrInt(attrs, "retry_count", "attempt", "retry"),
	}
}

func parseGenericToolEvent(lr *logspb.LogRecord, provider, kind string, res RunContext, tower string, ts time.Time) ToolEvent {
	attrs := logAttrMap(lr.GetAttributes())
	return ToolEvent{
		SessionID:  res.SessionID,
		BeadID:     res.BeadID,
		AgentName:  res.AgentName,
		Step:       res.FormulaStep,
		ToolName:   attrStr(attrs, "tool_name", "tool.name"),
		Timestamp:  ts,
		Tower:      tower,
		Provider:   provider,
		EventKind:  kind,
		Attributes: extractToolAttrs(attrs),
	}
}

// --- attribute helpers ---

func logAttrMap(attrs []*commonpb.KeyValue) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		m[kv.GetKey()] = anyValueToString(kv.GetValue())
	}
	return m
}

func attrStr(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}

func attrInt(m map[string]string, keys ...string) int {
	for _, k := range keys {
		if v := m[k]; v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return int(f)
			}
		}
	}
	return 0
}

func attrInt64(m map[string]string, keys ...string) int64 {
	for _, k := range keys {
		if v := m[k]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return int64(f)
			}
		}
	}
	return 0
}

func attrFloat(m map[string]string, keys ...string) float64 {
	for _, k := range keys {
		if v := m[k]; v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

func attrBool(m map[string]string, key string, defaultVal bool) bool {
	v := m[key]
	switch strings.ToLower(v) {
	case "true", "1":
		return true
	case "false", "0":
		return false
	default:
		return defaultVal
	}
}

// anyValueToString converts an OTLP AnyValue to its string representation.
// Uses the oneof type to avoid silently dropping zero-valued ints/doubles
// and false booleans.
func anyValueToString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return v.GetStringValue()
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(v.GetIntValue(), 10)
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprintf("%g", v.GetDoubleValue())
	case *commonpb.AnyValue_BoolValue:
		if v.GetBoolValue() {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}
