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
//
// After the initial waves complete, dispatchWaveCore re-queries the epic's
// children for any that were injected after the plan phase and have not yet
// been dispatched. Any such children are dispatched as additional waves until
// the set is empty. This closes the protocol gap described in spi-g4yi6j:
// before the fix, a `spire inject` after wave-1 could silently strand the new
// child because the wizard never re-scanned for ready children after wave
// completion.
func (e *Executor) dispatchWaveCore(waves [][]string, stagingWt *spgit.StagingWorktree, model string, resolver func(string, string) error, maxApprentices int) ([]childResult, error) {
	if maxApprentices <= 0 {
		maxApprentices = repoconfig.DefaultMaxApprentices
	}
	e.log("dispatching %d wave(s) (max %d concurrent apprentice(s))", len(waves), maxApprentices)

	// Cross-owner dispatch: the apprentice produces a delivery artifact
	// (bundle) that the parent merges into staging, OR takes the legacy
	// push-transport path that phase 5a quarantines as HandoffTransitional.
	// The selection is resolved once up front from the tower config so every
	// apprentice in every wave carries the same handoff mode.
	apprenticeHandoff := e.resolveApprenticeHandoff()

	var allResults []childResult
	startRef := ""

	// Track which bead IDs have been dispatched across all waves (including
	// re-scan waves). Pre-populate from the initial set so ComputeWaves can
	// be called with this as a skip-list.
	dispatched := make(map[string]bool)
	for _, w := range waves {
		for _, id := range w {
			dispatched[id] = true
		}
	}

	waveIdx := 0
	for _, wave := range waves {
		newStartRef, err := e.runDispatchWave(wave, waveIdx, startRef, stagingWt, model, resolver, maxApprentices, apprenticeHandoff, &allResults)
		if err != nil {
			return allResults, err
		}
		startRef = newStartRef
		waveIdx++
	}

	// Re-scan for late-injected children. An inject that arrives after the
	// initial wave set was computed (e.g. archmage runs `spire inject` while
	// wave-1 is mid-flight) writes a new subtask into the epic's children
	// list. Without a re-scan here the new child falls through dispatch and
	// the epic closes with it stranded (spi-g4yi6j).
	//
	// The re-scan depends on GetChildren + GetBlockedIssues being wired. In
	// production both are set on Deps; legacy test harnesses that bypass
	// actionDispatchChildren and call dispatchWaveCore directly sometimes
	// leave them nil — treat that as "no re-scan" so those tests still
	// exercise the wave-dispatch machinery without a panic in ComputeWaves.
	if e.deps == nil || e.deps.GetChildren == nil || e.deps.GetBlockedIssues == nil {
		return allResults, nil
	}
	for {
		newWaves, waveErr := ComputeWaves(e.beadID, e.deps)
		if waveErr != nil {
			e.log("wave re-scan: compute waves failed: %s", waveErr)
			break
		}
		// Flatten and filter out already-dispatched beads. ComputeWaves
		// returns non-closed subtasks; dispatched ones are typically
		// in_progress until the epic close step runs, so we must skip them
		// explicitly rather than relying on status.
		var batch []string
		for _, w := range newWaves {
			for _, id := range w {
				if !dispatched[id] {
					batch = append(batch, id)
				}
			}
		}
		if len(batch) == 0 {
			break
		}
		e.log("wave re-scan: %d late-injected child(ren) detected — dispatching as wave %d", len(batch), waveIdx+1)
		for _, id := range batch {
			dispatched[id] = true
		}
		newStartRef, err := e.runDispatchWave(batch, waveIdx, startRef, stagingWt, model, resolver, maxApprentices, apprenticeHandoff, &allResults)
		if err != nil {
			return allResults, err
		}
		startRef = newStartRef
		waveIdx++
	}

	return allResults, nil
}

