package git

import (
	"fmt"
	"os/exec"
	"strings"
)

// ResumeWorktreeContext wraps an existing worktree directory in a WorktreeContext
// with all fields populated, including a session baseline (StartSHA). This is
// the canonical way for wizard code to obtain a WorktreeContext for an already-
// existing worktree (e.g. staging worktrees, shared review worktrees).
//
// The caller does NOT own the worktree lifecycle — Cleanup() should not be
// called on the returned context (the executor or staging manager owns it).
//
// StartSHA is captured as the current HEAD SHA at call time, providing a session
// baseline: "did THIS apprentice add a commit during THIS run?" Downstream
// callers (e.g. WizardCommit) use HasNewCommitsSinceStart() which compares
// StartSHA..HEAD instead of BaseBranch..HEAD, avoiding false positives on
// reused staging worktrees where BaseBranch..HEAD is already non-empty.
func ResumeWorktreeContext(dir, branch, baseBranch, repoPath string, log func(string, ...any)) (*WorktreeContext, error) {
	if dir == "" {
		return nil, fmt.Errorf("ResumeWorktreeContext: dir is required")
	}

	// Capture HEAD SHA as the session baseline.
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("ResumeWorktreeContext: capture HEAD: %w", err)
	}
	startSHA := strings.TrimSpace(string(out))

	return &WorktreeContext{
		Dir:        dir,
		Branch:     branch,
		BaseBranch: baseBranch,
		RepoPath:   repoPath,
		StartSHA:   startSHA,
		Log:        log,
	}, nil
}
