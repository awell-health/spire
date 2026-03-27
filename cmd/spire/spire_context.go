package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/steveyegge/beads"
)

// SpireContext holds the store connection and tower metadata for a single
// operational scope. It replaces the package-level activeStore/storeCtx/daemonDB
// globals, enabling multi-tower support and proper testing.
//
// Command entry points create a SpireContext via NewSpireContext() (for CLI
// commands) or NewSpireContextForTower() (for daemon/steward tower cycles)
// and pass it through the call chain.
type SpireContext struct {
	store    beads.Storage
	storeCtx context.Context

	// Tower is non-nil when the context was created for a specific tower.
	Tower *TowerConfig

	// RepoPath is the path to the repo that resolved this context (if any).
	RepoPath string

	// BeadsDir is the resolved .beads/ directory path.
	BeadsDir string

	// DBName is the dolt database name. Used by doltSQL() for --use-db.
	// Replaces the old daemonDB global.
	DBName string
}

// NewSpireContext creates a SpireContext from environment/config resolution.
// It resolves the beads directory using the same logic as the old
// resolveBeadsDir() + ensureStore() pattern but does NOT eagerly open the store.
func NewSpireContext() (*SpireContext, error) {
	beadsDir := resolveBeadsDir()
	if beadsDir == "" {
		return nil, fmt.Errorf("no .beads directory found")
	}
	return &SpireContext{BeadsDir: beadsDir}, nil
}

// NewSpireContextForTower creates a SpireContext for a specific tower.
// Used by daemon and steward tower cycles. Sets BeadsDir from the tower's
// data directory and DBName from the tower's database name.
func NewSpireContextForTower(tower TowerConfig) (*SpireContext, error) {
	beadsDir := filepath.Join(doltDataDir(), tower.Database, ".beads")
	return &SpireContext{
		BeadsDir: beadsDir,
		Tower:    &tower,
		DBName:   tower.Database,
	}, nil
}

// EnsureStore lazy-opens the beads store on first call. Subsequent calls
// return the cached store. This has the same semantics as the old global
// ensureStore() but scoped to this SpireContext.
func (sc *SpireContext) EnsureStore() (beads.Storage, error) {
	if sc.store != nil {
		return sc.store, nil
	}
	beadsDir := sc.BeadsDir
	if beadsDir == "" {
		beadsDir = resolveBeadsDir()
		if beadsDir == "" {
			return nil, fmt.Errorf("no .beads directory found")
		}
		sc.BeadsDir = beadsDir
	}
	ctx := context.Background()
	store, err := beads.OpenFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("open beads store: %w", err)
	}
	sc.store = store
	sc.storeCtx = ctx
	return store, nil
}

// Close releases the store connection and nils fields.
// Safe to call multiple times.
func (sc *SpireContext) Close() {
	if sc.store != nil {
		sc.store.Close()
		sc.store = nil
		sc.storeCtx = nil
	}
}
