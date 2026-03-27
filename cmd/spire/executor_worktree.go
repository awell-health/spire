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
type StagingWorktree struct {
	repoPath string // path to the main git repository
	branch   string // branch checked out in this worktree
	dir      string // path to this worktree directory
	tmpDir   string // temp directory parent (cleaned up on Close)
	log      func(string, ...interface{})
}

// NewStagingWorktree creates a new temporary git worktree checking out branch.
// nameHint is included in the temp directory name for debugging (e.g. "spire-staging").
// The caller must call Close() when done.
func NewStagingWorktree(repoPath, branch, nameHint string, log func(string, ...interface{})) (*StagingWorktree, error) {
	tmpDir, err := os.MkdirTemp("", nameHint+"-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	dir := filepath.Join(tmpDir, "wt")
	if out, wtErr := exec.Command("git", "-C", repoPath, "worktree", "add", dir, branch).CombinedOutput(); wtErr != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("worktree add %s: %s\n%s", branch, wtErr, string(out))
	}
	return &StagingWorktree{
		repoPath: repoPath,
		branch:   branch,
		dir:      dir,
		tmpDir:   tmpDir,
		log:      log,
	}, nil
}

// Dir returns the filesystem path to the worktree directory.
func (w *StagingWorktree) Dir() string { return w.dir }

// Branch returns the branch checked out in this worktree.
func (w *StagingWorktree) Branch() string { return w.branch }

// MergeBranch merges childBranch into this worktree's branch.
// It fetches from origin first, then tries origin/childBranch, then childBranch locally.
// On merge conflict, resolver is called (if non-nil) to attempt resolution.
// resolver receives the worktree dir and childBranch name.
func (w *StagingWorktree) MergeBranch(childBranch string, resolver func(dir, branch string) error) error {
	w.log("  merging %s into %s", childBranch, w.branch)

	// Fetch in case the apprentice pushed to remote.
	exec.Command("git", "-C", w.dir, "fetch", "origin", childBranch).Run()

	// Try remote branch first, fall back to local.
	branchRef := "origin/" + childBranch
	if _, mergeErr := exec.Command("git", "-C", w.dir, "merge", "--no-edit", branchRef).CombinedOutput(); mergeErr != nil {
		branchRef = childBranch
		if _, mergeErr2 := exec.Command("git", "-C", w.dir, "merge", "--no-edit", branchRef).CombinedOutput(); mergeErr2 != nil {
			// Check if git is in a conflict state.
			statusOut, _ := exec.Command("git", "-C", w.dir, "status", "--porcelain").Output()
			if strings.Contains(string(statusOut), "UU ") || strings.Contains(string(statusOut), "AA ") {
				if resolver != nil {
					w.log("  conflict detected, invoking resolver")
					if resolveErr := resolver(w.dir, childBranch); resolveErr != nil {
						exec.Command("git", "-C", w.dir, "merge", "--abort").Run()
						return fmt.Errorf("conflict resolution failed: %w", resolveErr)
					}
					return nil
				}
				exec.Command("git", "-C", w.dir, "merge", "--abort").Run()
				return fmt.Errorf("merge conflict in %s: no resolver provided", childBranch)
			}
			// Not a conflict — some other merge error.
			exec.Command("git", "-C", w.dir, "merge", "--abort").Run()
			return fmt.Errorf("merge failed: %w", mergeErr2)
		}
	}
	return nil
}

// RunBuild runs buildStr as a command in the worktree directory.
// buildStr is split on spaces and run directly (no shell).
func (w *StagingWorktree) RunBuild(buildStr string) error {
	parts := strings.Fields(buildStr)
	if len(parts) == 0 {
		return nil
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = w.dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.log("build failed: %s\n%s", err, string(out))
		return fmt.Errorf("%s: %w\n%s", buildStr, err, string(out))
	}
	w.log("build passed")
	return nil
}

