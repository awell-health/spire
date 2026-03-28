package executor

// terminal_steps.go — Terminal step enforcement for the review DAG.
//
// TerminalMerge, TerminalSplit, and TerminalDiscard enforce the branch lifecycle
// invariant from docs/review-dag.md: every path ends with the branch either
// merged to main or deleted. No hanging branches. No orphaned code.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: baseBranch}

	// 1. Build verification on the staging/feature branch
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
		// Clean up worktree before proceeding.
		wc.Cleanup()
		os.RemoveAll(tmpDir)
		if buildErr != nil {
			return fmt.Errorf("build failed on %s (aborting merge): %w\n%s", branch, buildErr, string(out))
		}
	}

	// 2. Fetch and ff-only merge to ensure base branch is up to date.
	if err := rc.FetchWithEnv("origin", baseBranch, mergeEnv); err != nil {
		log("warning: pull %s (fetch failed): %s", baseBranch, err)
	} else if err := rc.MergeFFOnly("origin/"+baseBranch, mergeEnv); err != nil {
		log("warning: pull %s: %s", baseBranch, err)
	}

	// 3. ff-only merge; on failure, rebase staging onto main and retry.
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

		// Remove worktree before retrying merge.
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
		branch = fmt.Sprintf("feat/%s", beadID)
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
		branch = fmt.Sprintf("feat/%s", beadID)
	}

	repoPath, _, _, resolveErr := deps.ResolveRepo(beadID)
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

// ResolveBeadBuildCmd returns the build command for a bead's formula.
func ResolveBeadBuildCmd(bead Bead, resolveFormula func(Bead) (*FormulaV2, error)) string {
	f, err := resolveFormula(bead)
	if err != nil {
		return ""
	}
	if pc, ok := f.Phases["merge"]; ok && pc.Build != "" {
		return pc.Build
	}
	if pc, ok := f.Phases["implement"]; ok && pc.Build != "" {
		return pc.Build
	}
	return ""
}
