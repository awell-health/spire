package executor

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/steveyegge/beads"
)

// ActionResult is the return type from an action handler.
type ActionResult struct {
	Outputs map[string]string
	Error   error
	Hooked  bool
}

// ActionHandler is the signature for a graph action handler.
type ActionHandler func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult

// actionRegistry maps opcode strings to handler functions.
// graph.run is registered in init() to break the initialization cycle
// (actionGraphRun → RunNestedGraph → dispatchAction → actionRegistry).
var actionRegistry = map[string]ActionHandler{
	"wizard.run":             actionWizardRun,
	"check.design-linked":   actionCheckDesignLinked,
	"beads.materialize_plan": actionMaterializePlan,
	"dispatch.children":     actionDispatchChildren,
	"verify.run":            actionVerifyRun,
	"git.merge_to_main":     actionMergeToMain,
	"bead.finish":           actionBeadFinish,
	"human.approve":         actionHumanApprove,
	"noop":                  actionNoop,
}

func init() {
	actionRegistry["graph.run"] = actionGraphRun
}

// dispatchAction looks up step.Action in the action registry and calls the handler.
func (e *Executor) dispatchAction(stepName string, step StepConfig, state *GraphState) ActionResult {
	if step.Action == "" {
		return ActionResult{Error: fmt.Errorf("step %q has no action defined", stepName)}
	}
	// Interpolate {steps.X.outputs.Y} and {vars.Z} in With values.
	// Copy the map to avoid mutating the formula definition.
	if len(step.With) > 0 {
		copied := make(map[string]string, len(step.With))
		for k, v := range step.With {
			copied[k] = v
		}
		step.With = copied
		e.interpolateWith(&step, state)
	}
	handler, ok := actionRegistry[step.Action]
	if !ok {
		return ActionResult{Error: fmt.Errorf("unknown action %q for step %q", step.Action, stepName)}
	}
	return handler(e, stepName, step, state)
}

// interpolateWith resolves {steps.X.outputs.Y}, {vars.Z}, etc. in step.With values
// using the same context map that conditions use.
func (e *Executor) interpolateWith(step *StepConfig, state *GraphState) {
	if len(step.With) == 0 {
		return
	}
	ctx := e.buildConditionContext(state)
	for k, v := range step.With {
		if !strings.Contains(v, "{") {
			continue
		}
		for ctxKey, ctxVal := range ctx {
			v = strings.ReplaceAll(v, "{"+ctxKey+"}", ctxVal)
		}
		step.With[k] = v
	}
}

// --- Real implementations ---

// effectiveRepoPath returns the repo path from the v2 state or v3 graph state,
// whichever is available. This allows plan methods (which historically used
// e.state.RepoPath) to work in both v2 and v3 execution contexts.
func (e *Executor) effectiveRepoPath() string {
	if e.state != nil && e.state.RepoPath != "" {
		return e.state.RepoPath
	}
	if e.graphState != nil {
		return e.graphState.RepoPath
	}
	return "."
}

