package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrMergeRace is returned when the base branch advances during the
// rebase→verify→merge window and all retry attempts are exhausted.
// Callers can check for this with errors.Is to distinguish a retryable
// race from a terminal failure (e.g. rebase conflict).
var ErrMergeRace = errors.New("merge race: main advanced during landing")

// maxMergeAttempts is the number of rebase→verify→ff-only cycles
// MergeToMain will attempt before returning ErrMergeRace.
const maxMergeAttempts = 3

// hasRebaseConflicts reports whether git status (porcelain format) indicates
// unresolved merge/rebase conflicts. UU = both modified, AA = both added.
func hasRebaseConflicts(status string) bool {
	return strings.Contains(status, "UU ") || strings.Contains(status, "AA ")
}

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
	rc := &RepoContext{Dir: repoPath, BaseBranch: baseBranch, Log: log}
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
	}, nil
}

// NewStagingWorktreeAt creates a staging worktree at a specific directory path.
// Unlike NewStagingWorktree (which creates a temp dir), this places the worktree
// at dir, making it discoverable by other processes that know the path.
// userName and userEmail configure the git identity in the worktree.
//
// The caller must call Close() when done. Close removes the git worktree and
// the directory itself.
func NewStagingWorktreeAt(repoPath, dir, branch, baseBranch, userName, userEmail string, log func(string, ...interface{})) (*StagingWorktree, error) {
	rc := &RepoContext{Dir: repoPath, BaseBranch: baseBranch, Log: log}

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
	}, nil
}

// ResumeStagingWorktree wraps an existing worktree directory in a StagingWorktree.
// The worktree already exists on disk and just needs to be wrapped for method access.
//
// Captures HEAD SHA as StartSHA for session-scoped commit detection. If HEAD
// cannot be read (e.g. worktree is corrupt), StartSHA is left empty and
// callers fall back to BaseBranch..HEAD comparison.
func ResumeStagingWorktree(repoPath, dir, branch, baseBranch string, log func(string, ...interface{})) *StagingWorktree {
	// Capture session baseline if the worktree exists.
	var startSHA string
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
		startSHA = strings.TrimSpace(string(out))
	}
	return &StagingWorktree{
		WorktreeContext: WorktreeContext{
			Dir:        dir,
			Branch:     branch,
			BaseBranch: baseBranch,
			RepoPath:   repoPath,
			StartSHA:   startSHA,
			Log:        log,
		},
	}
}

// FetchBranch fetches a specific branch from a remote into this staging worktree.
// Fetch operations live on StagingWorktree (not WorktreeContext) because
// WorktreeContext enforces local-ref-only semantics.
//
// Returns the underlying git error (including stderr) on failure. Callers that
// treat fetch as best-effort (e.g. the push-transport fallback in
// action_dispatch) may still discard it and rely on MergeBranch failing later,
// but surfacing a genuine fetch error (network, auth) here prevents the
// confusing "merge failed" message when the root cause was the fetch.
func (w *StagingWorktree) FetchBranch(remote, branch string) error {
	out, err := exec.Command("git", "-C", w.Dir, "fetch", remote, branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch %s %s: %w\n%s", remote, branch, err, string(out))
	}
	return nil
}

