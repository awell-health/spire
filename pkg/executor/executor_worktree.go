package executor

// executor_worktree.go — Executor staging worktree lifecycle.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/awell-health/spire/pkg/formula"
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

	// Check if the staging branch already exists with commits ahead of base.
	// This handles recovery after resummon/reset-to-merge where state.json and
	// the worktree directory were deleted but the staging branch survives.
	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: e.state.BaseBranch, Log: e.log}
	if rc.BranchExists(stagingBranch) {
		ahead, _ := rc.CommitsAhead(stagingBranch, e.state.BaseBranch)
		if ahead > 0 {
			e.log("recovering staging branch %s (%d commits ahead of %s)", stagingBranch, ahead, e.state.BaseBranch)
			// Prune stale worktree refs so git worktree add succeeds.
			rc.PruneWorktrees()
		} else {
			// Branch exists but at base — reset is harmless.
			rc.ForceBranch(stagingBranch, e.state.BaseBranch)
		}
	} else {
		// Branch doesn't exist — create from base.
		rc.ForceBranch(stagingBranch, e.state.BaseBranch)
	}

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

	if e.state.AttemptBeadID != "" {
		e.deps.AddLabel(e.state.AttemptBeadID, "worktree:"+wtDir)
	}
	e.deps.AddLabel(e.beadID, "feat-branch:"+stagingBranch)
	e.saveState()
	return wt, nil
}

// ensureGraphStagingWorktree creates or resumes the staging worktree for v3
// graph execution. Mirrors ensureStagingWorktree() but reads from GraphState
// instead of State (which is nil in v3 mode).
func (e *Executor) ensureGraphStagingWorktree(state *GraphState) (*spgit.StagingWorktree, error) {
	if e.stagingWt != nil {
		return e.stagingWt, nil
	}

	stagingBranch := state.StagingBranch
	if stagingBranch == "" {
		return nil, fmt.Errorf("no staging branch configured in graph state")
	}

	repoPath := state.RepoPath

	// Resume from persisted state if the worktree still exists on disk.
	if state.WorktreeDir != "" {
		if _, err := os.Stat(state.WorktreeDir); err == nil {
			e.log("resuming staging worktree at %s", state.WorktreeDir)
			e.stagingWt = spgit.ResumeStagingWorktree(repoPath, state.WorktreeDir, stagingBranch, state.BaseBranch, e.log)
			// Sync workspace state on resume too.
			for name, ws := range state.Workspaces {
				if ws.Kind == "staging" && (ws.Status == "pending" || ws.Dir == "") {
					ws.Status = "active"
					ws.Dir = state.WorktreeDir
					ws.Branch = stagingBranch
					ws.BaseBranch = state.BaseBranch
					ws.StartSHA = e.stagingWt.StartSHA
					state.Workspaces[name] = ws
					break
				}
			}
			// Route the save by the state's own AgentName, not e.agentName.
			// When this helper is called from an action running inside a nested
			// subgraph (e.g. dispatch-children in subgraph-implement), state is
			// the NESTED state while e.agentName is the PARENT executor's name.
			// Using e.agentName would write nested content to the parent's
			// graph_state.json path and clobber it (spi-dggw7p).
			state.Save(state.AgentName, e.deps.ConfigDir)
			return e.stagingWt, nil
		}
		e.log("stale worktree state %s — recreating", state.WorktreeDir)
		state.WorktreeDir = ""
	}

	// Check if the staging branch already exists with commits ahead of base.
	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: state.BaseBranch, Log: e.log}
	if rc.BranchExists(stagingBranch) {
		ahead, _ := rc.CommitsAhead(stagingBranch, state.BaseBranch)
		if ahead > 0 {
			e.log("recovering staging branch %s (%d commits ahead of %s)", stagingBranch, ahead, state.BaseBranch)
			rc.PruneWorktrees()
		} else {
			rc.ForceBranch(stagingBranch, state.BaseBranch)
		}
	} else {
		rc.ForceBranch(stagingBranch, state.BaseBranch)
	}

	wtDir := filepath.Join(repoPath, ".worktrees", e.beadID)
	for _, ws := range state.Workspaces {
		if ws.Kind == formula.WorkspaceKindStaging && ws.Dir != "" {
			wtDir = ws.Dir
			break
		}
	}
	archName, archEmail := ArchmageIdentity(e.deps)

	e.log("creating staging worktree at %s (branch: %s)", wtDir, stagingBranch)
	wt, err := spgit.NewStagingWorktreeAt(repoPath, wtDir, stagingBranch, state.BaseBranch, archName, archEmail, e.log)
	if err != nil {
		return nil, fmt.Errorf("create staging worktree: %w", err)
	}

	e.stagingWt = wt
	state.WorktreeDir = wtDir

	// Sync the workspace state so resolveGraphWorkspace finds this worktree
	// as "active" instead of trying to recreate it.
	for name, ws := range state.Workspaces {
		if ws.Kind == "staging" && (ws.Status == "pending" || ws.Dir == "" || ws.Branch == "" || ws.BaseBranch == "") {
			ws.Status = "active"
			ws.Dir = wtDir
			ws.Branch = stagingBranch
			ws.BaseBranch = state.BaseBranch
			ws.StartSHA = wt.StartSHA
			state.Workspaces[name] = ws
			break
		}
	}

	if state.AttemptBeadID != "" {
		e.deps.AddLabel(state.AttemptBeadID, "worktree:"+wtDir)
	}
	e.deps.AddLabel(e.beadID, "feat-branch:"+stagingBranch)
	// See comment on the sibling save above: route by state.AgentName so a
	// nested-state caller doesn't clobber the parent's graph_state.json.
	state.Save(state.AgentName, e.deps.ConfigDir)
	return wt, nil
}

// closeStagingWorktree removes the staging worktree and cleans up state.
func (e *Executor) closeStagingWorktree() {
	if e.stagingWt != nil {
		e.log("removing staging worktree at %s", e.stagingWt.Dir)
		e.stagingWt.Close()
		e.stagingWt = nil
	}
	if e.state != nil {
		e.state.WorktreeDir = ""
	}
	if e.graphState != nil {
		e.graphState.WorktreeDir = ""
	}
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
