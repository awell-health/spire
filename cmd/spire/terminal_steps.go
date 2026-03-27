package main

// terminal_steps.go — Terminal step enforcement for the review DAG.
//
// terminalMerge, terminalSplit and terminalDiscard enforce the branch lifecycle
// invariant from docs/review-dag.md: every path ends with the branch either
// merged to main or deleted. No hanging branches. No orphaned code.
//
// Step-graph formula types (FormulaStepGraph, StepConfig, etc.) live in formula.go.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	// 1. Build verification on the staging/feature branch before touching main.
	if buildCmd != "" {
		log("verifying build on %s: %s", branch, buildCmd)
		if out, err := exec.Command("git", "-C", repoPath, "checkout", branch).CombinedOutput(); err != nil {
			return fmt.Errorf("checkout %s for build verify: %s\n%s", branch, err, string(out))
		}
		parts := strings.Fields(buildCmd)
		buildExec := exec.Command(parts[0], parts[1:]...)
		buildExec.Dir = repoPath
		buildExec.Env = os.Environ()
		if out, err := buildExec.CombinedOutput(); err != nil {
			exec.Command("git", "-C", repoPath, "checkout", baseBranch).Run()
			return fmt.Errorf("build failed on %s (aborting merge): %w\n%s", branch, err, string(out))
		}
	}

	// 2. Checkout main and pull to ensure it is up to date.
	if out, err := exec.Command("git", "-C", repoPath, "checkout", baseBranch).CombinedOutput(); err != nil {
		return fmt.Errorf("checkout %s: %s\n%s", baseBranch, err, string(out))
	}
	pullCmd := exec.Command("git", "-C", repoPath, "pull", "--ff-only", "origin", baseBranch)
	pullCmd.Env = mergeEnv
	if out, err := pullCmd.CombinedOutput(); err != nil {
		log("warning: pull %s: %s\n%s", baseBranch, err, string(out))
	}

	// 3. ff-only merge; on failure, rebase staging onto main in a temp worktree and retry.
	ffCmd := exec.Command("git", "-C", repoPath, "merge", "--ff-only", branch)
	ffCmd.Env = mergeEnv
	if out, ffErr := ffCmd.CombinedOutput(); ffErr != nil {
		log("ff-only failed — rebasing %s onto %s in temp worktree", branch, baseBranch)
		_ = out

		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("spire-rebase-%s-", beadID))
		if err != nil {
			return fmt.Errorf("create temp dir: %w", err)
		}
		defer os.RemoveAll(tmpDir)

		wtPath := filepath.Join(tmpDir, "staging")
		if out, wtErr := exec.Command("git", "-C", repoPath, "worktree", "add", wtPath, branch).CombinedOutput(); wtErr != nil {
			return fmt.Errorf("create staging worktree: %s\n%s", wtErr, string(out))
		}
		defer exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", wtPath).Run()

		rebaseCmd := exec.Command("git", "-C", wtPath, "rebase", baseBranch)
		rebaseCmd.Env = os.Environ()
		if out, rbErr := rebaseCmd.CombinedOutput(); rbErr != nil {
			exec.Command("git", "-C", wtPath, "rebase", "--abort").Run()
			return fmt.Errorf("rebase %s onto %s failed (aborting, will not force merge): %s\n%s",
				branch, baseBranch, rbErr, string(out))
		}

		// Re-verify build after rebase.
		if buildCmd != "" {
			log("verifying build after rebase")
			parts := strings.Fields(buildCmd)
			buildAfter := exec.Command(parts[0], parts[1:]...)
			buildAfter.Dir = wtPath
			buildAfter.Env = os.Environ()
			if out, buildErr := buildAfter.CombinedOutput(); buildErr != nil {
				return fmt.Errorf("build failed after rebase (aborting merge): %w\n%s", buildErr, string(out))
			}
		}

		exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", wtPath).Run()

		log("retrying ff-only merge after rebase")
		ffCmd2 := exec.Command("git", "-C", repoPath, "merge", "--ff-only", branch)
		ffCmd2.Env = mergeEnv
		if out2, ffErr2 := ffCmd2.CombinedOutput(); ffErr2 != nil {
			return fmt.Errorf("ff-only merge failed even after rebase (will not force merge): %s\n%s",
				ffErr2, string(out2))
		}
	}

	// 4. Push main.
	log("pushing %s", baseBranch)
	pushCmd := exec.Command("git", "-C", repoPath, "push", "origin", baseBranch)
	pushCmd.Env = mergeEnv
	if out, pushErr := pushCmd.CombinedOutput(); pushErr != nil {
		return fmt.Errorf("push %s: %s\n%s", baseBranch, pushErr, string(out))
	}

	// 5. Delete branch — MUST happen before closing bead (DAG invariant).
	log("deleting branch %s", branch)
	exec.Command("git", "-C", repoPath, "branch", "-d", branch).Run()
	exec.Command("git", "-C", repoPath, "push", "origin", "--delete", branch).Run()

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

	// Delete local and remote branches BEFORE closing the bead (DAG invariant).
	log("deleting branch %s (discard)", branch)
	exec.Command("git", "-C", repoPath, "branch", "-D", branch).Run()
	exec.Command("git", "-C", repoPath, "push", "origin", "--delete", branch).Run()

	// Also delete epic branch if it exists.
	epicBranch := fmt.Sprintf("epic/%s", beadID)
	exec.Command("git", "-C", repoPath, "branch", "-D", epicBranch).Run()
	exec.Command("git", "-C", repoPath, "push", "origin", "--delete", epicBranch).Run()
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