// actionWizardRun maps step.Flow to the existing wizard dispatch: calls
// e.deps.Spawner.Spawn() with the appropriate role and args based on step.Flow,
// or invokes executor planning methods directly for plan flows.
func actionWizardRun(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	flow := step.Flow
	if flow == "" {
		flow = step.With["flow"]
	}

	// Resolve workspace dir from step declaration or fall back to state.WorktreeDir.
	wsDir := state.WorktreeDir
	if step.Workspace != "" {
		dir, err := e.resolveGraphWorkspace(step.Workspace, state)
		if err != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace %q for %s: %w", step.Workspace, stepName, err)}
		}
		wsDir = dir
	}

	switch flow {
	case "task-plan":
		return actionPlanTask(e, stepName, step, state)
	case "epic-plan":
		return actionPlanEpic(e, stepName, step, state)
	case "implement":
		extraArgs := []string{"--apprentice"}
		if wsDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", wsDir)
		}
		return wizardRunSpawn(e, stepName, step, state, agent.RoleApprentice, extraArgs)
	case "sage-review":
		extraArgs := []string{}
		if step.VerdictOnly {
			extraArgs = append(extraArgs, "--verdict-only")
		}
		if wsDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", wsDir)
		}
		result := wizardRunSpawn(e, stepName, step, state, agent.RoleSage, extraArgs)
		// Promote the review verdict into outputs so the formula can route on
		// steps.sage-review.outputs.verdict. The generic result field from
		// wizard-review carries the verdict string.
		verdict := result.Outputs["result"]
		if verdict == "approve" || verdict == "request_changes" {
			result.Outputs["verdict"] = verdict
		}
		// Note: review_round is no longer mutated here. The interpreter tracks
		// completed_count mechanically for every step; the formula routes on
		// steps.sage-review.completed_count instead.
		return result
	case "recovery-verify":
		var extraArgs []string
		if wsDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", wsDir)
		}
		result := wizardRunSpawn(e, stepName, step, state, agent.RoleApprentice, extraArgs)
		// Promote the result field to verification_status so the formula can
		// route on steps.verify.outputs.verification_status. The apprentice
		// writes a generic "result" field (e.g. "pass" or "fail") to
		// result.json; the agentResultJSON struct only surfaces Result,
		// Branch, Commit — no arbitrary fields. This mirrors the sage-review
		// pattern that promotes result → verdict.
		if v := result.Outputs["result"]; v == "pass" || v == "fail" {
			result.Outputs["verification_status"] = v
		}
		return result
	case "review-fix":
		extraArgs := []string{"--review-fix", "--apprentice"}
		if wsDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", wsDir)
		}
		return wizardRunSpawn(e, stepName, step, state, agent.RoleApprentice, extraArgs)
	case "arbiter":
		return actionArbiterEscalate(e, stepName, step, state)
	default:
		// For other flows, spawn with apprentice role + workspace if declared.
		var extraArgs []string
		if wsDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", wsDir)
		}
		return wizardRunSpawn(e, stepName, step, state, agent.RoleApprentice, extraArgs)
	}
}

// actionPlanTask routes the "task-plan" flow to the executor's inline planning
// method, which invokes Claude to produce an implementation plan comment.
func actionPlanTask(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("get bead for task plan: %w", err)}
	}

	if err := e.wizardPlanTask(bead, step.Model, step.MaxTurns); err != nil {
		return ActionResult{Error: fmt.Errorf("task plan: %w", err)}
	}

	return ActionResult{Outputs: map[string]string{"status": "planned"}}
}

// actionPlanEpic routes the "epic-plan" flow to the executor's inline planning
// method, which invokes Claude to break the epic into subtasks and create child beads.
func actionPlanEpic(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("get bead for epic plan: %w", err)}
	}

	if err := e.wizardPlanEpic(bead, step.Model, step.MaxTurns); err != nil {
		return ActionResult{Error: fmt.Errorf("epic plan: %w", err)}
	}

	return ActionResult{Outputs: map[string]string{"status": "planned"}}
}

// actionArbiterEscalate routes the "arbiter" flow to the executor's arbiter
// escalation logic. The v2 dispatchArbiter uses e.formula and e.state which
// are nil in v3 graph mode, so this builds a RevisionPolicy from the graph
// state vars and calls ReviewEscalateToArbiter directly.
func actionArbiterEscalate(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	sageName := fmt.Sprintf("%s-sage", e.agentName)

	// Build revision policy from graph vars and step config.
	arbiterModel := step.Model
	if arbiterModel == "" {
		arbiterModel = "claude-opus-4-6"
	}
	maxRounds := 3
	if mr, ok := state.Vars["max_review_rounds"]; ok {
		if v, err := fmt.Sscanf(mr, "%d", &maxRounds); v == 0 || err != nil {
			maxRounds = 3
		}
	}

	revPolicy := RevisionPolicy{
		MaxRounds:    maxRounds,
		ArbiterModel: arbiterModel,
	}

	lastReview := &Review{Verdict: "request_changes", Summary: "Max review rounds reached"}
	started := time.Now()
	err := e.deps.ReviewEscalateToArbiter(e.beadID, sageName, lastReview, revPolicy, e.log)

	reviewRound := state.Counters["review_round"]
	e.recordAgentRun(sageName, e.beadID, "", arbiterModel, "arbiter", "review", started, err,
		withReviewStep("arbiter", reviewRound+1),
		withParentRun(e.currentRunID))

	if err != nil {
		return ActionResult{Error: fmt.Errorf("arbiter escalation: %w", err)}
	}

	// Read arbiter decision from bead labels.
	decision := ""
	if bead, bErr := e.deps.GetBead(e.beadID); bErr == nil {
		decision = e.deps.HasLabel(bead, "arbiter-decision:")
		if decision == "" && e.deps.ContainsLabel(bead, "review-approved") {
			decision = "merge"
		}
	}

	return ActionResult{Outputs: map[string]string{
		"arbiter_decision": decision,
		"result":           "escalated",
	}}
}

