package pool

import (
	"context"
	"errors"
	"log"
	"time"
)

// errClaimGone is the sentinel returned from inside the MutateSlotState
// callback when the matching InFlightClaim is no longer present. Because
// MutateSlotState skips the write when the callback returns a non-nil
// error, this both signals "stop heartbeating" and avoids a pointless
// rewrite of unchanged state.
var errClaimGone = errors.New("pool: in-flight claim no longer present")

// Heartbeat keeps an in-flight claim's HeartbeatAt fresh while a dispatch
// is active so the steward sweep does not reap it as stale.
//
// At every tick of the given interval, Heartbeat opens the per-slot state
// at <stateDir>/<slotName>.json under the cache's exclusive lock, finds
// the InFlightClaim whose DispatchID matches dispatchID, and bumps
// HeartbeatAt to time.Now(). Per-tick errors (lock contention, decode
// failures, transient I/O) are logged and the loop continues — a
// momentary filesystem hiccup should drop a single beat, not kill an
// in-flight dispatch.
//
// Heartbeat returns:
//   - ctx.Err() once ctx.Done() fires (graceful shutdown).
//   - nil once the matching claim is no longer in the slot state. Two
//     cases produce this: the wizard already called Release, or the
//     steward sweep removed the claim. Either way there is nothing left
//     to refresh.
//
// Callers typically pass interval = 30s and bind ctx to the dispatch's
// own context so the goroutine ends when the dispatch ends.
func Heartbeat(ctx context.Context, stateDir, slotName, dispatchID string, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("pool.Heartbeat: interval must be > 0")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			gone, err := bumpHeartbeat(stateDir, slotName, dispatchID)
			if gone {
				return nil
			}
			if err != nil {
				log.Printf("pool: heartbeat tick failed for slot=%q dispatch=%q: %v", slotName, dispatchID, err)
			}
		}
	}
}

// bumpHeartbeat performs one read-modify-write pass over the slot state
// to refresh the matching claim's HeartbeatAt. The gone return is true
// when the matching claim is absent — a clean exit signal, not an error.
// The error return is non-nil only on real failures (lock, I/O, decode).
func bumpHeartbeat(stateDir, slotName, dispatchID string) (gone bool, err error) {
	mutateErr := MutateSlotState(stateDir, slotName, func(s *SlotState) error {
		for i := range s.InFlight {
			if s.InFlight[i].DispatchID == dispatchID {
				s.InFlight[i].HeartbeatAt = time.Now()
				return nil
			}
		}
		return errClaimGone
	})
	if errors.Is(mutateErr, errClaimGone) {
		return true, nil
	}
	return false, mutateErr
}