// runDispatchWave dispatches a single wave: spawns apprentices (capped by
// maxApprentices), collects their results, and merges successful bundles into
// staging. It appends to *allResults and returns the updated startRef for the
// next wave. Extracted from dispatchWaveCore so the same single-wave logic is
// shared between initial waves and re-scan waves.
func (e *Executor) runDispatchWave(
	wave []string,
	waveIdx int,
	startRef string,
	stagingWt *spgit.StagingWorktree,
	model string,
	resolver func(string, string) error,
	maxApprentices int,
	apprenticeHandoff HandoffMode,
	allResults *[]childResult,
) (string, error) {
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
			cfg := agent.SpawnConfig{
				Name:          name,
				BeadID:        beadID,
				Role:          agent.RoleApprentice,
				ExtraArgs:     extraArgs,
				StartRef:      startRef,
				LogPath:       filepath.Join(dolt.GlobalDir(), "wizards", name+".log"),
				AttemptID:     e.attemptID(),
				ApprenticeIdx: "0",
			}
			cfg, contractErr := e.withRuntimeContract(cfg, e.runtimeTowerName(), e.effectiveRepoPath(), e.runtimeBaseBranch(), "implement", "", nil, apprenticeHandoff)
			if contractErr != nil {
				e.recordAgentRun(name, beadID, e.beadID, model, "apprentice", "implement", started, contractErr,
					withParentRun(e.currentRunID))
				resultCh <- waveResult{BeadID: beadID, Agent: name, Err: contractErr}
				return
			}
			h, spawnErr := e.deps.Spawner.Spawn(cfg)
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
		*allResults = append(*allResults, cr)
		if r.Err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", r.BeadID, r.Err))
		}
	}

	if len(errs) > 0 {
		e.log("wave %d: %d error(s): %s", waveIdx, len(errs), strings.Join(errs, "; "))
	}

	// Apply each successful apprentice's bundle into staging, then merge.
	// No-op signals skip merge entirely. The bundle is deleted only after
	// a successful merge so a conflict can be retried with the bundle
	// still present.
	if stagingWt != nil {
		for _, cr := range waveResults {
			if cr.Err != nil {
				continue
			}
			if e.deps.BundleStore != nil {
				outcome, err := e.applyApprenticeBundle(cr.BeadID, 0, stagingWt)
				if err != nil {
					return startRef, fmt.Errorf("apply apprentice bundle for %s: %w", cr.BeadID, err)
				}
				if outcome.NoOp {
					continue
				}
				if outcome.Applied {
					if mergeErr := stagingWt.MergeBranch(outcome.Branch, resolver); mergeErr != nil {
						return startRef, fmt.Errorf("merge %s into staging: %w", outcome.Branch, mergeErr)
					}
					e.deleteApprenticeBundle(cr.BeadID, outcome.Handle)
					continue
				}
			}
			// Push-transport / legacy fallback: fetch the apprentice's
			// feat branch from the remote (idempotent; cheap) then merge.
			if cr.Branch == "" {
				continue
			}
			if fetchErr := stagingWt.FetchBranch("origin", cr.Branch); fetchErr != nil {
				e.log("fetch %s (best-effort): %s", cr.Branch, fetchErr)
			}
			if mergeErr := stagingWt.MergeBranch(cr.Branch, resolver); mergeErr != nil {
				return startRef, fmt.Errorf("merge %s into staging: %w", cr.Branch, mergeErr)
			}
		}

		// Update startRef for next wave.
		if sha, err := stagingWt.HeadSHA(); err == nil && sha != "" {
			startRef = sha
			e.log("next wave start-ref: %s", startRef)
		}
	}

	return startRef, nil
}

