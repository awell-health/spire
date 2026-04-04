// formula_bridge.go provides backward-compatible aliases for cmd/spire callers
// after formula types and functions moved to pkg/formula.
package main

import (
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"

	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/steward"
	"github.com/awell-health/spire/pkg/store"
)

// --- Type aliases ---

type FormulaV2 = formula.FormulaV2
type RevisionPolicy = formula.RevisionPolicy

// beadToInfo converts a Bead to formula.BeadInfo for pkg/formula calls.
func beadToInfo(b Bead) formula.BeadInfo {
	return formula.BeadInfo{
		ID:     b.ID,
		Type:   b.Type,
		Labels: b.Labels,
	}
}

// resolveFormulaName returns the v3 formula name for a bead without loading it.
func resolveFormulaName(bead Bead) string {
	return formula.ResolveV3Name(beadToInfo(bead))
}

// ResolveFormulaAny resolves a v3 formula for a bead.
// Returns the formula (*formula.FormulaStepGraph), version 3, and any error.
func ResolveFormulaAny(bead Bead) (interface{}, int, error) {
	return formula.ResolveAny(beadToInfo(bead))
}

// ResolveFormulaV3 resolves a v3 step-graph formula for a bead.
// Returns nil and an error if no v3 formula can be found.
func ResolveFormulaV3(bead Bead) (*formula.FormulaStepGraph, error) {
	return formula.ResolveV3(beadToInfo(bead))
}

// init wires up formula package injection points:
//   - RepoFormulaNameFunc: resolve spire.yaml agent.formula without importing pkg/config
//   - TowerFetcher: look up formulas in the tower's dolt database without importing pkg/store
func init() {
	formula.RepoFormulaNameFunc = repoFormulaName
	formula.TowerFetcher = towerFormulaFetcher
}

// towerSQLDB is a lazy-initialized *sql.DB connection to the dolt server.
// Used by towerFormulaFetcher to query the tower formulas table.
var towerSQLDB *sql.DB

// towerFormulaFetcher retrieves a formula's TOML content from the tower
// database. Returns an error if the dolt server is unreachable or the formula
// doesn't exist — callers (LoadStepGraphByNameWithSource) fall through silently.
func towerFormulaFetcher(name string) (string, error) {
	db, err := ensureTowerSQLDB()
	if err != nil {
		return "", err
	}
	return store.GetTowerFormula(db, name)
}

// ensureTowerSQLDB returns a lazy-initialized *sql.DB pointing at the dolt
// server. The database name is resolved from steward.DaemonDB (set during
// daemon tower cycles), the cmd/spire-level daemonDB fallback, or
// detectDBName() as a last resort.
func ensureTowerSQLDB() (*sql.DB, error) {
	if towerSQLDB != nil {
		return towerSQLDB, nil
	}
	dbName := steward.DaemonDB
	if dbName == "" {
		dbName = daemonDB
	}
	if dbName == "" {
		var err error
		dbName, err = detectDBName()
		if err != nil {
			return nil, fmt.Errorf("tower formula: no database name: %w", err)
		}
	}
	dsn := fmt.Sprintf("root:@tcp(%s:%s)/%s", dolt.Host(), dolt.Port(), dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	towerSQLDB = db
	return db, nil
}

// repoFormulaName resolves a repo-level formula override for a bead.
func repoFormulaName(beadID string) string {
	repoPath, _, _, _ := wizardResolveRepo(beadID)
	if repoPath == "" {
		repoPath = "."
	}
	if cfg, err := repoconfig.Load(repoPath); err == nil && cfg.Agent.Formula != "" {
		return cfg.Agent.Formula
	}
	return ""
}
