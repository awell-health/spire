package pool

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// poolNameSubscription / poolNameAPIKey are the canonical pool labels the
// wizard passes to Pick. Other values are rejected with a descriptive error
// so misuse fails loudly rather than silently picking nothing.
const (
	poolNameSubscription = "subscription"
	poolNameAPIKey       = "api-key"
)

// ErrAllRateLimited signals that every slot in a pool has Status == "rejected"
// for at least one bucket and the wizard should park the dispatch until
// ResetsAt rather than retry. ResetsAt is the soonest non-zero reset across
// all rejected buckets so the caller can sleep that long and retry.
type ErrAllRateLimited struct {
	// ResetsAt is the soonest moment the pool may regain capacity. Zero
	// when no rejected bucket carried a reset hint — callers should treat
	// that as "unknown, retry after a configured backoff".
	ResetsAt time.Time
}

// Error implements the error interface.
func (e *ErrAllRateLimited) Error() string {
	if e.ResetsAt.IsZero() {
		return "pool: all slots rate-limited (no reset hint)"
	}
	return fmt.Sprintf("pool: all slots rate-limited; soonest reset at %s", e.ResetsAt.Format(time.RFC3339))
}

// Selector picks slots from a pool, tracks in-flight claims on each slot's
// cached state file, and releases them when the dispatch completes. One
// Selector instance is shared across all wizards in a tower (or process).
//
// Concurrency model:
//   - Pick acquires a pool-level exclusive flock at <stateDir>/.lock-<pool>
//     so only one Pick at a time is computing eligibility for a given pool.
//   - The chosen slot's claim list is mutated under the slot-level flock
//     enforced by cache.MutateSlotState.
//   - When no slot is eligible but capacity is recoverable (some slot is
//     non-rejected, just at cap), Pick releases the pool lock and blocks on
//     the wake primitive until a peer Release / sweep clears a claim.
type Selector struct {
	cfg      *Config
	stateDir string
	policy   Policy
	wake     PoolWake
}

// NewSelector wires the four collaborators that drive a Selector. None may
// be nil at call time; nil values surface as a clear error on the first
// Pick rather than as a panic deep inside a goroutine.
func NewSelector(cfg *Config, stateDir string, policy Policy, wake PoolWake) *Selector {
	return &Selector{
		cfg:      cfg,
		stateDir: stateDir,
		policy:   policy,
		wake:     wake,
	}
}

// Pick returns the name of a slot the dispatch may use. The slot is reserved
// by appending a fresh InFlightClaim to its cached state. Pick blocks until
// a slot is available, the pool exhausts every slot via rate-limit rejection
// (returns *ErrAllRateLimited), or ctx is cancelled (returns ctx.Err()).
//
// Caller contract: every successful Pick must be paired with a Release
// carrying the same poolName + dispatchID, and the dispatch should
// periodically Heartbeat to defend against the steward's stale-claim sweep.
func (s *Selector) Pick(ctx context.Context, poolName, dispatchID string) (string, error) {
	if s == nil || s.cfg == nil {
		return "", fmt.Errorf("pool: selector not initialized")
	}
	if s.policy == nil {
		return "", fmt.Errorf("pool: selector has nil policy")
	}
	if s.wake == nil {
		return "", fmt.Errorf("pool: selector has nil wake primitive")
	}
	if dispatchID == "" {
		return "", fmt.Errorf("pool: Pick: empty dispatchID")
	}

	slots, err := s.poolSlots(poolName)
	if err != nil {
		return "", err
	}
	if len(slots) == 0 {
		return "", fmt.Errorf("pool: pool %q has no slots configured", poolName)
	}

	if err := ensureStateDir(s.stateDir); err != nil {
		return "", err
	}

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		var (
			picked      string
			rateLimited *ErrAllRateLimited
			needsWait   bool
		)

		err := WithExclusiveLock(s.poolLockPath(poolName), func() error {
			states, err := ListSlotStates(s.stateDir)
			if err != nil {
				return err
			}

			ranked := s.policy.Rank(slots, states, time.Now())
			if len(ranked) > 0 {
				chosen := ranked[0]
				if err := MutateSlotState(s.stateDir, chosen.Name, func(st *SlotState) error {
					now := time.Now()
					st.InFlight = append(st.InFlight, InFlightClaim{
						DispatchID:  dispatchID,
						ClaimedAt:   now,
						HeartbeatAt: now,
					})
					return nil
				}); err != nil {
					return err
				}
				picked = chosen.Name
				return nil
			}

			if e := allRejected(slots, states); e != nil {
				rateLimited = e
				return nil
			}
			needsWait = true
			return nil
		})
		if err != nil {
			return "", err
		}

		switch {
		case picked != "":
			return picked, nil
		case rateLimited != nil:
			return "", rateLimited
		case needsWait:
			if err := s.wake.Wait(ctx, poolName); err != nil {
				return "", err
			}
			// Loop and re-evaluate eligibility under a fresh pool lock.
		default:
			// Defensive: the closure above sets exactly one of the three
			// outcome variables. Reaching here means the closure returned
			// without producing one, which is a bug — surface it.
			return "", fmt.Errorf("pool: selector reached no-decision branch for pool %q", poolName)
		}
	}
}

