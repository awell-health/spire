package executor

// actionRecoveryDecide is the ActionHandler for the "recovery.decide" opcode.
// Delegates to handleDecide for the agentic v3 recovery formula.
func actionRecoveryDecide(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return handleDecide(e, stepName, step, state)
}
