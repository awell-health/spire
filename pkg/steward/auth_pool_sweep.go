package steward

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/awell-health/spire/pkg/auth/pool"
	"github.com/awell-health/spire/pkg/config"
)

// authPoolSweepInterval is the cadence at which RunAuthPoolSweep
// invokes pool.Sweep across every configured tower. var (not const)
// so tests can drop it to a few milliseconds; production keeps the
// 30s default. Half the production staleAge so a stale claim is at
// most one full interval past expiry before being reaped.
var authPoolSweepInterval = 30 * time.Second

// authPoolSweepStaleAge is the heartbeat-age threshold passed to
// pool.Sweep. The wizard heartbeat helper bumps HeartbeatAt every
// ~15s, so 60s is four missed beats — wide enough to absorb a paused
// process or a slow filesystem flush, narrow enough to recover
// capacity within one short window.
const authPoolSweepStaleAge = 60 * time.Second

// Test seams. Production keeps these pointed at the real package
// functions. The cadence test stubs authPoolSweepTickFunc to count
// ticks without touching disk; the per-tower tests stub the inner
// load/wake/sweep trio so behavior can be asserted without standing
// up real auth-state directories.
var (
	authPoolSweepTickFunc = runOneAuthPoolSweep
	authPoolListTowersFn  = config.ListTowerConfigs
	authPoolLoadConfigFn  = pool.LoadConfig
	authPoolNewWakeFn     = func(stateDir string) pool.PoolWake { return pool.NewPoolWake(stateDir) }
	authPoolSweepFn       = pool.Sweep
)

// RunAuthPoolSweep blocks until ctx is cancelled, periodically
// running pool.Sweep across every configured tower's auth-state
// directory. On every tick the sweep:
//
//  1. Lists every configured tower via config.ListTowerConfigs.
//  2. Per tower, reloads pool.LoadConfig so a freshly-added auth.toml
//     is picked up without restarting the steward; an unconfigured
//     tower (no auth.toml AND no legacy credentials.toml) is skipped
//     silently — that is the steady state until the operator wires
//     the pool.
//  3. Constructs a fresh PoolWake against the tower's state directory
//     and calls pool.Sweep to remove stale in-flight claims and
//     broadcast wake on any pool that lost a claim.
//  4. Logs the removed count when > 0.
//
// All disk-touching errors are logged-and-skipped; a transient
// failure on one tower never tears down the goroutine. The function
// runs one sweep at startup before installing the ticker so a
// freshly-started steward immediately reaps claims left over from a
// crashed prior run.
func RunAuthPoolSweep(ctx context.Context) {
	authPoolSweepTickFunc()

	ticker := time.NewTicker(authPoolSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			authPoolSweepTickFunc()
		}
	}
}

// runOneAuthPoolSweep executes one sweep cycle across every
// configured tower. Errors listing towers are logged and the cycle
// is skipped — the next tick retries.
func runOneAuthPoolSweep() {
	towers, err := authPoolListTowersFn()
	if err != nil {
		log.Printf("[steward] auth-pool sweep: list towers: %v (skipping cycle)", err)
		return
	}
	for _, t := range towers {
		sweepOneTower(t)
	}
}

// sweepOneTower runs pool.Sweep against a single tower's auth-state
// directory. Missing-config (no auth.toml AND no legacy
// credentials.toml) is the steady state for a tower that has not
// configured the multi-token pool — it returns silently. Other load
// errors are logged but never propagated; the next tick will retry.
//
// towerDir is filepath.Dir(OLAPPath()) — OLAPPath returns
// <base>/spire/<slug>/analytics.db, so its parent is the per-tower
// data dir. The auth-state subdirectory holds the per-slot SlotState
// JSON files that pool.Sweep prunes.
func sweepOneTower(t config.TowerConfig) {
	towerDir := filepath.Dir(t.OLAPPath())
	stateDir := filepath.Join(towerDir, "auth-state")

	cfg, err := authPoolLoadConfigFn(towerDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		log.Printf("[steward] [%s] auth-pool sweep: load config: %v (skipping)", t.Name, err)
		return
	}

	wake := authPoolNewWakeFn(stateDir)
	removed, sErr := authPoolSweepFn(stateDir, authPoolSweepStaleAge, wake, cfg)
	if sErr != nil {
		log.Printf("[steward] [%s] auth-pool sweep: %v", t.Name, sErr)
	}
	if removed > 0 {
		log.Printf("[steward] [%s] auth-pool sweep: removed %d stale claim(s)", t.Name, removed)
	}
}
