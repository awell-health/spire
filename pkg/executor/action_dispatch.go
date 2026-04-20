package executor

// action_dispatch.go — dispatch.children action handler and extracted dispatch helpers.
// Moves wave/sequential/direct child execution behind a declared executor action.

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/repoconfig"
)

// actionDispatchChildren orchestrates child bead execution.
// Reads With parameters:
//
//	strategy: "dependency-wave" | "sequential" | "direct" (default: "direct")
//
// Workspace: resolved to a StagingWorktree for merge integration.
// Produces: status ("pass"/"fail"), failed_subtasks (comma-separated IDs)
func actionDispatchChildren(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	strategy := step.With["strategy"]
	if strategy == "" {
		strategy = "direct"
	}

	// Parse formula-declared conflict_max_turns.  When unset (empty string),
	// conflictMaxTurns stays 0 and the --max-turns flag is omitted entirely —
	// the executor does not invent a turn budget.
	var conflictMaxTurns int
	if raw := step.With["conflict_max_turns"]; raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			conflictMaxTurns = v
		}
	}
	resolver := e.conflictResolver(conflictMaxTurns)

	// Resolve staging worktree from the step's declared workspace when present.
	// This keeps dispatch aligned with the actual integration workspace branch
	// instead of assuming state.StagingBranch is the right ref.
	var stagingWt *spgit.StagingWorktree
	var err error
	if step.Workspace != "" {
		dir, wsErr := e.resolveGraphWorkspace(step.Workspace, state)
		if wsErr != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace %q for dispatch: %w", step.Workspace, wsErr)}
		}
		ws := state.Workspaces[step.Workspace]
		state.WorktreeDir = dir
		if ws.Kind == formula.WorkspaceKindStaging {
			stagingWt, err = e.ensureGraphStagingWorktree(state)
			if err != nil {
				return ActionResult{Error: fmt.Errorf("ensure staging workspace %q for dispatch: %w", step.Workspace, err)}
			}
		} else {
			if ws.Branch != "" {
				state.StagingBranch = ws.Branch
			}
			if ws.BaseBranch != "" {
				state.BaseBranch = ws.BaseBranch
			}
			stagingWt = spgit.ResumeStagingWorktree(state.RepoPath, dir, ws.Branch, ws.BaseBranch, e.log)
		}
	} else {
		stagingWt, err = e.ensureGraphStagingWorktree(state)
		if err != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace for dispatch: %w", err)}
		}
	}

	model := step.Model

	// Resolve apprentice concurrency cap (most-specific wins):
	//   step.With["max-apprentices"] > e.deps.MaxApprentices > default (3)
	// Env-var precedence (SPIRE_MAX_APPRENTICES) is applied in the CLI bridge
	// when Deps is built; by the time it reaches here it's already baked into
	// e.deps.MaxApprentices.
	maxApprentices := e.deps.MaxApprentices
	if raw := step.With["max-apprentices"]; raw != "" {
		if n, convErr := strconv.Atoi(raw); convErr == nil && n > 0 {
			maxApprentices = n
		}
	}
	if maxApprentices <= 0 {
		maxApprentices = repoconfig.DefaultMaxApprentices
	}

	switch strategy {
	case "dependency-wave":
		waves, waveErr := ComputeWaves(e.beadID, e.deps)
		if waveErr != nil {
			return ActionResult{Error: fmt.Errorf("compute waves: %w", waveErr)}
		}
		if len(waves) == 0 {
			e.log("no open subtasks for dispatch")
			return ActionResult{Outputs: map[string]string{"status": "pass", "dispatched": "0"}}
		}
		results, dispErr := e.dispatchWaveCore(waves, stagingWt, model, resolver, maxApprentices)
		return buildDispatchResult(results, dispErr)

	case "sequential":
		waves, waveErr := ComputeWaves(e.beadID, e.deps)
		if waveErr != nil {
			return ActionResult{Error: fmt.Errorf("compute waves: %w", waveErr)}
		}
		if len(waves) == 0 {
			e.log("no open subtasks for sequential dispatch")
			return ActionResult{Outputs: map[string]string{"status": "pass", "dispatched": "0"}}
		}
		// Flatten waves into ordered list.
		var subtasks []string
		for _, wave := range waves {
			subtasks = append(subtasks, wave...)
		}
		results, dispErr := e.dispatchSequentialCore(subtasks, stagingWt, model, resolver)
		return buildDispatchResult(results, dispErr)

	case "direct":
		dispErr := e.dispatchDirectCore(stagingWt, model, resolver)
		if dispErr != nil {
			return ActionResult{
				Outputs: map[string]string{"status": "fail"},
				Error:   fmt.Errorf("direct dispatch: %w", dispErr),
			}
		}
		return ActionResult{Outputs: map[string]string{"status": "pass", "dispatched": "1"}}

	default:
		return ActionResult{Error: fmt.Errorf("unknown dispatch strategy %q", strategy)}
	}
}

