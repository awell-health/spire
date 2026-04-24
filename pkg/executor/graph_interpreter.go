package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// RunGraph is the v3 graph interpreter. It walks the step graph, dispatching
// actions, collecting outputs, persisting state, and detecting terminal steps.
// It replaces the v2 phase loop for formulas that declare a step graph.
func (e *Executor) graphStateStore() GraphStateStore {
	if e.deps.GraphStateStore == nil {
		e.deps.GraphStateStore = &FileGraphStateStore{ConfigDir: e.deps.ConfigDir}
	}
	return e.deps.GraphStateStore
}

func (e *Executor) RunGraph(graph *FormulaStepGraph, state *GraphState) error {
	graphStore := e.graphStateStore()

	// Register with wizard registry inside RunGraph() — paired with the deferred
	// cleanup below so registration and cleanup are always atomic.
	regCleanup := e.deps.RegisterSelf(e.agentName, e.beadID, "graph:"+state.ActiveStep,
		agent.WithInstanceID(config.InstanceID()))
	defer regCleanup()
	defer func() {
		if e.terminated {
			graphStore.Remove(e.agentName)
		} else {
			graphStore.Save(e.agentName, state)
		}
	}()
	defer e.closeStagingWorktree()
	defer e.releaseGraphRunWorkspaces(state)

	// Ensure attempt bead (reuse existing ensureAttemptBead-like pattern for graph state).
	// FATAL ownership errors must stop execution immediately — do not proceed with
	// any graph steps if another instance owns the attempt.
	if err := e.ensureGraphAttemptBead(state); err != nil {
		if strings.HasPrefix(err.Error(), "FATAL:") {
			e.log("FATAL: %s", err)
			return fmt.Errorf("attempt ownership conflict: %w", err)
		}
		e.log("warning: create attempt bead: %s", err)
	}
	// The recover-then-repanic guard ensures that even if another defer or
	// the function body panics, the attempt bead and step beads are still
	// cleaned up before the panic propagates.
	defer func() {
		var panicVal interface{}
		if r := recover(); r != nil {
			panicVal = r
			e.log("executor cleanup panic: %v", r)
		}
		if !e.terminated && !state.HasHookedSteps() {
			e.closeAllOpenGraphStepBeads(state)
		}
		if state.AttemptBeadID != "" {
			if cerr := e.deps.CloseAttemptBead(state.AttemptBeadID, "executor exited"); cerr != nil {
				e.log("warning: close attempt bead: %s", cerr)
			}
			state.AttemptBeadID = ""
		}
		if panicVal != nil {
			panic(panicVal)
		}
	}()

	// Resolve branch state.
	if err := e.resolveGraphBranchState(graph, state); err != nil {
		e.closeGraphAttempt(state, "failure: repo-resolution: "+err.Error())
		EscalateHumanFailure(e.beadID, e.agentName, "repo-resolution", err.Error(), e.deps)
		return fmt.Errorf("resolve branch state: %w", err)
	}

	// Initialize vars from formula defaults (only on fresh state).
	if len(state.Vars) == 0 && graph.Vars != nil {
		for name, v := range graph.Vars {
			if v.Default != "" {
				state.Vars[name] = v.Default
			}
		}
		// Always set bead_id var.
		state.Vars["bead_id"] = e.beadID
	}

	e.initMissingGraphWorkspaces(graph, state)

	// Ensure step beads for each graph step.
	if err := e.ensureGraphStepBeads(graph, state); err != nil {
		e.log("warning: create graph step beads: %s", err)
	}

	// Resume any in-flight recovery cycles that were interrupted by a prior
	// wizard crash. This runs after branch/state resolution so the resume
	// policy has full context; it is a no-op on fresh starts.
	if err := e.resumeInFlightRepairs(context.Background(), state); err != nil {
		e.log("warning: resume in-flight repairs: %s", err)
	}

	// Record the executor's own top-level run before any child spawns,
	// so e.currentRunID is available as ParentRunID for child agent runs.
	e.currentRunID = e.recordAgentRun(e.agentName, e.beadID, "", e.repoModel(), "wizard", "execute", time.Now(), nil)

	// Main interpreter loop.
	for {
		// Heartbeat: keep LastSeenAt fresh for steward health monitoring.
		// Rate-limited to at most once per 30s to avoid noisy writes on fast loops.
		if state.AttemptBeadID != "" && e.deps.UpdateAttemptHeartbeat != nil && time.Since(e.lastHeartbeat) >= 30*time.Second {
			if err := e.deps.UpdateAttemptHeartbeat(state.AttemptBeadID); err != nil {
				e.log("warning: heartbeat: %s", err)
			}
			e.lastHeartbeat = time.Now()
		}

		e.collectMessages(state)

		e.dispatchInjectedTasks(state)

		// 1. Build condition context from state.
		ctx := e.buildConditionContext(state)

		// 2. Resolve ready steps.
		completed := e.completedStepsFromState(state)
		ready, err := formula.NextSteps(graph, completed, ctx)
		if err != nil {
			e.closeGraphAttempt(state, "failure: graph-walk: "+err.Error())
			EscalateHumanFailure(e.beadID, e.agentName, "step-failure",
				"graph walk: "+err.Error(), e.deps)
			return fmt.Errorf("graph walk: %w", err)
		}

		// Filter out hooked steps — they are parked, not ready for dispatch.
		var filteredReady []string
		for _, name := range ready {
			if ss, ok := state.Steps[name]; ok && ss.Status == "hooked" {
				continue
			}
			filteredReady = append(filteredReady, name)
		}
		ready = filteredReady

		if len(ready) == 0 {
			// Check if any terminal step completed -> success.
			for name, ss := range state.Steps {
				if ss.Status == "completed" && formula.IsTerminal(graph, name) {
					// Reconcile: close remaining step beads (same as inline terminal path).
					for sn, sid := range state.StepBeadIDs {
						if sn == name || sid == "" {
							continue
						}
						if s := state.Steps[sn]; s.Status != "completed" && s.Status != "failed" {
							if err := e.deps.CloseStepBead(sid); err != nil {
								e.log("warning: reconcile step bead %s (%s): %s", sid, sn, err)
							}
						}
					}
					e.terminated = true
					e.closeGraphAttempt(state, "success: terminal step "+name)
					return nil
				}
			}
			// Check if graph is parked (hooked steps present).
			for _, ss := range state.Steps {
				if ss.Status == "hooked" {
					e.log("graph parked: hooked step(s) present, exiting without escalation")
					e.closeGraphAttempt(state, "parked: hooked steps")
					return nil
				}
			}
			stuckMsg := fmt.Sprintf("graph stuck: no ready steps and no terminal completed (steps=%v)", summarizeSteps(state.Steps))
			e.closeGraphAttempt(state, "failure: "+stuckMsg)
			EscalateHumanFailure(e.beadID, e.agentName, "step-failure",
				stuckMsg, e.deps)
			return fmt.Errorf("%s", stuckMsg)
		}

		stepName := ready[0] // take first ready step (sequential for now)
		stepCfg := graph.Steps[stepName]

		// 3. Activate step.
		state.ActiveStep = stepName
		ss := state.Steps[stepName]
		ss.Status = "active"
		ss.StartedAt = time.Now().UTC().Format(time.RFC3339)
		state.Steps[stepName] = ss
		graphStore.Save(e.agentName, state)

		// Activate step bead if tracked.
		if stepBeadID, ok := state.StepBeadIDs[stepName]; ok {
			// If the step bead was hooked (from a previous parked state that was
			// externally reset by the steward), unhook it before reactivating.
			if b, berr := e.deps.GetBead(stepBeadID); berr == nil && b.Status == "hooked" {
				if err := e.deps.UnhookStepBead(stepBeadID); err != nil {
					e.log("warning: unhook step bead %s (%s): %s", stepBeadID, stepName, err)
				}
				// If no other graph state steps are still hooked, restore parent to in_progress.
				if !state.HasHookedSteps() {
					if err := e.deps.UpdateBead(e.beadID, map[string]interface{}{"status": "in_progress"}); err != nil {
						e.log("warning: restore parent status to in_progress: %s", err)
					}
				}
			}
			if err := e.deps.ActivateStepBead(stepBeadID); err != nil {
				e.log("warning: activate step bead %s (%s): %s", stepBeadID, stepName, err)
			}
		}

		e.log("step: %s (action: %s)", stepName, stepCfg.Action)

		// 4. Dispatch action.
		// Mark review-loop entry (once) so review_seconds spans the full
		// sage→fix/arbiter→merge cycle even across wizard re-summons.
		if isReviewSubgraphStep(stepCfg) {
			e.markReviewLoopEntry()
		}
		// Capture any prior waitForHuman entry so we can emit working_seconds
		// on the resuming completion. The entry itself is set below when the
		// step first parks with Hooked=true.
		waitForHumanEntry := ""
		if stepCfg.Action == "human.approve" {
			waitForHumanEntry = state.Vars[waitForHumanVarKey(stepName)]
		}
		result := e.dispatchAction(stepName, stepCfg, state)

		// 4b. Notify the optional step observer before any branching. The
		// observer sees the raw dispatch outcome (outputs + error) so a
		// foreground cleric dispatcher can render one PhaseEvent per
		// phase completion regardless of whether the step parks, records
		// an error, or completes cleanly. Guarded with a recover so a
		// panicking observer can't take the interpreter down.
		if e.deps.OnStepCompleted != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						e.log("warning: OnStepCompleted panic on %s: %v", stepName, r)
					}
				}()
				e.deps.OnStepCompleted(stepName, result.Outputs, result.Error)
			}()
		}

		// 5. Record outputs and update state.
		if result.Error != nil {
			// Opt-in: if step declares on_error = "record", treat the error as a
			// recoverable outcome — record it as outputs.error and mark the step
			// completed so the formula can route on it (e.g. cleric-default's
			// retry_on_error path). Default stays park-and-escalate.
			if stepCfg.OnError == formula.OnErrorRecord {
				outputs := result.Outputs
				if outputs == nil {
					outputs = make(map[string]string)
				}
				if _, ok := outputs["error"]; !ok {
					outputs["error"] = result.Error.Error()
				}
				if _, ok := outputs["status"]; !ok {
					outputs["status"] = "failed"
				}
				ss = state.Steps[stepName]
				ss.Status = "completed"
				ss.Outputs = outputs
				ss.CompletedAt = time.Now().UTC().Format(time.RFC3339)
				ss.CompletedCount++
				state.Steps[stepName] = ss
				graphStore.Save(e.agentName, state)

				// Persist a recovery_attempts failure row so RepeatedFailures grows
				// across retries. Without this, decide's >=2 prior-failures guard
				// never fires and the loop pins to the same failed action (spi-uh5oo).
				// The chosen action comes from the decide step's output — not the
				// execute step's raw error class.
				e.recordOnErrorRecoveryAttempt(state, result.Error)

				// Close the step bead — the step is done (error recorded, not parked).
				if stepBeadID, ok := state.StepBeadIDs[stepName]; ok {
					if err := e.deps.CloseStepBead(stepBeadID); err != nil {
						e.log("warning: close step bead %s (%s): %s", stepBeadID, stepName, err)
					}
				}

				e.log("step %s errored (recorded as outputs.error, on_error=record): %s",
					stepName, result.Error)
				continue
			}

			// In-wizard recovery dispatch: before parking the step as hooked,
			// attempt an inline recovery cycle. runRecoveryCycle runs
			// diagnose/decide/execute against the wizard's own workspace and
			// returns a RecoveryOutcomeKind telling us whether the repair
			// landed, the step should escalate, the round budget is
			// exhausted, or the failure cleared (noop). Full background in
			// pkg/executor/recovery_dispatch.go.
			//
			// Hook invariant (spi-gdzd7d acceptance #6): `Status = "hooked"`
			// below is only reachable when either
			//   (a) the cycle ran and returned RecoveryEscalated /
			//       RecoveryBudgetExhausted / RecoveryFailed, or
			//   (b) recoveryDisabled() explicitly skipped the cycle
			//       (kill-switch on, or the recovery formulas themselves
			//       — which would otherwise recurse).
			// There is no static control flow from `result.Error != nil`
			// to hook that bypasses both branches.
			if !e.recoveryDisabled() {
				e.log("recovery: step %s failed — entering recovery cycle (pre-cycle)", stepName)
				stepCopy := state.Steps[stepName]
				outcome, recErr := e.runRecoveryCycle(&stepCopy, stepName, state, result.Error)
				if recErr != nil {
					e.log("recovery: cycle error for step %s: %s (continuing to hook)", stepName, recErr)
				}
				switch outcome {
				case RecoveryRepaired, RecoveryNoop:
					// Rewind the step to pending so the interpreter loop
					// re-dispatches it (seam 11/12). This branch does NOT
					// reach the hook path — the invariant holds because
					// `continue` returns to the top of the loop.
					rewound := state.Steps[stepName]
					rewound.Status = "pending"
					rewound.Outputs = nil
					rewound.StartedAt = ""
					rewound.CompletedAt = ""
					state.Steps[stepName] = rewound
					state.ActiveStep = ""
					graphStore.Save(e.agentName, state)
					e.log("recovery: step %s rewound to pending after %s outcome", stepName, outcome)
					continue
				case RecoveryEscalated, RecoveryBudgetExhausted, RecoveryFailed:
					// Post-cycle log so operators can see the exact
					// outcome before the step parks. Fall through to the
					// hook-and-escalate block below.
					e.log("recovery: step %s cycle closed with outcome=%s — parking (post-cycle)",
						stepName, outcome)
				default:
					// Defensive: unknown outcome. Log loudly and treat as
					// failed so we don't silently diverge from the
					// documented invariant.
					e.log("recovery: step %s cycle returned unexpected outcome=%s — parking as RecoveryFailed",
						stepName, outcome)
				}
			}

			// Park the step as hooked (not failed) so the resolve→steward→
			// re-summon flow can retry it. The graph loop will detect hooked
			// steps and exit gracefully without a second escalation.
			ss = state.Steps[stepName]
			ss.Status = "hooked"
			// Do NOT set CompletedAt — the step is parked, not completed.
			state.Steps[stepName] = ss
			state.ActiveStep = "" // clear so graph detects parking
			graphStore.Save(e.agentName, state)

			// Hook the step bead in store (do NOT close — it stays hooked for retry).
			if stepBeadID, ok := state.StepBeadIDs[stepName]; ok {
				if err := e.deps.HookStepBead(stepBeadID); err != nil {
					e.log("warning: hook step bead %s (%s): %s", stepBeadID, stepName, err)
				}
			}
			// Set parent bead to hooked (at least one step is parked).
			if err := e.deps.UpdateBead(e.beadID, map[string]interface{}{"status": "hooked"}); err != nil {
				e.log("warning: set parent bead hooked: %s", err)
			}

			// Escalate to archmage with node-scoped context so the parent bead
			// gets needs-human + interrupted:* labels and an alert bead.
			EscalateGraphStepFailure(e.beadID, e.agentName, "step-failure",
				result.Error.Error(), stepName, stepCfg.Action, stepCfg.Flow, stepCfg.Workspace, e.deps)

			e.log("step %s failed — hooked for recovery, continuing graph loop", stepName)
			continue
		}

		if result.Hooked {
			// Mark the moment the wizard parked on a human-approval gate so
			// the subsequent unblock can emit a waitForHuman row with the
			// actual blocked duration. Only set on the FIRST park — repeated
			// hook-and-wake cycles preserve the original entry timestamp.
			if stepCfg.Action == "human.approve" {
				if state.Vars == nil {
					state.Vars = make(map[string]string)
				}
				if _, exists := state.Vars[waitForHumanVarKey(stepName)]; !exists {
					state.Vars[waitForHumanVarKey(stepName)] = time.Now().UTC().Format(time.RFC3339)
				}
			}
			ss = state.Steps[stepName]
			ss.Status = "hooked"
			ss.Outputs = result.Outputs
			state.Steps[stepName] = ss
			state.ActiveStep = ""
			e.deps.GraphStateStore.Save(e.agentName, state)

			// Hook the step bead in store.
			if stepBeadID, ok := state.StepBeadIDs[stepName]; ok {
				if err := e.deps.HookStepBead(stepBeadID); err != nil {
					e.log("warning: hook step bead %s (%s): %s", stepBeadID, stepName, err)
				}
			}
			// Set parent bead to hooked (at least one step is parked).
			if err := e.deps.UpdateBead(e.beadID, map[string]interface{}{"status": "hooked"}); err != nil {
				e.log("warning: set parent bead hooked: %s", err)
			}

			e.log("step %s hooked — graph parked", stepName)
			e.closeGraphAttempt(state, "parked: step "+stepName+" hooked")
			return nil // graceful exit, not an error
		}

		ss = state.Steps[stepName]
		ss.Status = "completed"
		ss.Outputs = result.Outputs
		ss.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		ss.CompletedCount++
		state.Steps[stepName] = ss

		// 5b. Emit dispatcher-level pseudo-phase recordings.
		//   - human.approve resolving after a previous hook → waitForHuman row.
		//   - review sub-graph step completing → wizard-level review row with
		//     review_seconds, plus auto-approve when the review auto-advanced
		//     to a merge outcome.
		if stepCfg.Action == "human.approve" && waitForHumanEntry != "" {
			if entered, perr := time.Parse(time.RFC3339, waitForHumanEntry); perr == nil {
				e.recordWaitForHuman(e.beadID, "", entered, time.Now())
			}
			delete(state.Vars, waitForHumanVarKey(stepName))
		}
		if isReviewSubgraphStep(stepCfg) {
			e.recordReviewPhase(e.beadID, "", time.Now())
			if result.Outputs["outcome"] == "merge" {
				e.recordAutoApprove(e.beadID, "")
			}
		}

		// 6. Close step bead.
		if stepBeadID, ok := state.StepBeadIDs[stepName]; ok {
			if err := e.deps.CloseStepBead(stepBeadID); err != nil {
				e.log("warning: close step bead %s (%s): %s", stepBeadID, stepName, err)
			}
		}

		// 6b. Check for loop_to directive — step requests re-execution from an earlier step.
		if loopTo, ok := result.Outputs["loop_to"]; ok && loopTo != "" {
			if _, exists := graph.Steps[loopTo]; exists {
				// Safety valve: prevent infinite loops via CompletedCount.
				if ss.CompletedCount > maxStepLoopCount {
					e.log("step %s exceeded max loop count (%d), escalating", stepName, maxStepLoopCount)
					EscalateHumanFailure(e.beadID, e.agentName, "step-loop-limit",
						fmt.Sprintf("step %s looped %d times", stepName, ss.CompletedCount), e.deps)
					// Fall through to terminal check — don't loop.
				} else {
					e.log("step %s requests loop to %s (iteration %d)", stepName, loopTo, ss.CompletedCount)
					resetStepsForLoop(state, graph, loopTo)
					graphStore.Save(e.agentName, state)
					continue
				}
			}
		}

		// 7. Check terminal.
		if formula.IsTerminal(graph, stepName) {
			// Reconcile: close all remaining step beads that didn't execute.
			for name, sid := range state.StepBeadIDs {
				if name == stepName {
					continue // already closed above
				}
				if sid == "" {
					continue
				}
				ss := state.Steps[name]
				if ss.Status != "completed" && ss.Status != "failed" {
					if err := e.deps.CloseStepBead(sid); err != nil {
						e.log("warning: reconcile step bead %s (%s): %s", sid, name, err)
					}
				}
			}
			// Only mark terminated (which removes graph state) on clean exits.
			// Escalations need graph state preserved for reset --to / resummon.
			isEscalation := stepCfg.With["status"] == "escalate" ||
				(result.Outputs != nil && result.Outputs["status"] == "escalated")
			if !isEscalation {
				e.terminated = true
			}
			// Save parent state before cleaning up nested state (crash-safe ordering).
			graphStore.Save(e.agentName, state)
			if stepCfg.Action == "graph.run" {
				graphStore.Remove(e.agentName + "-" + stepName)
			}
			e.closeGraphAttempt(state, "success: terminal step "+stepName)
			return nil
		}

		// 8. Apply formula-declared resets: set named steps back to pending.
		// This enables loops (e.g. fix resets sage-review and fix for re-review).
		for _, target := range stepCfg.Resets {
			ts := state.Steps[target]
			ts.Status = "pending"
			ts.Outputs = nil
			ts.StartedAt = ""
			ts.CompletedAt = ""
			// CompletedCount is preserved — it's a mechanical fact, not reset.
			state.Steps[target] = ts
			e.log("reset step %s to pending (declared by %s)", target, stepName)

			// Reopen step bead if tracked.
			if stepBeadID, ok := state.StepBeadIDs[target]; ok {
				if err := e.deps.ActivateStepBead(stepBeadID); err != nil {
					e.log("warning: reopen step bead %s (%s): %s", stepBeadID, target, err)
				}
			}
		}

		// 9. Persist and loop.
		graphStore.Save(e.agentName, state)

		// 10. Clean up nested graph state files after the parent save is durable.
		// This is crash-safe: the parent step is already recorded as completed,
		// so if the process dies here, the nested file is orphaned (harmless)
		// but the parent won't re-run the step.
		if stepCfg.Action == "graph.run" {
			nestedAgentName := e.agentName + "-" + stepName
			graphStore.Remove(nestedAgentName)
		}
	}
}

