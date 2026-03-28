package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// StagingWorktree manages a temporary git worktree for staging operations.
// It is the single point responsible for git worktree create/remove and
// main-worktree branch switching, ensuring the main worktree stays on its
// base branch throughout all staging work.
//
// It embeds WorktreeContext for all git operations, ensuring worktree/main-repo
// boundary is enforced through a single abstraction.
type StagingWorktree struct {
	WorktreeContext        // embedded — all git ops go through this
	tmpDir          string // temp directory parent (cleaned up on Close)
	log             func(string, ...interface{})
}

// NewStagingWorktree creates a new temporary git worktree checking out branch.
// baseBranch is the branch this was forked from (e.g. "main") — stored in the
// embedded WorktreeContext so methods like HasNewCommits work correctly.
// nameHint is included in the temp directory name for debugging (e.g. "spire-staging").
// The caller must call Close() when done.
func NewStagingWorktree(repoPath, branch, baseBranch, nameHint string, log func(string, ...interface{})) (*StagingWorktree, error) {
	tmpDir, err := os.MkdirTemp("", nameHint+"-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	dir := filepath.Join(tmpDir, "wt")
	rc := &RepoContext{Dir: repoPath, BaseBranch: baseBranch}
	wcPtr, wtErr := rc.CreateWorktree(dir, branch)
	if wtErr != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("worktree add %s: %w", branch, wtErr)
	}
	wc := *wcPtr

	// Configure git user in the staging worktree to the archmage identity so
	// commits from conflict resolution or doc review are attributed correctly.
	// Uses WorktreeContext.ConfigureUser which scopes settings with --worktree
	// so they don't pollute the main repo's config.
	archName, archEmail := "spire", "spire@spire.local" // fallback
	if tower, tErr := activeTowerConfig(); tErr == nil && tower != nil {
		if tower.Archmage.Name != "" {
			archName = tower.Archmage.Name
		}
		if tower.Archmage.Email != "" {
			archEmail = tower.Archmage.Email
		}
	}
	wc.ConfigureUser(archName, archEmail)

	return &StagingWorktree{
		WorktreeContext: wc,
		tmpDir:         tmpDir,
		log:            log,
	}, nil
}

// FetchBranch fetches a specific branch from a remote into this staging worktree.
// Fetch operations live on StagingWorktree (not WorktreeContext) because
// WorktreeContext enforces local-ref-only semantics.
func (w *StagingWorktree) FetchBranch(remote, branch string) {
	exec.Command("git", "-C", w.Dir, "fetch", remote, branch).Run()
}

// MergeBranch merges childBranch into this staging worktree's branch.
// It fetches from origin first (since the apprentice may have pushed to remote),
// then tries origin/childBranch, falling back to a local branch ref.
// On merge conflict, resolver is called (if non-nil) to attempt resolution.
//
// Uses WorktreeContext.Merge/MergeAbort/StatusPorcelain for all in-worktree
// git operations — FetchBranch is the only StagingWorktree-specific escape
// hatch (WorktreeContext forbids fetch by design).
func (w *StagingWorktree) MergeBranch(childBranch string, resolver func(dir, branch string) error) error {
	w.log("  merging %s into %s", childBranch, w.Branch)

	// Fetch in case the apprentice pushed to remote.
	// FetchBranch lives on StagingWorktree because WorktreeContext enforces local-ref-only semantics.
	w.FetchBranch("origin", childBranch)

	// Try remote branch first, fall back to local.
	branchRef := "origin/" + childBranch
	if _, mergeErr := w.Merge(branchRef); mergeErr != nil {
		branchRef = childBranch
		if _, mergeErr2 := w.Merge(branchRef); mergeErr2 != nil {
			// Check if git is in a conflict state.
			status := w.StatusPorcelain()
			if strings.Contains(status, "UU ") || strings.Contains(status, "AA ") {
				if resolver != nil {
					if resolveErr := resolver(w.Dir, childBranch); resolveErr != nil {
						w.MergeAbort()
						return fmt.Errorf("conflict resolution failed: %w", resolveErr)
					}
					return nil
				}
				w.MergeAbort()
				return fmt.Errorf("merge conflict in %s: no resolver provided", childBranch)
			}
			// Not a conflict — some other merge error.
			w.MergeAbort()
			return fmt.Errorf("merge failed: %w", mergeErr2)
		}
	}
	return nil
}

// RunBuild runs buildStr as a command in the worktree directory.
// buildStr is split on spaces and run directly (no shell).
func (w *StagingWorktree) RunBuild(buildStr string) error {
	out, err := w.RunCommandOutput(buildStr)
	if err != nil {
		w.log("build failed: %s\n%s", err, out)
		return fmt.Errorf("%s: %w\n%s", buildStr, err, out)
	}
	w.log("build passed")
	return nil
}

