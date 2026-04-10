package executor

// terminal_steps.go — Terminal step enforcement for the review DAG.
//
// TerminalMerge, TerminalSplit, and TerminalDiscard enforce the branch lifecycle
// invariant from docs/review-dag.md: every path ends with the branch either
// merged to main or deleted. No hanging branches. No orphaned code.

import (
	"fmt"
	"os"
	"path/filepath"

	spgit "github.com/awell-health/spire/pkg/git"
)

// TerminalMerge implements the merge terminal step:
//
//	rebase staging onto main → build verify → ff-only merge →
//	push main → delete local+remote branch → close bead.
//
// DAG invariant: branch is deleted before bead is closed.
func TerminalMerge(beadID, branch, baseBranch, repoPath, buildCmd string, deps *Deps, log func(string, ...interface{})) error {
	mergeEnv := os.Environ()
	if tower, err := deps.ActiveTowerConfig(); err == nil && tower != nil {
		mergeEnv = deps.ArchmageGitEnv(tower)
	}

	// 1. Resume or create a StagingWorktree for the feature branch.
	//    This avoids the "branch already checked out" fatal error that occurs
	//    when CreateWorktree is called for a branch that has an existing worktree.
	wtDir := filepath.Join(repoPath, ".worktrees", beadID)
	var stagingWt *spgit.StagingWorktree

	if _, err := os.Stat(wtDir); err == nil {
		log("resuming existing worktree at %s", wtDir)
		stagingWt = spgit.ResumeStagingWorktree(repoPath, wtDir, branch, baseBranch, log)
	} else {
		archName, archEmail := ArchmageIdentity(deps)
		log("creating staging worktree at %s (branch: %s)", wtDir, branch)
		var wtErr error
		stagingWt, wtErr = spgit.NewStagingWorktreeAt(repoPath, wtDir, branch, baseBranch, archName, archEmail, log)
		if wtErr != nil {
			return fmt.Errorf("create staging worktree: %w", wtErr)
		}
	}

	// 2. Build verification in the staging worktree.
	if buildCmd != "" {
		log("verifying build on %s: %s", branch, buildCmd)
		if err := stagingWt.RunBuild(buildCmd); err != nil {
			stagingWt.Close()
			return fmt.Errorf("build failed on %s (aborting merge): %w", branch, err)
		}
	}

	// 3. Delegate merge to MergeToMain — handles ff-only, rebase fallback,
	//    and post-rebase build/test re-verification.
	log("merging %s → %s via MergeToMain", branch, baseBranch)
	if err := stagingWt.MergeToMain(baseBranch, mergeEnv, buildCmd, "", nil); err != nil {
		stagingWt.Close()
		return err
	}

	// Clean up the staging worktree after successful merge.
	stagingWt.Close()

	// 4. Push main.
	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: baseBranch, Log: log}
	log("pushing %s", baseBranch)
	if err := rc.Push("origin", baseBranch, mergeEnv); err != nil {
		return fmt.Errorf("push %s: %w", baseBranch, err)
	}

	// 5. Delete branch — MUST happen before closing bead (DAG invariant).
	log("deleting branch %s", branch)
	rc.DeleteBranch(branch)
	rc.DeleteRemoteBranch("origin", branch)

	// 6. Close bead — only reached after branch is deleted.
	deps.RemoveLabel(beadID, "review-approved")
	deps.RemoveLabel(beadID, "feat-branch:"+branch)
	deps.RemoveLabel(beadID, "phase:merge")
	if err := deps.CloseBead(beadID); err != nil {
		log("warning: close bead: %s", err)
	}

	log("terminal merge complete — branch deleted and bead closed")
	return nil
}

// TerminalSplit is the arbiter "split" terminal path.
//
// It merges approved work to main, creates child beads for the remaining work,
// and closes the original bead.
func TerminalSplit(beadID, reviewerName string, splitTasks []SplitTask, deps *Deps, log func(string, ...interface{})) error {
	log("arbiter split: merging approved work + creating %d child task(s)", len(splitTasks))

	bead, err := deps.GetBead(beadID)
	if err != nil {
		return fmt.Errorf("terminal split: get bead: %w", err)
	}

	branch := deps.HasLabel(bead, "feat-branch:")
	if branch == "" {
		if deps.ResolveBranch != nil {
			branch = deps.ResolveBranch(beadID)
		} else {
			branch = "feat/" + beadID
		}
	}

	repoPath, _, baseBranch, err := deps.ResolveRepo(beadID)
	if err != nil {
		EscalateHumanFailure(beadID, reviewerName, "repo-resolution",
			fmt.Sprintf("arbiter split: %s", err.Error()), deps)
		return nil
	}

	// Merge the staging branch to main first.
	if err := deps.ReviewHandleApproval(beadID, reviewerName, branch, baseBranch, repoPath, log); err != nil {
		return fmt.Errorf("terminal split: merge staging: %w", err)
	}

	// Create child beads for the remaining work.
	for _, task := range splitTasks {
		childID, cerr := deps.CreateBead(CreateOpts{
			Title:       task.Title,
			Description: task.Description,
			Priority:    bead.Priority,
			Type:        deps.ParseIssueType(bead.Type),
			Parent:      beadID,
		})
		if cerr != nil {
			log("warning: create split task %q: %s", task.Title, cerr)
			continue
		}
		log("created split task: %s — %s", childID, task.Title)
		deps.AddComment(beadID, fmt.Sprintf("Split task created: %s — %s", childID, task.Title))
	}

	return nil
}

// TerminalDiscard is the arbiter "discard" terminal path.
//
// It deletes the staging branch without merging, then closes the bead as wontfix.
func TerminalDiscard(beadID string, deps *Deps, log func(string, ...interface{})) error {
	log("arbiter discard: deleting branches and closing as wontfix")

	bead, err := deps.GetBead(beadID)
	if err != nil {
		return fmt.Errorf("terminal discard: get bead: %w", err)
	}

	branch := deps.HasLabel(bead, "feat-branch:")
	if branch == "" {
		if deps.ResolveBranch != nil {
			branch = deps.ResolveBranch(beadID)
		} else {
			branch = "feat/" + beadID
		}
	}

	repoPath, _, _, resolveErr := deps.ResolveRepo(beadID)
	if resolveErr != nil {
		return fmt.Errorf("discard: repo path empty for %s — branch %s left intact, bead not closed",
			beadID, branch)
	}

	rc := &spgit.RepoContext{Dir: repoPath, Log: log}

	// Delete local and remote branches BEFORE closing the bead (DAG invariant).
	log("deleting branch %s (discard)", branch)
	rc.ForceDeleteBranch(branch)
	rc.DeleteRemoteBranch("origin", branch)

	// Also delete epic branch if it exists.
	epicBranch := fmt.Sprintf("epic/%s", beadID)
	rc.ForceDeleteBranch(epicBranch)
	rc.DeleteRemoteBranch("origin", epicBranch)
	log("branches deleted")

	// Close bead as wontfix.
	deps.RemoveLabel(beadID, "feat-branch:"+branch)
	deps.AddLabel(beadID, "wontfix")
	deps.AddComment(beadID, "Arbiter: closing as wontfix — branches deleted")
	if err := deps.CloseBead(beadID); err != nil {
		return fmt.Errorf("close bead: %w", err)
	}

	log("terminal discard complete — branch deleted and bead closed as wontfix")
	return nil
}

