// formula_bridge.go provides backward-compatible aliases for cmd/spire callers
// after formula types and functions moved to pkg/formula.
package main

import (
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/repoconfig"
)

// --- Type aliases ---
// Dead aliases removed: PhaseConfig, FormulaVar, FormulaStepGraph, StepConfig.

type FormulaV2 = formula.FormulaV2
type RevisionPolicy = formula.RevisionPolicy

// --- Variable aliases ---
// Dead aliases removed: validPhases, DefaultFormulaMap, isValidPhase — no callers.

// --- Function aliases ---
// Dead aliases removed: ParseFormulaV2, ParseFormulaStepGraph, LoadFormulaV2,
// FindFormula, LoadEmbeddedFormula, LoadReviewPhaseFormula — no callers.

var (
	LoadFormulaByName = formula.LoadFormulaByName
)

// beadToInfo converts a Bead to formula.BeadInfo for pkg/formula calls.
func beadToInfo(b Bead) formula.BeadInfo {
	return formula.BeadInfo{
		ID:     b.ID,
		Type:   b.Type,
		Labels: b.Labels,
	}
}

// ResolveFormula determines which formula to use for a bead.
// Delegates to formula.Resolve, converting Bead -> formula.BeadInfo.
func ResolveFormula(bead Bead) (*FormulaV2, error) {
	return formula.Resolve(beadToInfo(bead))
}

// resolveFormulaName returns the formula name for a bead without loading it.
// Uses ResolveAny to respect the v3-default resolution order.
func resolveFormulaName(bead Bead) string {
	info := beadToInfo(bead)
	if formula.WantsV2(info) {
		return formula.ResolveName(info)
	}
	return formula.ResolveV3Name(info)
}

// ResolveFormulaAny resolves either a v2 or v3 formula for a bead.
// Returns the formula (either *FormulaV2 or *formula.FormulaStepGraph),
// the version number, and any error.
func ResolveFormulaAny(bead Bead) (interface{}, int, error) {
	return formula.ResolveAny(beadToInfo(bead))
}

// ResolveFormulaV3 resolves a v3 step-graph formula for a bead.
// Returns nil and an error if no v3 formula can be found.
func ResolveFormulaV3(bead Bead) (*formula.FormulaStepGraph, error) {
	return formula.ResolveV3(beadToInfo(bead))
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