// wizardRunSpawn is the common spawn logic for wizard.run actions.
func wizardRunSpawn(e *Executor, stepName string, step StepConfig, state *GraphState, role agent.SpawnRole, extraArgs []string) ActionResult {
	spawnName := fmt.Sprintf("%s-%s", e.agentName, stepName)
	started := time.Now()

	handle, err := e.deps.Spawner.Spawn(agent.SpawnConfig{
		Name:         spawnName,
		BeadID:       e.beadID,
		Role:         role,
		Provider:     e.resolveStepProvider(step),
		ExtraArgs:    extraArgs,
		CustomPrompt: step.With["prompt"],
	})
	if err != nil {
		return ActionResult{Error: fmt.Errorf("spawn %s: %w", stepName, err)}
	}

	waitErr := handle.Wait()

	// Read result.json for outputs. If the child wrote result.json, trust
	// its declared output as the node's terminal value — regardless of
	// waitErr. The child may exit non-zero after writing (e.g. signal
	// during cleanup) but its declared result is authoritative.
	outputs := make(map[string]string)
	hasResultJSON := false
	if ar := e.readAgentResult(spawnName); ar != nil {
		hasResultJSON = true
		outputs["result"] = ar.Result
		if ar.Branch != "" {
			outputs["branch"] = ar.Branch
		}
		if ar.Commit != "" {
			outputs["commit"] = ar.Commit
		}
	} else if waitErr != nil {
		outputs["result"] = "error"
	} else {
		outputs["result"] = "success"
	}

	model := step.Model
	e.recordAgentRun(spawnName, e.beadID, "", model, string(role), stepName, started, waitErr,
		withParentRun(e.currentRunID))

	// Propagate child process failure as a node error only when no
	// result.json was written. If the child declared its output, trust
	// it mechanically — the executor does not reinterpret subprocess
	// results.
	if waitErr != nil && !hasResultJSON {
		return ActionResult{Outputs: outputs, Error: fmt.Errorf("subprocess %s exited: %w", stepName, waitErr)}
	}

	return ActionResult{Outputs: outputs}
}

