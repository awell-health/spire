package git

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
// userName and userEmail configure the git identity in the worktree.
// The caller must call Close() when done.
func NewStagingWorktree(repoPath, branch, baseBranch, nameHint, userName, userEmail string, log func(string, ...interface{})) (*StagingWorktree, error) {
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

	wc.ConfigureUser(userName, userEmail)

	return &StagingWorktree{
		WorktreeContext: wc,
		tmpDir:         tmpDir,
		log:            log,
	}, nil
}

// NewStagingWorktreeAt creates a staging worktree at a specific directory path.
// Unlike NewStagingWorktree (which creates a temp dir), this places the worktree
// at dir (e.g. .worktrees/<bead-id>), making it discoverable by all participants
// in the epic lifecycle (wizard, sage, fix apprentice).
// userName and userEmail configure the git identity in the worktree.
//
// The caller must call Close() when done. Close removes the git worktree and
// the directory itself.
func NewStagingWorktreeAt(repoPath, dir, branch, baseBranch, userName, userEmail string, log func(string, ...interface{})) (*StagingWorktree, error) {
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

	wc.ConfigureUser(userName, userEmail)

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

// FetchBranch fetches a specific branch from a remote into this staging worktree.
// Fetch operations live on StagingWorktree (not WorktreeContext) because
// WorktreeContext enforces local-ref-only semantics.
func (w *StagingWorktree) FetchBranch(remote, branch string) {
	exec.Command("git", "-C", w.Dir, "fetch", remote, branch).Run()
}

// MergeBranch merges childBranch into this staging worktree's branch with
// linear history (no merge commits). Strategy:
//  1. Try ff-only merge — succeeds when staging hasn't diverged.
//  2. If ff-only fails, rebase the child onto staging, then ff-only again.
//
// Apprentices never push feature branches to origin — branches are local
// only (worktree in local mode, shared PVC in k8s mode). MergeBranch uses
// local refs exclusively.
//
// On rebase conflict, resolver is called (if non-nil) to attempt resolution.
func (w *StagingWorktree) MergeBranch(childBranch string, resolver func(dir, branch string) error) error {
	w.log("  merging %s into %s", childBranch, w.Branch)

	// Use local branch ref directly — apprentices don't push to origin.
	branchRef := childBranch

	// Step 1: Try fast-forward-only merge.
	if err := w.MergeFFOnly(branchRef); err == nil {
		return nil
	}
	w.log("  ff-only failed, rebasing %s onto %s", branchRef, w.Branch)

	// Step 2: Rebase the child branch onto staging.
	// This checks out branchRef (detached HEAD for remote refs), replays
	// its commits on top of w.Branch, then we switch back and ff-only merge.
	rebaseCmd := exec.Command("git", "-C", w.Dir, "rebase", w.Branch, branchRef)
	rebaseCmd.Env = os.Environ()
	if out, err := rebaseCmd.CombinedOutput(); err != nil {
		// Check if rebase stopped due to conflicts.
		status := w.StatusPorcelain()
		if strings.Contains(status, "UU ") || strings.Contains(status, "AA ") {
			if resolver != nil {
				if resolveErr := resolver(w.Dir, childBranch); resolveErr != nil {
					exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
					return fmt.Errorf("conflict resolution failed during rebase: %w", resolveErr)
				}
				// Resolver succeeded — continue rebase.
				contCmd := exec.Command("git", "-C", w.Dir, "rebase", "--continue")
				contCmd.Env = os.Environ()
				if contOut, contErr := contCmd.CombinedOutput(); contErr != nil {
					exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
					return fmt.Errorf("rebase --continue failed after resolution: %s\n%s", contErr, string(contOut))
				}
			} else {
				exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
				return fmt.Errorf("rebase conflict in %s: no resolver provided", childBranch)
			}
		} else {
			exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
			return fmt.Errorf("rebase %s onto %s failed: %s\n%s", branchRef, w.Branch, err, string(out))
		}
	}

	// Capture the rebased tip SHA (HEAD is now at the rebased result).
	rebasedSHA, _ := exec.Command("git", "-C", w.Dir, "rev-parse", "HEAD").Output()
	tip := strings.TrimSpace(string(rebasedSHA))

	// Switch back to the staging branch.
	if out, err := exec.Command("git", "-C", w.Dir, "checkout", w.Branch).CombinedOutput(); err != nil {
		return fmt.Errorf("checkout %s after rebase: %w\n%s", w.Branch, err, string(out))
	}

	// Step 3: ff-only merge the rebased commits — should succeed now.
	if err := w.MergeFFOnly(tip); err != nil {
		return fmt.Errorf("ff-only merge failed after rebase: %w", err)
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

	// ff-only failed — main has diverged. Rebase the staging branch onto
	// baseBranch in-place in the existing staging worktree. We cannot create
	// a second worktree for the same branch — git forbids two worktrees
	// checking out the same branch simultaneously.
	w.log("rebasing %s onto %s in place", w.Branch, baseBranch)
	rebaseCmd := exec.Command("git", "-C", w.Dir, "rebase", baseBranch)
	rebaseCmd.Env = os.Environ()
	if out, rbErr := rebaseCmd.CombinedOutput(); rbErr != nil {
		exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
		return fmt.Errorf("rebase %s onto %s failed (aborting, will not force merge): %s\n%s", w.Branch, baseBranch, rbErr, string(out))
	}

	// Re-verify build in the staging worktree after rebase.
	if buildStr != "" {
		w.log("verifying build after rebase")
		out, buildErr := w.RunCommandOutput(buildStr)
		if buildErr != nil {
			return fmt.Errorf("build failed after rebase (aborting merge): %s\n%s", buildErr, out)
		}
	}

	// Run tests after rebase.
	if testStr != "" {
		w.log("running tests after rebase")
		out, testErr := w.RunCommandOutput(testStr)
		if testErr != nil {
			return fmt.Errorf("tests failed after rebase (aborting merge): %s\n%s", testErr, out)
		}
	}

	// Second attempt: ff-only should now succeed since staging was rebased.
	w.log("retrying ff-only merge after rebase")
	if err := rc.MergeFFOnly(w.Branch, env); err != nil {
		return fmt.Errorf("ff-only merge failed even after rebase (will not force merge): %w", err)
	}
	return nil
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
