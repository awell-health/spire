package pool

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Policy ranks the eligible subset of a pool's slots for selection.
// Implementations return the eligible slots in preference order; the
// caller picks index 0. An empty result means no slot in the pool can
// accept a new claim right now (the caller decides whether that is a
// "park-until-reset" or "wait-for-release" situation).
//
// A slot is eligible iff its rate-limit Status is not "rejected" for
// either the five-hour or the overage bucket AND its in-flight claim
// count is strictly below MaxConcurrent. A missing entry in the states
// map is treated as zero-state — fully eligible.
type Policy interface {
	Rank(slots []SlotConfig, states map[string]*SlotState, now time.Time) []SlotConfig
}

// NewPolicy returns the Policy implementation matching p. Unknown values
// (including the zero value) default to PreemptivePolicy so that a
// misconfigured tower still behaves reasonably under load.
func NewPolicy(p SelectionPolicy) Policy {
	switch p {
	case PolicyRoundRobin:
		return &RoundRobinPolicy{}
	case PolicyPreemptive:
		return &PreemptivePolicy{}
	default:
		return &PreemptivePolicy{}
	}
}

// isEligible reports whether slot can accept a new claim given state.
// A nil state is treated as zero-state per the Policy contract: no
// rate-limit signals, zero in-flight claims.
func isEligible(slot SlotConfig, state *SlotState) bool {
	inFlight := 0
	if state != nil {
		if state.RateLimit.FiveHour.Status == RateLimitStatusRejected {
			return false
		}
		if state.RateLimit.Overage.Status == RateLimitStatusRejected {
			return false
		}
		inFlight = len(state.InFlight)
	}
	return inFlight < slot.MaxConcurrent
}

// filterEligible returns the eligible slots in input (pool) order.
func filterEligible(slots []SlotConfig, states map[string]*SlotState) []SlotConfig {
	out := make([]SlotConfig, 0, len(slots))
	for _, s := range slots {
		if isEligible(s, states[s.Name]) {
			out = append(out, s)
		}
	}
	return out
}

// RoundRobinPolicy cycles through eligible slots in pool order. The
// cursor is per-instance and advances by one each call so consecutive
// Rank calls offer different starting points even when the eligible
// set is unchanged. Safe for concurrent use; the cursor is guarded by
// an internal mutex.
type RoundRobinPolicy struct {
	mu     sync.Mutex
	cursor int
}

// Rank returns the eligible slots rotated so a fresh starting slot is
// at index 0 on each call.
func (p *RoundRobinPolicy) Rank(slots []SlotConfig, states map[string]*SlotState, now time.Time) []SlotConfig {
	eligible := filterEligible(slots, states)
	if len(eligible) == 0 {
		return nil
	}

	p.mu.Lock()
	start := p.cursor % len(eligible)
	if start < 0 {
		start += len(eligible)
	}
	p.cursor++
	p.mu.Unlock()

	out := make([]SlotConfig, len(eligible))
	for i := range eligible {
		out[i] = eligible[(start+i)%len(eligible)]
	}
	return out
}

// PreemptivePolicy ranks eligible slots by, in order:
//
//  1. Status: "allowed" above "allowed_warning".
//  2. Greatest distance from now to the soonest non-zero ResetsAt
//     across both buckets (further-away resets ranks higher).
//  3. Lowest len(InFlight) / MaxConcurrent ratio.
//
// The policy is stateless and safe for concurrent use.
type PreemptivePolicy struct{}

// Rank returns the eligible slots ordered preemptively.
func (p *PreemptivePolicy) Rank(slots []SlotConfig, states map[string]*SlotState, now time.Time) []SlotConfig {
	eligible := filterEligible(slots, states)
	if len(eligible) == 0 {
		return nil
	}

	type ranked struct {
		slot       SlotConfig
		idx        int
		statusRank int
		distance   time.Duration
		ratio      float64
	}
	items := make([]ranked, len(eligible))
	for i, s := range eligible {
		st := states[s.Name]
		items[i] = ranked{
			slot:       s,
			idx:        i,
			statusRank: statusRank(st),
			distance:   soonestResetDistance(st, now),
			ratio:      inFlightRatio(s, st),
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.statusRank != b.statusRank {
			return a.statusRank > b.statusRank
		}
		if a.distance != b.distance {
			return a.distance > b.distance
		}
		if a.ratio != b.ratio {
			return a.ratio < b.ratio
		}
		return a.idx < b.idx
	})

	out := make([]SlotConfig, len(items))
	for i, r := range items {
		out[i] = r.slot
	}
	return out
}

// statusRank returns 1 when both buckets are fully allowed and 0 when
// at least one is in the warning band. Rejected slots are filtered out
// before this is called.
func statusRank(state *SlotState) int {
	if state == nil {
		return 1
	}
	if state.RateLimit.FiveHour.Status == RateLimitStatusAllowedWarning {
		return 0
	}
	if state.RateLimit.Overage.Status == RateLimitStatusAllowedWarning {
		return 0
	}
	return 1
}

// soonestResetDistance returns the duration from now to the earliest
// non-zero ResetsAt across both buckets. A slot with no reset pending
// returns the maximum duration so it ranks ahead of any slot whose
// limits are about to reset.
func soonestResetDistance(state *SlotState, now time.Time) time.Duration {
	if state == nil {
		return time.Duration(math.MaxInt64)
	}
	fh := state.RateLimit.FiveHour.ResetsAt
	ov := state.RateLimit.Overage.ResetsAt
	var soonest time.Time
	switch {
	case !fh.IsZero() && !ov.IsZero():
		if fh.Before(ov) {
			soonest = fh
		} else {
			soonest = ov
		}
	case !fh.IsZero():
		soonest = fh
	case !ov.IsZero():
		soonest = ov
	default:
		return time.Duration(math.MaxInt64)
	}
	return soonest.Sub(now)
}

// inFlightRatio returns len(InFlight) / MaxConcurrent. Eligibility has
// already required MaxConcurrent > 0 by the time this is called, so
// the only zero-divisor case is a degenerate slot config that filterEligible
// would have rejected.
func inFlightRatio(slot SlotConfig, state *SlotState) float64 {
	if slot.MaxConcurrent <= 0 {
		return 0
	}
	if state == nil {
		return 0
	}
	return float64(len(state.InFlight)) / float64(slot.MaxConcurrent)
}