// actionCheckDesignLinked extracts the design validation logic into a
// self-contained check that verifies design beads are linked and closed.
// When auto_create is "true", missing or unready design beads cause a Hooked
// result (graph parks) instead of a hard error.
func actionCheckDesignLinked(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	autoCreate := step.With["auto_create"] == "true"

	deps, err := e.deps.GetDepsWithMeta(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("get deps: %w", err)}
	}

	// Find existing design bead via discovered-from dep.
	var designRef string
	var designOpen bool
	var designEmpty bool
	for _, dep := range deps {
		if dep.DependencyType != beads.DepDiscoveredFrom || dep.IssueType != "design" {
			continue
		}
		designRef = dep.ID
		if string(dep.Status) != "closed" {
			designOpen = true
			break
		}
		comments, _ := e.deps.GetComments(dep.ID)
		if len(comments) == 0 && dep.Description == "" {
			designEmpty = true
			break
		}
		// Design bead exists, closed, has content — success.
		return ActionResult{Outputs: map[string]string{"design_ref": designRef}}
	}

	// No design bead found.
	if designRef == "" {
		if !autoCreate {
			return ActionResult{Error: fmt.Errorf("no linked design bead found (discovered-from dep with type=design)")}
		}
		// Auto-create design bead.
		newID, err := e.deps.CreateBead(CreateOpts{
			Title:  "Design: " + e.beadID,
			Type:   e.deps.ParseIssueType("design"),
			Labels: []string{"needs-human"},
		})
		if err != nil {
			return ActionResult{Error: fmt.Errorf("create design bead: %w", err)}
		}
		// Link via discovered-from dep (epic depends on design).
		if err := e.deps.AddDepTyped(e.beadID, newID, "discovered-from"); err != nil {
			e.log("warning: link design bead %s: %s", newID, err)
		}
		// Comment on both beads.
		e.deps.AddComment(e.beadID, fmt.Sprintf("Auto-created design bead %s — awaiting human design input.", newID))
		e.deps.AddComment(newID, fmt.Sprintf("Created automatically for epic %s. Please add design content and close when ready.", e.beadID))
		// Message archmage.
		e.sendArchmageMessage(fmt.Sprintf("Design bead %s created for epic %s — needs human design input.", newID, e.beadID))
		return ActionResult{Hooked: true, Outputs: map[string]string{"design_ref": newID}}
	}

	// Design bead exists but is open or empty.
	if designOpen || designEmpty {
		if !autoCreate {
			if designOpen {
				return ActionResult{Error: fmt.Errorf("design bead %s is not closed", designRef)}
			}
			return ActionResult{Error: fmt.Errorf("design bead %s has no content", designRef)}
		}
		// Design bead exists but not ready — hook and wait.
		return ActionResult{Hooked: true, Outputs: map[string]string{"design_ref": designRef}}
	}

	// Should not reach here.
	return ActionResult{Outputs: map[string]string{"design_ref": designRef}}
}

// actionHumanApprove implements the human.approve gate action.
//
// First run (no labels yet): adds "needs-human" + "awaiting-approval" labels,
// posts a comment, and returns Hooked so the graph parks.
//
// Re-run after spire approve clears labels: detects "awaiting-approval" is gone
// and returns success so the graph advances.
//
// Re-run while still waiting: returns Hooked again.
func actionHumanApprove(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("human.approve: get bead: %w", err)}
	}

	hasAwaitingApproval := e.deps.ContainsLabel(bead, "awaiting-approval")
	hasNeedsHuman := e.deps.ContainsLabel(bead, "needs-human")

	// Case 1: needs-human present but awaiting-approval gone.
	// This is an INCONSISTENT STATE — spire approve removes both labels
	// atomically, so this means someone manually removed only awaiting-approval.
	// Treat as approved to avoid blocking the graph, but log a warning.
	if !hasAwaitingApproval && hasNeedsHuman {
		e.log("warning: human.approve inconsistent labels for %s: needs-human present but awaiting-approval missing — treating as approved", e.beadID)
		return ActionResult{Outputs: map[string]string{"status": "approved"}}
	}
	if !hasAwaitingApproval && !hasNeedsHuman {
		// Either first run (neither label exists) or fully cleared.
		// Distinguish by checking if the step has run before (outputs present).
		if state.Steps[stepName].CompletedCount > 0 {
			// Re-run after labels cleared — approved.
			return ActionResult{Outputs: map[string]string{"status": "approved"}}
		}
		// First run: add labels and comment, then park.
		if err := e.deps.AddLabel(e.beadID, "needs-human"); err != nil {
			return ActionResult{Error: fmt.Errorf("human.approve: add needs-human label: %w", err)}
		}
		if err := e.deps.AddLabel(e.beadID, "awaiting-approval"); err != nil {
			return ActionResult{Error: fmt.Errorf("human.approve: add awaiting-approval label: %w", err)}
		}
		title := step.Title
		if title == "" {
			title = stepName
		}
		e.deps.AddComment(e.beadID, fmt.Sprintf("Waiting for human approval before advancing past %s", title))
		return ActionResult{Hooked: true}
	}

	// Case 2: awaiting-approval still present → not yet approved, keep waiting.
	return ActionResult{Hooked: true}
}