// buildDispatchResult converts dispatch results into an ActionResult.
func buildDispatchResult(results []childResult, err error) ActionResult {
	if err != nil {
		return ActionResult{
			Outputs: map[string]string{"status": "fail"},
			Error:   err,
		}
	}

	var failed []string
	for _, r := range results {
		if r.Err != nil {
			failed = append(failed, r.BeadID)
		}
	}

	outputs := map[string]string{
		"status":     "pass",
		"dispatched": fmt.Sprintf("%d", len(results)),
	}
	if len(failed) > 0 {
		outputs["status"] = "fail"
		outputs["failed_subtasks"] = strings.Join(failed, ",")
	}
	return ActionResult{Outputs: outputs}
}

// childResult tracks the outcome of dispatching a single child.
type childResult struct {
	BeadID string
	Agent  string
	Branch string
	Err    error
}

// dispatchWaveCore executes children in dependency waves, merging each wave's
// branches into staging before proceeding to the next wave. It does NOT include
// build verification, build-fix retry, or subtask closing (those are separate
// formula steps).
//
// maxApprentices caps how many apprentice subprocesses run concurrently within
// a single wave. Callers pass the resolved value (>=1); a non-positive value
// falls back to repoconfig.DefaultMaxApprentices.
func (e *Executor) dispatchWaveCore(waves [][]string, stagingWt *spgit.StagingWorktree, model string, resolver func(string, string) error, maxApprentices int) ([]childResult, error) {
	if maxApprentices <= 0 {
		maxApprentices = repoconfig.DefaultMaxApprentices
	}
	e.log("dispatching %d wave(s) (max %d concurrent apprentice(s))", len(waves), maxApprentices)

	var allResults []childResult
	var startRef string

	for waveIdx, wave := range waves {
		e.log("=== wave %d: %d subtask(s) ===", waveIdx, len(wave))

		type waveResult struct {
			BeadID string
			Agent  string
			Err    error
		}

		var wg sync.WaitGroup
		resultCh := make(chan waveResult, len(wave))
		sem := make(chan struct{}, maxApprentices)

		for i, subtaskID := range wave {
			wg.Add(1)
			go func(idx int, beadID string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				name := fmt.Sprintf("%s-w%d-%d", e.agentName, waveIdx+1, idx+1)
				e.log("  dispatching %s for %s", name, beadID)

				e.deps.UpdateBead(beadID, map[string]interface{}{"status": "in_progress"})

				extraArgs := []string{"--apprentice"}
				started := time.Now()
				h, spawnErr := e.deps.Spawner.Spawn(agent.SpawnConfig{
					Name:      name,
					BeadID:    beadID,
					Role:      agent.RoleApprentice,
					ExtraArgs: extraArgs,
					StartRef:  startRef,
					LogPath:   filepath.Join(dolt.GlobalDir(), "wizards", name+".log"),
				})
				if spawnErr != nil {
					e.recordAgentRun(name, beadID, e.beadID, model, "apprentice", "implement", started, spawnErr,
						withParentRun(e.currentRunID))
					resultCh <- waveResult{BeadID: beadID, Agent: name, Err: spawnErr}
					return
				}
				waitErr := h.Wait()
				e.recordAgentRun(name, beadID, e.beadID, model, "apprentice", "implement", started, waitErr,
					withParentRun(e.currentRunID))
				if waitErr != nil {
					resultCh <- waveResult{BeadID: beadID, Agent: name, Err: waitErr}
					return
				}
				resultCh <- waveResult{BeadID: beadID, Agent: name}
			}(i, subtaskID)
		}

		wg.Wait()
		close(resultCh)

		// Collect results.
		var errs []string
		var waveResults []childResult
		for r := range resultCh {
			cr := childResult{
				BeadID: r.BeadID,
				Agent:  r.Agent,
				Branch: e.resolveBranch(r.BeadID),
				Err:    r.Err,
			}
			waveResults = append(waveResults, cr)
			allResults = append(allResults, cr)
			if r.Err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", r.BeadID, r.Err))
			}
		}

		if len(errs) > 0 {
			e.log("wave %d: %d error(s): %s", waveIdx, len(errs), strings.Join(errs, "; "))
		}

		// Merge successful child branches into staging.
		if stagingWt != nil {
			for _, cr := range waveResults {
				if cr.Err != nil || cr.Branch == "" {
					continue
				}
				if mergeErr := stagingWt.MergeBranch(cr.Branch, resolver); mergeErr != nil {
					return allResults, fmt.Errorf("merge %s into staging: %w", cr.Branch, mergeErr)
				}
			}

			// Update startRef for next wave.
			if sha, err := stagingWt.HeadSHA(); err == nil && sha != "" {
				startRef = sha
				e.log("next wave start-ref: %s", startRef)
			}
		}
	}

	return allResults, nil
}

