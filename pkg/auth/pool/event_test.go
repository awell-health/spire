package pool

import (
	"fmt"
	"testing"
	"time"
)

func TestParseRateLimitEvent_AllCombinations(t *testing.T) {
	cases := []struct {
		rateLimitType string
		overageStatus string
	}{
		{"five_hour", "allowed"},
		{"five_hour", "allowed_warning"},
		{"five_hour", "rejected"},
		{"overage", "allowed"},
		{"overage", "allowed_warning"},
		{"overage", "rejected"},
	}

	for _, tc := range cases {
		name := fmt.Sprintf("%s/%s", tc.rateLimitType, tc.overageStatus)
		t.Run(name, func(t *testing.T) {
			line := []byte(fmt.Sprintf(
				`{"type":"rate_limit_event","rateLimitType":%q,"overageStatus":%q,"resetsAt":1700000000,"disabled_reason":"because"}`,
				tc.rateLimitType, tc.overageStatus,
			))
			event, isEvent, err := ParseRateLimitEvent(line)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !isEvent {
				t.Fatalf("isEvent=false, want true")
			}
			if event == nil {
				t.Fatalf("event=nil, want non-nil")
			}
			if event.Type != "rate_limit_event" {
				t.Errorf("Type=%q, want rate_limit_event", event.Type)
			}
			if event.RateLimitType != tc.rateLimitType {
				t.Errorf("RateLimitType=%q, want %q", event.RateLimitType, tc.rateLimitType)
			}
			if event.OverageStatus != tc.overageStatus {
				t.Errorf("OverageStatus=%q, want %q", event.OverageStatus, tc.overageStatus)
			}
			if event.ResetsAt != 1700000000 {
				t.Errorf("ResetsAt=%d, want 1700000000", event.ResetsAt)
			}
			if event.DisabledReason != "because" {
				t.Errorf("DisabledReason=%q, want because", event.DisabledReason)
			}
		})
	}
}

func TestParseRateLimitEvent_MissingDisabledReason(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rateLimitType":"five_hour","overageStatus":"allowed","resetsAt":1700000000}`)
	event, isEvent, err := ParseRateLimitEvent(line)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !isEvent {
		t.Fatalf("isEvent=false, want true")
	}
	if event == nil {
		t.Fatalf("event=nil, want non-nil")
	}
	if event.DisabledReason != "" {
		t.Errorf("DisabledReason=%q, want empty string", event.DisabledReason)
	}
}

func TestParseRateLimitEvent_NonEventLines(t *testing.T) {
	cases := []struct {
		name string
		line []byte
	}{
		{"different type", []byte(`{"type":"assistant","content":"hello"}`)},
		{"no type field", []byte(`{"foo":"bar"}`)},
		{"empty object", []byte(`{}`)},
		{"empty line", []byte(``)},
		{"plain text", []byte(`not json at all`)},
		{"system event", []byte(`{"type":"system","subtype":"init"}`)},
		{"tool use", []byte(`{"type":"tool_use","name":"Read"}`)},
		{"non-string type", []byte(`{"type":42}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, isEvent, err := ParseRateLimitEvent(tc.line)
			if err != nil {
				t.Errorf("unexpected err for non-event line: %v", err)
			}
			if isEvent {
				t.Errorf("isEvent=true, want false for non-event line")
			}
			if event != nil {
				t.Errorf("event=%+v, want nil", event)
			}
		})
	}
}

func TestParseRateLimitEvent_MalformedRateLimitShape(t *testing.T) {
	// These payloads peek as a rate-limit event (the "type" field is
	// present and correct) but unmarshalling the full struct fails
	// because some other field has the wrong shape. The contract is
	// (nil, true, err) so the caller can log a malformed-event signal
	// rather than silently dropping it as background noise.
	cases := []struct {
		name string
		line []byte
	}{
		{
			"resetsAt wrong type",
			[]byte(`{"type":"rate_limit_event","rateLimitType":"five_hour","overageStatus":"allowed","resetsAt":"not-a-number"}`),
		},
		{
			"rateLimitType wrong type",
			[]byte(`{"type":"rate_limit_event","rateLimitType":42,"overageStatus":"allowed","resetsAt":1700000000}`),
		},
		{
			"disabled_reason wrong type",
			[]byte(`{"type":"rate_limit_event","rateLimitType":"five_hour","overageStatus":"allowed","resetsAt":1700000000,"disabled_reason":42}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, isEvent, err := ParseRateLimitEvent(tc.line)
			if !isEvent {
				t.Errorf("isEvent=false, want true (line peeks as a rate-limit event)")
			}
			if err == nil {
				t.Errorf("err=nil, want non-nil for malformed payload")
			}
			if event != nil {
				t.Errorf("event=%+v, want nil on parse error", event)
			}
		})
	}
}