// sendArchmageMessage sends a message to the archmage about this bead.
// Uses the existing MessageArchmage pattern from executor_escalate.go.
func (e *Executor) sendArchmageMessage(msg string) {
	MessageArchmage(e.agentName, e.beadID, msg, e.deps)
}

// actionBeadFinish closes the bead and sets executor to terminated.
// Reads With parameters:
//
//	status: "closed" | "done" | "wontfix" | "discard" (default: "closed")
// actionNoop is a no-op action that completes immediately with success.
// Used for terminal signal steps in nested graphs (e.g. subgraph-review merge/discard
// terminals) where the parent graph is responsible for the real side effects.
func actionNoop(_ *Executor, _ string, _ StepConfig, _ *GraphState) ActionResult {
	return ActionResult{Outputs: map[string]string{"status": "done"}}
}

//	outcome: alias for status (used by some formulas)
//
// For epic formulas, also closes orphan subtask beads.
func actionBeadFinish(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	status := step.With["status"]
	if status == "" {
		status = step.With["outcome"]
	}

	switch status {
	case "closed", "done", "success", "resolved", "":
		// Close orphan subtask beads (for epic formulas).
		if children, childErr := e.deps.GetChildren(e.beadID); childErr == nil {
			for _, child := range children {
				if child.Status == "closed" {
					continue
				}
				if e.deps.IsAttemptBead(child) || e.deps.IsStepBead(child) || e.deps.IsReviewRoundBead(child) {
					continue
				}
				if err := e.deps.CloseBead(child.ID); err != nil {
					e.log("warning: close orphan subtask %s: %s", child.ID, err)
				}
			}
		}

		// Close the main bead.
		if err := e.deps.CloseBead(e.beadID); err != nil {
			return ActionResult{Error: fmt.Errorf("close bead: %w", err)}
		}

		// Close related recovery beads.
		if err := recovery.CloseRelatedRecoveryBeads(executorBeadOps{e.deps}, e.beadID, "bead finished successfully"); err != nil {
			e.log("warning: close recovery beads: %v", err)
		}

		// Close the attempt bead.
		if state.AttemptBeadID != "" {
			if err := e.deps.CloseAttemptBead(state.AttemptBeadID, "success: "+stepName); err != nil {
				e.log("warning: close attempt bead: %s", err)
			}
			state.AttemptBeadID = ""
		}

		e.terminated = true
		return ActionResult{Outputs: map[string]string{"status": "closed"}}

	case "wontfix", "discard":
		if err := TerminalDiscard(e.beadID, e.deps, e.log); err != nil {
			return ActionResult{Error: fmt.Errorf("terminal discard: %w", err)}
		}
		e.terminated = true
		return ActionResult{Outputs: map[string]string{"status": "discarded"}}

	case "escalate":
		EscalateGraphStepFailure(e.beadID, e.agentName, "bead-finish-escalate",
			"formula requested escalation", stepName, step.Action, step.Flow, step.Workspace, e.deps)
		return ActionResult{Outputs: map[string]string{"status": "escalated"}}

	default:
		return ActionResult{Error: fmt.Errorf("unknown bead.finish status %q", status)}
	}
}

