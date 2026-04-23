package executor

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// diagnoseFailure runs pkg/recovery.Diagnose against the wizard's own bead.
// When Diagnose returns an error, a stub Diagnosis is produced from what the
// failure itself tells us so the decide step still has something to reason
// over (parity with the formula-era collect_context step's fallback).
func (e *Executor) diagnoseFailure(stepName string, failure error, state *GraphState) (recovery.Diagnosis, error) {
	rdeps := buildExecutorRecoveryDeps(e)
	diag, err := recovery.Diagnose(e.beadID, rdeps)
	if err != nil || diag == nil {
		stub := recovery.Diagnosis{
			BeadID:      e.beadID,
			FailureMode: recovery.FailStepFailure,
			StepContext: &recovery.StepContext{StepName: stepName},
		}
		if failure != nil {
			stub.InterruptLabel = "interrupted:step-failure"
		}
		return stub, err
	}
	if diag.StepContext == nil {
		diag.StepContext = &recovery.StepContext{StepName: stepName}
	} else if diag.StepContext.StepName == "" {
		diag.StepContext.StepName = stepName
	}
	return *diag, nil
}

// decideRepair wraps recovery.Decide for the in-wizard path. The plumbing
// matches handleDecide's construction in spirit — Claude runner, promotion
// threshold, bead learnings — but is scoped down to what the inline path
// needs. Absent deps (e.g. no ClaudeRunner in tests) are silently tolerated;
// recovery.Decide falls back to a resummon plan.
func (e *Executor) decideRepair(diag recovery.Diagnosis, state *GraphState) (recovery.RepairPlan, error) {
	recDeps := recovery.Deps{
		RecoveryBeadID: e.beadID,
		Logf:           e.log,
		MaxAttempts:    DefaultRecoveryBudget,
	}

	if e.deps != nil {
		if e.deps.AddComment != nil {
			recDeps.AddRecoveryBeadComment = func(text string) error {
				return e.deps.AddComment(e.beadID, text)
			}
		}
		if e.deps.SetBeadMetadata != nil {
			recDeps.SetRecoveryBeadMeta = func(meta map[string]string) error {
				return e.deps.SetBeadMetadata(e.beadID, meta)
			}
		}
		if e.deps.ClaudeRunner != nil {
			recDeps.ClaudeRunner = func(args []string, label string) ([]byte, error) {
				return e.runClaude(args, label)
			}
		}
	}

	// History is the recovery_attempts table for this bead (if the DB is
	// wired). Absent DB yields empty history; Decide treats that as "first
	// attempt" which is correct for the inline path's first cycle.
	var history []recovery.Attempt
	if e.deps != nil && e.deps.DoltDB != nil {
		if db := e.deps.DoltDB(); db != nil {
			rows, qErr := loadRecoveryAttemptHistory(db, e.beadID)
			if qErr != nil {
				e.log("recovery: decide: load attempt history: %s", qErr)
			} else {
				history = rows
			}
		}
	}

	plan, err := recovery.Decide(context.Background(), diag, history, recDeps)
	if err != nil {
		return recovery.RepairPlan{}, fmt.Errorf("recovery.Decide: %w", err)
	}
	return plan, nil
}

// loadRecoveryAttemptHistory reads prior recovery_attempts rows for a bead from
// Dolt. The in-wizard path uses the same persistence surface as the legacy
// formula path so history accumulates consistently across paths.
func loadRecoveryAttemptHistory(db *sql.DB, beadID string) ([]recovery.Attempt, error) {
	return store.ListRecoveryAttempts(db, beadID)
}

// createOrReuseRecoveryBead opens or resumes the recovery bead for a given
// (step, round) tuple. The in-wizard path still creates a separate recovery
// bead so the existing bd show / audit tooling can surface the cycle history.
// Identity is deterministic per (parent, step, round), making the call
// idempotent on resume after a mid-create crash.
func (e *Executor) createOrReuseRecoveryBead(state *GraphState, stepName string, round int, failure error) (string, error) {
	if e.deps == nil {
		return "", fmt.Errorf("no deps — cannot create recovery bead")
	}

	// Look for an existing open recovery bead from this cycle.
	if e.deps.GetChildren != nil {
		existingID := findExistingRecoveryBeadID(e.deps, e.beadID, stepName, round)
		if existingID != "" {
			return existingID, nil
		}
	}

	if e.deps.CreateBead == nil {
		return "", nil
	}

	title := fmt.Sprintf("Recovery: %s step %s (round %d)", e.beadID, stepName, round)
	opts := store.CreateOpts{
		Title:       title,
		Description: recoveryFailureDescription(stepName, round, failure),
		Type:        beads.IssueType("recovery"),
		Priority:    1,
		Parent:      e.beadID,
		Labels: []string{
			"recovery",
			fmt.Sprintf("recovery:step:%s", stepName),
			fmt.Sprintf("recovery:round:%d", round),
			fmt.Sprintf("recovery:source:%s", e.beadID),
		},
	}
	id, err := e.deps.CreateBead(opts)
	if err != nil {
		return "", fmt.Errorf("create recovery bead: %w", err)
	}
	if e.deps.SetBeadMetadata != nil {
		_ = e.deps.SetBeadMetadata(id, map[string]string{
			recovery.KeySourceBead:   e.beadID,
			recovery.KeyFailureClass: string(recovery.FailStepFailure),
		})
	}
	return id, nil
}

// findExistingRecoveryBeadID walks the parent's children for a recovery bead
// that matches the (step, round) pair. Deterministic identity is what makes
// seam 3 idempotent — resume during PhaseCreateRecoveryBead finds the already-
// created bead instead of creating a duplicate.
func findExistingRecoveryBeadID(deps *Deps, parentID, stepName string, round int) string {
	children, err := deps.GetChildren(parentID)
	if err != nil {
		return ""
	}
	stepLabel := fmt.Sprintf("recovery:step:%s", stepName)
	roundLabel := fmt.Sprintf("recovery:round:%d", round)
	for _, c := range children {
		hasStep := false
		hasRound := false
		for _, l := range c.Labels {
			if l == stepLabel {
				hasStep = true
			}
			if l == roundLabel {
				hasRound = true
			}
		}
		if hasStep && hasRound && c.Status != "closed" {
			return c.ID
		}
	}
	return ""
}

// recoveryFailureDescription renders the failing step + error text for the
// recovery bead's description. Keeps the bead useful to humans reading bd show
// even though the wizard itself is the only consumer of the JSON decide path.
func recoveryFailureDescription(stepName string, round int, failure error) string {
	desc := fmt.Sprintf("Step %s failed (round %d).", stepName, round)
	if failure != nil {
		desc += "\n\nError:\n" + failure.Error()
	}
	return desc
}
