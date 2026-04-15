package executor

// actionClericDecide is the ActionHandler for the "cleric.decide" opcode.
// Delegates to handleDecide for the agentic v3 recovery formula.
func actionClericDecide(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return handleDecide(e, stepName, step, state)
}
