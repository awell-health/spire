package executor

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// executeDirect spawns one apprentice for the bead. V2 compat wrapper around
// dispatchDirectCore.
func (e *Executor) executeDirect(phase string, pc PhaseConfig) error {
	var stagingWt *spgit.StagingWorktree
	if e.state.StagingBranch != "" {
		var wtErr error
		stagingWt, wtErr = e.ensureStagingWorktree()
		if wtErr != nil {
			return fmt.Errorf("ensure staging worktree for direct: %w", wtErr)
		}
	}
	return e.dispatchDirectCore(stagingWt, pc.Model)
}

// executeWave dispatches apprentices in parallel waves using ComputeWaves.
// V2 compat wrapper that delegates to dispatchWaveCore and adds build verification,
// build-fix retry, and subtask closing.
func (e *Executor) executeWave(phase string, pc PhaseConfig) error {
	waves, err := ComputeWaves(e.beadID, e.deps)
	if err != nil {
		EscalateHumanFailure(e.beadID, e.agentName, "invalid-plan-cycle", err.Error(), e.deps)
		return fmt.Errorf("implement phase aborted: %w", err)
	}
	if len(waves) == 0 {
		e.log("no open subtasks")
		return nil
	}

	repoPath := e.state.RepoPath

	var stagingWt *spgit.StagingWorktree
	if e.state.StagingBranch != "" {
		var wtErr error
		stagingWt, wtErr = e.ensureStagingWorktree()
		if wtErr != nil {
			return fmt.Errorf("ensure staging worktree: %w", wtErr)
		}
	}

	// Dispatch all waves via extracted core logic.
	results, dispErr := e.dispatchWaveCore(waves, stagingWt, pc.Model)

	// Update v2 subtask state from results.
	for _, cr := range results {
		if cr.Err != nil {
			continue
		}
		e.state.Subtasks[cr.BeadID] = SubtaskState{
			Status: "done",
			Branch: cr.Branch,
			Agent:  cr.Agent,
		}
	}
	e.saveState()

	if dispErr != nil {
		return dispErr
	}

	// V2: build verification + build-fix retry (stays for v2 formulas).
	if buildStr := e.resolveBuildCommand(pc); buildStr != "" {
		e.log("verifying build after dispatch: %s", buildStr)
		var buildErr error
		if stagingWt != nil {
			buildErr = stagingWt.RunBuild(buildStr)
		} else {
			buildErr = e.runBuildCommand(repoPath, buildStr)
		}
		if buildErr != nil {
			fixErr := e.attemptBuildFix(e.state.Wave, buildErr, pc)
			if fixErr != nil {
				EscalateHumanFailure(e.beadID, e.agentName, "build-failure",
					fmt.Sprintf("build fix failed after retries: %s", fixErr), e.deps)
				return fmt.Errorf("build verification failed (fix exhausted): %w", fixErr)
			}
		}
	}

	// Close subtask beads AFTER successful merge and build verification.
	for _, cr := range results {
		if cr.Err != nil {
			continue
		}
		if closeErr := e.deps.CloseBead(cr.BeadID); closeErr != nil {
			e.log("warning: close subtask %s: %s", cr.BeadID, closeErr)
		}
		if st, ok := e.state.Subtasks[cr.BeadID]; ok {
			st.Status = "closed"
			e.state.Subtasks[cr.BeadID] = st
		}
	}
	e.saveState()

	return nil
}

