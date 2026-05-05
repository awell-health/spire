package pool

import (
	"encoding/json"
	"time"
)

// rateLimitEventType is the value of the JSONL "type" field that
// identifies a rate-limit event among the apprentice's other stream
// messages.
const rateLimitEventType = "rate_limit_event"

// RateLimitEvent is the parsed shape of a single
// {"type":"rate_limit_event", ...} line in the claude subprocess JSONL
// stream. The wizard reads these as it tees the apprentice's output
// and applies them to the slot's on-disk RateLimitInfo via ApplyTo so
// the selector sees fresh signal on the next Pick.
//
// Field naming mirrors the upstream JSONL shape: "rateLimitType" and
// "overageStatus" are camelCase as emitted by the API, while
// "disabled_reason" is snake_case in the source payload — the tags
// preserve the wire shape verbatim.
type RateLimitEvent struct {
	// Type is always "rate_limit_event" for events ParseRateLimitEvent
	// returns; non-matching lines are filtered out before this struct
	// is constructed.
	Type string `json:"type"`

	// RateLimitType is "five_hour" or "overage" — selects which
	// bucket inside RateLimitInfo ApplyTo writes to.
	RateLimitType string `json:"rateLimitType"`

	// OverageStatus is the bucket's new status: "allowed",
	// "allowed_warning", or "rejected". Despite the name, the API
	// uses this field for both five_hour and overage buckets.
	OverageStatus string `json:"overageStatus"`

	// ResetsAt is a Unix-epoch second timestamp at which the bucket
	// is expected to refill. ApplyTo translates it to a time.Time via
	// time.Unix(ResetsAt, 0).
	ResetsAt int64 `json:"resetsAt"`

	// DisabledReason is an optional structured reason populated when
	// the bucket is rejected for a named cause (e.g. an org-level
	// disable). Empty when absent or unspecified.
	DisabledReason string `json:"disabled_reason,omitempty"`
}

// ParseRateLimitEvent decodes a single JSONL line. It is designed for
// stream-scanning: callers pass every line they read from the
// apprentice subprocess and use the bool return to discriminate
// rate-limit events from the rest of the stream.
//
// Returns:
//   - (nil, false, nil)   if the line is not a rate-limit event
//     (unparseable JSON, missing "type", or "type" != "rate_limit_event").
//   - (nil, true,  err)   if the line peeks as a rate-limit event but
//     the full payload fails to unmarshal (wrong field types, etc).
//   - (event, true, nil)  on a successful parse.
//
// The peek-then-parse split lets the caller distinguish "this is some
// other JSONL message we should ignore" from "this looked like one of
// ours but was malformed and deserves a log".
func ParseRateLimitEvent(line []byte) (*RateLimitEvent, bool, error) {
	var peek struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &peek); err != nil {
		return nil, false, nil
	}
	if peek.Type != rateLimitEventType {
		return nil, false, nil
	}

	var event RateLimitEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return nil, true, err
	}
	return &event, true, nil
}

// ApplyTo updates the slot's runtime state with this event's signal:
// the matching bucket (FiveHour or Overage) is overwritten with the
// new status / resets-at / disabled-reason, LastSeen is bumped to
// now, and Recent429 is set to now iff the event is a rejection. An
// unrecognised RateLimitType is treated as a no-op on the buckets but
// still bumps LastSeen so the caller sees the slot is alive.
//
// now is taken as a parameter (rather than calling time.Now() inside)
// so the caller controls clock semantics — important for tests and
// for keeping LastSeen consistent across a batch of events processed
// in one pass.
func (e *RateLimitEvent) ApplyTo(state *SlotState, now time.Time) {
	bucket := RateLimitBucket{
		Status:         RateLimitStatus(e.OverageStatus),
		ResetsAt:       time.Unix(e.ResetsAt, 0),
		DisabledReason: e.DisabledReason,
	}

	switch e.RateLimitType {
	case "five_hour":
		state.RateLimit.FiveHour = bucket
	case "overage":
		state.RateLimit.Overage = bucket
	}

	state.LastSeen = now

	if e.OverageStatus == string(RateLimitStatusRejected) {
		ts := now
		state.RateLimit.Recent429 = &ts
	}
}
