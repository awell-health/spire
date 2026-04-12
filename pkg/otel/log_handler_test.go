package otel

import (
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// --- anyValueToString ---

func TestAnyValueToString(t *testing.T) {
	tests := []struct {
		name string
		val  *commonpb.AnyValue
		want string
	}{
		{"nil", nil, ""},
		{"string", &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hello"}}, "hello"},
		{"empty string", &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ""}}, ""},
		{"int positive", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 42}}, "42"},
		{"int zero", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 0}}, "0"},
		{"int negative", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: -7}}, "-7"},
		{"double positive", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 3.14}}, "3.14"},
		{"double zero", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0}}, "0"},
		{"bool true", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}, "true"},
		{"bool false", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}, "false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := anyValueToString(tt.val)
			if got != tt.want {
				t.Errorf("anyValueToString() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- resolveEventName ---

func TestResolveEventName_EventNameField(t *testing.T) {
	lr := &logspb.LogRecord{
		EventName: "claude_code.tool_result",
	}
	got := resolveEventName(lr)
	if got != "claude_code.tool_result" {
		t.Errorf("got %q, want claude_code.tool_result", got)
	}
}

func TestResolveEventName_BodyString(t *testing.T) {
	lr := &logspb.LogRecord{
		Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.tool_result"}},
	}
	got := resolveEventName(lr)
	if got != "codex.tool_result" {
		t.Errorf("got %q, want codex.tool_result", got)
	}
}

func TestResolveEventName_BodyNotKnownPrefix(t *testing.T) {
	// Body is a string but not a known event prefix — should fall through to attributes.
	lr := &logspb.LogRecord{
		Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "some random text"}},
		Attributes: []*commonpb.KeyValue{
			{Key: "log.event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.api_request"}}},
		},
	}
	got := resolveEventName(lr)
	if got != "claude_code.api_request" {
		t.Errorf("got %q, want claude_code.api_request", got)
	}
}

func TestResolveEventName_Attribute(t *testing.T) {
	lr := &logspb.LogRecord{
		Attributes: []*commonpb.KeyValue{
			{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.user_prompt"}}},
		},
	}
	got := resolveEventName(lr)
	if got != "claude_code.user_prompt" {
		t.Errorf("got %q, want claude_code.user_prompt", got)
	}
}

func TestResolveEventName_Empty(t *testing.T) {
	lr := &logspb.LogRecord{}
	got := resolveEventName(lr)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// --- classifyEvent ---

func TestClassifyEvent(t *testing.T) {
	tests := []struct {
		event        string
		wantProvider string
		wantKind     string
	}{
		{"claude_code.tool_result", "claude", "tool_result"},
		{"claude_code.api_request", "claude", "api_request"},
		{"claude_code.tool_decision", "claude", "tool_decision"},
		{"claude_code.user_prompt", "claude", "user_prompt"},
		{"codex.tool_result", "codex", "tool_result"},
		{"codex.api_request", "codex", "api_request"},
		{"codex.tool_decision", "codex", "tool_decision"},
		{"unknown.event", "unknown", "unknown.event"},
		{"bare_event", "unknown", "bare_event"},
	}
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			provider, kind := classifyEvent(tt.event)
			if provider != tt.wantProvider {
				t.Errorf("provider = %q, want %q", provider, tt.wantProvider)
			}
			if kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", kind, tt.wantKind)
			}
		})
	}
}

// --- parseToolEvent / parseAPIEvent ---

func TestParseToolEvent(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	lr := &logspb.LogRecord{
		TimeUnixNano: now,
		Attributes: []*commonpb.KeyValue{
			{Key: "tool_name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "Read"}}},
			{Key: "duration_ms", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 150}}},
			{Key: "success", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
		},
	}
	res := resourceAttrs{SessionID: "sess-1", BeadID: "spi-abc", AgentName: "apprentice-0", Step: "implement"}
	ts := time.Unix(0, int64(now)).UTC()

	ev := parseToolEvent(lr, "claude", "tool_result", res, "my-tower", ts)
	if ev.ToolName != "Read" {
		t.Errorf("ToolName = %q, want Read", ev.ToolName)
	}
	if ev.DurationMs != 150 {
		t.Errorf("DurationMs = %d, want 150", ev.DurationMs)
	}
	if !ev.Success {
		t.Error("Success should be true")
	}
	if ev.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", ev.Provider)
	}
	if ev.EventKind != "tool_result" {
		t.Errorf("EventKind = %q, want tool_result", ev.EventKind)
	}
	if ev.BeadID != "spi-abc" {
		t.Errorf("BeadID = %q, want spi-abc", ev.BeadID)
	}
	if ev.Tower != "my-tower" {
		t.Errorf("Tower = %q, want my-tower", ev.Tower)
	}
}

