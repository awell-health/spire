package executor

import (
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/agent"
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

// actionBeadFinish reads step.With["status"] and closes/discards the bead accordingly.
func actionBeadFinish(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	status := step.With["status"]
	switch status {
	case "closed", "done", "":
		if err := e.deps.CloseBead(e.beadID); err != nil {
			return ActionResult{Error: fmt.Errorf("close bead: %w", err)}
		}
		return ActionResult{Outputs: map[string]string{"status": "closed"}}
	case "wontfix", "discard":
		if err := TerminalDiscard(e.beadID, e.deps, e.log); err != nil {
			return ActionResult{Error: fmt.Errorf("terminal discard: %w", err)}
		}
		return ActionResult{Outputs: map[string]string{"status": "discarded"}}
	default:
		return ActionResult{Error: fmt.Errorf("unknown bead.finish status %q", status)}
	}
}

// actionMergeToMain wraps executeMerge(), returning the merge result.
func actionMergeToMain(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	mergePC := PhaseConfig{}
	if e.formula != nil {
		if pc, ok := e.formula.Phases["merge"]; ok {
			mergePC = pc
		}
	}

	if mergeErr := e.executeMerge(mergePC); mergeErr != nil {
		return ActionResult{Error: fmt.Errorf("merge to main: %w", mergeErr)}
	}

	return ActionResult{Outputs: map[string]string{"merged": "true"}}
}

// actionVerifyRun runs a build/test command from step.With["command"].
func actionVerifyRun(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	command := step.With["command"]
	if command == "" {
		return ActionResult{Outputs: map[string]string{"result": "skipped"}}
	}

	stagingWt, err := e.ensureStagingWorktree()
	if err != nil {
		return ActionResult{Error: fmt.Errorf("ensure staging worktree: %w", err)}
	}

	if buildErr := stagingWt.RunBuild(command); buildErr != nil {
		return ActionResult{
			Outputs: map[string]string{"result": "failed"},
			Error:   fmt.Errorf("verify command failed: %w", buildErr),
		}
	}

	return ActionResult{Outputs: map[string]string{"result": "passed"}}
}

// --- Stubs (to be implemented in later tasks) ---

// actionMaterializePlan will be implemented in spi-whcii.5.
func actionMaterializePlan(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return ActionResult{Error: fmt.Errorf("not yet implemented: beads.materialize_plan")}
}

// actionDispatchChildren will be implemented in spi-whcii.5.
func actionDispatchChildren(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return ActionResult{Error: fmt.Errorf("not yet implemented: dispatch.children")}
}

// actionGraphRun will be implemented after review graph becomes formula-selectable (spi-whcii.8).
func actionGraphRun(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return ActionResult{Error: fmt.Errorf("not yet implemented: graph.run")}
}
