package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// =============================================================================
// EnsureWorktreeAt
// =============================================================================

func TestEnsureWorktreeAt_NoOpWhenPresent(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}
	rc.CreateBranch("feat/present")

	wtDir := filepath.Join(t.TempDir(), "wt")
	if _, err := rc.CreateWorktree(wtDir, "feat/present"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	// Second call should be a no-op — path exists, branch matches.
	if err := EnsureWorktreeAt(dir, wtDir, "feat/present"); err != nil {
		t.Fatalf("EnsureWorktreeAt no-op case: %v", err)
	}
}

func TestEnsureWorktreeAt_RecreatesWhenMissing(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}
	rc.CreateBranch("feat/gone")

	wtDir := filepath.Join(t.TempDir(), "wt-gone")
	if err := EnsureWorktreeAt(dir, wtDir, "feat/gone"); err != nil {
		t.Fatalf("EnsureWorktreeAt create: %v", err)
	}

	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("expected %s to exist after EnsureWorktreeAt, got %v", wtDir, err)
	}
	// Verify it's actually on the requested branch.
	cur, err := currentBranchAt(wtDir)
	if err != nil {
		t.Fatalf("currentBranchAt: %v", err)
	}
	if cur != "feat/gone" {
		t.Errorf("currentBranch = %q, want feat/gone", cur)
	}
}

func TestEnsureWorktreeAt_BranchNotFoundError(t *testing.T) {
	dir := initTestRepo(t)
	wtDir := filepath.Join(t.TempDir(), "wt-ghost")

	err := EnsureWorktreeAt(dir, wtDir, "feat/ghost")
	if err == nil {
		t.Fatal("expected error when branch does not exist")
	}
	if !errors.Is(err, ErrBranchNotFound) {
		t.Errorf("error does not wrap ErrBranchNotFound: %v", err)
	}
	if !strings.Contains(err.Error(), "feat/ghost") {
		t.Errorf("error does not mention branch name: %v", err)
	}
}