// RunNestedGraph runs a step-graph formula as a nested sub-graph within the
// current executor. Unlike RunGraph, it does NOT:
//   - Remove the parent's registry entry
//   - Close the parent's staging worktree
//   - Create or close attempt beads
//   - Remove graph state files on terminal success
//
// This method is used by actionGraphRun to execute sub-graphs (e.g. the
// subgraph-review graph called from task-default) without interfering
// with the parent graph's lifecycle.
func (e *Executor) RunNestedGraph(graph *FormulaStepGraph, state *GraphState) error {
	store := e.graphStateStore()

	// Resolve branch state for the sub-graph (usually inherited from parent).
	if state.RepoPath == "" || state.BaseBranch == "" {
		if err := e.resolveGraphBranchState(graph, state); err != nil {
			return fmt.Errorf("nested: resolve branch state: %w", err)
		}
	}

	// Initialize vars from formula defaults (only on fresh state).
	if len(state.Vars) == 0 && graph.Vars != nil {
		for name, v := range graph.Vars {
			if v.Default != "" {
				state.Vars[name] = v.Default
			}
		}
		state.Vars["bead_id"] = e.beadID
	}

	e.initMissingGraphWorkspaces(graph, state)

	// Main interpreter loop — same logic as RunGraph but without cleanup.
	for {
		ctx := e.buildConditionContext(state)
		completed := e.completedStepsFromState(state)
		ready, err := formula.NextSteps(graph, completed, ctx)
		if err != nil {
			return fmt.Errorf("nested graph walk: %w", err)
		}

		if len(ready) == 0 {
			for name, ss := range state.Steps {
				if ss.Status == "completed" && formula.IsTerminal(graph, name) {
					return nil
				}
			}
			return fmt.Errorf("nested graph stuck: no ready steps and no terminal completed (steps=%v)", summarizeSteps(state.Steps))
		}

		stepName := ready[0]
		stepCfg := graph.Steps[stepName]

		state.ActiveStep = stepName
		ss := state.Steps[stepName]
		ss.Status = "active"
		ss.StartedAt = time.Now().UTC().Format(time.RFC3339)
		state.Steps[stepName] = ss

		e.log("nested step: %s (action: %s)", stepName, stepCfg.Action)

		result := e.dispatchAction(stepName, stepCfg, state)

		if result.Error != nil {
			ss = state.Steps[stepName]
			ss.Status = "failed"
			ss.CompletedAt = time.Now().UTC().Format(time.RFC3339)
			state.Steps[stepName] = ss
			store.Save(state.AgentName, state) // persist failure for resume
			return fmt.Errorf("nested step %s failed: %w", stepName, result.Error)
		}

		ss = state.Steps[stepName]
		ss.Status = "completed"
		ss.Outputs = result.Outputs
		ss.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		ss.CompletedCount++
		state.Steps[stepName] = ss

		if formula.IsTerminal(graph, stepName) {
			// Nested graphs don't create step beads (ensureGraphStepBeads is
			// only called by RunGraph), so no reconciliation needed here.
			// Persist final state before returning (caller removes on success).
			store.Save(state.AgentName, state)
			return nil
		}

		// Apply formula-declared resets (same as RunGraph).
		for _, target := range stepCfg.Resets {
			ts := state.Steps[target]
			ts.Status = "pending"
			ts.Outputs = nil
			ts.StartedAt = ""
			ts.CompletedAt = ""
			state.Steps[target] = ts
			e.log("nested: reset step %s to pending (declared by %s)", target, stepName)
		}

		// Persist after each step so nested graph progress survives interrupts.
		store.Save(state.AgentName, state)
	}
}

