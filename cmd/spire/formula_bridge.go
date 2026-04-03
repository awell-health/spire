// formula_bridge.go provides backward-compatible aliases for cmd/spire callers
// after formula types and functions moved to pkg/formula.
package main

import (
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/repoconfig"
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

// init wires up the repo-level formula name callback so pkg/formula
// can resolve spire.yaml agent.formula without importing pkg/config
// or cmd/spire internals.
func init() {
	formula.RepoFormulaNameFunc = repoFormulaName
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
