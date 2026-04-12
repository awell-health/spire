package otel

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
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
		res := extractLogResourceAttrs(rl.GetResource())

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
				case "tool_decision", "user_prompt":
					result.ToolEvents = append(result.ToolEvents, parseGenericToolEvent(lr, provider, kind, res, tower, ts))
				default:
					log.Printf("[otel] skipping unknown log event: %s", eventName)
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

func parseToolEvent(lr *logspb.LogRecord, provider, kind string, res resourceAttrs, tower string, ts time.Time) ToolEvent {
	attrs := logAttrMap(lr.GetAttributes())
	return ToolEvent{
		SessionID:  res.SessionID,
		BeadID:     res.BeadID,
		AgentName:  res.AgentName,
		Step:       res.Step,
		ToolName:   attrStr(attrs, "tool_name", "tool.name"),
		DurationMs: attrInt(attrs, "duration_ms", "duration"),
		Success:    attrBool(attrs, "success", true),
		Timestamp:  ts,
		Tower:      tower,
		Provider:   provider,
		EventKind:  kind,
	}
}

func parseAPIEvent(lr *logspb.LogRecord, provider string, res resourceAttrs, tower string, ts time.Time) APIEvent {
	attrs := logAttrMap(lr.GetAttributes())
	return APIEvent{
		SessionID:        res.SessionID,
		BeadID:           res.BeadID,
		AgentName:        res.AgentName,
		Step:             res.Step,
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
	}
}

func parseGenericToolEvent(lr *logspb.LogRecord, provider, kind string, res resourceAttrs, tower string, ts time.Time) ToolEvent {
	attrs := logAttrMap(lr.GetAttributes())
	return ToolEvent{
		SessionID: res.SessionID,
		BeadID:    res.BeadID,
		AgentName: res.AgentName,
		Step:      res.Step,
		ToolName:  attrStr(attrs, "tool_name", "tool.name"),
		Timestamp: ts,
		Tower:     tower,
		Provider:  provider,
		EventKind: kind,
	}
}

// extractLogResourceAttrs reads resource-level attributes from a log resource.
func extractLogResourceAttrs(res *resourcepb.Resource) resourceAttrs {
	var attrs resourceAttrs
	if res == nil {
		return attrs
	}
	for _, kv := range res.GetAttributes() {
		val := kvStringValue(kv)
		switch kv.GetKey() {
		case "bead.id":
			attrs.BeadID = val
		case "agent.name":
			attrs.AgentName = val
		case "step":
			attrs.Step = val
		case "tower":
			attrs.Tower = val
		case "session.id", "service.instance.id":
			attrs.SessionID = val
		}
	}
	return attrs
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
