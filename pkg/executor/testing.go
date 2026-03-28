package executor

// NewForTest creates an Executor with preset state and deps for testing.
// This bypasses the normal New() flow (no registry add, no state load).
func NewForTest(beadID, agentName string, formula *FormulaV2, state *State, deps *Deps) *Executor {
	if state == nil {
		state = &State{
			BeadID:    beadID,
			AgentName: agentName,
			Subtasks:  make(map[string]SubtaskState),
		}
	}
	return &Executor{
		beadID:    beadID,
		agentName: agentName,
		formula:   formula,
		state:     state,
		deps:      deps,
		log:       func(string, ...interface{}) {},
	}
}
