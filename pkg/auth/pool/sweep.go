package pool

import (
	"fmt"
	"log"
	"time"
)

// Sweep removes in-flight claims whose heartbeats have gone silent for
// longer than staleAge from every cached SlotState under stateDir. For
// each pool (subscription / api-key) that lost at least one claim,
// wake.Broadcast fires exactly once after the scan so Pickers parked
// at MaxConcurrent re-evaluate.
//
// Errors mutating an individual slot do not abort the sweep — they are
// logged and the next slot is processed. The first error encountered
// is returned alongside the count of claims actually removed across
// every slot that completed successfully.
//
// staleAge is the threshold the steward applies to a claim's
// HeartbeatAt: a claim is dropped iff time.Since(HeartbeatAt) >
// staleAge. Production callers pass 60*time.Second (the heartbeat
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
	affectedPools := make(map[string]struct{})
	totalRemoved := 0
	cutoff := time.Now().Add(-staleAge)

	for slotName := range states {
		var removed int
		mErr := MutateSlotState(stateDir, slotName, func(s *SlotState) error {
			kept := s.InFlight[:0]
			for _, c := range s.InFlight {
				if c.HeartbeatAt.Before(cutoff) {
					removed++
					continue
				}
				kept = append(kept, c)
			}
			s.InFlight = kept
			return nil
		})
		if mErr != nil {
			log.Printf("[pool.sweep] mutate slot %q: %v", slotName, mErr)
			if firstErr == nil {
				firstErr = mErr
			}
			continue
		}
		if removed == 0 {
			continue
		}
		totalRemoved += removed
		if pool, ok := slotToPool[slotName]; ok {
			affectedPools[pool] = struct{}{}
		}
	}

	if wake != nil {
		for pool := range affectedPools {
			if bErr := wake.Broadcast(pool); bErr != nil {
				log.Printf("[pool.sweep] broadcast pool %q: %v", pool, bErr)
				if firstErr == nil {
					firstErr = bErr
				}
			}
		}
	}

	return totalRemoved, firstErr
}

// buildSlotToPool inverts cfg's pool->slot mapping into a slot->pool
// lookup so Sweep decides which pool to broadcast on without
// re-scanning cfg per slot. nil cfg yields an empty map.
func buildSlotToPool(cfg *Config) map[string]string {
	out := make(map[string]string)
	if cfg == nil {
		return out
	}
	for _, s := range cfg.Subscription {
		out[s.Name] = poolNameSubscription
	}
	for _, s := range cfg.APIKey {
		out[s.Name] = poolNameAPIKey
	}
	return out
}