// actionMergeToMain lands the staging branch onto the base branch.
// Reads With parameters:
//
//	strategy: "squash" | "merge" | "rebase" (default: "squash")
//	build: pre-merge build verification command (optional)
//	test: pre-merge test verification command (optional)
//	doc_patterns: comma-separated glob patterns for doc review (optional)
//
// Does NOT close beads — that's bead.finish.
func actionMergeToMain(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// Resolve workspace and determine the actual branch being merged.
	// When a step declares a workspace, use that workspace's branch for merge
	// operations and cleanup. Fall back to state.StagingBranch for legacy paths.
	var stagingWt *spgit.StagingWorktree
	mergeBranch := state.StagingBranch // default for legacy/unresolved
	if step.Workspace != "" {
		dir, wsErr := e.resolveGraphWorkspace(step.Workspace, state)
		if wsErr != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace %q for merge: %w", step.Workspace, wsErr)}
		}
		ws := state.Workspaces[step.Workspace]
		stagingWt = spgit.ResumeStagingWorktree(state.RepoPath, dir, ws.Branch, ws.BaseBranch, e.log)
		mergeBranch = ws.Branch
	} else {
		var err error
		stagingWt, err = e.ensureGraphStagingWorktree(state)
		if err != nil {
			return ActionResult{Error: fmt.Errorf("ensure staging worktree for merge: %w", err)}
		}
	}

	buildStr := step.With["build"]
	testStr := step.With["test"]

	// Pre-merge build verification.
	if buildStr != "" {
		e.log("verifying build before merge: %s", buildStr)
		if buildErr := stagingWt.RunBuild(buildStr); buildErr != nil {
			return ActionResult{Error: fmt.Errorf("pre-merge build verification failed: %w", buildErr)}
		}
	}

	// Doc review (optional).
	if docPatternsStr := step.With["doc_patterns"]; docPatternsStr != "" {
		patterns := strings.Split(docPatternsStr, ",")
		if docErr := e.reviewDocsForStaleness(stagingWt.Dir, mergeBranch, state.BaseBranch, patterns, step.Model); docErr != nil {
			e.log("warning: doc review: %s", docErr)
		}
	}

	// Merge workspace branch -> main.
	mergeEnv := os.Environ()
	if tower, tErr := e.deps.ActiveTowerConfig(); tErr == nil && tower != nil {
		mergeEnv = e.deps.ArchmageGitEnv(tower)
	}

	e.log("merging %s -> %s", mergeBranch, state.BaseBranch)
	if mergeErr := stagingWt.MergeToMain(state.BaseBranch, mergeEnv, buildStr, testStr); mergeErr != nil {
		return ActionResult{Error: fmt.Errorf("merge to main: %w", mergeErr)}
	}

	// Push main.
	rc := &spgit.RepoContext{Dir: state.RepoPath, BaseBranch: state.BaseBranch, Log: e.log}
	if pushErr := rc.Push("origin", state.BaseBranch, mergeEnv); pushErr != nil {
		return ActionResult{Error: fmt.Errorf("push %s: %w", state.BaseBranch, pushErr)}
	}

	// Clean up the actual branch that was merged (best-effort).
	rc.DeleteBranch(mergeBranch)
	rc.DeleteRemoteBranch("origin", mergeBranch)

	return ActionResult{Outputs: map[string]string{"merged": "true"}}
}

