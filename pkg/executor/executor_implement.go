package executor

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/awell-health/spire/pkg/agent"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// executeDirect spawns one apprentice for the bead.
func (e *Executor) executeDirect(phase string, pc PhaseConfig) error {
	apprenticeName := fmt.Sprintf("%s-impl", e.agentName)
	e.log("dispatching apprentice %s", apprenticeName)

	extraArgs := []string{}
	if pc.Apprentice {
		extraArgs = append(extraArgs, "--apprentice")
	}

	handle, err := e.deps.Spawner.Spawn(agent.SpawnConfig{
		Name:      apprenticeName,
		BeadID:    e.beadID,
		Role:      agent.RoleApprentice,
		ExtraArgs: extraArgs,
	})
	if err != nil {
		return fmt.Errorf("spawn apprentice: %w", err)
	}

	if err := handle.Wait(); err != nil {
		e.log("apprentice failed: %s", err)
		return fmt.Errorf("apprentice: %w", err)
	}

	e.log("apprentice completed")

	// Merge the apprentice's feat branch into the staging worktree so that
	// downstream phases (review, merge) operate on the actual changes.
	// Without this, the staging branch stays at HEAD and the merge phase
	// would merge an empty branch into main, silently losing all work.
	if e.state.StagingBranch != "" {
		featBranch := fmt.Sprintf("feat/%s", e.beadID)
		e.log("merging %s into staging %s", featBranch, e.state.StagingBranch)
		stagingWt, wtErr := e.ensureStagingWorktree()
		if wtErr != nil {
			return fmt.Errorf("ensure staging worktree for direct merge: %w", wtErr)
		}
		if mergeErr := stagingWt.MergeBranch(featBranch, e.resolveConflicts); mergeErr != nil {
			return fmt.Errorf("merge %s into %s: %w", featBranch, e.state.StagingBranch, mergeErr)
		}
	}

	return nil
}

// executeWave dispatches apprentices in parallel waves using ComputeWaves.
func (e *Executor) executeWave(phase string, pc PhaseConfig) error {
	waves, err := ComputeWaves(e.beadID, e.deps)
	if err != nil {
		return err
	}
	if len(waves) == 0 {
		e.log("no open subtasks")
		return nil
	}

	e.log("computed %d wave(s)", len(waves))

	repoPath := e.state.RepoPath

	// Use the single staging worktree shared across the entire executor lifecycle.
	var stagingWt *spgit.StagingWorktree
	if e.state.StagingBranch != "" {
		var wtErr error
		stagingWt, wtErr = e.ensureStagingWorktree()
		if wtErr != nil {
			return fmt.Errorf("ensure staging worktree: %w", wtErr)
		}
		// Do NOT defer stagingWt.Close() — the worktree is shared across phases
		// and cleaned up by Run() on exit.
	}

	startWave := e.state.Wave
	for waveIdx := startWave; waveIdx < len(waves); waveIdx++ {
		wave := waves[waveIdx]
		e.state.Wave = waveIdx
		e.log("=== wave %d: %d subtask(s) ===", waveIdx, len(wave))

		type result struct {
			BeadID string
			Agent  string
			Err    error
		}

		var wg sync.WaitGroup
		resultCh := make(chan result, len(wave))

		for i, subtaskID := range wave {
			if st, ok := e.state.Subtasks[subtaskID]; ok && (st.Status == "closed" || st.Status == "done") {
				e.log("  %s already %s, skipping", subtaskID, st.Status)
				continue
			}

			wg.Add(1)
			go func(idx int, beadID string) {
				defer wg.Done()
				name := fmt.Sprintf("%s-w%d-%d", e.agentName, waveIdx, idx)
				e.log("  dispatching %s for %s", name, beadID)

				// Mark subtask as in_progress before dispatching
				e.deps.UpdateBead(beadID, map[string]interface{}{"status": "in_progress"})

				extraArgs := []string{"--apprentice"}
				h, spawnErr := e.deps.Spawner.Spawn(agent.SpawnConfig{
					Name:      name,
					BeadID:    beadID,
					Role:      agent.RoleApprentice,
					ExtraArgs: extraArgs,
				})
				if spawnErr != nil {
					resultCh <- result{BeadID: beadID, Agent: name, Err: spawnErr}
					return
				}
				if waitErr := h.Wait(); waitErr != nil {
					resultCh <- result{BeadID: beadID, Agent: name, Err: waitErr}
					return
				}
				resultCh <- result{BeadID: beadID, Agent: name}
			}(i, subtaskID)
		}

		wg.Wait()
		close(resultCh)

		// Collect results (single-threaded — no race).
		var errs []string
		for r := range resultCh {
			if r.Err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", r.BeadID, r.Err))
				continue
			}
			e.state.Subtasks[r.BeadID] = SubtaskState{
				Status: "done",
				Branch: e.resolveBranch(r.BeadID),
				Agent:  r.Agent,
			}
		}

		e.saveState()

		if len(errs) > 0 {
			e.log("wave %d: %d error(s): %s", waveIdx, len(errs), strings.Join(errs, "; "))
		}

		// Merge child branches into staging worktree
		if stagingWt != nil {
			for _, subtaskID := range wave {
				st, ok := e.state.Subtasks[subtaskID]
				if !ok || st.Status != "done" || st.Branch == "" {
					continue
				}
				if mergeErr := stagingWt.MergeBranch(st.Branch, e.resolveConflicts); mergeErr != nil {
					return fmt.Errorf("merge %s into %s: %w", st.Branch, e.state.StagingBranch, mergeErr)
				}
			}
		}

		// Verify build in the staging worktree (or main repo if no staging branch).
		if buildStr := e.resolveBuildCommand(pc); buildStr != "" {
			e.log("verifying build after wave %d: %s", waveIdx, buildStr)
			var buildErr error
			if stagingWt != nil {
				buildErr = stagingWt.RunBuild(buildStr)
			} else {
				buildErr = e.runBuildCommand(repoPath, buildStr)
			}
			if buildErr != nil {
				return fmt.Errorf("build verification failed after wave %d: %w", waveIdx, buildErr)
			}
		}

		// Close subtask beads AFTER successful merge and build verification.
		for _, subtaskID := range wave {
			st, ok := e.state.Subtasks[subtaskID]
			if !ok || st.Status != "done" {
				continue
			}
			if err := e.deps.CloseBead(subtaskID); err != nil {
				e.log("warning: close subtask %s: %s", subtaskID, err)
			}
			st.Status = "closed"
			e.state.Subtasks[subtaskID] = st
		}
		e.saveState()
	}

	return nil
}