// MergeBranch merges childBranch into this staging worktree's branch with
// linear history (no merge commits). Strategy:
//  1. Try ff-only merge — succeeds when staging hasn't diverged.
//  2. If ff-only fails, rebase the child onto staging, then ff-only again.
//
// All branch refs are local — MergeBranch does not fetch or push.
//
// On rebase conflict, resolver is called (if non-nil) to attempt resolution.
func (w *StagingWorktree) MergeBranch(childBranch string, resolver func(dir, branch string) error) error {
	w.logf("  merging %s into %s", childBranch, w.Branch)

	// Use local branch ref directly — no remote fetching.
	branchRef := childBranch

	// Step 1: Try fast-forward-only merge.
	if err := w.MergeFFOnly(branchRef); err == nil {
		return nil
	}
	w.logf("  ff-only failed, rebasing %s onto %s", branchRef, w.Branch)

	// Step 2: Rebase the child tip onto the current staging tip using detached
	// HEADs. Rebasing the branch name directly is fragile: it can fail if the
	// child branch is still checked out in another worktree, and some git
	// setups do not reliably resolve the staging branch name here. Using SHAs
	// keeps the rebase local to this worktree and avoids mutating the child ref.
	stagingTip, err := w.HeadSHA()
	if err != nil {
		return fmt.Errorf("read staging tip before rebase: %w", err)
	}
	childTipOut, err := exec.Command("git", "-C", w.Dir, "rev-parse", branchRef).CombinedOutput()
	if err != nil {
		return fmt.Errorf("resolve child branch %s: %w\n%s", branchRef, err, string(childTipOut))
	}
	childTip := strings.TrimSpace(string(childTipOut))

	rebaseCmd := exec.Command("git", "-C", w.Dir, "rebase", stagingTip, childTip)
	rebaseCmd.Env = os.Environ()
	if out, err := rebaseCmd.CombinedOutput(); err != nil {
		// Check if rebase stopped due to conflicts.
		status := w.StatusPorcelain()
		if hasRebaseConflicts(status) {
			if resolver == nil {
				exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
				return fmt.Errorf("rebase conflict in %s: no resolver provided", childBranch)
			}
			if resolveErr := resolveRebaseConflicts(w.Dir, childBranch, resolver, w.logf); resolveErr != nil {
				return resolveErr
			}
		} else {
			exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
			return fmt.Errorf("rebase %s onto %s failed: %s\n%s", branchRef, stagingTip, err, string(out))
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

// resolveRebaseConflicts handles a rebase that has stopped due to merge conflicts.
// It loops: call resolver → rebase --continue → check for new conflicts → repeat
// until the rebase completes or the resolver fails. Called after the initial
// rebase command has returned an error and the caller has confirmed conflicts
// are present (via hasRebaseConflicts).
//
// dir is the worktree directory in a mid-rebase state. branch is the logical
// branch name passed to the resolver for context.
func resolveRebaseConflicts(dir, branch string, resolver func(dir, branch string) error, logf func(string, ...any)) error {
	for {
		if logf != nil {
			logf("  resolving rebase conflicts for %s", branch)
		}
		if resolveErr := resolver(dir, branch); resolveErr != nil {
			exec.Command("git", "-C", dir, "rebase", "--abort").Run()
			return fmt.Errorf("conflict resolution failed during rebase: %w", resolveErr)
		}

		// Resolver succeeded — continue the rebase. May stop again if the
		// next commit in a multi-commit rebase also conflicts.
		contCmd := exec.Command("git", "-C", dir, "rebase", "--continue")
		contCmd.Env = os.Environ()
		contOut, contErr := contCmd.CombinedOutput()
		if contErr == nil {
			return nil // rebase completed
		}

		// Check if rebase --continue stopped on new conflicts (next commit).
		wc := &WorktreeContext{Dir: dir}
		status := wc.StatusPorcelain()
		if hasRebaseConflicts(status) {
			continue // loop to resolve the next batch
		}

		// Non-conflict error (e.g., empty commit after resolution).
		exec.Command("git", "-C", dir, "rebase", "--abort").Run()
		return fmt.Errorf("rebase --continue failed after resolution: %s\n%s", contErr, string(contOut))
	}
}

// RunBuild runs buildStr as a command in the worktree directory.
// buildStr is split on spaces and run directly (no shell).
func (w *StagingWorktree) RunBuild(buildStr string) error {
	out, err := w.RunCommandOutput(buildStr)
	if err != nil {
		w.logf("build failed: %s\n%s", err, out)
		return fmt.Errorf("%s: %w\n%s", buildStr, err, out)
	}
	w.logf("build passed")
	return nil
}

// RunTests runs testStr as a command in the worktree directory.
// testStr is split on spaces and run directly (no shell).
func (w *StagingWorktree) RunTests(testStr string) error {
	out, err := w.RunCommandOutput(testStr)
	if err != nil {
		w.logf("tests failed: %s\n%s", err, out)
		return fmt.Errorf("%s: %w\n%s", testStr, err, out)
	}
	w.logf("tests passed")
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
// resolver, when non-nil, is called to resolve merge conflicts during rebase.
// If nil, rebase conflicts are terminal errors (backward-compatible behavior).
// When a resolver is provided and conflicts occur, the function will attempt
// resolution and retry up to maxMergeAttempts times before returning ErrMergeRace.
//
// NOTE: Main-repo operations (checkout, pull, merge, worktree lifecycle) go
// through RepoContext. The rebase operations target a temporary worktree and
// remain as raw exec.Command calls since WorktreeContext doesn't expose rebase.
func (w *StagingWorktree) MergeToMain(baseBranch string, env []string, buildStr, testStr string, resolver func(dir, branch string) error) error {
	rc := &RepoContext{Dir: w.RepoPath, BaseBranch: baseBranch, Log: w.Log}

	// Ensure main worktree is on baseBranch.
	if rc.CurrentBranch() != baseBranch {
		if err := rc.Checkout(baseBranch); err != nil {
			return err
		}
	}

	// Pull baseBranch to be up to date.
	if pullErr := rc.PullFFOnly("origin", baseBranch, env); pullErr != nil {
		w.logf("warning: pull %s: %s", baseBranch, pullErr)
	}

	// Belt-and-suspenders: verify we're still on baseBranch after the pull.
	if rc.CurrentBranch() != baseBranch {
		if err := rc.Checkout(baseBranch); err != nil {
			return err
		}
	}

	w.logf("ff-only merge %s → %s (committer: archmage)", w.Branch, baseBranch)

	// First attempt: fast-forward only merge (common case — no rebase needed).
	if err := rc.MergeFFOnly(w.Branch, env); err == nil {
		return nil // success — done
	} else {
		w.logf("ff-only failed: %s — rebasing staging onto %s", err, baseBranch)
	}

	// ff-only failed — main has diverged. Enter a bounded retry loop:
	// pull main → rebase → verify build/tests → ff-only merge.
	// If main advances again during verification, loop again.
	for attempt := 0; attempt < maxMergeAttempts; attempt++ {
		if attempt > 0 {
			w.logf("merge race detected, retry %d/%d", attempt+1, maxMergeAttempts)
		}

		// Re-pull baseBranch to pick up any advances since last attempt.
		if pullErr := rc.PullFFOnly("origin", baseBranch, env); pullErr != nil {
			w.logf("warning: pull %s (attempt %d): %s", baseBranch, attempt+1, pullErr)
		}

		// Rebase staging onto the (possibly updated) baseBranch in-place.
		w.logf("rebasing %s onto %s in place (attempt %d)", w.Branch, baseBranch, attempt+1)
		rebaseCmd := exec.Command("git", "-C", w.Dir, "rebase", baseBranch)
		rebaseCmd.Env = os.Environ()
		if out, rbErr := rebaseCmd.CombinedOutput(); rbErr != nil {
			// Check if rebase stopped due to conflicts.
			status := w.StatusPorcelain()
			if hasRebaseConflicts(status) {
				if resolver == nil {
					exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
					return fmt.Errorf("rebase %s onto %s hit conflicts (no resolver, aborting): %s\n%s", w.Branch, baseBranch, rbErr, string(out))
				}
				if resolveErr := resolveRebaseConflicts(w.Dir, w.Branch, resolver, w.logf); resolveErr != nil {
					w.logf("conflict resolution failed (attempt %d): %s", attempt+1, resolveErr)
					continue // try next attempt
				}
				// Resolution succeeded, fall through to build/test verification.
			} else {
				exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
				return fmt.Errorf("rebase %s onto %s failed (aborting, will not force merge): %s\n%s", w.Branch, baseBranch, rbErr, string(out))
			}
		}

		// Re-verify build after rebase.
		if buildStr != "" {
			w.logf("verifying build after rebase (attempt %d)", attempt+1)
			out, buildErr := w.RunCommandOutput(buildStr)
			if buildErr != nil {
				return fmt.Errorf("build failed after rebase (aborting merge): %s\n%s", buildErr, out)
			}
		}

		// Re-verify tests after rebase.
		if testStr != "" {
			w.logf("running tests after rebase (attempt %d)", attempt+1)
			out, testErr := w.RunCommandOutput(testStr)
			if testErr != nil {
				return fmt.Errorf("tests failed after rebase (aborting merge): %s\n%s", testErr, out)
			}
		}

		// Attempt ff-only merge — succeeds unless main advanced again.
		w.logf("retrying ff-only merge after rebase (attempt %d)", attempt+1)
		if err := rc.MergeFFOnly(w.Branch, env); err == nil {
			return nil
		} else {
			w.logf("ff-only failed again (attempt %d): %s", attempt+1, err)
		}
	}

	return fmt.Errorf("ff-only merge failed after %d rebase attempts (will not force merge): %w", maxMergeAttempts, ErrMergeRace)
}

// Close removes the worktree from git and deletes its temp directory.
// It is safe to call multiple times.
func (w *StagingWorktree) Close() error {
	if w.Dir != "" {
		rc := &RepoContext{Dir: w.RepoPath, Log: w.Log}
		rc.ForceRemoveWorktree(w.Dir)
		w.Dir = ""
	}
	if w.tmpDir != "" {
		os.RemoveAll(w.tmpDir)
		w.tmpDir = ""
	}
	return nil
}