func TestParseToolEvent_FalseSuccess(t *testing.T) {
	lr := &logspb.LogRecord{
		TimeUnixNano: uint64(time.Now().UnixNano()),
		Attributes: []*commonpb.KeyValue{
			{Key: "tool_name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "Bash"}}},
			{Key: "duration_ms", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 0}}},
			{Key: "success", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}},
		},
	}
	res := resourceAttrs{}
	ts := time.Now().UTC()

	ev := parseToolEvent(lr, "codex", "tool_result", res, "tower", ts)
	if ev.Success {
		t.Error("Success should be false")
	}
	if ev.DurationMs != 0 {
		t.Errorf("DurationMs = %d, want 0", ev.DurationMs)
	}
}

func TestParseAPIEvent(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	lr := &logspb.LogRecord{
		TimeUnixNano: now,
		Attributes: []*commonpb.KeyValue{
			{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude-opus-4-6"}}},
			{Key: "duration_ms", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 2000}}},
			{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 5000}}},
			{Key: "output_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1500}}},
			{Key: "cache_read_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 3000}}},
			{Key: "cache_write_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 200}}},
			{Key: "cost_usd", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.15}}},
		},
	}
	res := resourceAttrs{SessionID: "sess-1", BeadID: "spi-abc", AgentName: "wizard-0", Step: "plan"}
	ts := time.Unix(0, int64(now)).UTC()

	ev := parseAPIEvent(lr, "claude", res, "my-tower", ts)
	if ev.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want claude-opus-4-6", ev.Model)
	}
	if ev.DurationMs != 2000 {
		t.Errorf("DurationMs = %d, want 2000", ev.DurationMs)
	}
	if ev.InputTokens != 5000 {
		t.Errorf("InputTokens = %d, want 5000", ev.InputTokens)
	}
	if ev.OutputTokens != 1500 {
		t.Errorf("OutputTokens = %d, want 1500", ev.OutputTokens)
	}
	if ev.CacheReadTokens != 3000 {
		t.Errorf("CacheReadTokens = %d, want 3000", ev.CacheReadTokens)
	}
	if ev.CacheWriteTokens != 200 {
		t.Errorf("CacheWriteTokens = %d, want 200", ev.CacheWriteTokens)
	}
	if ev.CostUSD != 0.15 {
		t.Errorf("CostUSD = %f, want 0.15", ev.CostUSD)
	}
	if ev.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", ev.Provider)
	}
}

// --- ParseLogRecords (integration) ---

func TestParseLogRecords_ToolResult(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					strKV("bead.id", "spi-xyz"),
					strKV("agent.name", "apprentice-spi-xyz-0"),
					strKV("step", "implement"),
					strKV("tower", "test-tower"),
					strKV("session.id", "sess-abc"),
				},
			},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.tool_result"}},
							Attributes: []*commonpb.KeyValue{
								strKV("tool_name", "Edit"),
								intKV("duration_ms", 120),
								boolKV("success", true),
							},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "default-tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	if len(result.APIEvents) != 0 {
		t.Fatalf("expected 0 API events, got %d", len(result.APIEvents))
	}

	ev := result.ToolEvents[0]
	if ev.ToolName != "Edit" {
		t.Errorf("ToolName = %q, want Edit", ev.ToolName)
	}
	if ev.DurationMs != 120 {
		t.Errorf("DurationMs = %d, want 120", ev.DurationMs)
	}
	if !ev.Success {
		t.Error("Success should be true")
	}
	if ev.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", ev.Provider)
	}
	if ev.EventKind != "tool_result" {
		t.Errorf("EventKind = %q, want tool_result", ev.EventKind)
	}
	if ev.BeadID != "spi-xyz" {
		t.Errorf("BeadID = %q, want spi-xyz", ev.BeadID)
	}
	if ev.AgentName != "apprentice-spi-xyz-0" {
		t.Errorf("AgentName = %q, want apprentice-spi-xyz-0", ev.AgentName)
	}
	if ev.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want sess-abc", ev.SessionID)
	}
	if ev.Tower != "test-tower" {
		t.Errorf("Tower = %q, want test-tower", ev.Tower)
	}
}

