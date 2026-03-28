package executor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	spgit "github.com/awell-health/spire/pkg/git"
)

// executeMerge handles the merge phase: ff-only merge of staging branch into main.
func (e *Executor) executeMerge(pc PhaseConfig) error {
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	branch := e.deps.HasLabel(bead, "feat-branch:")
	if branch == "" {
		if e.state.StagingBranch != "" {
			branch = e.state.StagingBranch
		} else {
			branch = e.resolveBranch(e.beadID)
		}
	}

	repoPath := e.state.RepoPath
	baseBranch := e.state.BaseBranch

	// Load archmage identity for the push.
	var mergeEnv []string
	if tower, tErr := e.deps.ActiveTowerConfig(); tErr == nil && tower != nil {
		mergeEnv = e.deps.ArchmageGitEnv(tower)
	} else {
		mergeEnv = os.Environ()
	}

	// Use the single staging worktree shared across all executor phases.
	buildStr := e.resolveBuildCommand(pc)
	stagingWt, wtErr := e.ensureStagingWorktree()
	if wtErr != nil {
		return fmt.Errorf("ensure staging worktree for merge: %w", wtErr)
	}

	if buildStr != "" {
		e.log("verifying build on %s before merge: %s", branch, buildStr)
		if buildErr := stagingWt.RunBuild(buildStr); buildErr != nil {
			return fmt.Errorf("pre-merge build verification failed on %s: %w", branch, buildErr)
		}
	}

	// Review documentation for stale language before merging to main.
	if docErr := e.reviewDocsForStaleness(stagingWt.Dir, branch, baseBranch, pc); docErr != nil {
		e.log("warning: doc review: %s", docErr)
	}

	// ff-only merge into main
	e.log("merging %s → %s (local, committer: archmage)", branch, baseBranch)
	testStr := e.resolveTestCommand(pc)
	if mergeErr := stagingWt.MergeToMain(baseBranch, mergeEnv, buildStr, testStr); mergeErr != nil {
		return mergeErr
	}

	// Push main (with archmage identity)
	e.log("pushing %s", baseBranch)
	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: baseBranch}
	if pushErr := rc.Push("origin", baseBranch, mergeEnv); pushErr != nil {
		return fmt.Errorf("push %s: %w", baseBranch, pushErr)
	}

	// Clean up the feature/staging branch (best-effort)
	rc.DeleteBranch(branch)
	rc.DeleteRemoteBranch("origin", branch)

	// Close any orphan subtasks
	if children, childErr := e.deps.GetChildren(e.beadID); childErr == nil {
		for _, child := range children {
			if child.Status != "closed" {
				if err := e.deps.CloseBead(child.ID); err != nil {
					e.log("warning: close orphan subtask %s: %s", child.ID, err)
				}
			}
		}
	}

	// Close the bead
	e.deps.RemoveLabel(e.beadID, "review-approved")
	e.deps.RemoveLabel(e.beadID, "feat-branch:"+branch)
	if err := e.deps.CloseBead(e.beadID); err != nil {
		e.log("warning: close bead: %s", err)
	}
	e.log("merged and closed")
	return nil
}

// reviewDocsForStaleness checks documentation files modified on the staging branch
// for stale language and fixes them.
func (e *Executor) reviewDocsForStaleness(repoPath, branch, baseBranch string, pc PhaseConfig) error {
	wc := &spgit.WorktreeContext{Dir: repoPath}
	changedFiles, err := wc.DiffNameOnly(baseBranch)
	if err != nil {
		return fmt.Errorf("diff --name-only: %w", err)
	}

	// Filter for documentation files.
	var docFiles []string
	for _, f := range changedFiles {
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
			docFiles = append(docFiles, f)
		}
	}

	if len(docFiles) == 0 {
		e.log("no documentation files changed — skipping doc review")
		return nil
	}

	e.log("reviewing %d documentation file(s) for stale language: %s", len(docFiles), strings.Join(docFiles, ", "))

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
