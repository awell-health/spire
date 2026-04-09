package executor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
)

// RunGraph is the v3 graph interpreter. It walks the step graph, dispatching
// actions, collecting outputs, persisting state, and detecting terminal steps.
// It replaces the v2 phase loop for formulas that declare a step graph.
func (e *Executor) RunGraph(graph *FormulaStepGraph, state *GraphState) error {
	// Register with wizard registry inside RunGraph() — paired with the deferred
	// RegistryRemove below so registration and cleanup are always atomic.
	e.deps.RegistryAdd(agent.Entry{
		Name:      e.agentName,
		PID:       os.Getpid(),
		BeadID:    e.beadID,
		StartedAt: state.StartedAt,
		Phase:     "graph:" + state.ActiveStep,
	})
	defer e.deps.RegistryRemove(e.agentName)
	defer func() {
		if e.terminated {
			RemoveGraphState(e.agentName, e.deps.ConfigDir)
		} else {
			state.Save(e.agentName, e.deps.ConfigDir)
		}
	}()
	defer e.closeStagingWorktree()
	defer e.releaseGraphRunWorkspaces(state)

	// Ensure attempt bead (reuse existing ensureAttemptBead-like pattern for graph state).
	if err := e.ensureGraphAttemptBead(state); err != nil {
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
		if !e.terminated {
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

	// Record the executor's own top-level run before any child spawns,
	// so e.currentRunID is available as ParentRunID for child agent runs.
	e.currentRunID = e.recordAgentRun(e.agentName, e.beadID, "", e.repoModel(), "wizard", "execute", time.Now(), nil)

	// Main interpreter loop.
	for {
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
		state.Save(e.agentName, e.deps.ConfigDir)

		// Activate step bead if tracked.
		if stepBeadID, ok := state.StepBeadIDs[stepName]; ok {
			if err := e.deps.ActivateStepBead(stepBeadID); err != nil {
				e.log("warning: activate step bead %s (%s): %s", stepBeadID, stepName, err)
			}
		}

		e.log("step: %s (action: %s)", stepName, stepCfg.Action)

		// 4. Dispatch action.
		result := e.dispatchAction(stepName, stepCfg, state)

		// 5. Record outputs and update state.
		if result.Error != nil {
			ss = state.Steps[stepName]
			ss.Status = "failed"
			ss.CompletedAt = time.Now().UTC().Format(time.RFC3339)
			state.Steps[stepName] = ss
			state.Save(e.agentName, e.deps.ConfigDir)

			// Close step bead on failure.
			if stepBeadID, ok := state.StepBeadIDs[stepName]; ok {
				e.deps.CloseStepBead(stepBeadID)
			}

			// Build node-scoped result with step metadata.
			resultParts := []string{fmt.Sprintf("failure: step %s", stepName)}
			if stepCfg.Action != "" {
				resultParts = append(resultParts, fmt.Sprintf("action=%s", stepCfg.Action))
			}
			if stepCfg.Flow != "" {
				resultParts = append(resultParts, fmt.Sprintf("flow=%s", stepCfg.Flow))
			}
			if stepCfg.Workspace != "" {
				resultParts = append(resultParts, fmt.Sprintf("workspace=%s", stepCfg.Workspace))
			}
			resultMsg := strings.Join(resultParts, " ") + ": " + result.Error.Error()
			e.closeGraphAttempt(state, resultMsg)

			// Escalate to archmage with node-scoped context so the parent bead
			// gets needs-human + interrupted:* labels and an alert bead.
			EscalateGraphStepFailure(e.beadID, e.agentName, "step-failure",
				result.Error.Error(), stepName, stepCfg.Action, stepCfg.Flow, stepCfg.Workspace, e.deps)

			return fmt.Errorf("step %s failed: %w", stepName, result.Error)
		}

		if result.Hooked {
			ss = state.Steps[stepName]
			ss.Status = "hooked"
			ss.Outputs = result.Outputs
			state.Steps[stepName] = ss
			state.ActiveStep = ""
			state.Save(e.agentName, e.deps.ConfigDir)
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

		// 6. Close step bead.
		if stepBeadID, ok := state.StepBeadIDs[stepName]; ok {
			if err := e.deps.CloseStepBead(stepBeadID); err != nil {
				e.log("warning: close step bead %s (%s): %s", stepBeadID, stepName, err)
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
			e.terminated = true
			// Save parent state before cleaning up nested state (crash-safe ordering).
			state.Save(e.agentName, e.deps.ConfigDir)
			if stepCfg.Action == "graph.run" {
				RemoveGraphState(e.agentName+"-"+stepName, e.deps.ConfigDir)
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
		state.Save(e.agentName, e.deps.ConfigDir)

		// 10. Clean up nested graph state files after the parent save is durable.
		// This is crash-safe: the parent step is already recorded as completed,
		// so if the process dies here, the nested file is orphaned (harmless)
		// but the parent won't re-run the step.
		if stepCfg.Action == "graph.run" {
			nestedAgentName := e.agentName + "-" + stepName
			RemoveGraphState(nestedAgentName, e.deps.ConfigDir)
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
// review-phase graph called from spire-agent-work-v3) without interfering
// with the parent graph's lifecycle.
func (e *Executor) RunNestedGraph(graph *FormulaStepGraph, state *GraphState) error {
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
			state.Save(state.AgentName, e.deps.ConfigDir) // persist failure for resume
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
			state.Save(state.AgentName, e.deps.ConfigDir)
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
		state.Save(state.AgentName, e.deps.ConfigDir)
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

// --- Graph-specific bead management helpers ---

// ensureGraphAttemptBead creates or resumes an attempt bead for graph execution.
func (e *Executor) ensureGraphAttemptBead(state *GraphState) error {
	if state.AttemptBeadID != "" {
		b, err := e.deps.GetBead(state.AttemptBeadID)
		if err == nil && (b.Status == "open" || b.Status == "in_progress") {
			e.log("resuming existing attempt bead %s", state.AttemptBeadID)
			return nil
		}
		state.AttemptBeadID = ""
	}

	existing, err := e.deps.GetActiveAttempt(e.beadID)
	if err != nil {
		return err
	}
	if existing != nil {
		agent := e.deps.HasLabel(*existing, "agent:")
		if agent == e.agentName {
			state.AttemptBeadID = existing.ID
			e.log("reusing attempt bead %s (created by claim)", existing.ID)
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
	return nil
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
	// precedence over repo defaults. Mirrors the v2 resolveBranchState
	// logic in executor.go.
	if bead, berr := e.deps.GetBead(e.beadID); berr == nil {
		if bb := e.deps.HasLabel(bead, "base-branch:"); bb != "" {
			e.log("using bead base-branch override: %s (was: %s)", bb, state.BaseBranch)
			state.BaseBranch = bb
		}
	}

	if state.StagingBranch == "" {
		state.StagingBranch = e.resolveDeclaredGraphStagingBranch(graph, state)
	}
	if state.StagingBranch == "" {
		state.StagingBranch = "staging/" + e.beadID
	}

	e.log("branch state resolved: repo=%s base=%s staging=%s",
		state.RepoPath, state.BaseBranch, state.StagingBranch)
	return nil
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
func (e *Executor) ensureGraphStepBeads(graph *FormulaStepGraph, state *GraphState) error {
	if len(state.StepBeadIDs) > 0 {
		e.log("graph step beads already exist (%d steps)", len(state.StepBeadIDs))
		return nil
	}

	state.StepBeadIDs = make(map[string]string, len(graph.Steps))
	for stepName, stepCfg := range graph.Steps {
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

	return state.Save(e.agentName, e.deps.ConfigDir)
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
