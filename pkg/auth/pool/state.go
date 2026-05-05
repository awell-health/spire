package pool

import "time"

// RateLimitStatus is the current rate-limit verdict for one window of one
// slot. Values mirror the apprentice JSONL `rate_limit_event.overageStatus`
// wire field.
type RateLimitStatus string

const (
	// RateLimitStatusAllowed means traffic is flowing normally.
	RateLimitStatusAllowed RateLimitStatus = "allowed"
	// RateLimitStatusAllowedWarning means traffic is allowed but the bucket
	// is approaching its cap; the selector ranks these below fully-allowed.
	RateLimitStatusAllowedWarning RateLimitStatus = "allowed_warning"
	// RateLimitStatusRejected means the bucket is exhausted; the slot is
	// not eligible until ResetsAt.
	RateLimitStatusRejected RateLimitStatus = "rejected"
)

// Backward-compatible spellings used by earlier callers in this package.
const (
	StatusAllowed        = RateLimitStatusAllowed
	StatusAllowedWarning = RateLimitStatusAllowedWarning
	StatusRejected       = RateLimitStatusRejected
)

// RateLimitBucket is the per-window state shared by the five-hour and overage
// buckets. DisabledReason is set only for the overage window when the
// upstream marks it disabled (e.g. "org_level_disabled_until").
type RateLimitBucket struct {
	Status         RateLimitStatus `json:"status"`
	ResetsAt       time.Time       `json:"resets_at"`
	DisabledReason string          `json:"disabled_reason,omitempty"`
}

// RateLimitWindow is an alias for RateLimitBucket retained for callers that
// used the earlier name.
type RateLimitWindow = RateLimitBucket

// RateLimitInfo aggregates the per-slot rate-limit signals. Recent429 is the
// timestamp of the most recent 429 observed on this slot, used as a tie-break
// hint for the selector; nil if no 429 has been seen.
type RateLimitInfo struct {
	FiveHour  RateLimitBucket `json:"five_hour"`
	Overage   RateLimitBucket `json:"overage"`
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