func TestParseRateLimitEvent_UnparseableJSON(t *testing.T) {
	// JSON that fails to unmarshal at the peek stage is treated as
	// not-a-rate-limit-event regardless of whether the prefix looks
	// like one. The peek itself failing means we have no way to
	// classify the line, and stream-scanning callers should ignore it
	// as arbitrary log noise rather than treat it as a malformed event.
	cases := []struct {
		name string
		line []byte
	}{
		{"truncated before type", []byte(`{"type":`)},
		{"truncated mid-event", []byte(`{"type":"rate_limit_event","rateLimitType":"five_hour"`)},
		{"unbalanced braces", []byte(`{"type":"rate_limit_event"`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, isEvent, err := ParseRateLimitEvent(tc.line)
			if err != nil {
				t.Errorf("err=%v, want nil for unparseable peek", err)
			}
			if isEvent {
				t.Errorf("isEvent=true, want false")
			}
			if event != nil {
				t.Errorf("event=%+v, want nil", event)
			}
		})
	}
}

func TestApplyTo_FiveHourAllowed(t *testing.T) {
	state := &SlotState{}
	event := &RateLimitEvent{
		Type:           "rate_limit_event",
		RateLimitType:  "five_hour",
		OverageStatus:  "allowed",
		ResetsAt:       1700000000,
		DisabledReason: "",
	}
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

	event.ApplyTo(state, now)

	if state.RateLimit.FiveHour.Status != RateLimitStatusAllowed {
		t.Errorf("FiveHour.Status=%q, want allowed", state.RateLimit.FiveHour.Status)
	}
	if !state.RateLimit.FiveHour.ResetsAt.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("FiveHour.ResetsAt=%v, want %v", state.RateLimit.FiveHour.ResetsAt, time.Unix(1700000000, 0))
	}
	if state.RateLimit.FiveHour.DisabledReason != "" {
		t.Errorf("FiveHour.DisabledReason=%q, want empty", state.RateLimit.FiveHour.DisabledReason)
	}
	if !state.LastSeen.Equal(now) {
		t.Errorf("LastSeen=%v, want %v", state.LastSeen, now)
	}
	if state.RateLimit.Recent429 != nil {
		t.Errorf("Recent429=%v, want nil for non-rejection", state.RateLimit.Recent429)
	}
	// Overage bucket should be untouched.
	if state.RateLimit.Overage.Status != "" {
		t.Errorf("Overage.Status=%q, want empty (untouched)", state.RateLimit.Overage.Status)
	}
}

func TestApplyTo_OverageRejectedSetsRecent429(t *testing.T) {
	state := &SlotState{}
	event := &RateLimitEvent{
		Type:           "rate_limit_event",
		RateLimitType:  "overage",
		OverageStatus:  "rejected",
		ResetsAt:       1700001000,
		DisabledReason: "org_level_disabled_until",
	}
	now := time.Date(2026, 5, 5, 13, 0, 0, 0, time.UTC)

	event.ApplyTo(state, now)

	if state.RateLimit.Overage.Status != RateLimitStatusRejected {
		t.Errorf("Overage.Status=%q, want rejected", state.RateLimit.Overage.Status)
	}
	if state.RateLimit.Overage.DisabledReason != "org_level_disabled_until" {
		t.Errorf("Overage.DisabledReason=%q, want org_level_disabled_until", state.RateLimit.Overage.DisabledReason)
	}
	if state.RateLimit.Recent429 == nil {
		t.Fatalf("Recent429=nil, want non-nil after rejection")
	}
	if !state.RateLimit.Recent429.Equal(now) {
		t.Errorf("Recent429=%v, want %v", *state.RateLimit.Recent429, now)
	}
	// FiveHour bucket should be untouched.
	if state.RateLimit.FiveHour.Status != "" {
		t.Errorf("FiveHour.Status=%q, want empty (untouched)", state.RateLimit.FiveHour.Status)
	}
}

