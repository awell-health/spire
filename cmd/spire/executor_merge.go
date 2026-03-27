package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// executeMerge handles the merge phase: ff-only merge of staging branch into main.
// If main has moved ahead, it rebases the staging branch onto main in a temporary
// worktree, re-verifies the build, then retries the ff-only merge. Never force merges.
func (e *formulaExecutor) executeMerge(pc PhaseConfig) error {
	bead, err := storeGetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	branch := hasLabel(bead, "feat-branch:")
	if branch == "" {
		if e.state.StagingBranch != "" {
			branch = e.state.StagingBranch
		} else {
			branch = fmt.Sprintf("feat/%s", e.beadID)
		}
	}

	repoPath := e.state.RepoPath
	baseBranch := e.state.BaseBranch

	// Load archmage identity for the push.
	var mergeEnv []string
	if tower, tErr := activeTowerConfig(); tErr == nil && tower != nil {
		mergeEnv = archmageGitEnv(tower)
	} else {
		mergeEnv = os.Environ()
	}

	// Run build verification and doc review in a staging worktree — never checkout
	// branches in the main worktree.
	buildStr := e.resolveBuildCommand(pc)
	mergeWt, mergeWtErr := NewStagingWorktree(repoPath, branch, baseBranch, fmt.Sprintf("spire-merge-%s", e.beadID), e.log)
	if mergeWtErr != nil {
		return fmt.Errorf("create merge worktree for %s: %w", branch, mergeWtErr)
	}
	defer mergeWt.Close()

	if buildStr != "" {
		e.log("verifying build on %s before merge: %s", branch, buildStr)
		if buildErr := mergeWt.RunBuild(buildStr); buildErr != nil {
			return fmt.Errorf("pre-merge build verification failed on %s: %w", branch, buildErr)
		}
	}

	// Review documentation for stale language before merging to main.
	if docErr := e.reviewDocsForStaleness(mergeWt.Dir, branch, baseBranch, pc); docErr != nil {
		e.log("warning: doc review: %s", docErr)
	}

	// ff-only merge into main, with rebase fallback if main has diverged.
	// Build and test are re-verified after rebase using the same commands.
	// MergeToMain handles all git checkout and worktree operations internally.
	e.log("merging %s → %s (local, committer: archmage)", branch, baseBranch)
	testStr := e.resolveTestCommand(pc)
	if mergeErr := mergeWt.MergeToMain(baseBranch, mergeEnv, buildStr, testStr); mergeErr != nil {
		return mergeErr
	}

	// Push main (with archmage identity)
	e.log("pushing %s", baseBranch)
	pushCmd := exec.Command("git", "-C", repoPath, "push", "origin", baseBranch)
	pushCmd.Env = mergeEnv
	if out, pushErr := pushCmd.CombinedOutput(); pushErr != nil {
		return fmt.Errorf("push %s: %s\n%s", baseBranch, pushErr, string(out))
	}

	// Clean up the feature/staging branch
	exec.Command("git", "-C", repoPath, "branch", "-d", branch).Run()
	exec.Command("git", "-C", repoPath, "push", "origin", "--delete", branch).Run()

	// Close any orphan subtasks that were not closed by the wave (e.g. skipped or failed).
	if children, childErr := storeGetChildren(e.beadID); childErr == nil {
		for _, child := range children {
			if child.Status != "closed" {
				if err := storeCloseBead(child.ID); err != nil {
					e.log("warning: close orphan subtask %s: %s", child.ID, err)
				}
			}
		}
	}

	// Close the bead
	storeRemoveLabel(e.beadID, "review-approved")
	storeRemoveLabel(e.beadID, "feat-branch:"+branch)
	if err := storeCloseBead(e.beadID); err != nil {
		e.log("warning: close bead: %s", err)
	}
	e.log("merged and closed")
	return nil
}

// reviewDocsForStaleness checks documentation files modified on the staging branch
// for stale language ("planned", "TODO", "not yet implemented", "will be") that
// refers to functionality now present in the merged code. If stale docs are found,
// Claude fixes them and commits the changes on the staging branch.
func (e *formulaExecutor) reviewDocsForStaleness(repoPath, branch, baseBranch string, pc PhaseConfig) error {
	// repoPath should be a worktree already on the staging branch.
	// Find files changed relative to the base branch.
	diffCmd := exec.Command("git", "-C", repoPath, "diff", baseBranch, "--name-only")
	diffOut, err := diffCmd.Output()
	if err != nil {
		return fmt.Errorf("git diff --name-only: %w", err)
	}

	// Filter for documentation files.
	var docFiles []string
	for _, f := range strings.Split(strings.TrimSpace(string(diffOut)), "\n") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		base := strings.ToUpper(filepath.Base(f))
		switch {
		case base == "README.MD":
			docFiles = append(docFiles, f)
		case base == "PLAYBOOK.MD":
			docFiles = append(docFiles, f)
		case base == "ARCHITECTURE.MD":
			docFiles = append(docFiles, f)
		case base == "VISION.MD":
			docFiles = append(docFiles, f)
		case base == "PLAN.MD":
			docFiles = append(docFiles, f)
		case base == "LOCAL.MD":
			docFiles = append(docFiles, f)
		case base == "CLAUDE.MD":
			docFiles = append(docFiles, f)
		case strings.HasSuffix(strings.ToLower(f), ".md") && strings.Contains(strings.ToLower(filepath.Dir(f)), "doc"):
			// Any .md file under a docs/ directory
			docFiles = append(docFiles, f)
		}
	}

	if len(docFiles) == 0 {
		e.log("no documentation files changed — skipping doc review")
		return nil
	}

	e.log("reviewing %d documentation file(s) for stale language: %s", len(docFiles), strings.Join(docFiles, ", "))

	// Build a prompt that asks Claude to review and fix stale language.
	prompt := fmt.Sprintf(`You are reviewing documentation files after code branches have been merged into a staging branch. Parallel workers wrote these docs against pre-merge code. Some docs may say "planned", "TODO", "not yet implemented", "will be added", "coming soon", or similar language for features that NOW EXIST in the merged code.

Your job:
1. Read each documentation file listed below.
2. For each file, check if it contains stale language — phrases like "planned", "TODO", "not yet implemented", "will be", "coming soon", "future work", "not yet supported" — that refers to functionality that is NOW present in the codebase.
3. To determine what is actually implemented, look at the actual source code files (not just docs).
4. If you find stale language, fix it to reflect the current state of the code. Change "will be implemented" to "is implemented", remove "TODO" items that are done, etc.
5. If no fixes are needed, do nothing — do NOT make unnecessary changes.
6. If you made any changes, stage them with git add and commit with the message: docs: fix stale documentation after merge

Documentation files to review:
%s

IMPORTANT: Only fix genuinely stale language where the described feature now exists in code. Do NOT remove TODOs for things that are actually still pending. Be conservative — when in doubt, leave it alone.`, strings.Join(docFiles, "\n"))

	model := pc.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	cmd := exec.Command("claude",
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
		"--output-format", "text",
		"--max-turns", "3",
	)
	cmd.Dir = repoPath
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude doc review: %w", err)
	}

	e.log("documentation review complete")
	return nil
}
