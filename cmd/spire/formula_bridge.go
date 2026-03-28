// formula_bridge.go provides backward-compatible aliases for cmd/spire callers
// after formula types and functions moved to pkg/formula.
package main

import (
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/repoconfig"
)

// --- Type aliases ---

type FormulaV2 = formula.FormulaV2
type PhaseConfig = formula.PhaseConfig
type RevisionPolicy = formula.RevisionPolicy
type FormulaVar = formula.FormulaVar
type FormulaStepGraph = formula.FormulaStepGraph
type StepConfig = formula.StepConfig

// --- Variable aliases ---

var (
	validPhases       = formula.ValidPhases
	DefaultFormulaMap = formula.DefaultFormulaMap
)

// --- Function aliases ---

var (
	isValidPhase          = formula.IsValidPhase
	ParseFormulaV2        = formula.ParseFormulaV2
	ParseFormulaStepGraph = formula.ParseFormulaStepGraph
	LoadFormulaV2         = formula.LoadFormulaV2
	FindFormula           = formula.FindFormula
	LoadEmbeddedFormula   = formula.LoadEmbeddedFormula
	LoadFormulaByName     = formula.LoadFormulaByName
	LoadReviewPhaseFormula = formula.LoadReviewPhaseFormula
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
func resolveFormulaName(bead Bead) string {
	return formula.ResolveName(beadToInfo(bead))
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