// actionVerifyRun runs build and/or test commands in the declared workspace.
// Reads With parameters:
//
//	command: single command string (backward compat)
//	build: build command string (optional)
//	test: test command string (optional)
//
// Produces: status ("pass"/"fail"), error_log (error message if failed)
func actionVerifyRun(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// Support both "command" (backward compat) and separate "build"/"test" params.
	buildCmd := step.With["build"]
	testCmd := step.With["test"]
	if buildCmd == "" && testCmd == "" {
		buildCmd = step.With["command"]
	}
	// Fall back to spire.yaml runtime config. The embedded formulas intentionally
	// omit build/test commands — spire.yaml is the source of truth for how a repo
	// builds and tests.
	if buildCmd == "" && testCmd == "" {
		if rc := e.deps.RepoConfig(); rc != nil {
			buildCmd = rc.Runtime.Build
			testCmd = rc.Runtime.Test
		}
	}
	if buildCmd == "" && testCmd == "" {
		return ActionResult{Outputs: map[string]string{"status": "pass", "result": "skipped"}}
	}

	// Resolve workspace: prefer step-declared workspace, fall back to staging worktree.
	var stagingWt *spgit.StagingWorktree
	if step.Workspace != "" {
		dir, wsErr := e.resolveGraphWorkspace(step.Workspace, state)
		if wsErr != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace %q for verify: %w", step.Workspace, wsErr)}
		}
		stagingWt = spgit.ResumeStagingWorktree(state.RepoPath, dir,
			state.Workspaces[step.Workspace].Branch, state.Workspaces[step.Workspace].BaseBranch, e.log)
	} else {
		var err error
		stagingWt, err = e.ensureGraphStagingWorktree(state)
		if err != nil {
			return ActionResult{Error: fmt.Errorf("ensure staging worktree: %w", err)}
		}
	}

	// Run install command (e.g. pnpm install) before build/test.
	// Staging worktrees are fresh checkouts with no node_modules.
	installCmd := step.With["install"]
	if installCmd == "" {
		if rc := e.deps.RepoConfig(); rc != nil {
			installCmd = rc.Runtime.Install
		}
	}
	if installCmd != "" {
		if installErr := stagingWt.RunBuild(installCmd); installErr != nil {
			return ActionResult{
				Outputs: map[string]string{
					"status":    "fail",
					"result":    "failed",
					"error_log": "install failed: " + installErr.Error(),
				},
			}
		}
	}

	// Run build command.
	if buildCmd != "" {
		if buildErr := stagingWt.RunBuild(buildCmd); buildErr != nil {
			return ActionResult{
				Outputs: map[string]string{
					"status":    "fail",
					"result":    "failed",
					"error_log": buildErr.Error(),
				},
			}
		}
	}

	// Run test command.
	if testCmd != "" {
		if testErr := stagingWt.RunBuild(testCmd); testErr != nil {
			return ActionResult{
				Outputs: map[string]string{
					"status":    "fail",
					"result":    "failed",
					"error_log": testErr.Error(),
				},
			}
		}
	}

	return ActionResult{Outputs: map[string]string{"status": "pass", "result": "passed"}}
}

// actionDispatchChildren is implemented in action_dispatch.go.

// actionMaterializePlan verifies that child beads were created by the
// preceding wizard plan step (epic-plan or task-plan). In the v2 flow, the
// wizard's epic-plan behavior BOTH generates the plan AND creates child beads
// in a single step. The materialize action confirms children exist and reports
// the count — it does not re-create them.
//
// This step exists to make the dependency between planning and dispatch
// explicit in the graph: dispatch.children needs children to exist, and
// this step gates that with a clear error if planning failed to produce any.
func actionMaterializePlan(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	allChildren, err := e.deps.GetChildren(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("get children for %s: %w", e.beadID, err)}
	}

	// Filter out internal DAG beads (step, attempt, review-round) that
	// are created by ensureStepBeads/ensureAttemptBead, not by planning.
	var realChildren []Bead
	for _, c := range allChildren {
		if e.deps.IsAttemptBead(c) || e.deps.IsStepBead(c) || e.deps.IsReviewRoundBead(c) {
			continue
		}
		realChildren = append(realChildren, c)
	}

	if len(realChildren) == 0 {
		return ActionResult{Error: fmt.Errorf("no subtask beads found for %s — plan step may have failed to create children", e.beadID)}
	}

	e.log("materialize: found %d subtask(s) for %s", len(realChildren), e.beadID)

	outputs := map[string]string{
		"status":     "pass",
		"child_count": fmt.Sprintf("%d", len(realChildren)),
	}
	return ActionResult{Outputs: outputs}
}