// buildConditionContext flattens the GraphState into the map[string]string
// that formula.EvalCondition / formula.NextSteps consume.
func (e *Executor) buildConditionContext(state *GraphState) map[string]string {
	ctx := make(map[string]string)

	// Flatten step outputs: "steps.X.outputs.Y" -> value
	for name, ss := range state.Steps {
		for k, v := range ss.Outputs {
			ctx[fmt.Sprintf("steps.%s.outputs.%s", name, k)] = v
		}
		// Also expose step status and completed_count.
		ctx[fmt.Sprintf("steps.%s.status", name)] = ss.Status
		ctx[fmt.Sprintf("steps.%s.completed_count", name)] = strconv.Itoa(ss.CompletedCount)
	}

	// Flatten counters: "state.counters.X" -> value, plus short-form.
	for k, v := range state.Counters {
		str := strconv.Itoa(v)
		ctx["state.counters."+k] = str
		ctx[k] = str // short-form for backward compat
	}

	// Flatten vars: "vars.X" -> value, plus short-form.
	for k, v := range state.Vars {
		ctx["vars."+k] = v
		ctx[k] = v // short-form
	}

	return ctx
}

// collectMessages queries open messages addressed to this wizard, stores them
// in state.Vars["pending_messages"] as JSON, and closes consumed messages.
func (e *Executor) collectMessages(state *GraphState) {
	if e.deps.ListBeads == nil {
		return
	}
	if !e.lastMessageCollect.IsZero() && time.Since(e.lastMessageCollect) < 30*time.Second {
		return
	}
	e.lastMessageCollect = time.Now()

	delete(state.Vars, "pending_messages")

	messages, err := e.deps.ListBeads(beads.IssueFilter{
		Labels: []string{"msg", "to:" + e.agentName},
		Status: store.StatusPtr(beads.StatusOpen),
	})
	if err != nil {
		e.log("warning: collect messages: %s", err)
		return
	}
	if len(messages) == 0 {
		return
	}

	type msgEntry struct {
		ID   string `json:"id"`
		From string `json:"from"`
		Ref  string `json:"ref"`
		Text string `json:"text"`
	}
	var entries []msgEntry
	for _, m := range messages {
		entry := msgEntry{ID: m.ID, Text: m.Title}
		entry.From = e.deps.HasLabel(m, "from:")
		entry.Ref = e.deps.HasLabel(m, "ref:")
		entries = append(entries, entry)
		if err := e.deps.CloseBead(m.ID); err != nil {
			e.log("warning: close message %s: %s", m.ID, err)
		}
	}
	data, _ := json.Marshal(entries)
	state.Vars["pending_messages"] = string(data)
}