func TestParseLogRecords_APIRequest(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					strKV("bead.id", "spi-abc"),
					strKV("session.id", "sess-1"),
				},
			},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.api_request"}},
							Attributes: []*commonpb.KeyValue{
								strKV("model", "claude-opus-4-6"),
								intKV("input_tokens", 3000),
								intKV("output_tokens", 1000),
								doubleKV("cost_usd", 0.08),
							},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "default-tower")
	if len(result.APIEvents) != 1 {
		t.Fatalf("expected 1 API event, got %d", len(result.APIEvents))
	}
	if len(result.ToolEvents) != 0 {
		t.Fatalf("expected 0 tool events, got %d", len(result.ToolEvents))
	}

	ev := result.APIEvents[0]
	if ev.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want claude-opus-4-6", ev.Model)
	}
	if ev.InputTokens != 3000 {
		t.Errorf("InputTokens = %d, want 3000", ev.InputTokens)
	}
	if ev.OutputTokens != 1000 {
		t.Errorf("OutputTokens = %d, want 1000", ev.OutputTokens)
	}
	if ev.CostUSD != 0.08 {
		t.Errorf("CostUSD = %f, want 0.08", ev.CostUSD)
	}
	if ev.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", ev.Provider)
	}
	if ev.Tower != "default-tower" {
		t.Errorf("Tower = %q, want default-tower (from fallback)", ev.Tower)
	}
}

func TestParseLogRecords_CodexToolResult(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.tool_result"}},
							Attributes: []*commonpb.KeyValue{
								strKV("tool_name", "shell"),
								intKV("duration_ms", 300),
								boolKV("success", false),
							},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "fallback-tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}

	ev := result.ToolEvents[0]
	if ev.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", ev.Provider)
	}
	if ev.ToolName != "shell" {
		t.Errorf("ToolName = %q, want shell", ev.ToolName)
	}
	if ev.Success {
		t.Error("Success should be false")
	}
	if ev.Tower != "fallback-tower" {
		t.Errorf("Tower = %q, want fallback-tower", ev.Tower)
	}
}

func TestParseLogRecords_ToolDecision(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.tool_decision"}},
							Attributes: []*commonpb.KeyValue{
								strKV("tool_name", "Bash"),
							},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	ev := result.ToolEvents[0]
	if ev.EventKind != "tool_decision" {
		t.Errorf("EventKind = %q, want tool_decision", ev.EventKind)
	}
	if ev.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", ev.Provider)
	}
}

func TestParseLogRecords_CodexReadFile(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					strKV("bead.id", "spi-codex-1"),
					strKV("session.id", "sess-codex"),
					strKV("step", "implement"),
				},
			},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.tool_result"}},
							Attributes: []*commonpb.KeyValue{
								strKV("tool_name", "read_file"),
								intKV("duration_ms", 25),
								boolKV("success", true),
							},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	ev := result.ToolEvents[0]
	if ev.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", ev.Provider)
	}
	if ev.ToolName != "read_file" {
		t.Errorf("ToolName = %q, want read_file", ev.ToolName)
	}
	if ev.DurationMs != 25 {
		t.Errorf("DurationMs = %d, want 25", ev.DurationMs)
	}
	if !ev.Success {
		t.Error("Success should be true")
	}
	if ev.Step != "implement" {
		t.Errorf("Step = %q, want implement", ev.Step)
	}
	if ev.EventKind != "tool_result" {
		t.Errorf("EventKind = %q, want tool_result", ev.EventKind)
	}
}

func TestParseLogRecords_CodexWriteFile(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.tool_result"}},
							Attributes: []*commonpb.KeyValue{
								strKV("tool_name", "write_file"),
								intKV("duration_ms", 80),
								boolKV("success", true),
							},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	ev := result.ToolEvents[0]
	if ev.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", ev.Provider)
	}
	if ev.ToolName != "write_file" {
		t.Errorf("ToolName = %q, want write_file", ev.ToolName)
	}
	if ev.DurationMs != 80 {
		t.Errorf("DurationMs = %d, want 80", ev.DurationMs)
	}
}

func TestParseLogRecords_CodexSearch(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.tool_result"}},
							Attributes: []*commonpb.KeyValue{
								strKV("tool_name", "search"),
								intKV("duration_ms", 150),
								boolKV("success", false),
							},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	ev := result.ToolEvents[0]
	if ev.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", ev.Provider)
	}
	if ev.ToolName != "search" {
		t.Errorf("ToolName = %q, want search", ev.ToolName)
	}
	if ev.Success {
		t.Error("Success should be false")
	}
}