// RunTests runs testStr as a command in the worktree directory.
// testStr is split on spaces and run directly (no shell).
func (w *StagingWorktree) RunTests(testStr string) error {
	parts := strings.Fields(testStr)
	if len(parts) == 0 {
		return nil
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = w.dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.log("tests failed: %s\n%s", err, string(out))
		return fmt.Errorf("%s: %w\n%s", testStr, err, string(out))
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
func (w *StagingWorktree) MergeToMain(baseBranch string, env []string, buildStr, testStr string) error {
	// Ensure main worktree is on baseBranch.
	if headOut, _ := exec.Command("git", "-C", w.repoPath, "rev-parse", "--abbrev-ref", "HEAD").Output(); strings.TrimSpace(string(headOut)) != baseBranch {
		if out, err := exec.Command("git", "-C", w.repoPath, "checkout", baseBranch).CombinedOutput(); err != nil {
			return fmt.Errorf("checkout %s: %s\n%s", baseBranch, err, string(out))
		}
	}

	// Pull baseBranch to be up to date.
	pullCmd := exec.Command("git", "-C", w.repoPath, "pull", "--ff-only", "origin", baseBranch)
	pullCmd.Env = env
	if out, pullErr := pullCmd.CombinedOutput(); pullErr != nil {
		w.log("warning: pull %s: %s\n%s", baseBranch, pullErr, string(out))
	}

	// Belt-and-suspenders: verify we're still on baseBranch after the pull.
	if headRef, _ := exec.Command("git", "-C", w.repoPath, "symbolic-ref", "--short", "HEAD").Output(); strings.TrimSpace(string(headRef)) != baseBranch {
		if out, err := exec.Command("git", "-C", w.repoPath, "checkout", baseBranch).CombinedOutput(); err != nil {
			return fmt.Errorf("checkout %s: %s\n%s", baseBranch, err, string(out))
		}
	}

	w.log("ff-only merge %s → %s (committer: archmage)", w.branch, baseBranch)

	// First attempt: fast-forward only merge.
	ffCmd := exec.Command("git", "-C", w.repoPath, "merge", "--ff-only", w.branch)
	ffCmd.Env = env
	if out, ffErr := ffCmd.CombinedOutput(); ffErr == nil {
		return nil // success — done
	} else {
		w.log("ff-only failed: %s — rebasing staging onto %s", strings.TrimSpace(string(out)), baseBranch)
	}

	// ff-only failed — main has diverged. Rebase staging onto main in a
	// temporary worktree so we don't disturb the main worktree's checkout.
	rebaseTmp, err := os.MkdirTemp("", "spire-rebase-")
	if err != nil {
		return fmt.Errorf("create rebase temp dir: %w", err)
	}
	defer os.RemoveAll(rebaseTmp)

	rebaseWtPath := filepath.Join(rebaseTmp, "staging")
	if out, wtErr := exec.Command("git", "-C", w.repoPath, "worktree", "add", rebaseWtPath, w.branch).CombinedOutput(); wtErr != nil {
		return fmt.Errorf("create rebase worktree: %s\n%s", wtErr, string(out))
	}
	defer exec.Command("git", "-C", w.repoPath, "worktree", "remove", "--force", rebaseWtPath).Run()

	// Rebase the staging branch onto main.
	w.log("rebasing %s onto %s in staging worktree", w.branch, baseBranch)
	rebaseCmd := exec.Command("git", "-C", rebaseWtPath, "rebase", baseBranch)
	rebaseCmd.Env = os.Environ()
	if out, rbErr := rebaseCmd.CombinedOutput(); rbErr != nil {
		exec.Command("git", "-C", rebaseWtPath, "rebase", "--abort").Run()
		return fmt.Errorf("rebase %s onto %s failed (aborting, will not force merge): %s\n%s", w.branch, baseBranch, rbErr, string(out))
	}

	// Re-verify build in the staging worktree after rebase.
	if buildStr != "" {
		w.log("verifying build after rebase")
		buildParts := strings.Fields(buildStr)
		buildCmd := exec.Command(buildParts[0], buildParts[1:]...)
		buildCmd.Dir = rebaseWtPath
		buildCmd.Env = os.Environ()
		if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
			return fmt.Errorf("build failed after rebase (aborting merge): %s\n%s", buildErr, string(out))
		}
	}

	// Run tests after rebase.
	if testStr != "" {
		w.log("running tests after rebase")
		testParts := strings.Fields(testStr)
		testCmd := exec.Command(testParts[0], testParts[1:]...)
		testCmd.Dir = rebaseWtPath
		testCmd.Env = os.Environ()
		if out, testErr := testCmd.CombinedOutput(); testErr != nil {
			return fmt.Errorf("tests failed after rebase (aborting merge): %s\n%s", testErr, string(out))
		}
	}

	// Remove the rebase worktree before retrying the merge (the branch ref is
	// already updated by the rebase — the worktree just holds a checkout).
	exec.Command("git", "-C", w.repoPath, "worktree", "remove", "--force", rebaseWtPath).Run()

	// Second attempt: ff-only should now succeed since staging was rebased.
	w.log("retrying ff-only merge after rebase")
	ffCmd2 := exec.Command("git", "-C", w.repoPath, "merge", "--ff-only", w.branch)
	ffCmd2.Env = env
	if out, ffErr2 := ffCmd2.CombinedOutput(); ffErr2 != nil {
		return fmt.Errorf("ff-only merge failed even after rebase (will not force merge): %s\n%s", ffErr2, string(out))
	}
	return nil
}

// Close removes the worktree from git and deletes its temp directory.
// It is safe to call multiple times.
func (w *StagingWorktree) Close() error {
	if w.dir != "" {
		exec.Command("git", "-C", w.repoPath, "worktree", "remove", "--force", w.dir).Run()
		w.dir = ""
	}
	if w.tmpDir != "" {
		os.RemoveAll(w.tmpDir)
		w.tmpDir = ""
	}
	return nil
}