// executeSequential dispatches one subtask at a time, merging each to staging,
// running an inline review and merge-to-main cycle before advancing to the next.
// V2 compat wrapper that delegates to per-subtask dispatch and adds inline
// review, merge-to-main, and subtask closing.
func (e *Executor) executeSequential(phase string, pc PhaseConfig) error {
	waves, err := ComputeWaves(e.beadID, e.deps)
	if err != nil {
		EscalateHumanFailure(e.beadID, e.agentName, "invalid-plan-cycle", err.Error(), e.deps)
		return fmt.Errorf("implement phase aborted: %w", err)
	}
	if len(waves) == 0 {
		e.log("no open subtasks")
		return nil
	}

	// Flatten waves into a single ordered list.
	var subtasks []string
	for _, wave := range waves {
		subtasks = append(subtasks, wave...)
	}

	e.log("sequential dispatch: %d subtask(s) across %d wave(s)", len(subtasks), len(waves))

	repoPath := e.state.RepoPath
	baseBranch := e.state.BaseBranch
	stagingBranch := e.state.StagingBranch
	// startRef tracks the integration ref for child worktrees. Empty for step 0
	// (children start from the repo base branch). After each step's merge-to-main
	// and push, updated to the base branch HEAD so the next child starts from the
	// current integrated code.
	var startRef string

	for i, subtaskID := range subtasks {
		if st, ok := e.state.Subtasks[subtaskID]; ok && (st.Status == "closed" || st.Status == "done") {
			e.log("  %s already %s, skipping", subtaskID, st.Status)
			continue
		}

		e.log("=== sequential step %d/%d: %s ===", i+1, len(subtasks), subtaskID)
		e.deps.UpdateBead(subtaskID, map[string]interface{}{"status": "in_progress"})

		// 1. Dispatch one apprentice.
		name := fmt.Sprintf("%s-seq-%d", e.agentName, i)
		started := time.Now()
		handle, spawnErr := e.deps.Spawner.Spawn(agent.SpawnConfig{
			Name:      name,
			BeadID:    subtaskID,
			Role:      agent.RoleApprentice,
			ExtraArgs: []string{"--apprentice"},
			StartRef:  startRef,
		})
		if spawnErr != nil {
			return fmt.Errorf("spawn apprentice for %s: %w", subtaskID, spawnErr)
		}
		waitErr := handle.Wait()
		e.recordAgentRun(name, subtaskID, e.beadID, pc.Model, "apprentice", "implement", started, waitErr)
		if waitErr != nil {
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

		// 3. Build verification.
		if buildStr := e.resolveBuildCommand(pc); buildStr != "" {
			e.log("verifying build for %s: %s", subtaskID, buildStr)
			if buildErr := stagingWt.RunBuild(buildStr); buildErr != nil {
				fixErr := e.attemptBuildFix(i, buildErr, pc)
				if fixErr != nil {
					EscalateHumanFailure(e.beadID, e.agentName, "build-failure",
						fmt.Sprintf("sequential step %s build fix failed after retries: %s", subtaskID, fixErr), e.deps)
					return fmt.Errorf("build verification failed for %s (fix exhausted): %w", subtaskID, fixErr)
				}
			}
		}

		// 4. Inline review.
		if reviewPC, ok := e.formula.Phases["review"]; ok {
			e.log("inline review for %s", subtaskID)
			if _, reviewErr := e.executeReview("review", reviewPC); reviewErr != nil {
				return fmt.Errorf("inline review for %s: %w", subtaskID, reviewErr)
			}
		}

		// 5. Inline merge: staging -> main.
		mergeEnv := os.Environ()
		if tower, tErr := e.deps.ActiveTowerConfig(); tErr == nil && tower != nil {
			mergeEnv = e.deps.ArchmageGitEnv(tower)
		}

		buildStr := e.resolveBuildCommand(pc)
		testStr := e.resolveTestCommand(pc)
		e.log("merging staging -> %s for %s", baseBranch, subtaskID)
		if mergeErr := stagingWt.MergeToMain(baseBranch, mergeEnv, buildStr, testStr); mergeErr != nil {
			return fmt.Errorf("merge staging -> %s for %s: %w", baseBranch, subtaskID, mergeErr)
		}

		rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: baseBranch, Log: e.log}
		if pushErr := rc.Push("origin", baseBranch, mergeEnv); pushErr != nil {
			return fmt.Errorf("push %s after %s: %w", baseBranch, subtaskID, pushErr)
		}

		startRef = rc.HeadSHA()
		if startRef != "" {
			e.log("next sequential start-ref: %s", startRef)
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

		// 7. Reset staging for next subtask.
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
		"--model", repoconfig.ResolveModel("", e.repoModel()),
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

// attemptBuildFix dispatches a build-fix apprentice in the staging worktree to
// fix compilation errors after a wave merge. Retries up to MaxBuildFixRounds
// (default 2). Returns nil on success, error if all attempts are exhausted.
//
// The fix apprentice works directly in the staging worktree (not on a separate
// branch) because the build error only manifests in the combined merged state.
func (e *Executor) attemptBuildFix(waveIdx int, buildErr error, pc PhaseConfig) error {
	// Check the build-failure policy from formula config.
	policy := pc.GetOnBuildFailure()
	switch policy {
	case "escalate":
		EscalateHumanFailure(e.beadID, e.agentName, "build-failure",
			fmt.Sprintf("build failed (on_build_failure=escalate): %s", buildErr), e.deps)
		return fmt.Errorf("build failed (on_build_failure=escalate): %w", buildErr)
	case "fail":
		return fmt.Errorf("build failed (on_build_failure=fail): %w", buildErr)
	case "retry":
		// fall through to existing retry loop
	default:
		log.Printf("WARN: unknown on_build_failure policy %q, defaulting to retry", policy)
	}

	maxRounds := pc.GetMaxBuildFixRounds()
	buildErrMsg := buildErr.Error()

	for round := 1; round <= maxRounds; round++ {
		e.state.BuildFixRounds++
		e.log("build-fix round %d/%d for wave %d", round, maxRounds, waveIdx)

		// Write build error to a temp file in the staging worktree for the apprentice.
		wtDir := e.state.WorktreeDir
		if wtDir == "" {
			return fmt.Errorf("no staging worktree dir for build-fix")
		}
		errFile := filepath.Join(wtDir, ".build-error.log")
		if writeErr := os.WriteFile(errFile, []byte(buildErrMsg), 0644); writeErr != nil {
			e.log("warning: write build error file: %s", writeErr)
		}

		// Also add a comment on the bead for audit trail.
		e.deps.AddComment(e.beadID, fmt.Sprintf(
			"Build fix round %d/%d (wave %d):\n```\n%s\n```",
			round, maxRounds, waveIdx, buildErrMsg,
		))

		// Spawn a build-fix apprentice in the staging worktree.
		fixName := fmt.Sprintf("%s-buildfix-%d-%d", e.agentName, waveIdx, round)
		extraArgs := []string{"--build-fix", "--apprentice", "--worktree-dir", wtDir}

		started := time.Now()
		handle, spawnErr := e.deps.Spawner.Spawn(agent.SpawnConfig{
			Name:      fixName,
			BeadID:    e.beadID,
			Role:      agent.RoleApprentice,
			ExtraArgs: extraArgs,
		})
		if spawnErr != nil {
			e.recordAgentRun(fixName, e.beadID, e.beadID, pc.Model, "apprentice", "build-fix", started, spawnErr)
			return fmt.Errorf("spawn build-fix apprentice (round %d): %w", round, spawnErr)
		}
		waitErr := handle.Wait()
		e.recordAgentRun(fixName, e.beadID, e.beadID, pc.Model, "apprentice", "build-fix", started, waitErr)
		if waitErr != nil {
			e.log("build-fix apprentice failed (round %d): %s", round, waitErr)
		}

		// Clean up the error file.
		os.Remove(errFile)

		// Re-verify the build in the staging worktree.
		buildStr := e.resolveBuildCommand(pc)
		if buildStr == "" {
			return nil // no build command means nothing to verify
		}

		e.log("re-verifying build after fix round %d: %s", round, buildStr)
		var reBuildErr error
		if e.stagingWt != nil {
			reBuildErr = e.stagingWt.RunBuild(buildStr)
		} else {
			reBuildErr = e.runBuildCommand(e.state.RepoPath, buildStr)
		}
		if reBuildErr == nil {
			e.log("build-fix succeeded on round %d", round)
			e.saveState()
			return nil
		}

		// Update buildErrMsg for next round.
		buildErrMsg = reBuildErr.Error()
		e.log("build still failing after fix round %d: %s", round, buildErrMsg)
		e.saveState()
	}

	return fmt.Errorf("build fix exhausted after %d rounds: %s", maxRounds, buildErrMsg)
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
			// Dependency cycle detected — fail closed.
			var stuck []string
			for _, id := range openIDs {
				if _, done := assigned[id]; !done {
					stuck = append(stuck, id)
				}
			}
			return nil, fmt.Errorf("dependency cycle detected among beads: %v", stuck)
		}

		for _, id := range wave {
			assigned[id] = waveNum
		}
		waves = append(waves, wave)
	}

	return waves, nil
}