// executeSequential dispatches one subtask at a time, merging each to staging,
// running an inline review and merge-to-main cycle before advancing to the next.
// Each subtask builds on the previous one's merged code — no merge conflicts.
// Ideal for serial extraction pipelines where each step depends on the previous.
func (e *Executor) executeSequential(phase string, pc PhaseConfig) error {
	waves, err := ComputeWaves(e.beadID, e.deps)
	if err != nil {
		return err
	}
	if len(waves) == 0 {
		e.log("no open subtasks")
		return nil
	}

	// Flatten waves into a single ordered list, preserving wave dependency order.
	var subtasks []string
	for _, wave := range waves {
		subtasks = append(subtasks, wave...)
	}

	e.log("sequential dispatch: %d subtask(s) across %d wave(s)", len(subtasks), len(waves))

	repoPath := e.state.RepoPath
	baseBranch := e.state.BaseBranch
	stagingBranch := e.state.StagingBranch

	for i, subtaskID := range subtasks {
		// Skip already completed subtasks (resume support).
		if st, ok := e.state.Subtasks[subtaskID]; ok && (st.Status == "closed" || st.Status == "done") {
			e.log("  %s already %s, skipping", subtaskID, st.Status)
			continue
		}

		e.log("=== sequential step %d/%d: %s ===", i+1, len(subtasks), subtaskID)
		e.deps.UpdateBead(subtaskID, map[string]interface{}{"status": "in_progress"})

		// 1. Dispatch one apprentice for this subtask.
		name := fmt.Sprintf("%s-seq-%d", e.agentName, i)
		handle, spawnErr := e.deps.Spawner.Spawn(agent.SpawnConfig{
			Name:      name,
			BeadID:    subtaskID,
			Role:      agent.RoleApprentice,
			ExtraArgs: []string{"--apprentice"},
		})
		if spawnErr != nil {
			return fmt.Errorf("spawn apprentice for %s: %w", subtaskID, spawnErr)
		}
		if waitErr := handle.Wait(); waitErr != nil {
			return fmt.Errorf("apprentice %s failed: %w", subtaskID, waitErr)
		}

		featBranch := e.resolveBranch(subtaskID)
		e.state.Subtasks[subtaskID] = SubtaskState{
			Status: "done",
			Branch: featBranch,
			Agent:  name,
		}
		e.saveState()

		// 2. Merge feat branch into staging.
		stagingWt, wtErr := e.ensureStagingWorktree()
		if wtErr != nil {
			return fmt.Errorf("ensure staging worktree: %w", wtErr)
		}
		if mergeErr := stagingWt.MergeBranch(featBranch, e.resolveConflicts); mergeErr != nil {
			return fmt.Errorf("merge %s into staging: %w", featBranch, mergeErr)
		}

		// 3. Build verification on staging.
		if buildStr := e.resolveBuildCommand(pc); buildStr != "" {
			e.log("verifying build for %s: %s", subtaskID, buildStr)
			if buildErr := stagingWt.RunBuild(buildStr); buildErr != nil {
				return fmt.Errorf("build verification failed for %s: %w", subtaskID, buildErr)
			}
		}

		// 4. Inline review (if the formula has a review phase).
		if reviewPC, ok := e.formula.Phases["review"]; ok {
			e.log("inline review for %s", subtaskID)
			if reviewErr := e.executeReview("review", reviewPC); reviewErr != nil {
				return fmt.Errorf("inline review for %s: %w", subtaskID, reviewErr)
			}
		}

		// 5. Inline merge: staging → main.
		mergeEnv := os.Environ()
		if tower, tErr := e.deps.ActiveTowerConfig(); tErr == nil && tower != nil {
			mergeEnv = e.deps.ArchmageGitEnv(tower)
		}

		buildStr := e.resolveBuildCommand(pc)
		testStr := e.resolveTestCommand(pc)
		e.log("merging staging → %s for %s", baseBranch, subtaskID)
		if mergeErr := stagingWt.MergeToMain(baseBranch, mergeEnv, buildStr, testStr); mergeErr != nil {
			return fmt.Errorf("merge staging → %s for %s: %w", baseBranch, subtaskID, mergeErr)
		}

		// Push main.
		rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: baseBranch}
		if pushErr := rc.Push("origin", baseBranch, mergeEnv); pushErr != nil {
			return fmt.Errorf("push %s after %s: %w", baseBranch, subtaskID, pushErr)
		}

		// 6. Close subtask bead.
		if closeErr := e.deps.CloseBead(subtaskID); closeErr != nil {
			e.log("warning: close subtask %s: %s", subtaskID, closeErr)
		}
		e.state.Subtasks[subtaskID] = SubtaskState{
			Status: "closed",
			Branch: featBranch,
			Agent:  name,
		}
		e.saveState()

		// 7. Reset staging to main for next subtask.
		// Close the current staging worktree, reset the staging branch to main,
		// so the next iteration creates a fresh staging from the updated main.
		e.closeStagingWorktree()
		rc.ForceBranch(stagingBranch, baseBranch)
		e.log("staging reset to %s — ready for next subtask", baseBranch)
	}

	return nil
}