// RunTests runs testStr as a command in the worktree directory.
// testStr is split on spaces and run directly (no shell).
func (w *StagingWorktree) RunTests(testStr string) error {
	out, err := w.RunCommandOutput(testStr)
	if err != nil {
		w.log("tests failed: %s\n%s", err, out)
		return fmt.Errorf("%s: %w\n%s", testStr, err, out)
	}
	w.log("tests passed")
	return nil
}

// MergeToMain ensures the main worktree is on baseBranch, pulls it, and
// performs a ff-only merge of this staging branch into baseBranch.
// env is used for git operations that need identity (e.g. archmage git env).
//
// If ff-only fails (main has diverged), it rebases the staging branch onto
// baseBranch in a new temporary worktree. After a successful rebase, it
// verifies build (buildStr) and tests (testStr) in that worktree — empty
// strings skip the respective step — then retries the ff-only merge.
// Never force-merges; returns an error if rebase fails.
//
// NOTE: Main-repo operations (checkout, pull, merge, worktree lifecycle) go
// through RepoContext. The rebase operations target a temporary worktree and
// remain as raw exec.Command calls since WorktreeContext doesn't expose rebase.
func (w *StagingWorktree) MergeToMain(baseBranch string, env []string, buildStr, testStr string) error {
	rc := &RepoContext{Dir: w.RepoPath, BaseBranch: baseBranch}

	// Ensure main worktree is on baseBranch.
	if rc.CurrentBranch() != baseBranch {
		if err := rc.Checkout(baseBranch); err != nil {
			return err
		}
	}

	// Pull baseBranch to be up to date.
	if pullErr := rc.PullFFOnly("origin", baseBranch, env); pullErr != nil {
		w.log("warning: pull %s: %s", baseBranch, pullErr)
	}

	// Belt-and-suspenders: verify we're still on baseBranch after the pull.
	if rc.CurrentBranch() != baseBranch {
		if err := rc.Checkout(baseBranch); err != nil {
			return err
		}
	}

	w.log("ff-only merge %s → %s (committer: archmage)", w.Branch, baseBranch)

	// First attempt: fast-forward only merge.
	if err := rc.MergeFFOnly(w.Branch, env); err == nil {
		return nil // success — done
	} else {
		w.log("ff-only failed: %s — rebasing staging onto %s", err, baseBranch)
	}

	// ff-only failed — main has diverged. Rebase staging onto main in a
	// temporary worktree so we don't disturb the main worktree's checkout.
	rebaseTmp, err := os.MkdirTemp("", "spire-rebase-")
	if err != nil {
		return fmt.Errorf("create rebase temp dir: %w", err)
	}
	defer os.RemoveAll(rebaseTmp)

	rebaseWtPath := filepath.Join(rebaseTmp, "staging")
	rebaseWc, wtErr := rc.CreateWorktree(rebaseWtPath, w.Branch)
	if wtErr != nil {
		return fmt.Errorf("create rebase worktree: %w", wtErr)
	}
	defer rc.ForceRemoveWorktree(rebaseWtPath)

	// Rebase the staging branch onto main (raw exec — WorktreeContext doesn't expose rebase).
	w.log("rebasing %s onto %s in staging worktree", w.Branch, baseBranch)
	rebaseCmd := exec.Command("git", "-C", rebaseWtPath, "rebase", baseBranch)
	rebaseCmd.Env = os.Environ()
	if out, rbErr := rebaseCmd.CombinedOutput(); rbErr != nil {
		exec.Command("git", "-C", rebaseWtPath, "rebase", "--abort").Run()
		return fmt.Errorf("rebase %s onto %s failed (aborting, will not force merge): %s\n%s", w.Branch, baseBranch, rbErr, string(out))
	}

	// Re-verify build in the staging worktree after rebase.
	if buildStr != "" {
		w.log("verifying build after rebase")
		out, buildErr := rebaseWc.RunCommandOutput(buildStr)
		if buildErr != nil {
			return fmt.Errorf("build failed after rebase (aborting merge): %s\n%s", buildErr, out)
		}
	}

	// Run tests after rebase.
	if testStr != "" {
		w.log("running tests after rebase")
		out, testErr := rebaseWc.RunCommandOutput(testStr)
		if testErr != nil {
			return fmt.Errorf("tests failed after rebase (aborting merge): %s\n%s", testErr, out)
		}
	}

	// Remove the rebase worktree before retrying the merge (the branch ref is
	// already updated by the rebase — the worktree just holds a checkout).
	rc.ForceRemoveWorktree(rebaseWtPath)

	// Second attempt: ff-only should now succeed since staging was rebased.
	w.log("retrying ff-only merge after rebase")
	if err := rc.MergeFFOnly(w.Branch, env); err != nil {
		return fmt.Errorf("ff-only merge failed even after rebase (will not force merge): %w", err)
	}
	return nil
}