// actionGraphRun executes a nested step-graph formula as a sub-graph within
// the current executor. It loads the named graph, runs the interpreter loop
// inline (without the deferred cleanup that RunGraph applies), and captures
// the terminal step's outputs for the parent graph to route on.
//
// Key design decisions:
//   - Uses RunNestedGraph (not RunGraph) to avoid deferred cleanup of the
//     parent's staging worktree, registry entry, and graph state file.
//   - The nested graph gets its own GraphState with a derived agent name
//     (parent-stepName) so state files don't collide.
//   - Parent vars are copied into the sub-state so subgraph-review can access
//     max_review_rounds, branch, etc.
//   - The terminal step name from the sub-graph becomes the "outcome" output,
//     which parent steps route on (e.g. steps.review.outputs.outcome == "merge").
func actionGraphRun(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	graphName := step.Graph
	if graphName == "" {
		graphName = step.With["graph"]
	}
	if graphName == "" {
		return ActionResult{Error: fmt.Errorf("graph.run: no graph name specified")}
	}

	// Load the nested graph formula.
	subGraph, err := formula.LoadStepGraphByName(graphName)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("load nested graph %q: %w", graphName, err)}
	}

	// Load or create sub-state for the nested graph.
	// On resume, the sub-state may already be persisted from a prior interrupted run.
	subAgentName := e.agentName + "-" + stepName
	subState, loadErr := LoadGraphState(subAgentName, e.deps.ConfigDir)
	if loadErr != nil {
		e.log("warning: load nested graph state for %s: %s (starting fresh)", subAgentName, loadErr)
	}
	if subState == nil {
		subState = NewGraphState(subGraph, e.beadID, subAgentName)
		subState.TowerName = state.TowerName // inherit tower scope from parent

		// Copy parent vars into sub-state (e.g. max_review_rounds, base_branch).
		for k, v := range state.Vars {
			subState.Vars[k] = v
		}

		// Copy branch/workspace info so the sub-graph can resolve the same
		// integration surface the parent step is already using.
		subState.RepoPath = state.RepoPath
		subState.BaseBranch = state.BaseBranch
		subState.StagingBranch = state.StagingBranch
		subState.WorktreeDir = state.WorktreeDir
		if err := e.propagateGraphRunWorkspace(step, state, subState); err != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace %q for graph.run %s: %w", step.Workspace, stepName, err)}
		}
	} else {
		e.log("resuming nested graph %s from persisted state (active: %s)", subAgentName, subState.ActiveStep)

		// Always refresh branch and workspace info from the parent state.
		subState.RepoPath = state.RepoPath
		subState.BaseBranch = state.BaseBranch
		subState.StagingBranch = state.StagingBranch
		subState.WorktreeDir = state.WorktreeDir
		if err := e.propagateGraphRunWorkspace(step, state, subState); err != nil {
			e.log("warning: resolve workspace %q on resume: %s", step.Workspace, err)
		}
	}

	// Run the nested graph using the isolated interpreter (saves after each step).
	runErr := e.RunNestedGraph(subGraph, subState)

	// Capture outputs from the terminal step that completed.
	outputs := make(map[string]string)
	for name, ss := range subState.Steps {
		if ss.Status == "completed" && formula.IsTerminal(subGraph, name) {
			outputs["outcome"] = name
			for k, v := range ss.Outputs {
				outputs[k] = v
			}
			break
		}
	}

	if runErr != nil {
		// Sub-state was saved after each step by RunNestedGraph.
		// On interrupt/failure it persists for resume.
		return ActionResult{Outputs: outputs, Error: runErr}
	}

	// Do NOT remove nested state here — the parent has not yet durably saved
	// this step as completed. If the process dies between here and the parent
	// save, the nested progress would be lost. The parent interpreter cleans
	// up nested state files after its own save (crash-safe ordering).

	return ActionResult{Outputs: outputs}
}

func (e *Executor) propagateGraphRunWorkspace(step StepConfig, parentState, subState *GraphState) error {
	if step.Workspace == "" {
		return nil
	}

	ws, ok := parentState.Workspaces[step.Workspace]
	if !ok || ws.Dir == "" {
		dir, err := e.resolveGraphWorkspace(step.Workspace, parentState)
		if err != nil {
			return err
		}
		ws = parentState.Workspaces[step.Workspace]
		if ws.Dir == "" {
			ws.Dir = dir
		}
	}

	subState.WorktreeDir = ws.Dir
	if subState.Workspaces == nil {
		subState.Workspaces = make(map[string]WorkspaceState)
	}
	subState.Workspaces[step.Workspace] = ws

	if step.Workspace == "staging" || ws.Kind == formula.WorkspaceKindStaging {
		if ws.Branch != "" {
			subState.StagingBranch = ws.Branch
		}
		if ws.BaseBranch != "" {
			subState.BaseBranch = ws.BaseBranch
		}
	}

	return nil
}