// Release removes the in-flight claim recorded by Pick and broadcasts on the
// pool's wake channel so any blocked Pick on the same pool re-evaluates. It
// is safe to Release a claim that no longer exists (e.g. already swept by
// the steward) — the slot state mutation is a no-op in that case.
func (s *Selector) Release(poolName, slotName, dispatchID string) error {
	if s == nil {
		return fmt.Errorf("pool: selector not initialized")
	}
	if dispatchID == "" {
		return fmt.Errorf("pool: Release: empty dispatchID")
	}
	if err := MutateSlotState(s.stateDir, slotName, func(st *SlotState) error {
		out := st.InFlight[:0]
		for _, c := range st.InFlight {
			if c.DispatchID != dispatchID {
				out = append(out, c)
			}
		}
		st.InFlight = out
		return nil
	}); err != nil {
		return err
	}
	if s.wake != nil {
		return s.wake.Broadcast(poolName)
	}
	return nil
}

// Heartbeat refreshes the HeartbeatAt timestamp of the matching in-flight
// claim. Returns an error if no claim with dispatchID is on the slot — that
// can mean the steward already swept it, in which case the dispatch should
// abort rather than continue running on a slot it no longer owns.
func (s *Selector) Heartbeat(slotName, dispatchID string) error {
	if s == nil {
		return fmt.Errorf("pool: selector not initialized")
	}
	if dispatchID == "" {
		return fmt.Errorf("pool: Heartbeat: empty dispatchID")
	}
	return MutateSlotState(s.stateDir, slotName, func(st *SlotState) error {
		for i := range st.InFlight {
			if st.InFlight[i].DispatchID == dispatchID {
				st.InFlight[i].HeartbeatAt = time.Now()
				return nil
			}
		}
		return fmt.Errorf("pool: heartbeat: slot %q has no in-flight claim for dispatch %q", slotName, dispatchID)
	})
}

// poolSlots returns the slot list for poolName. Unknown pool names produce
// a descriptive error so misconfigured callers fail loudly.
func (s *Selector) poolSlots(poolName string) ([]SlotConfig, error) {
	switch poolName {
	case poolNameSubscription:
		return s.cfg.Subscription, nil
	case poolNameAPIKey:
		return s.cfg.APIKey, nil
	default:
		return nil, fmt.Errorf("pool: unknown pool %q (want %q or %q)", poolName, poolNameSubscription, poolNameAPIKey)
	}
}

// poolLockPath is the file path of the pool-level exclusive lock. The dot
// prefix keeps it out of ListSlotStates' *.json glob.
func (s *Selector) poolLockPath(poolName string) string {
	return filepath.Join(s.stateDir, ".lock-"+poolName)
}

// allRejected returns a non-nil ErrAllRateLimited iff every slot in slots
// has at least one rejected bucket. The carried ResetsAt is the soonest
// non-zero reset across the rejected buckets, so a parked wizard can sleep
// until then and retry.
//
// "Rejected" is per-slot: a slot counts as rejected if either its FiveHour
// or Overage bucket is RateLimitStatusRejected. A slot with no cached state
// (states[name] == nil) is by definition NOT rejected (zero state) — so a
// pool with even one un-stated slot is treated as "wait for capacity",
// never as "all rejected".
func allRejected(slots []SlotConfig, states map[string]*SlotState) *ErrAllRateLimited {
	if len(slots) == 0 {
		return nil
	}
	var soonest time.Time
	for _, slot := range slots {
		st := states[slot.Name]
		if st == nil {
			return nil
		}
		fhRej := st.RateLimit.FiveHour.Status == RateLimitStatusRejected
		ovRej := st.RateLimit.Overage.Status == RateLimitStatusRejected
		if !fhRej && !ovRej {
			return nil
		}
		if fhRej {
			soonest = earlierNonZero(soonest, st.RateLimit.FiveHour.ResetsAt)
		}
		if ovRej {
			soonest = earlierNonZero(soonest, st.RateLimit.Overage.ResetsAt)
		}
	}
	return &ErrAllRateLimited{ResetsAt: soonest}
}

// earlierNonZero returns the earlier of a and b, ignoring zero values.
// If both are zero, zero is returned.
func earlierNonZero(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case a.Before(b):
		return a
	default:
		return b
	}
}
