package executor

// dispatch_auth.go — auth-pool integration helper for the dispatch sites.
//
// Every apprentice/sage/cleric Spawner.Spawn site goes through
// acquireAuthPoolSlot to reserve a credential slot from the multi-token
// auth pool before the spawn and release it after Wait returns. The helper
// is a thin wrapper around Deps.AuthPool that:
//
//   1. Calls AuthPool.Acquire(ctx, dispatchID).
//   2. On success, stamps the lease's AuthEnv / SlotName / PoolStateDir
//      onto the SpawnConfig so the spawned subprocess sees the right env
//      vars and can write rate-limit events back to the slot's state file.
//   3. Returns the (possibly nil) Release closure for the caller to defer.
//
// When Deps.AuthPool is nil, every call is a no-op: the lease is empty,
// the SpawnConfig is unchanged, and Release is a non-nil no-op so callers
// can `defer release()` unconditionally.

import (
	"context"
	"errors"

	"github.com/awell-health/spire/pkg/agent"
)

// acquireAuthPoolSlot reserves an auth-pool slot for a single dispatch and
// returns the (possibly mutated) SpawnConfig + a Release closure to defer.
//
// dispatchID must be unique per spawn within the wizard process; the
// callers pass cfg.Name (e.g. "wizard-spi-abc-implement-1"), which is
// already monotonic across resets.
//
// On *RateLimitedError (every slot rejected, no fallback) the original
// error is propagated unchanged so the caller can errors.As against
// *RateLimitedError and park the step with the carried ResetsAt.
func acquireAuthPoolSlot(ctx context.Context, deps *Deps, cfg agent.SpawnConfig, dispatchID string) (agent.SpawnConfig, func(), error) {
	if deps == nil || deps.AuthPool == nil {
		return cfg, noopRelease, nil
	}
	if dispatchID == "" {
		return cfg, noopRelease, errors.New("acquireAuthPoolSlot: empty dispatchID")
	}

	lease, err := deps.AuthPool.Acquire(ctx, dispatchID)
	if err != nil {
		return cfg, noopRelease, err
	}

	// Apply the lease to cfg. AuthEnv carries the single Anthropic env
	// entry the spawned process should see; AuthSlot is the slot name for
	// observability; PoolStateDir flows through to SPIRE_AUTH_POOL_STATE_DIR
	// so the apprentice can mutate the slot file.
	if len(lease.AuthEnv) > 0 {
		cfg.AuthEnv = lease.AuthEnv
	}
	if lease.SlotName != "" {
		cfg.AuthSlot = lease.SlotName
	}
	if lease.PoolStateDir != "" {
		cfg.PoolStateDir = lease.PoolStateDir
	}

	release := lease.Release
	if release == nil {
		release = noopRelease
	}
	return cfg, release, nil
}

// noopRelease is the canonical no-op cleanup func. Returned in place of nil
// from acquireAuthPoolSlot so callers can `defer release()` unconditionally
// without a nil-check.
func noopRelease() {}
