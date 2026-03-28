package main

// terminal_steps.go — Terminal step enforcement for the review DAG.
//
// terminalMerge, terminalSplit and terminalDiscard enforce the branch lifecycle
// invariant from docs/review-dag.md: every path ends with the branch either
// merged to main or deleted. No hanging branches. No orphaned code.
//
// Step-graph formula types (FormulaStepGraph, StepConfig, etc.) live in pkg/formula.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	spgit "github.com/awell-health/spire/pkg/git"
)

// SplitTask represents a follow-on task created when an arbiter decides to split a bead.
type SplitTask struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// terminalMerge implements the merge terminal step:
//
//	rebase staging onto main → build verify → ff-only merge →
//	push main → delete local+remote branch → close bead.
//
// DAG invariant: branch is deleted before bead is closed.
// Returns an error and leaves the bead open (branch intact) if any step fails,
// so a human can diagnose.
func terminalMerge(beadID, branch, baseBranch, repoPath, buildCmd string, log func(string, ...interface{})) error {
	mergeEnv := os.Environ()
	if tower, err := activeTowerConfig(); err == nil && tower != nil {
		mergeEnv = archmageGitEnv(tower)
	}

	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: baseBranch}

	// 1. Build verification on the staging/feature branch — use a worktree
	// instead of checking out in the main repo (fixes the checkout-in-main bug).
	if buildCmd != "" {
		log("verifying build on %s: %s", branch, buildCmd)
		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("spire-build-verify-%s-", beadID))
		if err != nil {
			return fmt.Errorf("create build verify dir: %w", err)
		}
		wtPath := filepath.Join(tmpDir, "verify")
		wc, err := rc.CreateWorktree(wtPath, branch)
		if err != nil {
			os.RemoveAll(tmpDir)
			return fmt.Errorf("create build verify worktree: %w", err)
		}
		parts := strings.Fields(buildCmd)
		buildExec := exec.Command(parts[0], parts[1:]...)
		buildExec.Dir = wc.Dir
		buildExec.Env = os.Environ()
		out, buildErr := buildExec.CombinedOutput()
		// Clean up worktree before proceeding — branch must be free for later delete.
		wc.Cleanup()
		os.RemoveAll(tmpDir)
		if buildErr != nil {
			return fmt.Errorf("build failed on %s (aborting merge): %w\n%s", branch, buildErr, string(out))
		}
	}

	// 2. Fetch and ff-only merge to ensure base branch is up to date (best-effort).
	if err := rc.FetchWithEnv("origin", baseBranch, mergeEnv); err != nil {
		log("warning: pull %s (fetch failed): %s", baseBranch, err)
	} else if err := rc.MergeFFOnly("origin/"+baseBranch, mergeEnv); err != nil {
		log("warning: pull %s: %s", baseBranch, err)
	}

	// 3. ff-only merge; on failure, rebase staging onto main in a temp worktree and retry.
	if err := rc.MergeFFOnly(branch, mergeEnv); err != nil {
		log("ff-only failed — rebasing %s onto %s in temp worktree", branch, baseBranch)

		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("spire-rebase-%s-", beadID))
		if err != nil {
			return fmt.Errorf("create temp dir: %w", err)
		}
		defer os.RemoveAll(tmpDir)

		wtPath := filepath.Join(tmpDir, "staging")
		wc, err := rc.CreateWorktree(wtPath, branch)
		if err != nil {
			return fmt.Errorf("create staging worktree: %w", err)
		}

		rebaseOut, rbErr := wc.RunCommandOutput(fmt.Sprintf("git rebase %s", baseBranch))
		if rbErr != nil {
			wc.RunCommand("git rebase --abort")
			wc.Cleanup()
			return fmt.Errorf("rebase %s onto %s failed (aborting, will not force merge): %s\n%s",
				branch, baseBranch, rbErr, rebaseOut)
		}

		// Re-verify build after rebase.
		if buildCmd != "" {
			log("verifying build after rebase")
			parts := strings.Fields(buildCmd)
			buildAfter := exec.Command(parts[0], parts[1:]...)
			buildAfter.Dir = wc.Dir
			buildAfter.Env = os.Environ()
			if out, buildErr := buildAfter.CombinedOutput(); buildErr != nil {
				wc.Cleanup()
				return fmt.Errorf("build failed after rebase (aborting merge): %w\n%s", buildErr, string(out))
			}
		}

		// Remove worktree before retrying merge — branch must be free.
		wc.Cleanup()

		log("retrying ff-only merge after rebase")
		if err := rc.MergeFFOnly(branch, mergeEnv); err != nil {
			return fmt.Errorf("ff-only merge failed even after rebase (will not force merge): %w", err)
		}
	}

	// 4. Push main.
	log("pushing %s", baseBranch)
	if err := rc.Push("origin", baseBranch, mergeEnv); err != nil {
		return fmt.Errorf("push %s: %w", baseBranch, err)
	}

	// 5. Delete branch — MUST happen before closing bead (DAG invariant).
	log("deleting branch %s", branch)
	rc.DeleteBranch(branch)
	rc.DeleteRemoteBranch("origin", branch)

	// 6. Close bead — only reached after branch is deleted.
	storeRemoveLabel(beadID, "review-approved")
	storeRemoveLabel(beadID, "feat-branch:"+branch)
	storeRemoveLabel(beadID, "phase:merge")
	if err := storeCloseBead(beadID); err != nil {
		log("warning: close bead: %s", err)
	}

	log("terminal merge complete — branch deleted and bead closed")
	return nil
}