// NewStagingWorktreeAt creates a staging worktree at a specific directory path.
// Unlike NewStagingWorktree (which creates a temp dir), this places the worktree
// at dir (e.g. .worktrees/<bead-id>), making it discoverable by all participants
// in the epic lifecycle (wizard, sage, fix apprentice).
//
// The caller must call Close() when done. Close removes the git worktree and
// the directory itself.
func NewStagingWorktreeAt(repoPath, dir, branch, baseBranch string, log func(string, ...interface{})) (*StagingWorktree, error) {
	rc := &RepoContext{Dir: repoPath, BaseBranch: baseBranch}

	// Clean up stale worktree at this path
	if _, err := os.Stat(dir); err == nil {
		rc.ForceRemoveWorktree(dir)
		os.RemoveAll(dir)
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return nil, fmt.Errorf("create parent dir: %w", err)
	}

	wcPtr, wtErr := rc.CreateWorktree(dir, branch)
	if wtErr != nil {
		return nil, fmt.Errorf("worktree add %s at %s: %w", branch, dir, wtErr)
	}
	wc := *wcPtr

	// Configure git user in the staging worktree to the archmage identity.
	archName, archEmail := "spire", "spire@spire.local" // fallback
	if tower, tErr := activeTowerConfig(); tErr == nil && tower != nil {
		if tower.Archmage.Name != "" {
			archName = tower.Archmage.Name
		}
		if tower.Archmage.Email != "" {
			archEmail = tower.Archmage.Email
		}
	}
	wc.ConfigureUser(archName, archEmail)

	return &StagingWorktree{
		WorktreeContext: wc,
		// tmpDir is empty — no temp dir to clean up. Close() handles
		// git worktree removal; the dir itself is removed by git.
		log: log,
	}, nil
}

// ResumeStagingWorktree wraps an existing worktree directory in a StagingWorktree.
// Used when resuming from persisted executor state — the worktree already exists
// on disk and just needs to be wrapped for method access.
func ResumeStagingWorktree(repoPath, dir, branch, baseBranch string, log func(string, ...interface{})) *StagingWorktree {
	return &StagingWorktree{
		WorktreeContext: WorktreeContext{
			Dir:        dir,
			Branch:     branch,
			BaseBranch: baseBranch,
			RepoPath:   repoPath,
		},
		log: log,
	}
}

// --- Executor staging worktree lifecycle ---

// ensureStagingWorktree creates or resumes the single staging worktree for the
// entire executor lifecycle. Created once, shared across all phases (implement,
// review, merge). The main worktree NEVER leaves the base branch.
func (e *formulaExecutor) ensureStagingWorktree() (*StagingWorktree, error) {
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
			e.stagingWt = ResumeStagingWorktree(repoPath, e.state.WorktreeDir, stagingBranch, e.state.BaseBranch, e.log)
			return e.stagingWt, nil
		}
		e.log("stale worktree state %s — recreating", e.state.WorktreeDir)
		e.state.WorktreeDir = ""
	}

	// Create the staging branch from current HEAD.
	rc := &RepoContext{Dir: repoPath, BaseBranch: e.state.BaseBranch}
	rc.ForceBranch(stagingBranch)

	// Worktree dir: .worktrees/<bead-id> — traceable to the bead.
	wtDir := filepath.Join(repoPath, ".worktrees", e.beadID)

	e.log("creating staging worktree at %s (branch: %s)", wtDir, stagingBranch)
	wt, err := NewStagingWorktreeAt(repoPath, wtDir, stagingBranch, e.state.BaseBranch, e.log)
	if err != nil {
		return nil, fmt.Errorf("create staging worktree: %w", err)
	}

	e.stagingWt = wt
	e.state.WorktreeDir = wtDir

	if e.state.AttemptBeadID != "" {
		storeAddLabel(e.state.AttemptBeadID, "worktree:"+wtDir)
	}
	storeAddLabel(e.beadID, "feat-branch:"+stagingBranch)
	e.saveState()
	return wt, nil
}

// closeStagingWorktree removes the staging worktree and cleans up state.
func (e *formulaExecutor) closeStagingWorktree() {
	if e.stagingWt != nil {
		e.log("removing staging worktree at %s", e.stagingWt.Dir)
		e.stagingWt.Close()
		e.stagingWt = nil
	}
	e.state.WorktreeDir = ""
}

// Close removes the worktree from git and deletes its temp directory.
// It is safe to call multiple times.
func (w *StagingWorktree) Close() error {
	if w.Dir != "" {
		rc := &RepoContext{Dir: w.RepoPath}
		rc.ForceRemoveWorktree(w.Dir)
		w.Dir = ""
	}
	if w.tmpDir != "" {
		os.RemoveAll(w.tmpDir)
		w.tmpDir = ""
	}
	return nil
}
