package executor

import (
	"fmt"

	"github.com/awell-health/spire/pkg/recovery"
)

// executeEscalate marks the bead needs-human, emits an alert bead, hooks the
// step, and returns terminal. Decide can choose this mode directly; the
// interpreter also routes to it via round-budget exhaustion (see the
// RecoveryBudgetExhausted branch in runRecoveryCycle's caller).
//
// Escalate is always terminal for this graph — the interpreter hooks the bead
// and exits after an escalate outcome.
func (e *Executor) executeEscalate(plan recovery.RepairPlan, step *StepState, stepName string, state *GraphState) (RecoveryOutcomeKind, error) {
	reason := plan.Reason
	if reason == "" {
		reason = fmt.Sprintf("step %s escalated after recovery", stepName)
	}
	EscalateGraphStepFailure(e.beadID, e.agentName, "step-failure",
		reason, stepName, "", "", "", e.deps)
	return RecoveryEscalated, nil
}
