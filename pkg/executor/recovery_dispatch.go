package executor

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/awell-health/spire/pkg/recovery"
)

// DefaultRecoveryBudget is the per-step limit on recovery cycles before the
// wizard escalates to human review. Overridable via SPIRE_RECOVERY_BUDGET or a
// wizard-config entry (future). The budget counts every attempt appended to
// StepState.RepairAttempts — including interrupted cycles — so a crash loop
// cannot spin forever.
const DefaultRecoveryBudget = 3

// recoveryDisabled returns true when inline recovery should be skipped on step
// failure. Inline recovery is opt-in via SPIRE_INLINE_RECOVERY=1 until the
// surrounding Decide/Worker/Mechanical paths are wired end-to-end in a
// wizard's live pod; the default stays on the legacy hook-and-escalate path so
// existing behavior is preserved. The recovery formula itself always skips
// (running recovery inside a recovery cycle would recurse).
func (e *Executor) recoveryDisabled() bool {
	if e.graphState != nil {
		switch e.graphState.Formula {
		case "cleric-default", "recovery", "spire-recovery-v3":
			return true
		}
	}
	if v := os.Getenv("SPIRE_INLINE_RECOVERY"); v == "1" || v == "true" {
		return false
	}
	return true
}

// maxRecoveryAttempts returns the per-step recovery budget. Production reads
// SPIRE_RECOVERY_BUDGET for ops/test overrides; absent or invalid values fall
// back to DefaultRecoveryBudget. Kept as a free function so tests can inject a
// budget without having to reach into executor state.
func maxRecoveryAttempts(_ *StepState) int {
	if v := os.Getenv("SPIRE_RECOVERY_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultRecoveryBudget
}

// runRecoveryCycle is the in-wizard recovery dispatch entry point. When a
// graph step errors, the interpreter calls this instead of immediately hooking
// the bead. The cycle:
//
//  1. Checks the per-step round budget; if exhausted, returns
//     RecoveryBudgetExhausted so the interpreter escalates.
//  2. Opens a recovery cycle: creates (or reuses) a recovery bead, persists
//     graph state with CurrentRepair set, and advances through Diagnose →
//     Decide → Execute phases.
//  3. Persists between every phase so a mid-cycle crash leaves
//     CurrentRepair.Phase pointing at the in-flight work — which is what
//     resumeInFlightRepairs consumes on the next wizard startup.
//  4. Appends a RepairAttempt and clears CurrentRepair before returning.
//
// The attempt is recorded whether the cycle repaired, escalated, or failed —
// interrupted cycles are recorded by the resume path, not here.
func (e *Executor) runRecoveryCycle(step *StepState, stepName string, state *GraphState, failure error) (RecoveryOutcomeKind, error) {
	if step == nil {
		return RecoveryFailed, fmt.Errorf("runRecoveryCycle: nil step")
	}

	budget := maxRecoveryAttempts(step)
	if len(step.RepairAttempts) >= budget {
		e.log("recovery: step %s round budget exhausted (%d >= %d) — escalating",
			stepName, len(step.RepairAttempts), budget)
		return RecoveryBudgetExhausted, nil
	}

	round := len(step.RepairAttempts) + 1
	startedAt := time.Now().UTC().Format(time.RFC3339)

	// Open in-flight state and persist before doing any external work.
	step.CurrentRepair = &InFlightRepair{
		Round:     round,
		Phase:     PhaseCreateRecoveryBead,
		StartedAt: startedAt,
	}
	state.Steps[stepName] = *step
	e.persistGraphState(state)

	// Create (or reuse) the recovery bead. Identity is deterministic per
	// (parent bead, step, round) — idempotent resume after a crash during
	// PhaseCreateRecoveryBead.
	recoveryBeadID, err := e.createOrReuseRecoveryBead(state, stepName, round, failure)
	if err != nil {
		e.log("recovery: create recovery bead for step %s round %d: %s", stepName, round, err)
	}
	step.CurrentRepair.RecoveryBeadID = recoveryBeadID
	step.CurrentRepair.Phase = PhaseDiagnose
	state.Steps[stepName] = *step
	e.persistGraphState(state)

	// Diagnose: reuse pkg/recovery.Diagnose via the same adapter the
	// formula-mode collect_context step uses.
	diag, diagErr := e.diagnoseFailure(stepName, failure, state)
	if diagErr != nil {
		e.log("recovery: diagnose for step %s: %s (continuing with partial context)", stepName, diagErr)
	}

	step.CurrentRepair.Phase = PhaseDecide
	state.Steps[stepName] = *step
	e.persistGraphState(state)

	// Decide: produce a typed RepairPlan.
	plan, decideErr := e.decideRepair(diag, state)
	if decideErr != nil {
		e.log("recovery: decide for step %s: %s (escalating)", stepName, decideErr)
		step.CurrentRepair.Phase = PhaseExecuteMechanical // terminal; phase is immaterial
		return e.finishCycle(step, stepName, state, RepairAttempt{
			Round:          round,
			Outcome:        RecoveryEscalated,
			StartedAt:      startedAt,
			EndedAt:        time.Now().UTC().Format(time.RFC3339),
			RecoveryBeadID: recoveryBeadID,
			FinalPhase:     PhaseDecide,
			Error:          decideErr.Error(),
		}), nil
	}

	step.CurrentRepair.Mode = string(plan.Mode)
	step.CurrentRepair.Action = plan.Action

	// Dispatch by RepairMode.
	outcome, execErr := e.dispatchRepair(plan, step, stepName, state)

	attempt := RepairAttempt{
		Round:          round,
		Mode:           string(plan.Mode),
		Action:         plan.Action,
		Outcome:        outcome,
		StartedAt:      startedAt,
		EndedAt:        time.Now().UTC().Format(time.RFC3339),
		RecoveryBeadID: recoveryBeadID,
		FinalPhase:     step.CurrentRepair.Phase,
	}
	if execErr != nil {
		attempt.Error = execErr.Error()
	}
	return e.finishCycle(step, stepName, state, attempt), execErr
}

// dispatchRepair routes a RepairPlan to the matching execute function based on
// Mode. Each branch advances CurrentRepair.Phase before doing work so a crash
// mid-dispatch resumes correctly.
func (e *Executor) dispatchRepair(plan recovery.RepairPlan, step *StepState, stepName string, state *GraphState) (RecoveryOutcomeKind, error) {
	switch plan.Mode {
	case recovery.RepairModeNoop:
		return e.executeNoop(plan, step, stepName, state)
	case recovery.RepairModeMechanical, recovery.RepairModeRecipe:
		// Merge-conflict resolution is a labeled mechanical action that
		// runs a Claude-driven resolver on staging. Keep it as a named
		// branch so tests can target it explicitly.
		if plan.Action == "resolve-conflicts" {
			return e.executeMergeConflict(plan, step, stepName, state)
		}
		return e.executeMechanical(plan, step, stepName, state)
	case recovery.RepairModeWorker:
		return e.executeWorker(plan, step, stepName, state)
	case recovery.RepairModeEscalate:
		return e.executeEscalate(plan, step, stepName, state)
	default:
		return RecoveryFailed, fmt.Errorf("unsupported repair mode %q", plan.Mode)
	}
}

// finishCycle appends the RepairAttempt, clears CurrentRepair, persists, and
// returns the outcome unchanged. It is the single closing function for every
// exit from runRecoveryCycle.
func (e *Executor) finishCycle(step *StepState, stepName string, state *GraphState, attempt RepairAttempt) RecoveryOutcomeKind {
	step.RepairAttempts = append(step.RepairAttempts, attempt)
	step.CurrentRepair = nil
	state.Steps[stepName] = *step
	e.persistGraphState(state)
	e.log("recovery: step %s round %d closed: outcome=%s mode=%s action=%s",
		stepName, attempt.Round, attempt.Outcome, attempt.Mode, attempt.Action)
	return attempt.Outcome
}

// persistGraphState saves the graph state through the configured store. A save
// failure is logged but not fatal: the in-memory state is still correct; the
// next save attempt may succeed. Crash-resume tolerance is the reason we write
// between every phase advance — it is acceptable for a single write to fail.
func (e *Executor) persistGraphState(state *GraphState) {
	if state == nil {
		return
	}
	store := e.graphStateStore()
	if store == nil {
		return
	}
	if err := store.Save(e.agentName, state); err != nil {
		e.log("warning: persist graph state: %s", err)
	}
}
