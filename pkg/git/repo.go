package git

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RepoContext is the single abstraction for all git operations on the main
// repository (branch management, worktree lifecycle, merge, push). Every git
// command that targets the main repo must go through this type so that Dir is
// always explicit — no accidental cwd-dependent operations.
//
// RepoContext owns the worktree lifecycle: CreateWorktree produces a
// *WorktreeContext, establishing the ownership chain.
type RepoContext struct {
	Dir        string // absolute path to main repo root
	BaseBranch string // e.g. "main"
}

// git builds an exec.Cmd for a git command rooted at rc.Dir.
// All RepoContext git calls flow through this helper.
func (rc *RepoContext) git(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = rc.Dir
	return cmd
}

// CreateBranch creates a new local branch.
func (rc *RepoContext) CreateBranch(name string) error {
	if out, err := rc.git("branch", name).CombinedOutput(); err != nil {
		return fmt.Errorf("git branch %s: %w\n%s", name, err, out)
	}
	return nil
}

// DeleteBranch deletes a local branch (soft delete, fails if not fully merged).
func (rc *RepoContext) DeleteBranch(name string) error {
	if out, err := rc.git("branch", "-d", name).CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -d %s: %w\n%s", name, err, out)
	}
	return nil
}

// ForceDeleteBranch force-deletes a local branch regardless of merge status.
func (rc *RepoContext) ForceDeleteBranch(name string) error {
	if out, err := rc.git("branch", "-D", name).CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -D %s: %w\n%s", name, err, out)
	}
	return nil
}

// DeleteRemoteBranch deletes a branch on the given remote.
func (rc *RepoContext) DeleteRemoteBranch(remote, name string) error {
	if out, err := rc.git("push", remote, "--delete", name).CombinedOutput(); err != nil {
		return fmt.Errorf("git push %s --delete %s: %w\n%s", remote, name, err, out)
	}
	return nil
}

// BranchExists returns true if a local branch with the given name exists.
func (rc *RepoContext) BranchExists(name string) bool {
	out, _ := rc.git("branch", "--list", name).Output()
	return strings.TrimSpace(string(out)) != ""
}

// ListBranches returns local branch names matching the given pattern.
func (rc *RepoContext) ListBranches(pattern string) []string {
	out, _ := rc.git("branch", "--list", pattern).Output()
	lines := strings.Split(string(out), "\n")
	var branches []string
	for _, line := range lines {
		// Trim the "* " prefix on the current branch and any whitespace
		b := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "* "))
		if b != "" {
			branches = append(branches, b)
		}
	}
	return branches
}

// CreateWorktree creates a new git worktree at dir on the given branch,
// returning a WorktreeContext for operations within it.
func (rc *RepoContext) CreateWorktree(dir, branch string) (*WorktreeContext, error) {
	if out, err := rc.git("worktree", "add", dir, branch).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git worktree add %s %s: %w\n%s", dir, branch, err, out)
	}
	return &WorktreeContext{
		Dir:        dir,
		Branch:     branch,
		BaseBranch: rc.BaseBranch,
		RepoPath:   rc.Dir,
	}, nil
}

// RemoveWorktree removes a git worktree at the given directory.
func (rc *RepoContext) RemoveWorktree(dir string) error {
	if out, err := rc.git("worktree", "remove", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove %s: %w\n%s", dir, err, out)
	}
	return nil
}

// MergeFFOnly performs a fast-forward-only merge of the given branch.
// Extra environment variables can be passed via env (e.g. GIT_AUTHOR_*).
func (rc *RepoContext) MergeFFOnly(branch string, env []string) error {
	cmd := rc.git("merge", "--ff-only", branch)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git merge --ff-only %s: %w\n%s", branch, err, out)
	}
	return nil
}

// Push pushes a branch to the given remote.
// Extra environment variables can be passed via env (e.g. for auth tokens).
func (rc *RepoContext) Push(remote, branch string, env []string) error {
	cmd := rc.git("push", remote, branch)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push %s %s: %w\n%s", remote, branch, err, out)
	}
	return nil
}

// RemoteURL returns the URL for the given remote, or "" if it cannot be determined.
func (rc *RepoContext) RemoteURL(remote string) string {
	out, err := rc.git("remote", "get-url", remote).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// CurrentBranch returns the name of the currently checked-out branch.
func (rc *RepoContext) CurrentBranch() string {
	out, _ := rc.git("rev-parse", "--abbrev-ref", "HEAD").Output()
	return strings.TrimSpace(string(out))
}

// HeadSHA returns the full SHA of the current HEAD commit.
func (rc *RepoContext) HeadSHA() string {
	out, _ := rc.git("rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(out))
}

// Checkout checks out the given branch in the main repo.
func (rc *RepoContext) Checkout(branch string) error {
	if out, err := rc.git("checkout", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout %s: %w\n%s", branch, err, out)
	}
	return nil
}

// Fetch fetches a ref from the given remote.
func (rc *RepoContext) Fetch(remote, ref string) error {
	if out, err := rc.git("fetch", remote, ref).CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch %s %s: %w\n%s", remote, ref, err, out)
	}
	return nil
}

// FetchWithEnv fetches a ref from the given remote, using additional env vars.
func (rc *RepoContext) FetchWithEnv(remote, ref string, env []string) error {
	cmd := rc.git("fetch", remote, ref)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch %s %s: %w\n%s", remote, ref, err, out)
	}
	return nil
}

// PullFFOnly pulls a branch from the given remote using fast-forward only.
func (rc *RepoContext) PullFFOnly(remote, branch string, env []string) error {
	cmd := rc.git("pull", "--ff-only", remote, branch)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git pull --ff-only %s %s: %w\n%s", remote, branch, err, out)
	}
	return nil
}

// ForceRemoveWorktree force-removes a git worktree at the given directory.
func (rc *RepoContext) ForceRemoveWorktree(dir string) error {
	if out, err := rc.git("worktree", "remove", "--force", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove --force %s: %w\n%s", dir, err, out)
	}
	return nil
}

// CreateWorktreeNewBranch creates a new branch from startPoint and a worktree at dir,
// returning a WorktreeContext for operations within it.
func (rc *RepoContext) CreateWorktreeNewBranch(dir, newBranch, startPoint string) (*WorktreeContext, error) {
	if out, err := rc.git("worktree", "add", "-b", newBranch, dir, startPoint).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git worktree add -b %s %s %s: %w\n%s", newBranch, dir, startPoint, err, out)
	}
	return &WorktreeContext{
		Dir:        dir,
		Branch:     newBranch,
		BaseBranch: rc.BaseBranch,
		RepoPath:   rc.Dir,
	}, nil
}

// ForceBranch creates or resets a branch to current HEAD (git branch -f).
func (rc *RepoContext) ForceBranch(name string) error {
	if out, err := rc.git("branch", "-f", name).CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -f %s: %w\n%s", name, err, out)
	}
	return nil
}

// ConfigGet reads a git config value. Pass extra args like "--global" before the key.
// Returns "" if the key is not set or git fails.
func ConfigGet(args ...string) string {
	cmdArgs := append([]string{"config"}, args...)
	out, err := exec.Command("git", cmdArgs...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
