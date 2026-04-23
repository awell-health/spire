package executor

// workspace_validate.go — dispatch-time workspace validation policy.
//
// The executor validates a workspace immediately before spawning a step into
// it. Two classes of drift happen between runs: disk-state drift (the
// workspace path was removed while the branch survives) and transitional-state
// drift (a prior run crashed mid-rebase/merge, leaving rebase-merge,
// MERGE_HEAD, or a stale index.lock on disk). This policy recovers from both
// in-process so resume either proceeds cleanly or fails with an actionable
// error — "silently wedged" is not an option.
//
// Recovery primitives live in pkg/git (EnsureWorktreeAt, InspectWorkspaceState,
// AbortRebase, AbortMerge, RemoveStaleIndexLock, AttachHEAD). This file is the
// policy wrapper that sequences them, emits structured log events, and decides
// when to fail loudly vs. heal.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	spgit "github.com/awell-health/spire/pkg/git"
)

// validateWorkspaceForDispatch ensures the workspace at handle.Path is usable
// for a step spawn. It recovers from four recoverable conditions:
//
//  1. Missing path, branch still present → recreate via git worktree add.
//  2. Paused rebase (.git/rebase-merge) → git rebase --abort.
//  3. Paused merge (MERGE_HEAD present) → git merge --abort.
//  4. Stale .git/index.lock → remove.
//  5. Detached HEAD → checkout the expected branch.
//
// Fails loudly (returns error) on:
//
//   - Missing path AND missing branch → recommends spire reset --hard.
//   - Uncommitted changes in the working tree → lists them so the operator can
//     decide whether to discard. Silent stashing can lose work, so policy is
//     to refuse to proceed rather than pick for the user.
//
// stepName and stepBeadID are used as identity fields in the structured log
// events emitted on recovery. repoDir is the main repo path used by the
// worktree-recreate primitive.
func (e *Executor) validateWorkspaceForDispatch(repoDir, stepName, stepBeadID string, handle *WorkspaceHandle) error {
	if handle == nil {
		return nil
	}
	// Repo-kind workspaces are the main repo itself — no validation needed;
	// the main repo is always on its own branch and the executor does not
	// mutate it.
	if handle.Kind == WorkspaceKindRepo {
		return nil
	}
	if handle.Path == "" {
		// A workspace handle without a path cannot be validated; nothing to
		// heal and nothing to fail on. Callers should still get a usable
		// handle — upstream normalization covers the "empty path" case.
		return nil
	}

	path := handle.Path
	branch := handle.Branch

	// 1. Disk-state drift: workspace path missing.
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat workspace %s: %w", path, err)
		}
		if branch == "" {
			return fmt.Errorf("workspace %s missing and no branch recorded; run `spire reset --hard` to rebuild", path)
		}
		if ensureErr := spgit.EnsureWorktreeAt(repoDir, path, branch); ensureErr != nil {
			if errors.Is(ensureErr, spgit.ErrBranchNotFound) {
				return fmt.Errorf("workspace %s and branch %s both missing; run `spire reset --hard` to rebuild (root cause: %w)",
					path, branch, ensureErr)
			}
			return fmt.Errorf("recreate workspace %s at branch %s: %w", path, branch, ensureErr)
		}
		e.logRecoveryEvent("missing_path", stepName, stepBeadID, path, branch)
		return nil
	}

	// If the path exists but isn't a git workspace at all (no .git
	// directory or pointer), there's no transitional state to heal — it's
	// either a mock fixture, a pre-materialization placeholder, or
	// otherwise unmanaged. Skip inspection rather than converting
	// "unable-to-inspect" into a hard failure; the spawn will catch a
	// genuinely broken workspace on its own.
	if !isGitWorkspace(path) {
		return nil
	}

	// 2-5. Transitional-state drift: inspect + clean up in order. The initial
	// snapshot drives the recovery actions; a paused rebase/merge leaves the
	// tree dirty with conflict markers, so the Dirty check must wait until
	// after the aborts. Re-inspect before the Dirty gate so we see the
	// post-abort tree, not the conflict-marker tree.
	state, err := spgit.InspectWorkspaceState(path)
	if err != nil {
		return fmt.Errorf("inspect workspace state at %s: %w", path, err)
	}
	hadTransitionalOp := state.RebaseInProgress || state.MergeInProgress

	if state.RebaseInProgress {
		if abortErr := spgit.AbortRebase(path); abortErr != nil {
			return fmt.Errorf("abort paused rebase at %s: %w", path, abortErr)
		}
		e.logRecoveryEvent("rebase_aborted", stepName, stepBeadID, path, branch)
	}
	if state.MergeInProgress {
		if abortErr := spgit.AbortMerge(path); abortErr != nil {
			return fmt.Errorf("abort paused merge at %s: %w", path, abortErr)
		}
		e.logRecoveryEvent("merge_aborted", stepName, stepBeadID, path, branch)
	}
	if state.StaleIndexLock {
		if rmErr := spgit.RemoveStaleIndexLock(path); rmErr != nil {
			return fmt.Errorf("remove stale index.lock at %s: %w", path, rmErr)
		}
		e.logRecoveryEvent("stale_lock_removed", stepName, stepBeadID, path, branch)
	}
	if state.DetachedHEAD {
		if branch == "" {
			return fmt.Errorf("workspace %s has detached HEAD and no branch recorded; run `spire reset --hard` to rebuild", path)
		}
		if attachErr := spgit.AttachHEAD(path, branch); attachErr != nil {
			return fmt.Errorf("reattach HEAD at %s to %s: %w", path, branch, attachErr)
		}
		e.logRecoveryEvent("head_reattached", stepName, stepBeadID, path, branch)
	}

	// Dirty tree: refuse to proceed. Stashing silently could lose work; the
	// operator needs to know the workspace is non-empty so they can inspect
	// and either discard or commit.
	//
	// Re-inspect after transitional aborts so we're checking the post-abort
	// tree, not the one with conflict markers that the abort just reverted.
	dirtyState := state
	if hadTransitionalOp {
		fresh, rErr := spgit.InspectWorkspaceState(path)
		if rErr != nil {
			return fmt.Errorf("re-inspect workspace state at %s: %w", path, rErr)
		}
		dirtyState = fresh
	}
	if dirtyState.Dirty {
		return fmt.Errorf("workspace %s has uncommitted changes; refusing to dispatch step %s. Run `spire reset --hard` to discard, or commit/stash manually. Changes:\n%s",
			path, stepName, strings.Join(dirtyState.DirtyFiles, "\n"))
	}

	return nil
}

// logRecoveryEvent emits a structured log line describing a workspace recovery
// action. Fields align with the design spec: event, bead, step, path, branch,
// condition. When a structured logger is wired into the executor, replace the
// format-string logger with a structured emitter — the field set is already
// correct.
func (e *Executor) logRecoveryEvent(condition, stepName, stepBeadID, path, branch string) {
	e.log("event=workspace_recovered bead=%s step=%s step_bead=%s path=%s branch=%s condition=%s",
		e.beadID, stepName, stepBeadID, path, branch, condition)
}

// isGitWorkspace reports whether path contains a .git directory (main repo
// or linked-worktree gitdir) or a .git pointer file (linked worktree). The
// check is pure filesystem so a mock workspace (t.TempDir without git init)
// is cleanly skipped.
func isGitWorkspace(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}
