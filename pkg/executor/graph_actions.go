package executor

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/steveyegge/beads"
)

// ActionResult is the return type from an action handler.
type ActionResult struct {
	Outputs map[string]string
	Error   error
}

// ActionHandler is the signature for a graph action handler.
type ActionHandler func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult

// actionRegistry maps opcode strings to handler functions.
var actionRegistry = map[string]ActionHandler{
	"wizard.run":             actionWizardRun,
	"check.design-linked":   actionCheckDesignLinked,
	"beads.materialize_plan": actionMaterializePlan,
	"dispatch.children":     actionDispatchChildren,
	"verify.run":            actionVerifyRun,
	"graph.run":             actionGraphRun,
	"git.merge_to_main":     actionMergeToMain,
	"bead.finish":           actionBeadFinish,
}

// dispatchAction looks up step.Action in the action registry and calls the handler.
func (e *Executor) dispatchAction(stepName string, step StepConfig, state *GraphState) ActionResult {
	if step.Action == "" {
		return ActionResult{Error: fmt.Errorf("step %q has no action defined", stepName)}
	}
	handler, ok := actionRegistry[step.Action]
	if !ok {
		return ActionResult{Error: fmt.Errorf("unknown action %q for step %q", step.Action, stepName)}
	}
	return handler(e, stepName, step, state)
}

// --- Real implementations ---

// actionWizardRun maps step.Flow to the existing wizard dispatch: calls
// e.deps.Spawner.Spawn() with the appropriate role and args based on step.Flow.
func actionWizardRun(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	flow := step.Flow
	if flow == "" {
		flow = step.With["flow"]
	}

	switch flow {
	case "implement":
		return wizardRunSpawn(e, stepName, step, state, agent.RoleApprentice, nil)
	case "sage-review":
		extraArgs := []string{}
		if step.VerdictOnly {
			extraArgs = append(extraArgs, "--verdict-only")
		}
		if state.WorktreeDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", state.WorktreeDir)
		}
		return wizardRunSpawn(e, stepName, step, state, agent.RoleSage, extraArgs)
	case "review-fix":
		extraArgs := []string{"--review-fix", "--apprentice"}
		if state.WorktreeDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", state.WorktreeDir)
		}
		return wizardRunSpawn(e, stepName, step, state, agent.RoleApprentice, extraArgs)
	default:
		// For task-plan, epic-plan, and other flows, spawn with wizard role.
		return wizardRunSpawn(e, stepName, step, state, agent.RoleApprentice, nil)
	}
}

// wizardRunSpawn is the common spawn logic for wizard.run actions.
func wizardRunSpawn(e *Executor, stepName string, step StepConfig, state *GraphState, role agent.SpawnRole, extraArgs []string) ActionResult {
	spawnName := fmt.Sprintf("%s-%s", e.agentName, stepName)
	started := time.Now()

	handle, err := e.deps.Spawner.Spawn(agent.SpawnConfig{
		Name:      spawnName,
		BeadID:    e.beadID,
		Role:      role,
		ExtraArgs: extraArgs,
	})
	if err != nil {
		return ActionResult{Error: fmt.Errorf("spawn %s: %w", stepName, err)}
	}

	waitErr := handle.Wait()

	// Read result.json for outputs.
	outputs := make(map[string]string)
	if ar := e.readAgentResult(spawnName); ar != nil {
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
	e.recordAgentRun(spawnName, e.beadID, "", model, string(role), stepName, started, waitErr)

	return ActionResult{Outputs: outputs}
}

// actionCheckDesignLinked extracts the design validation logic into a
// self-contained check that verifies design beads are linked and closed.
func actionCheckDesignLinked(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	deps, err := e.deps.GetDepsWithMeta(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("get deps: %w", err)}
	}

	var designRef string
	for _, dep := range deps {
		if dep.DependencyType != beads.DepDiscoveredFrom {
			continue
		}
		if dep.IssueType != "design" {
			continue
		}
		if string(dep.Status) != "closed" {
			return ActionResult{Error: fmt.Errorf("design bead %s is not closed", dep.ID)}
		}
		// Check for content.
		comments, _ := e.deps.GetComments(dep.ID)
		if len(comments) == 0 && dep.Description == "" {
			return ActionResult{Error: fmt.Errorf("design bead %s has no content", dep.ID)}
		}
		designRef = dep.ID
	}

	if designRef == "" {
		return ActionResult{Error: fmt.Errorf("no linked design bead found (discovered-from dep with type=design)")}
	}

	return ActionResult{Outputs: map[string]string{"design_ref": designRef}}
}

// actionBeadFinish closes the bead and sets executor to terminated.
// Reads With parameters:
//
//	status: "closed" | "done" | "wontfix" | "discard" (default: "closed")
//	outcome: alias for status (used by some formulas)
//
// For epic formulas, also closes orphan subtask beads.
func actionBeadFinish(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	status := step.With["status"]
	if status == "" {
		status = step.With["outcome"]
	}

	switch status {
	case "closed", "done", "success", "":
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
		EscalateHumanFailure(e.beadID, e.agentName, "bead-finish-escalate",
			"formula requested escalation", e.deps)
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
	stagingWt, err := e.ensureGraphStagingWorktree(state)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("ensure staging worktree for merge: %w", err)}
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
	if docPatterns := step.With["doc_patterns"]; docPatterns != "" {
		patterns := strings.Split(docPatterns, ",")
		pc := PhaseConfig{DocPatterns: patterns, Model: step.Model}
		if docErr := e.reviewDocsForStaleness(stagingWt.Dir, state.StagingBranch, state.BaseBranch, pc); docErr != nil {
			e.log("warning: doc review: %s", docErr)
		}
	}

	// Merge staging -> main.
	mergeEnv := os.Environ()
	if tower, tErr := e.deps.ActiveTowerConfig(); tErr == nil && tower != nil {
		mergeEnv = e.deps.ArchmageGitEnv(tower)
	}

	e.log("merging %s -> %s", state.StagingBranch, state.BaseBranch)
	if mergeErr := stagingWt.MergeToMain(state.BaseBranch, mergeEnv, buildStr, testStr); mergeErr != nil {
		return ActionResult{Error: fmt.Errorf("merge to main: %w", mergeErr)}
	}

	// Push main.
	rc := &spgit.RepoContext{Dir: state.RepoPath, BaseBranch: state.BaseBranch, Log: e.log}
	if pushErr := rc.Push("origin", state.BaseBranch, mergeEnv); pushErr != nil {
		return ActionResult{Error: fmt.Errorf("push %s: %w", state.BaseBranch, pushErr)}
	}

	// Clean up branches (best-effort).
	rc.DeleteBranch(state.StagingBranch)
	rc.DeleteRemoteBranch("origin", state.StagingBranch)

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
	if buildCmd == "" && testCmd == "" {
		return ActionResult{Outputs: map[string]string{"status": "pass", "result": "skipped"}}
	}

	stagingWt, err := e.ensureGraphStagingWorktree(state)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("ensure staging worktree: %w", err)}
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

// --- Stubs (to be implemented in later tasks) ---

// actionMaterializePlan will be implemented in a future task.
func actionMaterializePlan(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return ActionResult{Error: fmt.Errorf("not yet implemented: beads.materialize_plan")}
}

// actionDispatchChildren is implemented in action_dispatch.go.

// actionGraphRun will be implemented after review graph becomes formula-selectable (spi-whcii.8).
func actionGraphRun(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return ActionResult{Error: fmt.Errorf("not yet implemented: graph.run")}
}
