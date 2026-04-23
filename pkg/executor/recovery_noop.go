package executor

import "github.com/awell-health/spire/pkg/recovery"

// executeNoop handles RepairMode=Noop plans. Decide chooses this mode when the
// failure cleared itself (rare; typically after a human edit outside the
// wizard's control). The step is rewound and re-runs without any repair work.
func (e *Executor) executeNoop(plan recovery.RepairPlan, step *StepState, stepName string, state *GraphState) (RecoveryOutcomeKind, error) {
	e.log("recovery: noop for step %s — resuming without repair", stepName)
	return RecoveryNoop, nil
}