func TestApplyTo_AllowedWarningDoesNotSetRecent429(t *testing.T) {
	state := &SlotState{}
	event := &RateLimitEvent{
		Type:          "rate_limit_event",
		RateLimitType: "five_hour",
		OverageStatus: "allowed_warning",
		ResetsAt:      1700002000,
	}
	now := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)

	event.ApplyTo(state, now)

	if state.RateLimit.FiveHour.Status != RateLimitStatusAllowedWarning {
		t.Errorf("FiveHour.Status=%q, want allowed_warning", state.RateLimit.FiveHour.Status)
	}
	if state.RateLimit.Recent429 != nil {
		t.Errorf("Recent429=%v, want nil for allowed_warning", state.RateLimit.Recent429)
	}
}

func TestApplyTo_RoundTripFromParse(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rateLimitType":"overage","overageStatus":"allowed_warning","resetsAt":1700003000,"disabled_reason":"approaching_cap"}`)
	event, isEvent, err := ParseRateLimitEvent(line)
	if err != nil || !isEvent || event == nil {
		t.Fatalf("ParseRateLimitEvent: err=%v isEvent=%v event=%v", err, isEvent, event)
	}

	state := &SlotState{Slot: "slot-a"}
	now := time.Date(2026, 5, 5, 15, 0, 0, 0, time.UTC)
	event.ApplyTo(state, now)

	if state.Slot != "slot-a" {
		t.Errorf("Slot=%q, want slot-a (untouched)", state.Slot)
	}
	if state.RateLimit.Overage.Status != RateLimitStatusAllowedWarning {
		t.Errorf("Overage.Status=%q, want allowed_warning", state.RateLimit.Overage.Status)
	}
	if !state.RateLimit.Overage.ResetsAt.Equal(time.Unix(1700003000, 0)) {
		t.Errorf("Overage.ResetsAt=%v, want %v", state.RateLimit.Overage.ResetsAt, time.Unix(1700003000, 0))
	}
	if state.RateLimit.Overage.DisabledReason != "approaching_cap" {
		t.Errorf("Overage.DisabledReason=%q, want approaching_cap", state.RateLimit.Overage.DisabledReason)
	}
	if !state.LastSeen.Equal(now) {
		t.Errorf("LastSeen=%v, want %v", state.LastSeen, now)
	}
}

func TestApplyTo_MultipleEventsAccumulate(t *testing.T) {
	state := &SlotState{}
	t0 := time.Date(2026, 5, 5, 16, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Minute)
	t2 := t0.Add(2 * time.Minute)

	(&RateLimitEvent{
		RateLimitType: "five_hour",
		OverageStatus: "allowed",
		ResetsAt:      1700004000,
	}).ApplyTo(state, t0)

	(&RateLimitEvent{
		RateLimitType: "overage",
		OverageStatus: "allowed_warning",
		ResetsAt:      1700005000,
	}).ApplyTo(state, t1)

	(&RateLimitEvent{
		RateLimitType: "five_hour",
		OverageStatus: "rejected",
		ResetsAt:      1700006000,
	}).ApplyTo(state, t2)

	if state.RateLimit.FiveHour.Status != RateLimitStatusRejected {
		t.Errorf("FiveHour.Status=%q, want rejected (latest)", state.RateLimit.FiveHour.Status)
	}
	if state.RateLimit.Overage.Status != RateLimitStatusAllowedWarning {
		t.Errorf("Overage.Status=%q, want allowed_warning (preserved across other event)", state.RateLimit.Overage.Status)
	}
	if !state.LastSeen.Equal(t2) {
		t.Errorf("LastSeen=%v, want %v (latest)", state.LastSeen, t2)
	}
	if state.RateLimit.Recent429 == nil || !state.RateLimit.Recent429.Equal(t2) {
		t.Errorf("Recent429=%v, want %v", state.RateLimit.Recent429, t2)
	}
}

func TestApplyTo_UnknownRateLimitTypeIsNoOpOnBuckets(t *testing.T) {
	// Defensive: an unrecognised rateLimitType should not panic and
	// should not silently corrupt either bucket. LastSeen still bumps
	// so the slot is observed as alive.
	state := &SlotState{}
	now := time.Date(2026, 5, 5, 17, 0, 0, 0, time.UTC)
	(&RateLimitEvent{
		RateLimitType: "weekly",
		OverageStatus: "allowed",
		ResetsAt:      1700007000,
	}).ApplyTo(state, now)

	if state.RateLimit.FiveHour.Status != "" {
		t.Errorf("FiveHour.Status=%q, want empty", state.RateLimit.FiveHour.Status)
	}
	if state.RateLimit.Overage.Status != "" {
		t.Errorf("Overage.Status=%q, want empty", state.RateLimit.Overage.Status)
	}
	if !state.LastSeen.Equal(now) {
		t.Errorf("LastSeen=%v, want %v", state.LastSeen, now)
	}
}

