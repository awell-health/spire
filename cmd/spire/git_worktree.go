package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorktreeContext is the single abstraction for all git operations inside a
// worktree. Every git command that runs in a worktree must go through this
// type so that:
//   - Dir is always used as the working directory (no accidental main-repo ops)
//   - BaseBranch is a local ref, not origin/* (worktrees don't always have origin fetched)
//   - Config is scoped with --worktree (no pollution of the main repo's .git/config)
//
// Forbidden operations (these MUST NOT exist on WorktreeContext):
//   - Checkout — worktrees don't switch branches
//   - SetGlobalConfig — use --worktree flag instead
//   - FetchOrigin — worktrees use local refs
type WorktreeContext struct {
	Dir        string // absolute path to this worktree
	Branch     string // branch checked out in this worktree
	BaseBranch string // the branch this was forked from (e.g. "main")
	RepoPath   string // the main repo (for worktree management only)
}

// Commit stages all changes and commits with the given message.
// Returns the commit SHA. If there are no staged changes after git add,
// returns ("", nil).
func (wc *WorktreeContext) Commit(msg string) (string, error) {
	// Remove prompt files before staging
	os.Remove(filepath.Join(wc.Dir, ".spire-prompt.txt"))
	os.Remove(filepath.Join(wc.Dir, ".spire-design-prompt.txt"))

	// Stage all
	if err := exec.Command("git", "-C", wc.Dir, "add", "-A").Run(); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}

	// Check if there's anything staged
	if exec.Command("git", "-C", wc.Dir, "diff", "--cached", "--quiet").Run() == nil {
		return "", nil // nothing staged
	}

	// Commit
	commitCmd := exec.Command("git", "-C", wc.Dir, "commit", "-m", msg)
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %w\n%s", err, string(out))
	}

	return wc.HeadSHA()
}

// Push pushes the worktree's branch to the given remote.
func (wc *WorktreeContext) Push(remote string) error {
	pushCmd := exec.Command("git", "-C", wc.Dir, "push", "-u", remote, wc.Branch)
	pushCmd.Env = os.Environ()
	if out, err := pushCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push %s %s: %w\n%s", remote, wc.Branch, err, string(out))
	}
	return nil
}

// HasNewCommits returns true if there are commits on HEAD that are not on
// BaseBranch. Uses local refs only — no origin/ prefix — because worktrees
// don't always have origin fetched.
func (wc *WorktreeContext) HasNewCommits() bool {
	logCmd := exec.Command("git", "-C", wc.Dir, "log", wc.BaseBranch+"..HEAD", "--oneline")
	out, _ := logCmd.Output()
	return len(strings.TrimSpace(string(out))) > 0
}

// Diff returns the diff between the given base ref and HEAD.
// For worktree use, pass wc.BaseBranch (a local ref). If you need the
// three-dot diff (merge-base), use DiffMergeBase instead.
func (wc *WorktreeContext) Diff(base string) (string, error) {
	cmd := exec.Command("git", "-C", wc.Dir, "diff", base+"..HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff %s..HEAD: %w", base, err)
	}
	return string(out), nil
}

// DiffMergeBase returns the three-dot diff (from merge-base) between base and HEAD.
func (wc *WorktreeContext) DiffMergeBase(base string) (string, error) {
	cmd := exec.Command("git", "-C", wc.Dir, "diff", base+"...HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff %s...HEAD: %w", base, err)
	}
	return string(out), nil
}

// RunCommand runs an arbitrary command string in the worktree directory.
// The command is executed via "sh -c" for shell expansion.
func (wc *WorktreeContext) RunCommand(cmdStr string) error {
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir = wc.Dir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RunCommandOutput runs a command in the worktree directory and returns combined output.
func (wc *WorktreeContext) RunCommandOutput(cmdStr string) (string, error) {
	parts := strings.Fields(cmdStr)
	if len(parts) == 0 {
		return "", nil
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = wc.Dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// HeadSHA returns the current HEAD commit SHA.
func (wc *WorktreeContext) HeadSHA() (string, error) {
	out, err := exec.Command("git", "-C", wc.Dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// HasUncommittedChanges returns true if the working tree has uncommitted changes.
func (wc *WorktreeContext) HasUncommittedChanges() bool {
	out, _ := exec.Command("git", "-C", wc.Dir, "status", "--porcelain").Output()
	return len(strings.TrimSpace(string(out))) > 0
}

// ConfigureUser sets user.name and user.email in the worktree-scoped config.
// Uses --worktree flag so the setting doesn't pollute the main repo's config.
// Also enables extensions.worktreeConfig on the main repo if needed.
func (wc *WorktreeContext) ConfigureUser(name, email string) {
	// Enable worktree-scoped config on the main repo
	exec.Command("git", "-C", wc.RepoPath, "config", "extensions.worktreeConfig", "true").Run()
	// Set user identity scoped to this worktree only
	exec.Command("git", "-C", wc.Dir, "config", "--worktree", "user.name", name).Run()
	exec.Command("git", "-C", wc.Dir, "config", "--worktree", "user.email", email).Run()
}

// Cleanup removes this worktree from git and deletes its directory.
func (wc *WorktreeContext) Cleanup() {
	if wc.Dir != "" {
		exec.Command("git", "-C", wc.RepoPath, "worktree", "remove", "--force", wc.Dir).Run()
		os.RemoveAll(wc.Dir)
	}
}