// resolveConflicts invokes Claude to resolve merge conflicts in the working tree.
func (e *Executor) resolveConflicts(repoPath, childBranch string) error {
	wc := &spgit.WorktreeContext{Dir: repoPath}

	// Get the list of conflicted files
	conflicted, err := wc.ConflictedFiles()
	if err != nil {
		return fmt.Errorf("list conflicts: %w", err)
	}
	if len(conflicted) == 0 {
		return fmt.Errorf("no conflicted files found")
	}
	conflictedFiles := strings.Join(conflicted, "\n")

	// Build a prompt with the conflicts
	prompt := fmt.Sprintf(`You are resolving merge conflicts for branch %s being merged into the staging branch.

The following files have conflicts. For each file, read it, resolve the conflict markers (<<<<<<< ======= >>>>>>>), and write the resolved version. Keep both sides' changes where they don't contradict. When they do contradict, prefer the incoming branch (%s) since it has the newer implementation.

Conflicted files:
%s

After resolving all conflicts, stage them with git add.
Do NOT commit — the merge commit will be created automatically.`,
		childBranch, childBranch, conflictedFiles)

	// Invoke Claude to resolve
	cmd := exec.Command("claude",
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", "claude-sonnet-4-6",
		"--max-turns", "10",
	)
	cmd.Dir = repoPath
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude resolve: %w", err)
	}

	// Verify all conflicts are resolved (no more conflict markers)
	status := wc.StatusPorcelain()
	if strings.Contains(status, "UU ") {
		return fmt.Errorf("conflicts still unresolved after Claude")
	}

	// Complete the merge
	if commitErr := wc.CommitMerge(); commitErr != nil {
		return fmt.Errorf("commit merge: %w", commitErr)
	}

	e.log("  conflicts resolved by Claude")
	return nil
}

