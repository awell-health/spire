package executor

import (
	"fmt"

	"github.com/awell-health/spire/pkg/recovery"
)

// executeMechanical runs a deterministic RepairMode=Mechanical plan against
// the wizard's own staging workspace. Mechanical actions (rebase-onto-base,
// cherry-pick, rebuild, reset-to-step) live in pkg/executor/recovery_actions.go
// as mechanicalActions entries — the in-wizard path reuses those so
// behavior stays uniform with the legacy formula path.
//
// Each mechanical fn is expected to self-detect "already applied" (seam 6) —
// if the workspace state is already what the repair would produce, the fn
// returns success without re-running.
func (e *Executor) executeMechanical(plan recovery.RepairPlan, step *StepState, stepName string, state *GraphState) (RecoveryOutcomeKind, error) {
	step.CurrentRepair.Phase = PhaseExecuteMechanical
	state.Steps[stepName] = *step
	e.persistGraphState(state)

	actionCtx, ws, cleanup, err := e.buildRecoveryActionCtx(e.beadID, plan, StepConfig{With: map[string]string{}}, state)
	if err != nil {
		return RecoveryFailed, fmt.Errorf("executeMechanical: provision workspace: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	fn, ok := mechanicalActions[plan.Action]
	if !ok {
		return RecoveryFailed, fmt.Errorf("unknown mechanical action %q", plan.Action)
	}
	if _, err := fn(actionCtx, plan, ws); err != nil {
		return RecoveryFailed, fmt.Errorf("mechanical %s: %w", plan.Action, err)
	}
	return RecoveryRepaired, nil
}