// dispatchInjectedTasks processes state.InjectedTasks by spawning an apprentice
// for each task, merging its branch into staging, then clearing the queue.
// Called in the RunGraph loop after collectMessages and before NextSteps.
func (e *Executor) dispatchInjectedTasks(state *GraphState) {
	if len(state.InjectedTasks) == 0 {
		return
	}

	graphStore := e.graphStateStore()

	stagingWt, err := e.ensureGraphStagingWorktree(state)
	if err != nil {
		e.log("warning: ensure staging for injection: %s — dispatching without merge", err)
		stagingWt = nil
	}

	resolver := e.conflictResolver(0)
	model := e.repoModel()

	// Injected tasks are cross-owner dispatches on peer beads — same
	// handoff rules as dispatch.children: tower transport decides bundle
	// vs transitional.
	apprenticeHandoff := e.resolveApprenticeHandoff()

	if state.Counters == nil {
		state.Counters = make(map[string]int)
	}

	var remaining []string
	for _, taskID := range state.InjectedTasks {
		if b, berr := e.deps.GetBead(taskID); berr == nil && b.Status == "closed" {
			e.log("injected task %s already closed, skipping", taskID)
			continue
		}

		state.Counters["inject"]++
		name := fmt.Sprintf("%s-inj-%d", e.agentName, state.Counters["inject"])

		e.log("dispatching injected task %s as %s", taskID, name)
		e.deps.UpdateBead(taskID, map[string]interface{}{"status": "in_progress"})

		var startRef string
		if stagingWt != nil {
			if sha, serr := stagingWt.HeadSHA(); serr == nil && sha != "" {
				startRef = sha
			}
		}

		started := time.Now()
		cfg := agent.SpawnConfig{
			Name:          name,
			BeadID:        taskID,
			Role:          agent.RoleApprentice,
			ExtraArgs:     []string{"--apprentice"},
			StartRef:      startRef,
			LogPath:       filepath.Join(dolt.GlobalDir(), "wizards", name+".log"),
			AttemptID:     e.attemptID(),
			ApprenticeIdx: "0",
		}
		cfg, contractErr := e.withRuntimeContract(cfg, state.TowerName, state.RepoPath, state.BaseBranch, e.runtimeStep(state, "inject"), "", nil, apprenticeHandoff)
		if contractErr != nil {
			e.recordAgentRun(name, taskID, e.beadID, model, "apprentice", "implement", started, contractErr,
				withParentRun(e.currentRunID))
			e.log("warning: handoff selection for injected task %s: %s", taskID, contractErr)
			remaining = append(remaining, taskID)
			continue
		}
		h, spawnErr := e.deps.Spawner.Spawn(cfg)
		if spawnErr != nil {
			e.recordAgentRun(name, taskID, e.beadID, model, "apprentice", "implement", started, spawnErr,
				withParentRun(e.currentRunID))
			e.log("warning: spawn injected task %s: %s", taskID, spawnErr)
			remaining = append(remaining, taskID)
			continue
		}

		waitErr := h.Wait()
		e.recordAgentRun(name, taskID, e.beadID, model, "apprentice", "implement", started, waitErr,
			withParentRun(e.currentRunID))

		if waitErr != nil {
			e.log("warning: injected task %s failed: %s", taskID, waitErr)
			continue
		}

		if stagingWt != nil {
			merged := false
			if e.deps.BundleStore != nil {
				outcome, err := e.applyApprenticeBundle(taskID, 0, stagingWt)
				if err != nil {
					e.log("warning: apply apprentice bundle for injected task %s: %s", taskID, err)
					continue
				}
				if outcome.NoOp {
					e.log("injected task %s signalled no-op — nothing to merge", taskID)
					merged = true
				} else if outcome.Applied {
					if mergeErr := stagingWt.MergeBranch(outcome.Branch, resolver); mergeErr != nil {
						e.log("warning: merge injected task %s branch %s: %s", taskID, outcome.Branch, mergeErr)
						continue
					}
					e.deleteApprenticeBundle(taskID, outcome.Handle)
					merged = true
				}
			}
			if !merged {
				// Legacy fallback: assume the apprentice's feat branch is
				// already present locally.
				featBranch := e.resolveBranch(taskID)
				if mergeErr := stagingWt.MergeBranch(featBranch, resolver); mergeErr != nil {
					e.log("warning: merge injected task %s branch %s: %s", taskID, featBranch, mergeErr)
					continue
				}
			}
		}

		e.log("injected task %s dispatched and merged", taskID)
	}

	state.InjectedTasks = nil
	if len(remaining) > 0 {
		state.InjectedTasks = remaining
	}
	graphStore.Save(e.agentName, state)
}

