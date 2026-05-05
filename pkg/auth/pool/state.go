package pool

import "time"

// RateLimitStatus is the current rate-limit verdict for one window of one
// slot. Values mirror the apprentice JSONL `rate_limit_event.overageStatus`
// wire field.
type RateLimitStatus string

const (
	StatusAllowed        RateLimitStatus = "allowed"
	StatusAllowedWarning RateLimitStatus = "allowed_warning"
	StatusRejected       RateLimitStatus = "rejected"
)

// RateLimitWindow is the per-window state shared by the five-hour and overage
// buckets. DisabledReason is set only for the overage window when the
// upstream marks it disabled (e.g. "org_level_disabled_until").
type RateLimitWindow struct {
	Status         RateLimitStatus `json:"status"`
	ResetsAt       time.Time       `json:"resets_at"`
	DisabledReason string          `json:"disabled_reason,omitempty"`
}

// RateLimitInfo aggregates the per-slot rate-limit signals. Recent429 is the
// timestamp of the most recent 429 observed on this slot, used as a tie-break
// hint for the selector; nil if no 429 has been seen.
type RateLimitInfo struct {
	FiveHour  RateLimitWindow `json:"five_hour"`
	Overage   RateLimitWindow `json:"overage"`
	Recent429 *time.Time      `json:"recent_429,omitempty"`
}

// InFlightClaim is one active dispatch holding a slot. HeartbeatAt is bumped
// periodically by the wizard; the steward sweep reaps claims whose heartbeat
// is older than 2× the heartbeat interval.
type InFlightClaim struct {
	DispatchID  string    `json:"dispatch_id"`
	ClaimedAt   time.Time `json:"claimed_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// SlotState is the cached runtime state for one slot, persisted at
// <stateDir>/<slot-name>.json. Slot identifies which slot this state
// belongs to (matches SlotConfig.Name).
type SlotState struct {
	Slot      string          `json:"slot"`
	LastSeen  time.Time       `json:"last_seen"`
	RateLimit RateLimitInfo   `json:"rate_limit"`
	InFlight  []InFlightClaim `json:"in_flight"`
}