// dispatchSequentialCore executes children one at a time, merging each into
// staging before advancing. It does NOT include inline review, inline
// merge-to-main, or subtask closing (those are separate formula steps).
func (e *Executor) dispatchSequentialCore(subtasks []string, stagingWt *spgit.StagingWorktree, model string, resolver func(string, string) error) ([]childResult, error) {
	e.log("sequential dispatch: %d subtask(s)", len(subtasks))

	var allResults []childResult
	var startRef string

	for i, subtaskID := range subtasks {
		e.log("=== sequential step %d/%d: %s ===", i+1, len(subtasks), subtaskID)
		e.deps.UpdateBead(subtaskID, map[string]interface{}{"status": "in_progress"})

		name := fmt.Sprintf("%s-seq-%d", e.agentName, i+1)
		started := time.Now()
		handle, spawnErr := e.deps.Spawner.Spawn(agent.SpawnConfig{
			Name:      name,
			BeadID:    subtaskID,
			Role:      agent.RoleApprentice,
			ExtraArgs: []string{"--apprentice"},
			StartRef:  startRef,
			LogPath:   filepath.Join(dolt.GlobalDir(), "wizards", name+".log"),
		})
		if spawnErr != nil {
			e.recordAgentRun(name, subtaskID, e.beadID, model, "apprentice", "implement", started, spawnErr,
				withParentRun(e.currentRunID))
			return allResults, fmt.Errorf("spawn apprentice for %s: %w", subtaskID, spawnErr)
		}
		waitErr := handle.Wait()
		e.recordAgentRun(name, subtaskID, e.beadID, model, "apprentice", "implement", started, waitErr,
			withParentRun(e.currentRunID))

		featBranch := e.resolveBranch(subtaskID)
		cr := childResult{
			BeadID: subtaskID,
			Agent:  name,
			Branch: featBranch,
			Err:    waitErr,
		}
		allResults = append(allResults, cr)

		if waitErr != nil {
			return allResults, fmt.Errorf("apprentice %s failed: %w", subtaskID, waitErr)
		}

		// Merge feat branch into staging.
		if stagingWt != nil {
			if mergeErr := stagingWt.MergeBranch(featBranch, resolver); mergeErr != nil {
				return allResults, fmt.Errorf("merge %s into staging: %w", featBranch, mergeErr)
			}
			// Update startRef for next child.
			if sha, err := stagingWt.HeadSHA(); err == nil && sha != "" {
				startRef = sha
				e.log("next sequential start-ref: %s", startRef)
			}
		}
	}

	return allResults, nil
}

// dispatchDirectCore spawns one apprentice for the bead, then merges the
// feat branch into staging.
func (e *Executor) dispatchDirectCore(stagingWt *spgit.StagingWorktree, model string, resolver func(string, string) error) error {
	apprenticeName := fmt.Sprintf("%s-impl", e.agentName)
	e.log("dispatching apprentice %s", apprenticeName)

	started := time.Now()
	handle, err := e.deps.Spawner.Spawn(agent.SpawnConfig{
		Name:          apprenticeName,
		BeadID:        e.beadID,
		Role:          agent.RoleApprentice,
		ExtraArgs:     []string{"--apprentice"},
		LogPath:       filepath.Join(dolt.GlobalDir(), "wizards", apprenticeName+".log"),
		AttemptID:     e.attemptID(),
		ApprenticeIdx: "0",
	})
	if err != nil {
		e.recordAgentRun(apprenticeName, e.beadID, "", model, "apprentice", "implement", started, err,
			withParentRun(e.currentRunID))
		return fmt.Errorf("spawn apprentice: %w", err)
	}

	waitErr := handle.Wait()
	e.recordAgentRun(apprenticeName, e.beadID, "", model, "apprentice", "implement", started, waitErr,
		withParentRun(e.currentRunID))
	if waitErr != nil {
		e.log("apprentice failed: %s", waitErr)
		return fmt.Errorf("apprentice: %w", waitErr)
	}

	e.log("apprentice completed")

	// Apply the apprentice's submitted bundle onto staging, then merge.
	// A no-op signal short-circuits: nothing to merge.
	if stagingWt == nil {
		return nil
	}
	if e.deps.BundleStore != nil {
		outcome, err := e.applyApprenticeBundle(e.beadID, 0, stagingWt)
		if err != nil {
			return fmt.Errorf("apply apprentice bundle: %w", err)
		}
		if outcome.NoOp {
			return nil
		}
		if outcome.Applied {
			if mergeErr := stagingWt.MergeBranch(outcome.Branch, resolver); mergeErr != nil {
				return fmt.Errorf("merge %s into staging: %w", outcome.Branch, mergeErr)
			}
			return nil
		}
	}

	// Legacy fallback: assume the apprentice's feat branch is already
	// present locally and merge by branch name.
	featBranch := fmt.Sprintf("feat/%s", e.beadID)
	e.log("merging %s into staging (legacy path)", featBranch)
	if mergeErr := stagingWt.MergeBranch(featBranch, resolver); mergeErr != nil {
		return fmt.Errorf("merge %s into staging: %w", featBranch, mergeErr)
	}
	return nil
}