// resolveBuildCommand returns the build command to use for verification.
func (e *Executor) resolveBuildCommand(pc PhaseConfig) string {
	// 1. Current phase config
	if pc.Build != "" {
		return pc.Build
	}
	// 2. Implement phase fallback
	if impl, ok := e.formula.Phases["implement"]; ok && impl.Build != "" {
		return impl.Build
	}
	// 3. Repo config fallback
	if cfg, err := repoconfig.Load(e.state.RepoPath); err == nil && cfg.Runtime.Build != "" {
		return cfg.Runtime.Build
	}
	return ""
}

// resolveTestCommand returns the test command to use for verification.
func (e *Executor) resolveTestCommand(pc PhaseConfig) string {
	// 1. Current phase config
	if pc.Test != "" {
		return pc.Test
	}
	// 2. Merge phase fallback
	if merge, ok := e.formula.Phases["merge"]; ok && merge.Test != "" {
		return merge.Test
	}
	// 3. Repo config fallback
	if cfg, err := repoconfig.Load(e.state.RepoPath); err == nil && cfg.Runtime.Test != "" {
		return cfg.Runtime.Test
	}
	return ""
}

// runBuildCommand executes a build command string in the given repo directory.
func (e *Executor) runBuildCommand(repoPath, buildStr string) error {
	parts := strings.Fields(buildStr)
	if len(parts) == 0 {
		return nil
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = repoPath
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.log("build failed: %s\n%s", err, string(out))
		return fmt.Errorf("%s: %w\n%s", buildStr, err, string(out))
	}
	e.log("build passed")
	return nil
}

// ComputeWaves takes an epic ID and returns waves — groups of subtask IDs
// that can be executed in parallel. Wave 0 has no deps, wave 1 depends
// on wave 0, etc.
func ComputeWaves(epicID string, deps *Deps) ([][]string, error) {
	children, err := deps.GetChildren(epicID)
	if err != nil {
		return nil, fmt.Errorf("get children: %w", err)
	}

	// Filter to open subtasks only — exclude internal DAG beads.
	var openIDs []string
	for _, c := range children {
		if c.Status == "closed" {
			continue
		}
		if deps.IsAttemptBead(c) || deps.IsStepBead(c) || deps.IsReviewRoundBead(c) {
			continue
		}
		openIDs = append(openIDs, c.ID)
	}

	if len(openIDs) == 0 {
		return nil, nil
	}

	// Build a set of open child IDs for fast lookup.
	childSet := make(map[string]bool)
	for _, id := range openIDs {
		childSet[id] = true
	}

	// Get blocked issues to determine dependencies.
	blockedBeads, _ := deps.GetBlockedIssues(beads.WorkFilter{})

	// Build dep map: childID -> []blockerIDs (only blockers that are also open children).
	depMap := make(map[string][]string)
	for _, bb := range blockedBeads {
		if !childSet[bb.ID] {
			continue
		}
		for _, dep := range bb.Dependencies {
			blockerID := dep.DependsOnID
			if childSet[blockerID] {
				depMap[bb.ID] = append(depMap[bb.ID], blockerID)
			}
		}
	}

	// Topological sort into waves.
	assigned := make(map[string]int) // ID -> wave number
	var waves [][]string

	for len(assigned) < len(openIDs) {
		var wave []string
		waveNum := len(waves)

		for _, id := range openIDs {
			if _, done := assigned[id]; done {
				continue
			}
			ready := true
			for _, dep := range depMap[id] {
				if _, done := assigned[dep]; !done {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, id)
			}
		}

		if len(wave) == 0 {
			// Circular dependency or stuck — add remaining as final wave.
			for _, id := range openIDs {
				if _, done := assigned[id]; !done {
					wave = append(wave, id)
				}
			}
		}

		for _, id := range wave {
			assigned[id] = waveNum
		}
		waves = append(waves, wave)
	}

	return waves, nil
}