// completedStepsFromState converts GraphState.Steps to the map[string]bool
// that formula.NextSteps expects.
func (e *Executor) completedStepsFromState(state *GraphState) map[string]bool {
	m := make(map[string]bool, len(state.Steps))
	for name, ss := range state.Steps {
		if ss.Status == "completed" {
			m[name] = true
		}
	}
	return m
}

// resetStepsForLoop resets the target step and all transitive dependents back
// to pending so the graph interpreter can re-execute them. CompletedCount is
// preserved on each step — it tracks loop iterations for the safety valve.
func resetStepsForLoop(state *GraphState, graph *formula.FormulaStepGraph, targetStep string) {
	// Build reverse-dependency map: step → steps that need it.
	dependents := make(map[string][]string)
	for name, step := range graph.Steps {
		for _, dep := range step.Needs {
			dependents[dep] = append(dependents[dep], name)
		}
	}
	// BFS from targetStep to find all transitive dependents.
	toReset := map[string]bool{targetStep: true}
	queue := []string{targetStep}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dep := range dependents[cur] {
			if !toReset[dep] {
				toReset[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	// Reset each step to pending. Leave StepBeadIDs intact — the activation
	// code at step dispatch handles re-activating closed/hooked step beads.
	for name := range toReset {
		if ss, ok := state.Steps[name]; ok {
			ss.Status = ""
			ss.Outputs = nil
			ss.StartedAt = ""
			ss.CompletedAt = ""
			// CompletedCount is preserved — it tracks iterations.
			state.Steps[name] = ss
		}
	}
}

// --- Graph-specific bead management helpers ---

// ensureGraphAttemptBead creates or resumes an attempt bead for graph execution.
// It stamps instance metadata on the attempt and verifies instance ownership
// (fail-closed) when reclaiming an existing active attempt.
func (e *Executor) ensureGraphAttemptBead(state *GraphState) error {
	if state.AttemptBeadID != "" {
		b, err := e.deps.GetBead(state.AttemptBeadID)
		if err == nil && (b.Status == "open" || b.Status == "in_progress") {
			e.log("resuming existing attempt bead %s", state.AttemptBeadID)
			e.stampAttemptInstance(state.AttemptBeadID, state)
			return nil
		}
		state.AttemptBeadID = ""
	}

	existing, err := e.deps.GetActiveAttempt(e.beadID)
	if err != nil {
		return err
	}
	if existing != nil {
		// Fail-closed instance ownership check: verify this instance owns the attempt.
		if e.deps.IsOwnedByInstance != nil {
			owned, oerr := e.deps.IsOwnedByInstance(existing.ID, config.InstanceID())
			if oerr != nil {
				return fmt.Errorf("check attempt ownership: %w", oerr)
			}
			if !owned {
				ownerName := ""
				if e.deps.GetAttemptInstance != nil {
					meta, _ := e.deps.GetAttemptInstance(existing.ID)
					if meta != nil {
						ownerName = meta.InstanceName
					}
				}
				return fmt.Errorf("FATAL: attempt %s owned by instance %q, not this instance %q — exiting to prevent conflict",
					existing.ID, ownerName, config.InstanceName())
			}
		}

		agent := e.deps.HasLabel(*existing, "agent:")
		if agent == e.agentName {
			state.AttemptBeadID = existing.ID
			e.log("reusing attempt bead %s (created by claim)", existing.ID)
			e.stampAttemptInstance(existing.ID, state)
			return nil
		}
		return fmt.Errorf("active attempt %s already exists (agent: %s)", existing.ID, agent)
	}

	branch := state.StagingBranch
	if branch == "" {
		branch = e.resolveBranch(e.beadID)
	}

	id, err := e.deps.CreateAttemptBead(e.beadID, e.agentName, "unknown", branch)
	if err != nil {
		return err
	}
	state.AttemptBeadID = id
	e.log("created attempt bead %s", id)
	e.stampAttemptInstance(id, state)
	return nil
}

// stampAttemptInstance writes instance ownership metadata onto an attempt bead.
func (e *Executor) stampAttemptInstance(attemptID string, state *GraphState) {
	if e.deps.StampAttemptInstance == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tower := state.TowerName
	if err := e.deps.StampAttemptInstance(attemptID, store.InstanceMeta{
		InstanceID:   config.InstanceID(),
		SessionID:    e.sessionID,
		InstanceName: config.InstanceName(),
		Backend:      "process",
		Tower:        tower,
		StartedAt:    now,
		LastSeenAt:   now,
	}); err != nil {
		e.log("warning: stamp attempt instance metadata: %s", err)
	}
}

// closeGraphAttempt closes the current attempt bead with the given result.
func (e *Executor) closeGraphAttempt(state *GraphState, result string) {
	if state.AttemptBeadID == "" {
		return
	}
	if err := e.deps.CloseAttemptBead(state.AttemptBeadID, result); err != nil {
		e.log("warning: close attempt bead %s: %s", state.AttemptBeadID, err)
	}
	state.AttemptBeadID = ""
}

// resolveGraphBranchState resolves repo path, base branch, and staging branch
// for graph execution. When the graph declares a staging workspace, its branch
// is the source of truth for StagingBranch.
func (e *Executor) resolveGraphBranchState(graph *FormulaStepGraph, state *GraphState) error {
	if state.RepoPath != "" && state.BaseBranch != "" {
		if state.StagingBranch == "" {
			state.StagingBranch = e.resolveDeclaredGraphStagingBranch(graph, state)
		}
		e.log("branch state loaded from persisted graph state: repo=%s base=%s staging=%s",
			state.RepoPath, state.BaseBranch, state.StagingBranch)
		return nil
	}

	repoPath, _, baseBranch, err := e.deps.ResolveRepo(e.beadID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	if repoPath == "" {
		repoPath = "."
	}

	state.RepoPath = repoPath
	state.BaseBranch = baseBranch

	// Bead-level base-branch override (from spire file --branch) takes
	// precedence over repo defaults. Walks up the parent chain so that
	// child tasks inherit the base branch from their epic.
	if bb := e.findBaseBranchInParentChain(e.beadID); bb != "" {
		e.log("using bead base-branch override: %s (was: %s)", bb, state.BaseBranch)
		state.BaseBranch = bb
	}

	if state.StagingBranch == "" {
		state.StagingBranch = e.resolveDeclaredGraphStagingBranch(graph, state)
	}
	if state.StagingBranch == "" {
		state.StagingBranch = "staging/" + e.beadID
	}

	e.log("branch state resolved: repo=%s base=%s staging=%s",
		state.RepoPath, state.BaseBranch, state.StagingBranch)
	e.log("recovery budget: max_merge_attempts=%d recovery_budget=%d",
		spgit.MaxMergeAttempts(), maxRecoveryAttempts(nil))
	return nil
}

// findBaseBranchInParentChain walks up the bead's parent chain looking for a
// base-branch: label. Returns the branch name from the first bead that has one,
// or "" if none in the chain do. This lets child tasks inherit the base branch
// from their epic without needing the label copied to every child.
func (e *Executor) findBaseBranchInParentChain(beadID string) string {
	visited := make(map[string]bool)
	current := beadID
	for current != "" && !visited[current] {
		visited[current] = true
		bead, err := e.deps.GetBead(current)
		if err != nil {
			break
		}
		if bb := e.deps.HasLabel(bead, "base-branch:"); bb != "" {
			return bb
		}
		current = bead.Parent
	}
	return ""
}

func (e *Executor) resolveDeclaredGraphStagingBranch(graph *FormulaStepGraph, state *GraphState) string {
	if graph == nil || len(graph.Workspaces) == 0 {
		return ""
	}

	if decl, ok := graph.Workspaces["staging"]; ok {
		formula.DefaultWorkspaceDecl(&decl)
		if decl.Kind == formula.WorkspaceKindStaging && decl.Branch != "" {
			return e.resolveGraphWorkspaceBranch(decl.Branch, state)
		}
	}

	for _, decl := range graph.Workspaces {
		formula.DefaultWorkspaceDecl(&decl)
		if decl.Kind == formula.WorkspaceKindStaging && decl.Branch != "" {
			return e.resolveGraphWorkspaceBranch(decl.Branch, state)
		}
	}

	return ""
}

func (e *Executor) initMissingGraphWorkspaces(graph *FormulaStepGraph, state *GraphState) {
	if graph == nil || len(graph.Workspaces) == 0 {
		return
	}
	if state.Workspaces == nil {
		state.Workspaces = make(map[string]WorkspaceState)
	}

	for name, decl := range graph.Workspaces {
		formula.DefaultWorkspaceDecl(&decl)
		ws, ok := state.Workspaces[name]
		if !ok {
			state.Workspaces[name] = WorkspaceState{
				Name:       name,
				Kind:       decl.Kind,
				Branch:     decl.Branch,
				BaseBranch: decl.Base,
				Status:     "pending",
				Scope:      decl.Scope,
				Ownership:  decl.Ownership,
				Cleanup:    decl.Cleanup,
			}
			continue
		}

		if ws.Name == "" {
			ws.Name = name
		}
		if ws.Kind == "" {
			ws.Kind = decl.Kind
		}
		if ws.Branch == "" {
			ws.Branch = decl.Branch
		}
		if ws.BaseBranch == "" {
			ws.BaseBranch = decl.Base
		}
		if ws.Status == "" {
			ws.Status = "pending"
		}
		if ws.Scope == "" {
			ws.Scope = decl.Scope
		}
		if ws.Ownership == "" {
			ws.Ownership = decl.Ownership
		}
		if ws.Cleanup == "" {
			ws.Cleanup = decl.Cleanup
		}
		state.Workspaces[name] = ws
	}
}

// ensureGraphStepBeads creates step beads for each graph step (idempotent).
// On resummon after reset, reuses existing step-type children instead of creating duplicates.
func (e *Executor) ensureGraphStepBeads(graph *FormulaStepGraph, state *GraphState) error {
	if len(state.StepBeadIDs) > 0 {
		e.log("graph step beads already exist (%d steps)", len(state.StepBeadIDs))
		e.reopenRewoundStepBeads(state)
		return nil
	}

	// Check for existing step bead children from a previous run.
	// After reset --hard, the step beads are deleted. After soft reset or resummon,
	// they may still exist — reuse them to avoid duplicates.
	existing := make(map[string]string) // stepName → beadID
	if children, err := e.deps.GetChildren(e.beadID); err == nil {
		for _, child := range children {
			if child.Type == "step" {
				// Extract step name from "step:<name>" label
				for _, l := range child.Labels {
					if strings.HasPrefix(l, "step:") {
						existing[strings.TrimPrefix(l, "step:")] = child.ID
					}
				}
			}
		}
	}

	if len(existing) > 0 {
		e.log("reusing %d existing step beads", len(existing))
	}

	state.StepBeadIDs = make(map[string]string, len(graph.Steps))
	for stepName, stepCfg := range graph.Steps {
		if existingID, ok := existing[stepName]; ok {
			state.StepBeadIDs[stepName] = existingID
			continue
		}
		title := stepCfg.Title
		if title == "" {
			title = stepName
		}
		id, err := e.deps.CreateStepBead(e.beadID, stepName)
		if err != nil {
			return fmt.Errorf("create step bead for %s: %w", stepName, err)
		}
		state.StepBeadIDs[stepName] = id
		e.log("created step bead %s for step %s", id, stepName)
	}

	// Defense in depth: after reconciling reused step beads, reopen any that
	// were left "closed" but whose graph state is now "pending" (e.g. from a
	// reset path that forgot to reopen). Mirrors the review-substep pattern
	// in ensureReviewSubStepBeads.
	e.reopenRewoundStepBeads(state)

	return e.graphStateStore().Save(e.agentName, state)
}

// reopenRewoundStepBeads re-activates any step beads in state.StepBeadIDs whose
// graph-state status is "pending" but whose bead status is "closed" or
// "hooked". This handles two classes of resummon drift:
//
//   - "closed" reused step bead + "pending" graph state: a reset path rewound
//     the graph without reopening the bead. transitionStepBead treats a closed
//     bead as a no-op, so the executor would silently skip the step on resume.
//   - "hooked" reused step bead + "pending" graph state: the steward already
//     cleaned up the hook (parent graph state advanced to pending), but the
//     step bead was left hooked. Same silent-skip hazard.
//
// Only pending steps are reopened. Completed/in-progress/hooked (when graph
// state agrees)/skipped steps are left alone. In particular, a "closed" step
// bead whose graph state is "completed" is the correct terminal state — no
// activation attempted.
//
// After per-step reconciliation, a final pass clears the parent bead's hook
// when graph state has no hooked step: the parent was presumably hooked by a
// prior run whose step bead has since been rewound. Leaving the parent hooked
// with no matching step confuses status consumers.
func (e *Executor) reopenRewoundStepBeads(state *GraphState) {
	if e.deps.ActivateStepBead == nil || e.deps.GetBead == nil {
		return
	}
	for stepName, stepID := range state.StepBeadIDs {
		if stepID == "" {
			continue
		}
		ss, ok := state.Steps[stepName]
		if !ok || ss.Status != "pending" {
			continue
		}
		b, err := e.deps.GetBead(stepID)
		if err != nil {
			e.log("warning: read step bead %s (%s): %s", stepID, stepName, err)
			continue
		}
		if b.Status != "closed" && b.Status != "hooked" {
			continue
		}
		if err := e.deps.ActivateStepBead(stepID); err != nil {
			e.log("warning: reopen rewound step bead %s (%s, was %s): %s", stepID, stepName, b.Status, err)
		} else {
			e.log("reopened rewound step bead %s (%s, was %s) — graph state is pending", stepID, stepName, b.Status)
		}
	}

	e.reconcileParentHook(state)
}

// reconcileParentHook clears the parent bead's "hooked" status when no step in
// graph state is hooked. The graph state is authoritative for step lifecycle;
// a hooked parent with no hooked child means an earlier run parked and the
// steward already cleaned up the step state, but the parent status was left
// stale. Without this, status consumers (board, is-blocked checks) see a
// hooked parent indefinitely.
//
// The target status is derived from the graph: if any step has progressed past
// pending (active/completed/failed/hooked), the parent is "in_progress";
// otherwise it falls back to "open". This matches the pre-hook lifecycle the
// interpreter itself establishes (transitionStepBead unhook path).
func (e *Executor) reconcileParentHook(state *GraphState) {
	if e.deps.GetBead == nil || e.deps.UpdateBead == nil {
		return
	}
	if state.HasHookedSteps() {
		return
	}
	parent, err := e.deps.GetBead(e.beadID)
	if err != nil {
		e.log("warning: read parent bead %s for hook reconcile: %s", e.beadID, err)
		return
	}
	if parent.Status != "hooked" {
		return
	}
	target := inferPreHookParentStatus(state)
	if err := e.deps.UpdateBead(e.beadID, map[string]interface{}{"status": target}); err != nil {
		e.log("warning: clear stale parent hook on %s: %s", e.beadID, err)
		return
	}
	e.log("cleared stale parent hook on %s (set to %s) — no steps hooked in graph state", e.beadID, target)
}

// inferPreHookParentStatus returns the parent bead status that best matches
// the graph's current lifecycle. If any step has advanced past "pending",
// some work was in flight, so "in_progress" is right; if all steps are still
// pending (nothing has run), "open" is the stable pre-run status.
func inferPreHookParentStatus(state *GraphState) string {
	for _, ss := range state.Steps {
		switch ss.Status {
		case "active", "completed", "failed":
			return "in_progress"
		}
	}
	return "open"
}

// recordOnErrorRecoveryAttempt persists a recovery_attempts failure row for
// the execute step's chosen action when on_error=record fires. This is what
// makes RepeatedFailures[action] grow across retries — the signal
// parseHumanGuidance's >=2 guard reads (spi-uh5oo bug 2).
//
// The action label is derived from decide.outputs.plan (RepairPlan.Action);
// the attempt number is derived from CountAttemptsByAction(+1). Silently
// no-ops when DoltDB is not wired or decide never produced a plan.
//
// Note: recovery's plan-mode execute path (handlePlanExecute) records its
// own attempts through the mechanical/worker dispatches when DB is wired.
// In that path this call can duplicate — a full de-dup requires
// centralizing attempt recording in the interpreter, which is out of scope
// for this fix.
func (e *Executor) recordOnErrorRecoveryAttempt(state *GraphState, stepErr error) {
	if state == nil || stepErr == nil {
		return
	}
	if e.deps == nil || e.deps.DoltDB == nil {
		return
	}
	db := e.deps.DoltDB()
	if db == nil {
		return
	}
	ds, ok := state.Steps["decide"]
	if !ok {
		return
	}
	chosenAction := ""
	if planJSON := ds.Outputs["plan"]; planJSON != "" {
		var plan recovery.RepairPlan
		if err := json.Unmarshal([]byte(planJSON), &plan); err == nil {
			chosenAction = plan.Action
		}
	}
	if chosenAction == "" {
		return
	}

	targetBeadID := ""
	if e.deps.GetBead != nil {
		if b, err := e.deps.GetBead(e.beadID); err == nil {
			targetBeadID = b.Meta(recovery.KeySourceBead)
		}
	}

	attemptNum := 1
	if count, err := store.CountAttemptsByAction(db, e.beadID, chosenAction); err == nil {
		attemptNum = count + 1
	}

	attempt := store.RecoveryAttempt{
		RecoveryBeadID: e.beadID,
		TargetBeadID:   targetBeadID,
		Action:         chosenAction,
		Outcome:        "failure",
		Error:          stepErr.Error(),
		AttemptNumber:  attemptNum,
	}
	if err := store.RecordRecoveryAttempt(db, attempt); err != nil {
		e.log("warning: record recovery attempt (%s on_error=record): %s", chosenAction, err)
	}
}

// summarizeSteps returns a compact string representation of step states.
func summarizeSteps(steps map[string]StepState) string {
	result := "{"
	first := true
	for name, ss := range steps {
		if !first {
			result += ", "
		}
		result += name + ":" + ss.Status
		first = false
	}
	return result + "}"
}