// dispatchSequentialCore executes children one at a time, merging each into
// staging before advancing. It does NOT include inline review, inline
// merge-to-main, or subtask closing (those are separate formula steps).
func (e *Executor) dispatchSequentialCore(subtasks []string, stagingWt *spgit.StagingWorktree, model string, resolver func(string, string) error) ([]childResult, error) {
	e.log("sequential dispatch: %d subtask(s)", len(subtasks))

	// Same cross-owner dispatch handoff as the wave path — the tower's
	// configured apprentice transport decides bundle vs transitional.
	apprenticeHandoff := e.resolveApprenticeHandoff()

	var allResults []childResult
	var startRef string

	for i, subtaskID := range subtasks {
		e.log("=== sequential step %d/%d: %s ===", i+1, len(subtasks), subtaskID)
		e.deps.UpdateBead(subtaskID, map[string]interface{}{"status": "in_progress"})

		name := fmt.Sprintf("%s-seq-%d", e.agentName, i+1)
		started := time.Now()
		cfg := agent.SpawnConfig{
			Name:          name,
			BeadID:        subtaskID,
			Role:          agent.RoleApprentice,
			ExtraArgs:     []string{"--apprentice"},
			StartRef:      startRef,
			LogPath:       filepath.Join(dolt.GlobalDir(), "wizards", name+".log"),
			AttemptID:     e.attemptID(),
			ApprenticeIdx: "0",
		}
		cfg, contractErr := e.withRuntimeContract(cfg, e.runtimeTowerName(), e.effectiveRepoPath(), e.runtimeBaseBranch(), "implement", "", nil, apprenticeHandoff)
		if contractErr != nil {
			e.recordAgentRun(name, subtaskID, e.beadID, model, "apprentice", "implement", started, contractErr,
				withParentRun(e.currentRunID))
			return allResults, fmt.Errorf("handoff selection for %s: %w", subtaskID, contractErr)
		}
		handle, spawnErr := e.deps.Spawner.Spawn(cfg)
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

		// Apply the apprentice's bundle into staging, then merge. No-op
		// signals skip merge entirely. The bundle is deleted only after a
		// successful merge so a conflict can be retried with the bundle
		// still present.
		if stagingWt != nil {
			merged := false
			if e.deps.BundleStore != nil {
				outcome, err := e.applyApprenticeBundle(subtaskID, 0, stagingWt)
				if err != nil {
					return allResults, fmt.Errorf("apply apprentice bundle for %s: %w", subtaskID, err)
				}
				switch {
				case outcome.NoOp:
					merged = true // nothing to merge counts as success
				case outcome.Applied:
					if mergeErr := stagingWt.MergeBranch(outcome.Branch, resolver); mergeErr != nil {
						return allResults, fmt.Errorf("merge %s into staging: %w", outcome.Branch, mergeErr)
					}
					e.deleteApprenticeBundle(subtaskID, outcome.Handle)
					merged = true
				}
			}
			if !merged {
				// Push-transport / legacy fallback: fetch the apprentice's
				// feat branch from the remote (idempotent) then merge.
				if fetchErr := stagingWt.FetchBranch("origin", featBranch); fetchErr != nil {
					e.log("fetch %s (best-effort): %s", featBranch, fetchErr)
				}
				if mergeErr := stagingWt.MergeBranch(featBranch, resolver); mergeErr != nil {
					return allResults, fmt.Errorf("merge %s into staging: %w", featBranch, mergeErr)
				}
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

	// Same cross-owner dispatch handoff selection: bundle when the tower
	// configures it, otherwise the quarantined transitional push path.
	apprenticeHandoff := e.resolveApprenticeHandoff()

	started := time.Now()
	cfg := agent.SpawnConfig{
		Name:          apprenticeName,
		BeadID:        e.beadID,
		Role:          agent.RoleApprentice,
		ExtraArgs:     []string{"--apprentice"},
		LogPath:       filepath.Join(dolt.GlobalDir(), "wizards", apprenticeName+".log"),
		AttemptID:     e.attemptID(),
		ApprenticeIdx: "0",
	}
	cfg, contractErr := e.withRuntimeContract(cfg, e.runtimeTowerName(), e.effectiveRepoPath(), e.runtimeBaseBranch(), "implement", "", nil, apprenticeHandoff)
	if contractErr != nil {
		e.recordAgentRun(apprenticeName, e.beadID, "", model, "apprentice", "implement", started, contractErr,
			withParentRun(e.currentRunID))
		return fmt.Errorf("handoff selection: %w", contractErr)
	}
	handle, err := e.deps.Spawner.Spawn(cfg)
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
			e.deleteApprenticeBundle(e.beadID, outcome.Handle)
			return nil
		}
	}

	// Push-transport / legacy fallback: fetch the apprentice's feat branch
	// from the remote (idempotent; succeeds silently if already local) then
	// merge.
	featBranch := fmt.Sprintf("feat/%s", e.beadID)
	e.log("merging %s into staging (push/legacy path)", featBranch)
	if fetchErr := stagingWt.FetchBranch("origin", featBranch); fetchErr != nil {
		e.log("fetch %s (best-effort): %s", featBranch, fetchErr)
	}
	if mergeErr := stagingWt.MergeBranch(featBranch, resolver); mergeErr != nil {
		return fmt.Errorf("merge %s into staging: %w", featBranch, mergeErr)
	}
	return nil
}
