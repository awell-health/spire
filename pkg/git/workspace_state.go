package git

// workspace_state.go — inspection and cleanup primitives for worktree state
// left over from an interrupted git operation. These are pure primitives used
// by the executor's dispatch-time workspace validation policy: detect a
// rebase/merge/stale-lock/detached-HEAD/dirty-tree condition, then clean it up.
// No policy or logging lives here.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// staleIndexLockAge is the minimum age (by mtime) before an .git/index.lock
// is treated as abandoned. Git holds the lock for the duration of an index
// write, which is sub-second in practice; anything older is almost certainly
// left behind by a crashed process.
const staleIndexLockAge = 60 * time.Second

// WorkspaceState summarizes the transitional-state flags detectable at a
// worktree path. All booleans are independent — a worktree can be simultaneously
// in a paused rebase AND have a stale index lock, for example, and callers
// handle them in order.
type WorkspaceState struct {
	RebaseInProgress bool
	MergeInProgress  bool
	StaleIndexLock   bool
	DetachedHEAD     bool
	Dirty            bool
	CurrentBranch    string
	DirtyFiles       []string // porcelain lines when Dirty is true (empty otherwise)
}

// InspectWorkspaceState reports transitional-state flags for the worktree at
// path. path must be an existing worktree (caller has already verified it
// exists on disk). Detection rules:
//
//   - .git/rebase-merge/ or .git/rebase-apply/ present → RebaseInProgress.
//   - .git/MERGE_HEAD present → MergeInProgress.
//   - .git/index.lock present, mtime older than staleIndexLockAge → StaleIndexLock.
//   - `git symbolic-ref HEAD` fails → DetachedHEAD.
//   - `git status --porcelain` non-empty → Dirty (and DirtyFiles populated).
//
// Callers should treat a non-nil error as "could not inspect"; the returned
// WorkspaceState is still partially populated in that case but should not be
// acted on blindly.
func InspectWorkspaceState(path string) (WorkspaceState, error) {
	if path == "" {
		return WorkspaceState{}, fmt.Errorf("inspect workspace state: empty path")
	}

	gitDir, err := resolveGitDirAt(path)
	if err != nil {
		return WorkspaceState{}, fmt.Errorf("resolve git-dir at %s: %w", path, err)
	}

	state := WorkspaceState{}

	if fileExists(filepath.Join(gitDir, "rebase-merge")) ||
		fileExists(filepath.Join(gitDir, "rebase-apply")) {
		state.RebaseInProgress = true
	}
	if fileExists(filepath.Join(gitDir, "MERGE_HEAD")) {
		state.MergeInProgress = true
	}

	lockPath := filepath.Join(gitDir, "index.lock")
	if info, err := os.Stat(lockPath); err == nil {
		if time.Since(info.ModTime()) >= staleIndexLockAge {
			state.StaleIndexLock = true
		}
	}

	// symbolic-ref exits non-zero when HEAD is detached. When HEAD is on a
	// branch, its output is the full symbolic ref (e.g. "refs/heads/main").
	if out, err := exec.Command("git", "-C", path, "symbolic-ref", "HEAD").Output(); err == nil {
		ref := strings.TrimSpace(string(out))
		state.CurrentBranch = strings.TrimPrefix(ref, "refs/heads/")
	} else {
		state.DetachedHEAD = true
	}

	if out, err := exec.Command("git", "-C", path, "status", "--porcelain").Output(); err == nil {
		trimmed := strings.TrimRight(string(out), "\n")
		if trimmed != "" {
			state.Dirty = true
			state.DirtyFiles = strings.Split(trimmed, "\n")
		}
	}

	return state, nil
}

// AbortRebase runs `git rebase --abort` at path. Safe to call when no rebase
// is in progress — git returns non-zero in that case, but we treat "no rebase
// in progress" as a no-op since the caller's goal (clean workspace) is already
// satisfied. Returns an error only when the abort itself fails with output
// that indicates a real problem.
func AbortRebase(path string) error {
	out, err := exec.Command("git", "-C", path, "rebase", "--abort").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// Match git's own "no rebase in progress" output, case-insensitive to
		// cover version drift. When already clean, caller's goal is satisfied.
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "no rebase in progress") {
			return nil
		}
		return fmt.Errorf("git rebase --abort at %s: %w: %s", path, err, msg)
	}
	return nil
}

// AbortMerge runs `git merge --abort` at path. No-op when no merge is in
// progress (git's own exit message is recognized).
func AbortMerge(path string) error {
	out, err := exec.Command("git", "-C", path, "merge", "--abort").CombinedOutput()
	if err != nil {
		lower := strings.ToLower(strings.TrimSpace(string(out)))
		if strings.Contains(lower, "no merge to abort") ||
			strings.Contains(lower, "no merge in progress") {
			return nil
		}
		return fmt.Errorf("git merge --abort at %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveStaleIndexLock removes .git/index.lock after re-verifying its age. The
// re-check closes the race where the lock became live between the earlier
// InspectWorkspaceState call and this cleanup; if the lock is now fresh, the
// function returns an error rather than racing a live git process.
func RemoveStaleIndexLock(path string) error {
	gitDir, err := resolveGitDirAt(path)
	if err != nil {
		return fmt.Errorf("resolve git-dir at %s: %w", path, err)
	}
	lockPath := filepath.Join(gitDir, "index.lock")
	info, err := os.Stat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", lockPath, err)
	}
	if time.Since(info.ModTime()) < staleIndexLockAge {
		return fmt.Errorf("index.lock at %s is not stale (age=%s)", lockPath, time.Since(info.ModTime()))
	}
	if err := os.Remove(lockPath); err != nil {
		return fmt.Errorf("remove %s: %w", lockPath, err)
	}
	return nil
}

// AttachHEAD reattaches a detached HEAD by checking out branch at path.
// No-op when HEAD is already on branch.
func AttachHEAD(path, branch string) error {
	if branch == "" {
		return fmt.Errorf("attach HEAD at %s: empty branch", path)
	}
	if cur, err := currentBranchAt(path); err == nil && cur == branch {
		return nil
	}
	out, err := exec.Command("git", "-C", path, "checkout", branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git checkout %s at %s: %w: %s", branch, path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolveGitDirAt returns the absolute path to the .git directory (or per-
// worktree gitdir for linked worktrees) at path. Used by the inspection +
// cleanup primitives so they read the right state file regardless of whether
// path is a main repo or a linked worktree.
func resolveGitDirAt(path string) (string, error) {
	out, err := exec.Command("git", "-C", path, "rev-parse", "--git-dir").Output()
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("empty git-dir for %s", path)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(path, dir)
	}
	return dir, nil
}
