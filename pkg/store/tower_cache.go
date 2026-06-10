package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/steveyegge/beads"
)

// Warm per-tower store cache.
//
// Background: the steward (TowerCycle) and daemon (DaemonTowerCycle) used to
// call store.OpenAt(beadsDir) + defer store.Reset() on EVERY cycle. OpenAt
// closes the prior store's connection pool and opens a fresh one; the deferred
// Reset tears it down again at cycle end. Because cycles run effectively
// back-to-back, this rebuilt the Dolt connection pool continuously — every
// cycle started cold and every query inside it opened fresh connections that
// were thrown away at cycle end. Each new Dolt connection pays a server-side
// session-bootstrap cost, so the churn pinned several CPU cores on the dolt
// sql-server even when little real work was happening. (See releases/v0.52.0.)
//
// UseTowerStore replaces that pattern: it opens each tower's store once, caches
// it keyed by beadsDir, and reuses it across cycles so the underlying pool
// (maxIdle=5, ConnMaxLifetime=5m) stays warm. Callers MUST NOT pair it with a
// per-cycle store.Reset().
//
// Resilience: the previous per-cycle Reset doubled as a reconnect-on-restart
// stopgap (see pkg/board/README.md). To preserve that, a cache hit health-checks
// the cached connection (Ping) and transparently reopens if the dolt server was
// restarted out from under us.
var (
	towerStoresMu sync.Mutex
	towerStores   = map[string]beads.Storage{}
)

// UseTowerStore returns a warm, cached store for beadsDir, opening one on first
// use and reusing it across cycles. It sets the package active store so the
// usual store.X helpers operate against this tower. Unlike OpenAt it does NOT
// close the previously-active store and MUST NOT be paired with a per-cycle
// Reset().
func UseTowerStore(beadsDir string) (beads.Storage, error) {
	if beadsDir == "" {
		return nil, fmt.Errorf("UseTowerStore: beadsDir must not be empty")
	}

	towerStoresMu.Lock()
	defer towerStoresMu.Unlock()

	if s, ok := towerStores[beadsDir]; ok {
		if towerStoreHealthy(s) {
			if connDebugEnabled() {
				log.Printf("[store-conn] tower-store cache HIT beadsDir=%s", beadsDir)
			}
			activeStore = s
			storeCtx = context.Background()
			return s, nil
		}
		// Cached connection is dead — most likely the dolt server restarted.
		// Drop it and fall through to reopen, preserving the old per-cycle
		// Reset's reconnect-on-restart behavior.
		if connDebugEnabled() {
			log.Printf("[store-conn] tower-store cache STALE (reopening) beadsDir=%s", beadsDir)
		}
		_ = s.Close()
		delete(towerStores, beadsDir)
	}

	ConnOpen("store.UseTowerStore:" + beadsDir)
	if connDebugEnabled() {
		log.Printf("[store-conn] tower-store cache MISS (opening) beadsDir=%s", beadsDir)
	}
	ctx := context.Background()
	s, err := beads.OpenFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("UseTowerStore: open %s: %w", beadsDir, err)
	}
	towerStores[beadsDir] = s
	activeStore = s
	storeCtx = ctx
	return s, nil
}

// CloseTowerStores closes and evicts every cached tower store. Call on daemon
// or steward shutdown. Safe to call when the cache is empty.
func CloseTowerStores() {
	towerStoresMu.Lock()
	defer towerStoresMu.Unlock()
	for dir, s := range towerStores {
		_ = s.Close()
		delete(towerStores, dir)
		if connDebugEnabled() {
			log.Printf("[store-conn] tower-store closed beadsDir=%s", dir)
		}
	}
	activeStore = nil
	storeCtx = nil
}

// towerStoreHealthy reports whether a cached store's underlying connection is
// still usable. Stores that don't expose a *sql.DB (or where it's nil) are
// assumed healthy — we can't probe them, and treating "unknown" as dead would
// force a needless reopen on every hit.
func towerStoreHealthy(s beads.Storage) bool {
	acc, ok := s.(interface{ DB() *sql.DB })
	if !ok {
		return true
	}
	db := acc.DB()
	if db == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return db.PingContext(ctx) == nil
}
