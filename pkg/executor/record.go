package executor

import "time"

// recordAgentRun records an agent run to the agent_runs table.
// Safe to call even when RecordAgentRun is nil (tests, legacy callers).
func (e *Executor) recordAgentRun(name, beadID, epicID, model, role string, started time.Time, spawnErr error) {
	if e.deps.RecordAgentRun == nil {
		return
	}
	result := "success"
	if spawnErr != nil {
		result = "error"
	}
	completed := time.Now()
	run := AgentRun{
		BeadID:          beadID,
		EpicID:          epicID,
		AgentName:       name,
		Model:           model,
		Role:            role,
		Result:          result,
		DurationSeconds: int(completed.Sub(started).Seconds()),
		StartedAt:       started.Format(time.RFC3339),
		CompletedAt:     completed.Format(time.RFC3339),
	}
	if err := e.deps.RecordAgentRun(run); err != nil {
		e.log("warning: record agent run: %s", err)
	}
}
