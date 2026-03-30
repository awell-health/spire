package git

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
type WorktreeContext struct {
	Dir        string              // absolute path to this worktree
	Branch     string              // branch checked out in this worktree
	BaseBranch string              // the branch this was forked from (e.g. "main")
	RepoPath   string              // the main repo (for worktree management only)
	Log        func(string, ...any) // optional structured logger; nil = silent
}

// logf logs a message if a logger is set. Nil-safe.
func (wc *WorktreeContext) logf(format string, args ...any) {
	if wc.Log != nil {
		wc.Log(format, args...)
	}
}

// Commit stages all changes and commits with the given message.
// Returns the commit SHA. If there are no staged changes after git add,
// returns ("", nil).
//
// Before staging, it removes any files matching the patterns in cleanFiles
// (e.g. prompt files that should not be committed). Pass nil to skip cleanup.
func (wc *WorktreeContext) Commit(msg string, cleanFiles ...string) (string, error) {
	// Remove specified files before staging
	for _, f := range cleanFiles {
		os.Remove(filepath.Join(wc.Dir, f))
	}

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

	sha, err := wc.HeadSHA()
	if err == nil {
		wc.logf("committed %s on branch %s in %s", sha, wc.Branch, wc.Dir)
	}
	return sha, err
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
// The command is executed via "sh -c" for shell expansion, consistent with RunCommand.
func (wc *WorktreeContext) RunCommandOutput(cmdStr string) (string, error) {
	cmd := exec.Command("sh", "-c", cmdStr)
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

// Merge attempts to merge the given ref into the worktree's current branch.
// Uses --no-edit to avoid opening an editor. Returns the combined output and
// any error from the merge command.
func (wc *WorktreeContext) Merge(ref string) (string, error) {
	out, err := exec.Command("git", "-C", wc.Dir, "merge", "--no-edit", ref).CombinedOutput()
	return string(out), err
}

// MergeFFOnly performs a fast-forward-only merge of the given ref.
// Returns an error if the merge cannot be completed as a fast-forward.
func (wc *WorktreeContext) MergeFFOnly(ref string) error {
	out, err := exec.Command("git", "-C", wc.Dir, "merge", "--ff-only", ref).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git merge --ff-only %s: %w\n%s", ref, err, string(out))
	}
	wc.logf("ff-only merge %s into %s", ref, wc.Branch)
	return nil
}

// MergeAbort aborts an in-progress merge. Safe to call even if no merge is active.
func (wc *WorktreeContext) MergeAbort() {
	exec.Command("git", "-C", wc.Dir, "merge", "--abort").Run()
}

// StatusPorcelain returns the machine-readable status output (git status --porcelain).
func (wc *WorktreeContext) StatusPorcelain() string {
	out, _ := exec.Command("git", "-C", wc.Dir, "status", "--porcelain").Output()
	return string(out)
}

// EnsureRemoteRef fetches a ref from a remote so it's available in this worktree.
// Worktrees share refs with the main repo, so the fetch runs against RepoPath.
// This is the ONLY operation that touches the remote — all other methods use local refs.
func (wc *WorktreeContext) EnsureRemoteRef(remote, ref string) {
	exec.Command("git", "-C", wc.RepoPath, "fetch", remote, ref).Run()
}

// ConflictedFiles returns the list of files with unresolved merge conflicts.
func (wc *WorktreeContext) ConflictedFiles() ([]string, error) {
	out, err := exec.Command("git", "-C", wc.Dir, "diff", "--name-only", "--diff-filter=U").Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --diff-filter=U: %w", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// CommitMerge commits an in-progress merge (after conflict resolution) using
// the default merge message. Equivalent to "git commit --no-edit".
func (wc *WorktreeContext) CommitMerge() error {
	if out, err := exec.Command("git", "-C", wc.Dir, "commit", "--no-edit").CombinedOutput(); err != nil {
		return fmt.Errorf("git commit --no-edit: %w\n%s", err, out)
	}
	return nil
}

// DiffNameOnly returns the list of file paths changed between the given ref and HEAD.
func (wc *WorktreeContext) DiffNameOnly(ref string) ([]string, error) {
	out, err := exec.Command("git", "-C", wc.Dir, "diff", ref, "--name-only").Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s --name-only: %w", ref, err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// Cleanup removes this worktree from git and deletes its directory.
func (wc *WorktreeContext) Cleanup() {
	if wc.Dir != "" {
		exec.Command("git", "-C", wc.RepoPath, "worktree", "remove", "--force", wc.Dir).Run()
		os.RemoveAll(wc.Dir)
	}
}
