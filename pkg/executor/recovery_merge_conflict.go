package executor

import (
	"fmt"

	"github.com/awell-health/spire/pkg/recovery"
)

// executeMergeConflict runs the Claude-driven merge-conflict resolver on
// staging. It is a labeled mechanical action (plan.Action == "resolve-conflicts")
// rather than a distinct RepairMode; the label lets tests assert the path
// explicitly and lets the mode switch route to a single resolver entry point
// even though the concept aligns closely with the worker path.
//
// The resolver itself lives in dispatch_helpers.go (resolveMergeConflicts);
// this wrapper adapts it to the RepairPlan dispatch surface.
func (e *Executor) executeMergeConflict(plan recovery.RepairPlan, step *StepState, stepName string, state *GraphState) (RecoveryOutcomeKind, error) {
	step.CurrentRepair.Phase = PhaseExecuteMergeConflict
	state.Steps[stepName] = *step
	e.persistGraphState(state)

	// The merge-conflict resolver expects to run on a workspace — it commits
	// the resolution in place. We reuse the SpawnRepairWorker path for
	// consistency with the worker-mode dispatch: SpawnRepairWorker already
	// routes action=="resolve-conflicts" through the conflict bundle
	// apprentice. That keeps a single spawn site.
	actionCtx, ws, cleanup, err := e.buildRecoveryActionCtx(e.beadID, plan, StepConfig{With: map[string]string{}}, state)
	if err != nil {
		return RecoveryFailed, fmt.Errorf("executeMergeConflict: provision workspace: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Narrow the action to the conflict-resolver path. SpawnRepairWorker
	// special-cases action=="resolve-conflicts" to assemble a conflict
	// bundle and run the conflict-marker gates after the apprentice exits.
	localPlan := plan
	localPlan.Mode = recovery.RepairModeWorker
	if _, err := SpawnRepairWorker(actionCtx, localPlan, ws); err != nil {
		return RecoveryFailed, fmt.Errorf("merge-conflict resolver: %w", err)
	}
	return RecoveryRepaired, nil
}
