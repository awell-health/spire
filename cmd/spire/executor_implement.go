package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// executeDirect spawns one apprentice for the bead.
func (e *formulaExecutor) executeDirect(phase string, pc PhaseConfig) error {
	apprenticeName := fmt.Sprintf("%s-impl", e.agentName)
	e.log("dispatching apprentice %s", apprenticeName)

	extraArgs := []string{}
	if pc.Apprentice {
		extraArgs = append(extraArgs, "--apprentice")
	}

	handle, err := e.spawner.Spawn(SpawnConfig{
		Name:      apprenticeName,
		BeadID:    e.beadID,
		Role:      RoleApprentice,
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
	return nil
}

// executeWave dispatches apprentices in parallel waves using computeWaves.
func (e *formulaExecutor) executeWave(phase string, pc PhaseConfig) error {
	waves, err := computeWaves(e.beadID)
	if err != nil {
		return err
	}
	if len(waves) == 0 {
		e.log("no open subtasks")
		return nil
	}

	e.log("computed %d wave(s)", len(waves))

	repoPath := e.state.RepoPath
	stagingBranch := e.state.StagingBranch

	// Create staging branch in a dedicated worktree — never checkout in main worktree.
	var stagingWt *StagingWorktree
	if stagingBranch != "" {
		e.log("creating staging branch %s", stagingBranch)
		// Create the branch from current HEAD before adding the worktree.
		exec.Command("git", "-C", repoPath, "branch", "-f", stagingBranch).Run()
		var wtErr error
		stagingWt, wtErr = NewStagingWorktree(repoPath, stagingBranch, e.state.BaseBranch, fmt.Sprintf("spire-staging-%s", e.beadID), e.log)
		if wtErr != nil {
			return fmt.Errorf("create staging worktree: %w", wtErr)
		}
		defer stagingWt.Close()
		storeAddLabel(e.beadID, "feat-branch:"+stagingBranch)
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
				storeUpdateBead(beadID, map[string]interface{}{"status": "in_progress"})

				extraArgs := []string{"--apprentice"}
				h, spawnErr := e.spawner.Spawn(SpawnConfig{
					Name:      name,
					BeadID:    beadID,
					Role:      RoleApprentice,
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
		// Mark successful subtasks as "done" (apprentice finished) but do NOT
		// close them yet — closing happens after merge + build verification.
		var errs []string
		for r := range resultCh {
			if r.Err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", r.BeadID, r.Err))
				continue
			}
			e.state.Subtasks[r.BeadID] = subtaskState{
				Status: "done",
				Branch: fmt.Sprintf("feat/%s", r.BeadID),
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
					return fmt.Errorf("merge %s into %s: %w", st.Branch, stagingBranch, mergeErr)
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
		// This ensures the bead graph stays accurate if merge or build fails.
		for _, subtaskID := range wave {
			st, ok := e.state.Subtasks[subtaskID]
			if !ok || st.Status != "done" {
				continue
			}
			if err := storeCloseBead(subtaskID); err != nil {
				e.log("warning: close subtask %s: %s", subtaskID, err)
			}
			st.Status = "closed"
			e.state.Subtasks[subtaskID] = st
		}
		e.saveState()
	}

	// No need to switch branches — staging work happened in its own worktree.
	// The main worktree stayed on the base branch the entire time.

	return nil
}

// resolveConflicts invokes Claude to resolve merge conflicts in the working tree.
func (e *formulaExecutor) resolveConflicts(repoPath, childBranch string) error {
	// Get the list of conflicted files
	diffCmd := exec.Command("git", "-C", repoPath, "diff", "--name-only", "--diff-filter=U")
	diffOut, err := diffCmd.Output()
	if err != nil {
		return fmt.Errorf("list conflicts: %w", err)
	}
	conflictedFiles := strings.TrimSpace(string(diffOut))
	if conflictedFiles == "" {
		return fmt.Errorf("no conflicted files found")
	}

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
	statusCmd := exec.Command("git", "-C", repoPath, "status", "--porcelain")
	statusOut, _ := statusCmd.Output()
	if strings.Contains(string(statusOut), "UU ") {
		return fmt.Errorf("conflicts still unresolved after Claude")
	}

	// Complete the merge
	commitCmd := exec.Command("git", "-C", repoPath, "commit", "--no-edit")
	if out, commitErr := commitCmd.CombinedOutput(); commitErr != nil {
		return fmt.Errorf("commit merge: %s\n%s", commitErr, string(out))
	}

	e.log("  conflicts resolved by Claude")
	return nil
}

// resolveBuildCommand returns the build command to use for verification.
// Resolution order:
//  1. Current phase's Build field
//  2. Implement phase's Build field (build is most commonly configured there)
//  3. Repo config runtime.build (spire.yaml)
//  4. Empty string (no build verification)
func (e *formulaExecutor) resolveBuildCommand(pc PhaseConfig) string {
	// 1. Current phase config
	if pc.Build != "" {
		return pc.Build
	}
	// 2. Implement phase fallback (build commands live here for wave-based formulas)
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
// Resolution order:
//  1. Current phase's Test field
//  2. Merge phase's Test field (test may be configured there)
//  3. Repo config runtime.test (spire.yaml)
//  4. Empty string (no test verification)
func (e *formulaExecutor) resolveTestCommand(pc PhaseConfig) string {
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
// The command is split on spaces and run directly (no shell).
func (e *formulaExecutor) runBuildCommand(repoPath, buildStr string) error {
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
