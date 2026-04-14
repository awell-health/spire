package executor

// NewForTest creates an Executor with preset state and deps for testing.
// V2 formula support removed — the executor runs v3 step-graph only.
// Pass nil for state to get a default empty state.
func NewForTest(beadID, agentName string, state *State, deps *Deps) *Executor {
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
		state:     state,
		deps:      deps,
		log:       func(string, ...interface{}) {},
	}
}

// NewGraphForTest creates a v3 graph Executor with preset state and deps for testing.
// This bypasses the normal NewGraph() flow (no registry add, no state load).
func NewGraphForTest(beadID, agentName string, graph *FormulaStepGraph, state *GraphState, deps *Deps) *Executor {
	if state == nil && graph != nil {
		state = NewGraphState(graph, beadID, agentName)
	}
	return &Executor{
		beadID:     beadID,
		agentName:  agentName,
		graph:      graph,
		graphState: state,
		deps:       deps,
		log:        func(string, ...interface{}) {},
	}
}