// terminalSplit is the arbiter "split" terminal path.
//
// It merges approved work to main (via reviewHandleApproval → terminalMerge),
// creates child beads for the remaining work, and closes the original bead.
// The arbiter only chooses "split" when partial work is good — child beads are
// additive (they address gaps, not replacements).
//
// Invariant: staging branch is merged and deleted BEFORE child beads are created
// and BEFORE the original bead is closed. If the merge fails, this function
// returns an error and no child beads are created, preventing orphaned beads
// from unmerged code.
func terminalSplit(beadID, reviewerName string, splitTasks []SplitTask, log func(string, ...interface{})) error {
	log("arbiter split: merging approved work + creating %d child task(s)", len(splitTasks))

	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("terminal split: get bead: %w", err)
	}

	branch := hasLabel(bead, "feat-branch:")
	if branch == "" {
		branch = fmt.Sprintf("feat/%s", beadID)
	}

	repoPath, _, baseBranch, err := wizardResolveRepo(beadID)
	if err != nil {
		escalateHumanFailure(beadID, reviewerName, "repo-resolution",
			fmt.Sprintf("arbiter split: %s", err.Error()))
		return nil
	}

	// Merge the staging branch to main first. reviewHandleApproval handles the
	// full merge path: labels, molecule step, phase transition, terminalMerge,
	// branch delete, and bead close. If this fails, we abort before creating
	// child beads so they are never orphaned from unmerged code.
	if err := reviewHandleApproval(beadID, reviewerName, branch, baseBranch, repoPath, log); err != nil {
		return fmt.Errorf("terminal split: merge staging: %w", err)
	}

	// Create child beads for the remaining work. The original bead has been
	// closed by reviewHandleApproval → terminalMerge at this point.
	for _, task := range splitTasks {
		childID, cerr := storeCreateBead(createOpts{
			Title:       task.Title,
			Description: task.Description,
			Priority:    bead.Priority,
			Type:        parseIssueType(bead.Type),
			Parent:      beadID,
		})
		if cerr != nil {
			log("warning: create split task %q: %s", task.Title, cerr)
			continue
		}
		log("created split task: %s — %s", childID, task.Title)
		storeAddComment(beadID, fmt.Sprintf("Split task created: %s — %s", childID, task.Title))
	}

	return nil
}

// terminalDiscard is the arbiter "discard" terminal path.
//
// It deletes the staging branch (local and remote) without merging, then closes
// the bead as wontfix.
//
// DAG invariant: both local and remote branches are deleted before the bead is closed.
// Returns an error (leaving the bead open, branch intact) if repoPath cannot be resolved,
// so a human can intervene.
func terminalDiscard(beadID string, log func(string, ...interface{})) error {
	log("arbiter discard: deleting branches and closing as wontfix")

	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("terminal discard: get bead: %w", err)
	}

	branch := hasLabel(bead, "feat-branch:")
	if branch == "" {
		branch = fmt.Sprintf("feat/%s", beadID)
	}

	repoPath, _, _, resolveErr := wizardResolveRepo(beadID)
	if resolveErr != nil {
		return fmt.Errorf("discard: repo path empty for %s — branch %s left intact, bead not closed",
			beadID, branch)
	}

	rc := &spgit.RepoContext{Dir: repoPath}

	// Delete local and remote branches BEFORE closing the bead (DAG invariant).
	log("deleting branch %s (discard)", branch)
	rc.ForceDeleteBranch(branch)
	rc.DeleteRemoteBranch("origin", branch)

	// Also delete epic branch if it exists.
	epicBranch := fmt.Sprintf("epic/%s", beadID)
	rc.ForceDeleteBranch(epicBranch)
	rc.DeleteRemoteBranch("origin", epicBranch)
	log("branches deleted")

	// Close bead as wontfix — only reached after branch deletion attempted.
	storeRemoveLabel(beadID, "feat-branch:"+branch)
	storeAddLabel(beadID, "wontfix")
	storeAddComment(beadID, "Arbiter: closing as wontfix — branches deleted")
	if err := storeCloseBead(beadID); err != nil {
		return fmt.Errorf("close bead: %w", err)
	}

	log("terminal discard complete — branch deleted and bead closed as wontfix")
	return nil
}

// resolveBeadBuildCmd returns the build command for a bead's formula.
// Checks the merge phase config first, then the implement phase config.
// Returns "" if no build command is configured.
func resolveBeadBuildCmd(bead Bead) string {
	formula, err := ResolveFormula(bead)
	if err != nil {
		return ""
	}
	if pc, ok := formula.Phases["merge"]; ok && pc.Build != "" {
		return pc.Build
	}
	if pc, ok := formula.Phases["implement"]; ok && pc.Build != "" {
		return pc.Build
	}
	return ""
}
