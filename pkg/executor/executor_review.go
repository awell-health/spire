package executor

// executor_review.go — recording bookends for dispatcher-level pseudo-phases
// (skip, auto-approve, waitForHuman) and review-loop timing.
//
// These phases do not spawn a subprocess and therefore do not go through the
// standard wizard.run → spawn → recordAgentRun path. They represent decisions
// or blocks taken by the wizard itself. Recording them gives metrics visibility
// into time spent in planning skips, auto-advancing gates, and human-wait
// parks — all of which currently disappear from agent_runs.

import (
	"time"
)

// reviewLoopEntryVar is the graph-state var key that stores the RFC3339
// timestamp at which the current review loop started. Persisted in state.Vars
// so the entry survives wizard restarts (review may span multiple re-summons).
const reviewLoopEntryVar = "__review_loop_entered_at"

// waitForHumanVarKey returns the graph-state var key used to persist the
// timestamp at which a human-approval gate first parked for the given step.
// Persisted in state.Vars so the entry survives wizard restarts while the
// archmage reviews.
func waitForHumanVarKey(stepName string) string {
	return "__wait_for_human_" + stepName
}

// recordSkipPhase emits an agent_runs row for a skipped wizard phase.
// reason is surfaced via withSkipReason for later debugging (e.g.
// "plan-already-exists", "subtask-already-enriched").
func (e *Executor) recordSkipPhase(beadID, epicID, reason string) {
	now := time.Now()
	e.recordAgentRun(e.agentName, beadID, epicID, e.repoModel(), "wizard", "skip",
		now, nil,
		withParentRun(e.currentRunID),
		withResult("success"),
		withSkipReason(reason))
}

// recordAutoApprove emits an agent_runs row for an auto-approve pseudo-phase.
// Fires when the executor advances past a gate (review, approval) without
// dispatching a sage or human — e.g. sage verdict was 'approve' on first
// round so no fix loop runs.
func (e *Executor) recordAutoApprove(beadID, epicID string) {
	now := time.Now()
	e.recordAgentRun(e.agentName, beadID, epicID, e.repoModel(), "wizard", "auto-approve",
		now, nil,
		withParentRun(e.currentRunID),
		withResult("success"))
}

// recordWaitForHuman emits an agent_runs row capturing a human-wait block.
// blockStarted is the time the wizard first parked on this gate; blockEnded
// is when the block resolved (the human cleared the approval labels or the
// wizard is re-entering the step). working_seconds is populated with the
// elapsed block duration so the delay shows up in metrics.
func (e *Executor) recordWaitForHuman(beadID, epicID string, blockStarted, blockEnded time.Time) {
	elapsed := blockEnded.Sub(blockStarted).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	e.recordAgentRun(e.agentName, beadID, epicID, e.repoModel(), "wizard", "waitForHuman",
		blockStarted, nil,
		withParentRun(e.currentRunID),
		withResult("success"),
		withWorkingSeconds(elapsed))
}

// markReviewLoopEntry stamps the current time into graph state Vars as the
// entry to a review loop iteration, iff no entry is already recorded. The
// entry survives re-summons (persisted in GraphState.Vars) so review_seconds
// spans the full review cycle even when the wizard process exits between
// rounds.
func (e *Executor) markReviewLoopEntry() {
	if e.graphState == nil {
		return
	}
	if e.graphState.Vars == nil {
		e.graphState.Vars = make(map[string]string)
	}
	if _, exists := e.graphState.Vars[reviewLoopEntryVar]; exists {
		return // already in a review loop
	}
	e.graphState.Vars[reviewLoopEntryVar] = time.Now().UTC().Format(time.RFC3339)
}

// reviewLoopSeconds returns the duration (in seconds) since the review loop
// entry was marked, and clears the entry marker. Returns 0 when no entry
// was recorded (the caller should not emit a review_seconds value in that
// case).
func (e *Executor) reviewLoopSeconds() float64 {
	if e.graphState == nil || e.graphState.Vars == nil {
		return 0
	}
	raw, ok := e.graphState.Vars[reviewLoopEntryVar]
	if !ok || raw == "" {
		return 0
	}
	entered, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		delete(e.graphState.Vars, reviewLoopEntryVar)
		return 0
	}
	delete(e.graphState.Vars, reviewLoopEntryVar)
	return time.Since(entered).Seconds()
}

// recordReviewPhase emits a wizard-level agent_runs row for a completed
// review-loop cycle. phase='review', role='wizard', review_seconds is the
// full loop duration captured via markReviewLoopEntry / reviewLoopSeconds.
// Skipped when no entry timestamp was recorded.
func (e *Executor) recordReviewPhase(beadID, epicID string, started time.Time) {
	secs := e.reviewLoopSeconds()
	if secs <= 0 {
		return
	}
	e.recordAgentRun(e.agentName, beadID, epicID, e.repoModel(), "wizard", "review",
		started, nil,
		withParentRun(e.currentRunID),
		withResult("success"),
		withReviewSeconds(secs))
}

// isReviewSubgraphStep reports whether the given step config invokes the
// review sub-graph (graph.run with graph=subgraph-review). Used by the
// interpreter to delimit review-loop entry/exit.
func isReviewSubgraphStep(step StepConfig) bool {
	if step.Action != "graph.run" {
		return false
	}
	if step.Graph == "subgraph-review" {
		return true
	}
	if step.With["graph"] == "subgraph-review" {
		return true
	}
	return false
}
