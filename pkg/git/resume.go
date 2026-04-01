package git

import (
	"fmt"
	"os/exec"
	"strings"
)

// ResumeWorktreeContext opens an existing worktree directory and returns a
// WorktreeContext with all fields populated. It reads the current HEAD SHA
// as StartSHA (session baseline for scoped commit detection) and, when
// branch is "", detects the checked-out branch from the worktree.
//
// The returned context does not own the worktree — the caller that created
// the worktree is responsible for its lifecycle.
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

	// Detect the checked-out branch when the caller passes "".
	if branch == "" {
		branchOut, branchErr := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if branchErr != nil {
			return nil, fmt.Errorf("ResumeWorktreeContext: detect branch: %w", branchErr)
		}
		branch = strings.TrimSpace(string(branchOut))
	}

	return &WorktreeContext{
		Dir:        dir,
		Branch:     branch,
		BaseBranch: baseBranch,
		RepoPath:   repoPath,
		StartSHA:   startSHA,
		Log:        log,
	}, nil
}
