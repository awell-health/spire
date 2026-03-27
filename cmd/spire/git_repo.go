package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// RepoContext is the single abstraction for all git operations that target the
// main repository (not a worktree). Every git command that runs against the
// main repo outside of a worktree must go through this type so that:
//   - Dir is always passed via -C (no accidental cwd-dependent operations)
//   - All repo-level operations are discoverable in one place
//
// WorktreeContext handles in-worktree operations; RepoContext handles everything else:
// branch management, worktree lifecycle, fetch, push, and merge-to-main flows.
type RepoContext struct {
	Dir string // absolute path to the main repository
}

// NewRepoContext creates a RepoContext for the given repository directory.
func NewRepoContext(dir string) *RepoContext {
	return &RepoContext{Dir: dir}
}

// --- Remote operations ---

// RemoteURL returns the URL of the named remote, or empty string on error.
func (rc *RepoContext) RemoteURL(remote string) string {
	out, err := exec.Command("git", "-C", rc.Dir, "remote", "get-url", remote).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Fetch fetches a ref from a remote. Best-effort (errors are silently ignored).
func (rc *RepoContext) Fetch(remote, ref string) {
	exec.Command("git", "-C", rc.Dir, "fetch", remote, ref).Run()
}

// Push pushes a branch to a remote. env overrides the process environment
// (use nil to inherit the current environment).
func (rc *RepoContext) Push(remote, branch string, env []string) error {
	cmd := exec.Command("git", "-C", rc.Dir, "push", remote, branch)
	if env != nil {
		cmd.Env = env
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push %s %s: %w\n%s", remote, branch, err, string(out))
	}
	return nil
}

// PushDelete deletes a remote branch. Best-effort (errors are silently ignored).
func (rc *RepoContext) PushDelete(remote, branch string) {
	exec.Command("git", "-C", rc.Dir, "push", remote, "--delete", branch).Run()
}

// --- Branch operations ---

// CurrentBranch returns the current branch name (via rev-parse --abbrev-ref HEAD).
// Returns "HEAD" for detached HEAD state, or empty string on error.
func (rc *RepoContext) CurrentBranch() string {
	out, err := exec.Command("git", "-C", rc.Dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// SymbolicRefHead returns the short symbolic ref of HEAD (the branch name).
// Unlike CurrentBranch, this fails on detached HEAD. Returns empty string on error.
func (rc *RepoContext) SymbolicRefHead() string {
	out, err := exec.Command("git", "-C", rc.Dir, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Checkout checks out the given branch.
func (rc *RepoContext) Checkout(branch string) error {
	if out, err := exec.Command("git", "-C", rc.Dir, "checkout", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("checkout %s: %w\n%s", branch, err, string(out))
	}
	return nil
}

// DeleteBranch deletes a local branch with -d (safe delete, fails if unmerged).
func (rc *RepoContext) DeleteBranch(branch string) error {
	if out, err := exec.Command("git", "-C", rc.Dir, "branch", "-d", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("branch -d %s: %w\n%s", branch, err, string(out))
	}
	return nil
}

// ForceDeleteBranch deletes a local branch with -D (force delete, even if unmerged).
func (rc *RepoContext) ForceDeleteBranch(branch string) error {
	if out, err := exec.Command("git", "-C", rc.Dir, "branch", "-D", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("branch -D %s: %w\n%s", branch, err, string(out))
	}
	return nil
}

// ListBranches returns local branch names matching the given patterns
// (e.g. "feat/*", "epic/*"). Returns nil on error.
func (rc *RepoContext) ListBranches(patterns ...string) []string {
	args := []string{"-C", rc.Dir, "branch", "--list"}
	args = append(args, patterns...)
	args = append(args, "--format=%(refname:short)")
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// BranchForce creates or moves a branch to current HEAD (git branch -f).
// Best-effort (errors are silently ignored).
func (rc *RepoContext) BranchForce(branch string) {
	exec.Command("git", "-C", rc.Dir, "branch", "-f", branch).Run()
}

// --- Merge operations ---

// PullFFOnly pulls from remote with --ff-only. env overrides the process
// environment (use nil to inherit).
func (rc *RepoContext) PullFFOnly(remote, branch string, env []string) error {
	cmd := exec.Command("git", "-C", rc.Dir, "pull", "--ff-only", remote, branch)
	if env != nil {
		cmd.Env = env
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pull --ff-only %s %s: %w\n%s", remote, branch, err, string(out))
	}
	return nil
}

// MergeFFOnly performs a fast-forward-only merge of the given branch.
// env overrides the process environment (use nil to inherit).
// Returns combined output and error.
func (rc *RepoContext) MergeFFOnly(branch string, env []string) (string, error) {
	cmd := exec.Command("git", "-C", rc.Dir, "merge", "--ff-only", branch)
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --- Worktree lifecycle ---

// WorktreeAdd creates a git worktree at dir for the given branch.
// Returns combined output and error.
func (rc *RepoContext) WorktreeAdd(dir, branch string) (string, error) {
	out, err := exec.Command("git", "-C", rc.Dir, "worktree", "add", dir, branch).CombinedOutput()
	return string(out), err
}

// WorktreeAddNewBranch creates a worktree at dir on a new branch (-b) from startPoint.
// Returns combined output and error.
func (rc *RepoContext) WorktreeAddNewBranch(dir, newBranch, startPoint string) (string, error) {
	out, err := exec.Command("git", "-C", rc.Dir, "worktree", "add", "-b", newBranch, dir, startPoint).CombinedOutput()
	return string(out), err
}

// WorktreeRemove removes a worktree (force). Best-effort (errors are silently ignored).
func (rc *RepoContext) WorktreeRemove(dir string) {
	exec.Command("git", "-C", rc.Dir, "worktree", "remove", "--force", dir).Run()
}

// --- Utility functions (not repo or worktree operations) ---

// gitConfigGet reads a git config value from the local/global config chain.
// Returns empty string if the key is not set or on error.
func gitConfigGet(key string) string {
	out, err := exec.Command("git", "config", "--get", key).Output()
	if err != nil {
		// Also try without --get for simple keys (git config user.name)
		out, err = exec.Command("git", "config", key).Output()
		if err != nil {
			return ""
		}
	}
	return strings.TrimSpace(string(out))
}

// gitConfigGetGlobal reads a global git config value.
// Returns empty string if the key is not set or on error.
func gitConfigGetGlobal(key string) string {
	out, err := exec.Command("git", "config", "--global", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
