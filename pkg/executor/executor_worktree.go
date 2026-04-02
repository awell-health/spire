package executor

// executor_worktree.go — Executor staging worktree lifecycle.

import (
	"fmt"
	"os"
	"path/filepath"

	spgit "github.com/awell-health/spire/pkg/git"
)

// ensureStagingWorktree creates or resumes the single staging worktree for the
// entire executor lifecycle. Created once, shared across all phases (implement,
// review, merge). The main worktree NEVER leaves the base branch.
func (e *Executor) ensureStagingWorktree() (*spgit.StagingWorktree, error) {
	if e.stagingWt != nil {
		return e.stagingWt, nil
	}

	stagingBranch := e.state.StagingBranch
	if stagingBranch == "" {
		return nil, fmt.Errorf("no staging branch configured")
	}

	repoPath := e.state.RepoPath

	// Resume from persisted state if the worktree still exists on disk.
	if e.state.WorktreeDir != "" {
		if _, err := os.Stat(e.state.WorktreeDir); err == nil {
			e.log("resuming staging worktree at %s", e.state.WorktreeDir)
			e.stagingWt = spgit.ResumeStagingWorktree(repoPath, e.state.WorktreeDir, stagingBranch, e.state.BaseBranch, e.log)
			return e.stagingWt, nil
		}
		e.log("stale worktree state %s — recreating", e.state.WorktreeDir)
		e.state.WorktreeDir = ""
	}

	// Create the staging branch from the base branch (not HEAD, which may differ).
	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: e.state.BaseBranch, Log: e.log}
	rc.ForceBranch(stagingBranch, e.state.BaseBranch)

	// Worktree dir: .worktrees/<bead-id> — traceable to the bead.
	wtDir := filepath.Join(repoPath, ".worktrees", e.beadID)

	// Resolve archmage identity for git user config.
	archName, archEmail := ArchmageIdentity(e.deps)

	e.log("creating staging worktree at %s (branch: %s)", wtDir, stagingBranch)
	wt, err := spgit.NewStagingWorktreeAt(repoPath, wtDir, stagingBranch, e.state.BaseBranch, archName, archEmail, e.log)
	if err != nil {
		return nil, fmt.Errorf("create staging worktree: %w", err)
	}

	e.stagingWt = wt
	e.state.WorktreeDir = wtDir

	// Install dependencies in the fresh worktree (mirrors wizard.go behavior).
	// Skipped on resume — dependencies are already installed.
	if installStr := e.resolveInstallCommand(); installStr != "" {
		e.log("installing dependencies in staging: %s", installStr)
		if err := wt.RunInstall(installStr); err != nil {
			e.log("warning: staging install failed: %s", err)
		}
	}

	if e.state.AttemptBeadID != "" {
		e.deps.AddLabel(e.state.AttemptBeadID, "worktree:"+wtDir)
	}
	e.deps.AddLabel(e.beadID, "feat-branch:"+stagingBranch)
	e.saveState()
	return wt, nil
}

// closeStagingWorktree removes the staging worktree and cleans up state.
func (e *Executor) closeStagingWorktree() {
	if e.stagingWt != nil {
		e.log("removing staging worktree at %s", e.stagingWt.Dir)
		e.stagingWt.Close()
		e.stagingWt = nil
	}
	e.state.WorktreeDir = ""
}

// ArchmageIdentity returns the git user name and email from the active tower
// config, falling back to defaults if unavailable.
func ArchmageIdentity(deps *Deps) (name, email string) {
	name, email = "spire", "spire@spire.local"
	if tower, tErr := deps.ActiveTowerConfig(); tErr == nil && tower != nil {
		if tower.Archmage.Name != "" {
			name = tower.Archmage.Name
		}
		if tower.Archmage.Email != "" {
			email = tower.Archmage.Email
		}
	}
	return
}