func TestParseLogRecords_UserPrompt(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					strKV("bead.id", "spi-prompt"),
					strKV("agent.name", "apprentice-0"),
				},
			},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.user_prompt"}},
							Attributes:   []*commonpb.KeyValue{},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	ev := result.ToolEvents[0]
	if ev.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", ev.Provider)
	}
	if ev.EventKind != "user_prompt" {
		t.Errorf("EventKind = %q, want user_prompt", ev.EventKind)
	}
	if ev.BeadID != "spi-prompt" {
		t.Errorf("BeadID = %q, want spi-prompt", ev.BeadID)
	}
	if ev.AgentName != "apprentice-0" {
		t.Errorf("AgentName = %q, want apprentice-0", ev.AgentName)
	}
}

func TestParseLogRecords_CodexAllTools(t *testing.T) {
	// Verify all four Codex tool names are handled in a single batch.
	now := uint64(time.Now().UnixNano())
	codexTools := []string{"read_file", "write_file", "shell", "search"}
	var records []*logspb.LogRecord
	for i, tool := range codexTools {
		records = append(records, &logspb.LogRecord{
			TimeUnixNano: now + uint64(i)*1_000_000,
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.tool_result"}},
			Attributes:   []*commonpb.KeyValue{strKV("tool_name", tool)},
		})
	}
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource:  &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}},
		},
	}

	result := ParseLogRecords(resourceLogs, "tower")
	if len(result.ToolEvents) != 4 {
		t.Fatalf("expected 4 tool events, got %d", len(result.ToolEvents))
	}
	for i, ev := range result.ToolEvents {
		if ev.ToolName != codexTools[i] {
			t.Errorf("event %d: ToolName = %q, want %q", i, ev.ToolName, codexTools[i])
		}
		if ev.Provider != "codex" {
			t.Errorf("event %d: Provider = %q, want codex", i, ev.Provider)
		}
		if ev.EventKind != "tool_result" {
			t.Errorf("event %d: EventKind = %q, want tool_result", i, ev.EventKind)
		}
	}
}

func TestParseLogRecords_EventNameField(t *testing.T) {
	// Verify that the OTLP 1.5+ EventName field takes priority over Body.
	now := uint64(time.Now().UnixNano())
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							EventName:    "claude_code.tool_result",
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "tool output text here"}},
							Attributes: []*commonpb.KeyValue{
								strKV("tool_name", "Grep"),
								intKV("duration_ms", 30),
								boolKV("success", true),
							},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "tower")
	if len(result.ToolEvents) != 1 {
		t.Fatalf("expected 1 tool event, got %d", len(result.ToolEvents))
	}
	ev := result.ToolEvents[0]
	if ev.ToolName != "Grep" {
		t.Errorf("ToolName = %q, want Grep", ev.ToolName)
	}
	if ev.EventKind != "tool_result" {
		t.Errorf("EventKind = %q, want tool_result", ev.EventKind)
	}
}

func TestParseLogRecords_SkipsUnknownEvents(t *testing.T) {
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						// No event name at all
						{TimeUnixNano: uint64(time.Now().UnixNano())},
						// Body is just random text (not a known prefix)
						{
							TimeUnixNano: uint64(time.Now().UnixNano()),
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "some random log line"}},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "tower")
	if len(result.ToolEvents) != 0 {
		t.Errorf("expected 0 tool events, got %d", len(result.ToolEvents))
	}
	if len(result.APIEvents) != 0 {
		t.Errorf("expected 0 API events, got %d", len(result.APIEvents))
	}
}

func TestParseLogRecords_MixedBatch(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	resourceLogs := []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					strKV("bead.id", "spi-mix"),
				},
			},
			ScopeLogs: []*logspb.ScopeLogs{
				{
					LogRecords: []*logspb.LogRecord{
						{
							TimeUnixNano: now,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.tool_result"}},
							Attributes:   []*commonpb.KeyValue{strKV("tool_name", "Read")},
						},
						{
							TimeUnixNano: now + 1_000_000,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.api_request"}},
							Attributes:   []*commonpb.KeyValue{strKV("model", "sonnet")},
						},
						{
							TimeUnixNano: now + 2_000_000,
							Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.tool_result"}},
							Attributes:   []*commonpb.KeyValue{strKV("tool_name", "shell")},
						},
					},
				},
			},
		},
	}

	result := ParseLogRecords(resourceLogs, "tower")
	if len(result.ToolEvents) != 2 {
		t.Errorf("expected 2 tool events, got %d", len(result.ToolEvents))
	}
	if len(result.APIEvents) != 1 {
		t.Errorf("expected 1 API event, got %d", len(result.APIEvents))
	}
}