func TestEnsureWorktreeAt_EmptyPath(t *testing.T) {
	dir := initTestRepo(t)
	if err := EnsureWorktreeAt(dir, "", "main"); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestEnsureWorktreeAt_EmptyBranch(t *testing.T) {
	dir := initTestRepo(t)
	if err := EnsureWorktreeAt(dir, filepath.Join(t.TempDir(), "wt"), ""); err == nil {
		t.Error("expected error for empty branch")
	}
}

// =============================================================================
// InspectWorkspaceState
// =============================================================================

func TestInspectWorkspaceState_Clean(t *testing.T) {
	dir := initTestRepo(t)

	state, err := InspectWorkspaceState(dir)
	if err != nil {
		t.Fatalf("InspectWorkspaceState: %v", err)
	}
	if state.RebaseInProgress || state.MergeInProgress || state.StaleIndexLock || state.DetachedHEAD || state.Dirty {
		t.Errorf("expected clean state, got %+v", state)
	}
	if state.CurrentBranch != "main" {
		t.Errorf("CurrentBranch = %q, want main", state.CurrentBranch)
	}
}

func TestInspectWorkspaceState_RebaseInProgress(t *testing.T) {
	dir := initTestRepo(t)
	// Seed a rebase-merge directory inside .git — exactly what git creates when
	// a rebase pauses. InspectWorkspaceState detects it by presence.
	if err := os.MkdirAll(filepath.Join(dir, ".git", "rebase-merge"), 0755); err != nil {
		t.Fatalf("seed rebase-merge: %v", err)
	}

	state, err := InspectWorkspaceState(dir)
	if err != nil {
		t.Fatalf("InspectWorkspaceState: %v", err)
	}
	if !state.RebaseInProgress {
		t.Error("expected RebaseInProgress=true after seeding .git/rebase-merge")
	}
}

func TestInspectWorkspaceState_MergeInProgress(t *testing.T) {
	dir := initTestRepo(t)
	// MERGE_HEAD needs a valid SHA — write the current HEAD SHA.
	headSHA := strings.TrimSpace(run(t, dir, "git", "rev-parse", "HEAD"))
	writeFile(t, filepath.Join(dir, ".git", "MERGE_HEAD"), headSHA+"\n")

	state, err := InspectWorkspaceState(dir)
	if err != nil {
		t.Fatalf("InspectWorkspaceState: %v", err)
	}
	if !state.MergeInProgress {
		t.Error("expected MergeInProgress=true after seeding MERGE_HEAD")
	}
}

func TestInspectWorkspaceState_StaleIndexLock(t *testing.T) {
	dir := initTestRepo(t)
	lockPath := filepath.Join(dir, ".git", "index.lock")
	writeFile(t, lockPath, "")
	// Backdate the mtime past the staleness threshold.
	oldTime := time.Now().Add(-2 * staleIndexLockAge)
	if err := os.Chtimes(lockPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	state, err := InspectWorkspaceState(dir)
	if err != nil {
		t.Fatalf("InspectWorkspaceState: %v", err)
	}
	if !state.StaleIndexLock {
		t.Error("expected StaleIndexLock=true for aged lock")
	}
}

func TestInspectWorkspaceState_FreshLockIsNotStale(t *testing.T) {
	dir := initTestRepo(t)
	writeFile(t, filepath.Join(dir, ".git", "index.lock"), "")
	// Leave the mtime fresh.
	state, err := InspectWorkspaceState(dir)
	if err != nil {
		t.Fatalf("InspectWorkspaceState: %v", err)
	}
	if state.StaleIndexLock {
		t.Error("expected StaleIndexLock=false for fresh lock")
	}
}

func TestInspectWorkspaceState_DetachedHEAD(t *testing.T) {
	dir := initTestRepo(t)
	headSHA := strings.TrimSpace(run(t, dir, "git", "rev-parse", "HEAD"))
	// Detach HEAD by checking out the SHA directly.
	run(t, dir, "git", "checkout", "--detach", headSHA)

	state, err := InspectWorkspaceState(dir)
	if err != nil {
		t.Fatalf("InspectWorkspaceState: %v", err)
	}
	if !state.DetachedHEAD {
		t.Error("expected DetachedHEAD=true when HEAD is detached")
	}
	if state.CurrentBranch != "" {
		t.Errorf("expected empty CurrentBranch on detached HEAD, got %q", state.CurrentBranch)
	}
}

func TestInspectWorkspaceState_Dirty(t *testing.T) {
	dir := initTestRepo(t)
	writeFile(t, filepath.Join(dir, "dirty.txt"), "unstaged\n")

	state, err := InspectWorkspaceState(dir)
	if err != nil {
		t.Fatalf("InspectWorkspaceState: %v", err)
	}
	if !state.Dirty {
		t.Error("expected Dirty=true with untracked file")
	}
	if len(state.DirtyFiles) == 0 {
		t.Error("expected DirtyFiles to be populated")
	}
}

func TestInspectWorkspaceState_EmptyPath(t *testing.T) {
	if _, err := InspectWorkspaceState(""); err == nil {
		t.Error("expected error for empty path")
	}
}

// =============================================================================
// AbortRebase / AbortMerge / RemoveStaleIndexLock / AttachHEAD
// =============================================================================

func TestAbortRebase_NoOpWhenNoneInProgress(t *testing.T) {
	dir := initTestRepo(t)
	if err := AbortRebase(dir); err != nil {
		t.Fatalf("AbortRebase on clean repo should be a no-op, got %v", err)
	}
}

func TestAbortRebase_ClearsRebaseState(t *testing.T) {
	dir := initTestRepo(t)

	// Set up a paused rebase with a real conflict.
	writeFile(t, filepath.Join(dir, "conflict.txt"), "base\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "base")

	run(t, dir, "git", "checkout", "-b", "feat/rebase")
	writeFile(t, filepath.Join(dir, "conflict.txt"), "branch\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "branch")

	run(t, dir, "git", "checkout", "main")
	writeFile(t, filepath.Join(dir, "conflict.txt"), "main\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main")

	run(t, dir, "git", "checkout", "feat/rebase")
	_ = runAllow(t, dir, "git", "rebase", "main")

	// Confirm we actually paused.
	pre, _ := InspectWorkspaceState(dir)
	if !pre.RebaseInProgress {
		t.Fatalf("test setup: expected paused rebase, got %+v", pre)
	}

	if err := AbortRebase(dir); err != nil {
		t.Fatalf("AbortRebase: %v", err)
	}

	post, err := InspectWorkspaceState(dir)
	if err != nil {
		t.Fatalf("InspectWorkspaceState post-abort: %v", err)
	}
	if post.RebaseInProgress {
		t.Error("expected no rebase in progress after AbortRebase")
	}
}

func TestAbortMerge_NoOpWhenNoneInProgress(t *testing.T) {
	dir := initTestRepo(t)
	if err := AbortMerge(dir); err != nil {
		t.Fatalf("AbortMerge on clean repo should be a no-op, got %v", err)
	}
}

func TestAbortMerge_ClearsMergeState(t *testing.T) {
	dir := initTestRepo(t)

	writeFile(t, filepath.Join(dir, "m.txt"), "base\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "base")

	run(t, dir, "git", "checkout", "-b", "feat/merge")
	writeFile(t, filepath.Join(dir, "m.txt"), "branch\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "branch")

	run(t, dir, "git", "checkout", "main")
	writeFile(t, filepath.Join(dir, "m.txt"), "main\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main")

	_ = runAllow(t, dir, "git", "merge", "--no-edit", "feat/merge")

	pre, _ := InspectWorkspaceState(dir)
	if !pre.MergeInProgress {
		t.Fatalf("test setup: expected merge in progress, got %+v", pre)
	}

	if err := AbortMerge(dir); err != nil {
		t.Fatalf("AbortMerge: %v", err)
	}

	post, err := InspectWorkspaceState(dir)
	if err != nil {
		t.Fatalf("InspectWorkspaceState post-abort: %v", err)
	}
	if post.MergeInProgress {
		t.Error("expected no merge in progress after AbortMerge")
	}
}

func TestRemoveStaleIndexLock_RemovesAgedLock(t *testing.T) {
	dir := initTestRepo(t)
	lockPath := filepath.Join(dir, ".git", "index.lock")
	writeFile(t, lockPath, "")
	oldTime := time.Now().Add(-2 * staleIndexLockAge)
	os.Chtimes(lockPath, oldTime, oldTime)

	if err := RemoveStaleIndexLock(dir); err != nil {
		t.Fatalf("RemoveStaleIndexLock: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("expected %s removed, stat err=%v", lockPath, err)
	}
}

func TestRemoveStaleIndexLock_RefusesFreshLock(t *testing.T) {
	dir := initTestRepo(t)
	lockPath := filepath.Join(dir, ".git", "index.lock")
	writeFile(t, lockPath, "")

	err := RemoveStaleIndexLock(dir)
	if err == nil {
		t.Error("expected error for fresh lock")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Errorf("fresh lock should not be removed, stat err=%v", statErr)
	}
}

func TestRemoveStaleIndexLock_NoOpWhenAbsent(t *testing.T) {
	dir := initTestRepo(t)
	if err := RemoveStaleIndexLock(dir); err != nil {
		t.Errorf("RemoveStaleIndexLock on repo without lock should be no-op, got %v", err)
	}
}

func TestAttachHEAD_ReattachesDetached(t *testing.T) {
	dir := initTestRepo(t)
	headSHA := strings.TrimSpace(run(t, dir, "git", "rev-parse", "HEAD"))
	run(t, dir, "git", "checkout", "--detach", headSHA)

	pre, _ := InspectWorkspaceState(dir)
	if !pre.DetachedHEAD {
		t.Fatalf("test setup: expected detached HEAD, got %+v", pre)
	}

	if err := AttachHEAD(dir, "main"); err != nil {
		t.Fatalf("AttachHEAD: %v", err)
	}

	post, err := InspectWorkspaceState(dir)
	if err != nil {
		t.Fatalf("InspectWorkspaceState post-attach: %v", err)
	}
	if post.DetachedHEAD {
		t.Error("expected HEAD to be attached after AttachHEAD")
	}
	if post.CurrentBranch != "main" {
		t.Errorf("CurrentBranch = %q, want main", post.CurrentBranch)
	}
}

func TestAttachHEAD_NoOpWhenAlreadyAttached(t *testing.T) {
	dir := initTestRepo(t)
	if err := AttachHEAD(dir, "main"); err != nil {
		t.Errorf("AttachHEAD on already-attached repo should be no-op, got %v", err)
	}
}

func TestAttachHEAD_EmptyBranch(t *testing.T) {
	dir := initTestRepo(t)
	if err := AttachHEAD(dir, ""); err == nil {
		t.Error("expected error for empty branch")
	}
}
