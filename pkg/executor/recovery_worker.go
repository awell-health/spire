package executor

import (
	"fmt"

	"github.com/awell-health/spire/pkg/recovery"
)

// executeWorker dispatches a repair apprentice via the bundle-handoff path.
// The repair apprentice is structurally identical to a wave apprentice; only
// the prompt differs ("here-is-the-failure-and-diagnosis-please-fix-it" vs.
// "here-is-your-subtask"). The bundle-handoff wiring is owned by spi-tlj32a;
// this function calls into it via SpawnRepairWorker so the delivery shape stays
// uniform with wave dispatch.
func (e *Executor) executeWorker(plan recovery.RepairPlan, step *StepState, stepName string, state *GraphState) (RecoveryOutcomeKind, error) {
	step.CurrentRepair.Phase = PhaseExecuteWorker
	step.CurrentRepair.WorkerSignalKey = signalKeyFor(stepName, step.CurrentRepair.Round)
	state.Steps[stepName] = *step
	e.persistGraphState(state)

	actionCtx, ws, cleanup, err := e.buildRecoveryActionCtx(e.beadID, plan, StepConfig{With: map[string]string{}}, state)
	if err != nil {
		return RecoveryFailed, fmt.Errorf("executeWorker: provision workspace: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	result, err := SpawnRepairWorker(actionCtx, plan, ws)
	if err != nil {
		return RecoveryFailed, fmt.Errorf("spawn repair worker: %w", err)
	}
	if result.WorkerAttemptID != "" {
		e.log("recovery: worker repair completed (attempt=%s)", result.WorkerAttemptID)
	}

	step.CurrentRepair.Phase = PhaseApplyBundle
	state.Steps[stepName] = *step
	e.persistGraphState(state)

	return RecoveryRepaired, nil
}

// signalKeyFor produces a deterministic bundle-store key for a repair
// apprentice. The key is (step, round, wizard bead) so a crash-resume can
// look up the same key and find the apprentice's delivered bundle without
// guessing.
func signalKeyFor(stepName string, round int) string {
	return fmt.Sprintf("repair-signal-%s-round-%d", stepName, round)
}