// --- resolveLogTimestamp ---

func TestResolveLogTimestamp_PrefersTimestamp(t *testing.T) {
	ts := uint64(1700000000_000000000) // 2023-11-14T22:13:20Z
	observed := uint64(1700000001_000000000)
	lr := &logspb.LogRecord{
		TimeUnixNano:         ts,
		ObservedTimeUnixNano: observed,
	}
	got := resolveLogTimestamp(lr)
	want := time.Unix(0, int64(ts)).UTC()
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveLogTimestamp_FallsBackToObserved(t *testing.T) {
	observed := uint64(1700000001_000000000)
	lr := &logspb.LogRecord{
		TimeUnixNano:         0,
		ObservedTimeUnixNano: observed,
	}
	got := resolveLogTimestamp(lr)
	want := time.Unix(0, int64(observed)).UTC()
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// --- extractLogResourceAttrs ---

func TestExtractLogResourceAttrs(t *testing.T) {
	res := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			strKV("bead.id", "spi-xyz"),
			strKV("agent.name", "wizard-spi-xyz"),
			strKV("step", "plan"),
			strKV("tower", "my-tower"),
			strKV("session.id", "sess-123"),
		},
	}

	attrs := extractLogResourceAttrs(res)
	if attrs.BeadID != "spi-xyz" {
		t.Errorf("BeadID = %q, want spi-xyz", attrs.BeadID)
	}
	if attrs.AgentName != "wizard-spi-xyz" {
		t.Errorf("AgentName = %q, want wizard-spi-xyz", attrs.AgentName)
	}
	if attrs.Step != "plan" {
		t.Errorf("Step = %q, want plan", attrs.Step)
	}
	if attrs.Tower != "my-tower" {
		t.Errorf("Tower = %q, want my-tower", attrs.Tower)
	}
	if attrs.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want sess-123", attrs.SessionID)
	}
}

func TestExtractLogResourceAttrs_ServiceInstanceID(t *testing.T) {
	res := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			strKV("service.instance.id", "instance-456"),
		},
	}

	attrs := extractLogResourceAttrs(res)
	if attrs.SessionID != "instance-456" {
		t.Errorf("SessionID = %q, want instance-456", attrs.SessionID)
	}
}

func TestExtractLogResourceAttrs_Nil(t *testing.T) {
	attrs := extractLogResourceAttrs(nil)
	if attrs.BeadID != "" || attrs.AgentName != "" || attrs.Step != "" {
		t.Error("expected empty attrs for nil resource")
	}
}

// --- attribute helpers ---

func TestAttrStr_Fallback(t *testing.T) {
	m := map[string]string{
		"tool.name": "Read",
	}
	got := attrStr(m, "tool_name", "tool.name")
	if got != "Read" {
		t.Errorf("got %q, want Read", got)
	}
}

func TestAttrInt_FromIntValue(t *testing.T) {
	m := map[string]string{
		"duration_ms": "150",
	}
	got := attrInt(m, "duration_ms")
	if got != 150 {
		t.Errorf("got %d, want 150", got)
	}
}

func TestAttrInt_FromFloat(t *testing.T) {
	m := map[string]string{
		"duration": "3.14",
	}
	got := attrInt(m, "duration_ms", "duration")
	if got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestAttrBool(t *testing.T) {
	tests := []struct {
		name    string
		m       map[string]string
		key     string
		def     bool
		want    bool
	}{
		{"true string", map[string]string{"success": "true"}, "success", false, true},
		{"false string", map[string]string{"success": "false"}, "success", true, false},
		{"1", map[string]string{"success": "1"}, "success", false, true},
		{"0", map[string]string{"success": "0"}, "success", true, false},
		{"missing key", map[string]string{}, "success", true, true},
		{"missing key false default", map[string]string{}, "success", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := attrBool(tt.m, tt.key, tt.def)
			if got != tt.want {
				t.Errorf("attrBool() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- test helpers ---

func strKV(key, val string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: val}},
	}
}

func intKV(key string, val int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: val}},
	}
}

func doubleKV(key string, val float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: val}},
	}
}

func boolKV(key string, val bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: val}},
	}
}
