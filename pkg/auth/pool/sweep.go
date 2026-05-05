package pool

import (
	"fmt"
	"log"
	"time"
)

// poolSubscription and poolAPIKey are the canonical pool name strings
// used to key PoolWake broadcasts. The selector keys claims by these
// same names; Sweep must use the identical strings so a wake fired
// here actually unblocks waiters parked in Pick.
const (
	poolSubscription = "subscription"
	poolAPIKey       = "api-key"
)

// Sweep removes in-flight claims whose heartbeats have gone silent for
// longer than staleAge from every cached SlotState under stateDir. For
// each pool (subscription / api-key) that had at least one claim
// removed, wake.Broadcast is fired exactly once after the scan so
// Pickers parked at MaxConcurrent can re-evaluate.
//
// Errors mutating an individual slot do not abort the sweep — they are
// logged and the next slot is processed. The first error encountered
// is returned alongside the count of claims actually removed across
// all slots that completed successfully.
//
// staleAge is the threshold the steward applies to a claim's
// HeartbeatAt: a claim is dropped iff time.Since(HeartbeatAt) >
// staleAge. Production callers pass 60 * time.Second (the heartbeat
// helper bumps HeartbeatAt every ~15s, so 60s is four missed beats).
// A claim with a zero-value HeartbeatAt is always treated as stale —
// time.Since on the zero time returns a very large duration.
//
// Slots whose files exist but whose names are not in cfg (e.g. a
// credential rotated out of auth.toml) still get their stale claims
// pruned, but no broadcast fires for them — there is no pool to wake.
func Sweep(stateDir string, staleAge time.Duration, wake PoolWake, cfg *Config) (int, error) {
	states, err := ListSlotStates(stateDir)
	if err != nil {
		return 0, fmt.Errorf("pool.Sweep: list slot states: %w", err)
	}

	slotToPool := buildSlotToPool(cfg)

	var firstErr error
	affected := make(map[string]struct{})
	totalRemoved := 0
	cutoff := time.Now().Add(-staleAge)

	for slotName := range states {
		var slotRemoved int
		mErr := MutateSlotState(stateDir, slotName, func(s *SlotState) error {
			kept := make([]InFlightClaim, 0, len(s.InFlight))
			for _, c := range s.InFlight {
				if c.HeartbeatAt.Before(cutoff) {
					slotRemoved++
					continue
				}
				kept = append(kept, c)
			}
			s.InFlight = kept
			return nil
		})
		if mErr != nil {
			log.Printf("pool.Sweep: mutate slot %q: %v", slotName, mErr)
			if firstErr == nil {
				firstErr = mErr
			}
			continue
		}
		if slotRemoved > 0 {
			totalRemoved += slotRemoved
			if pool, ok := slotToPool[slotName]; ok {
				affected[pool] = struct{}{}
			}
		}
	}

	if wake != nil {
		for pool := range affected {
			if bErr := wake.Broadcast(pool); bErr != nil {
				log.Printf("pool.Sweep: broadcast pool %q: %v", pool, bErr)
				if firstErr == nil {
					firstErr = bErr
				}
			}
		}
	}

	return totalRemoved, firstErr
}

// buildSlotToPool inverts cfg's pool->slot mapping into a slot->pool
// lookup so Sweep can decide which pool to broadcast on without
// re-scanning cfg per slot. nil cfg yields an empty map.
func buildSlotToPool(cfg *Config) map[string]string {
	out := make(map[string]string)
	if cfg == nil {
		return out
	}
	for _, s := range cfg.Subscription {
		out[s.Name] = poolSubscription
	}
	for _, s := range cfg.APIKey {
		out[s.Name] = poolAPIKey
	}
	return out
}
