package executor

// workspace_validate_test.go — tests for dispatch-time workspace validation.
// The suite exercises both the disk-state drift case (missing path/branch) and
// the transitional-state cases (paused rebase, paused merge, stale lock,
// detached HEAD, dirty tree). Each test builds a real git worktree so we're
// running against real git behavior, not mocks.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newValidateTestRepo initializes a git repo with one commit on main. Returns
// the repo dir path. Used by all tests in this file.
func newValidateTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@test.com"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("init\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", "init"},
		{"branch", "-M", "main"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// runGit runs a git command in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// runGitAllow runs a git command but tolerates a non-zero exit (used for
// intentional conflicts that pause git mid-operation).
func runGitAllow(dir string, args ...string) string {
	out, _ := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out)
}

// testExecutor returns an Executor minimal enough for validateWorkspaceForDispatch
// tests. logs collects log lines so tests can assert on the recovery
// event strings; comments collects (id, text) pairs from AddComment so
// tests can assert on the comment-targeting behavior of the stash branch.
func testExecutor(t *testing.T, beadID string) (*Executor, *[]string, *[]commentRecord) {
	t.Helper()
	logs := &[]string{}
	comments := &[]commentRecord{}
	e := &Executor{
		beadID:    beadID,
		agentName: "wizard-test",
		log: func(format string, args ...interface{}) {
			*logs = append(*logs, fmt.Sprintf(format, args...))
		},
		deps: &Deps{
			AddComment: func(id, text string) error {
				*comments = append(*comments, commentRecord{ID: id, Text: text})
				return nil
			},
		},
	}
	return e, logs, comments
}

// commentRecord captures one AddComment(id, text) call so workspace_validate
// tests can assert that the stash branch posted to the source bead and
// nowhere else.
type commentRecord struct {
	ID   string
	Text string
}

// =============================================================================
// Disk-state drift (missing path / missing branch)
// =============================================================================

// TestStepDispatch_RecoversMissingWorkspace — path gone, branch still present.
// Validation recreates the worktree and emits the recovery event, then
// returns nil so the step can proceed.
func TestStepDispatch_RecoversMissingWorkspace(t *testing.T) {
	repo := newValidateTestRepo(t)
	runGit(t, repo, "branch", "feat/ghost")

	wtDir := filepath.Join(t.TempDir(), "wt-gone")
	// wtDir never exists on disk — simulate a workspace whose path was cleaned.

	e, logs, _ := testExecutor(t, "spi-bead")
	handle := &WorkspaceHandle{
		Kind:   WorkspaceKindOwnedWorktree,
		Branch: "feat/ghost",
		Path:   wtDir,
	}
	if err := e.validateWorkspaceForDispatch(repo, "implement", "step-impl", handle); err != nil {
		t.Fatalf("validateWorkspaceForDispatch: %v", err)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Errorf("expected workspace recreated at %s, got %v", wtDir, err)
	}
	foundEvent := false
	for _, l := range *logs {
		if strings.Contains(l, "event=workspace_recovered") &&
			strings.Contains(l, "condition=missing_path") &&
			strings.Contains(l, "step=implement") &&
			strings.Contains(l, "step_bead=step-impl") &&
			strings.Contains(l, "bead=spi-bead") &&
			strings.Contains(l, "branch=feat/ghost") {
			foundEvent = true
			break
		}
	}
	if !foundEvent {
		t.Errorf("expected structured recovery log, got: %v", *logs)
	}
}

// TestStepDispatch_FailsCleanlyWhenBranchGone — both the path and branch are
// missing. Validation must return an error that mentions the path, the branch,
// and points at `spire reset --hard`.
func TestStepDispatch_FailsCleanlyWhenBranchGone(t *testing.T) {
	repo := newValidateTestRepo(t)
	wtDir := filepath.Join(t.TempDir(), "wt-gone")

	e, _, _ := testExecutor(t, "spi-bead")
	handle := &WorkspaceHandle{
		Kind:   WorkspaceKindOwnedWorktree,
		Branch: "feat/ghost",
		Path:   wtDir,
	}
	err := e.validateWorkspaceForDispatch(repo, "implement", "step-impl", handle)
	if err == nil {
		t.Fatal("expected error when path and branch both missing")
	}
	msg := err.Error()
	if !strings.Contains(msg, wtDir) {
		t.Errorf("error does not mention path %q: %s", wtDir, msg)
	}
	if !strings.Contains(msg, "feat/ghost") {
		t.Errorf("error does not mention branch: %s", msg)
	}
	if !strings.Contains(msg, "reset --hard") {
		t.Errorf("error does not recommend reset --hard: %s", msg)
	}
}

// =============================================================================
// Transitional-state drift — each exercises one recoverable condition.
// =============================================================================

// TestWorkspaceValidate_RebaseInProgress — seed .git/rebase-merge/ to simulate
// an interrupted rebase, verify dispatch aborts the rebase.
func TestWorkspaceValidate_RebaseInProgress(t *testing.T) {
	repo := newValidateTestRepo(t)

	// Create a real paused rebase with a conflict — this guarantees
	// `.git/rebase-merge/` exists and that `git rebase --abort` succeeds.
	if err := os.WriteFile(filepath.Join(repo, "c.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "base")

	runGit(t, repo, "checkout", "-q", "-b", "feat/x")
	os.WriteFile(filepath.Join(repo, "c.txt"), []byte("branch\n"), 0644)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "branch")

	runGit(t, repo, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(repo, "c.txt"), []byte("main\n"), 0644)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "main")

	runGit(t, repo, "checkout", "-q", "feat/x")
	runGitAllow(repo, "rebase", "main")

	// Verify the rebase actually paused.
	if _, err := os.Stat(filepath.Join(repo, ".git", "rebase-merge")); err != nil {
		t.Fatalf("test setup: expected .git/rebase-merge, got %v", err)
	}

	e, logs, _ := testExecutor(t, "spi-bead")
	handle := &WorkspaceHandle{
		Kind:   WorkspaceKindOwnedWorktree,
		Branch: "feat/x",
		Path:   repo,
	}
	if err := e.validateWorkspaceForDispatch(repo, "implement", "step-impl", handle); err != nil {
		t.Fatalf("validateWorkspaceForDispatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "rebase-merge")); !os.IsNotExist(err) {
		t.Errorf("expected .git/rebase-merge removed after abort, stat=%v", err)
	}
	if !containsEvent(*logs, "rebase_aborted") {
		t.Errorf("expected rebase_aborted recovery log, got: %v", *logs)
	}
}

// TestWorkspaceValidate_MergeInProgress — seed MERGE_HEAD, verify
// dispatch aborts the merge.
func TestWorkspaceValidate_MergeInProgress(t *testing.T) {
	repo := newValidateTestRepo(t)

	// Create a real paused merge with a conflict.
	os.WriteFile(filepath.Join(repo, "m.txt"), []byte("base\n"), 0644)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "base")

	runGit(t, repo, "checkout", "-q", "-b", "feat/y")
	os.WriteFile(filepath.Join(repo, "m.txt"), []byte("branch\n"), 0644)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "branch")

	runGit(t, repo, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(repo, "m.txt"), []byte("main\n"), 0644)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "main")

	runGitAllow(repo, "merge", "--no-edit", "feat/y")

	if _, err := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); err != nil {
		t.Fatalf("test setup: expected MERGE_HEAD, got %v", err)
	}

	e, logs, _ := testExecutor(t, "spi-bead")
	handle := &WorkspaceHandle{
		Kind:   WorkspaceKindOwnedWorktree,
		Branch: "main",
		Path:   repo,
	}
	// Merge leaves the tree dirty after --abort of conflict markers.
	// But --abort reverts to the pre-merge state, which means the tree
	// is clean. Validate succeeds.
	if err := e.validateWorkspaceForDispatch(repo, "implement", "step-impl", handle); err != nil {
		t.Fatalf("validateWorkspaceForDispatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Errorf("expected MERGE_HEAD removed, stat=%v", err)
	}
	if !containsEvent(*logs, "merge_aborted") {
		t.Errorf("expected merge_aborted recovery log, got: %v", *logs)
	}
}

// TestWorkspaceValidate_StaleLockfile — seed an aged .git/index.lock, verify
// dispatch removes it.
func TestWorkspaceValidate_StaleLockfile(t *testing.T) {
	repo := newValidateTestRepo(t)
	lockPath := filepath.Join(repo, ".git", "index.lock")
	os.WriteFile(lockPath, nil, 0644)
	// Backdate far past staleness threshold.
	old := time.Now().Add(-5 * time.Minute)
	os.Chtimes(lockPath, old, old)

	e, logs, _ := testExecutor(t, "spi-bead")
	handle := &WorkspaceHandle{
		Kind:   WorkspaceKindOwnedWorktree,
		Branch: "main",
		Path:   repo,
	}
	if err := e.validateWorkspaceForDispatch(repo, "implement", "step-impl", handle); err != nil {
		t.Fatalf("validateWorkspaceForDispatch: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("expected %s removed, stat=%v", lockPath, err)
	}
	if !containsEvent(*logs, "stale_lock_removed") {
		t.Errorf("expected stale_lock_removed recovery log, got: %v", *logs)
	}
}

// TestWorkspaceValidate_DetachedHEAD_NoBranchName — detach HEAD, verify
// dispatch reattaches to the expected branch from the handle.
func TestWorkspaceValidate_DetachedHEAD_NoBranchName(t *testing.T) {
	repo := newValidateTestRepo(t)
	headSHA := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	runGit(t, repo, "checkout", "-q", "--detach", headSHA)

	e, logs, _ := testExecutor(t, "spi-bead")
	handle := &WorkspaceHandle{
		Kind:   WorkspaceKindOwnedWorktree,
		Branch: "main",
		Path:   repo,
	}
	if err := e.validateWorkspaceForDispatch(repo, "implement", "step-impl", handle); err != nil {
		t.Fatalf("validateWorkspaceForDispatch: %v", err)
	}

	// HEAD should now be back on main.
	branch := strings.TrimSpace(runGit(t, repo, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "main" {
		t.Errorf("expected HEAD reattached to main, got %q", branch)
	}
	if !containsEvent(*logs, "head_reattached") {
		t.Errorf("expected head_reattached recovery log, got: %v", *logs)
	}
}

// TestWorkspaceValidate_DirtyWorkingTree_AutoStashes — uncommitted changes
// present. Policy was changed for spi-wlb84w from "refuse to dispatch" to
// "auto-stash and notify": validation now runs `git stash push -u` so the
// next agent (cleric or retry) gets a clean workspace and the work is
// preserved on the stash list. The previous deadlock — apprentice dies
// dirty, cleric refuses on the same dirty tree — is broken because the
// validator stashes before any handoff.
func TestWorkspaceValidate_DirtyWorkingTree_AutoStashes(t *testing.T) {
	repo := newValidateTestRepo(t)

	// Mix of tracked-and-modified plus untracked so we exercise the `-u`
	// path the way an apprentice-half-edit would leave the tree.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# modified\n"), 0644); err != nil {
		t.Fatalf("modify README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("new\n"), 0644); err != nil {
		t.Fatalf("create untracked: %v", err)
	}

	e, logs, comments := testExecutor(t, "spi-bead")
	handle := &WorkspaceHandle{
		Kind:   WorkspaceKindOwnedWorktree,
		Branch: "main",
		Path:   repo,
	}
	if err := e.validateWorkspaceForDispatch(repo, "implement", "step-impl", handle); err != nil {
		t.Fatalf("validateWorkspaceForDispatch: dispatch should proceed after auto-stash, got %v", err)
	}

	// (a) Stash entry exists with our subject.
	stashList := runGit(t, repo, "stash", "list")
	if !strings.Contains(stashList, "spire-autoStash:spi-bead:implement") {
		t.Errorf("stash list missing entry for spi-bead/implement: %q", stashList)
	}

	// (b) Workspace is clean — both tracked-modified and untracked are gone.
	status := runGit(t, repo, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("workspace not clean after stash: %q", status)
	}
	if _, err := os.Stat(filepath.Join(repo, "untracked.txt")); !os.IsNotExist(err) {
		t.Errorf("expected untracked.txt removed by stash -u, stat=%v", err)
	}

	// (c) Comment was posted on the wizard's source bead, never on a step
	// bead or recovery bead. Acceptance criteria are explicit: cleric
	// beads are invisible in the desktop UI, so the comment must land on
	// the parent task (e.beadID).
	if len(*comments) != 1 {
		t.Fatalf("expected exactly 1 comment, got %d: %+v", len(*comments), *comments)
	}
	rec := (*comments)[0]
	if rec.ID != "spi-bead" {
		t.Errorf("comment posted on %q, want %q (the source bead, not the step or recovery bead)", rec.ID, "spi-bead")
	}
	if !strings.Contains(rec.Text, "stash@{0}") {
		t.Errorf("comment missing stash ref: %s", rec.Text)
	}
	if !strings.Contains(rec.Text, "README.md") {
		t.Errorf("comment missing README.md from file list: %s", rec.Text)
	}
	if !strings.Contains(rec.Text, "untracked.txt") {
		t.Errorf("comment missing untracked.txt from file list: %s", rec.Text)
	}
	if !strings.Contains(rec.Text, "stash list") || !strings.Contains(rec.Text, "stash pop") {
		t.Errorf("comment missing inspect/restore guidance (stash list/pop): %s", rec.Text)
	}

	// (d) Recovery event logged for observability.
	if !containsEvent(*logs, "workspace_stashed") {
		t.Errorf("expected workspace_stashed recovery log, got: %v", *logs)
	}
}

// TestWorkspaceValidate_DirtyWorkingTree_StashFailureFallsBack — when
// `git stash push` itself fails (corrupted refs / index), the validator
// must surface the original "uncommitted changes" error wrapped with the
// stash failure cause so the operator gets both signals. Acceptance:
// "If git stash itself fails (e.g. corrupted index), fall back to current
// escalation behavior with a clear error message."
func TestWorkspaceValidate_DirtyWorkingTree_StashFailureFallsBack(t *testing.T) {
	repo := newValidateTestRepo(t)

	// Leave an uncommitted change so the validator enters the stash branch.
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("unstaged\n"), 0644); err != nil {
		t.Fatalf("write dirty: %v", err)
	}

	// Pre-create refs/stash as a directory containing a child file. Git
	// stash push tries to write the ref at refs/stash, which fails because
	// a non-empty directory already lives there. This leaves `git status`
	// (read-only) working but blocks the stash push specifically.
	stashRefDir := filepath.Join(repo, ".git", "refs", "stash")
	if err := os.MkdirAll(stashRefDir, 0755); err != nil {
		t.Fatalf("mkdir refs/stash: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stashRefDir, "blocker"), []byte(""), 0644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	e, _, comments := testExecutor(t, "spi-bead")
	handle := &WorkspaceHandle{
		Kind:   WorkspaceKindOwnedWorktree,
		Branch: "main",
		Path:   repo,
	}
	err := e.validateWorkspaceForDispatch(repo, "implement", "step-impl", handle)
	if err == nil {
		t.Fatal("expected error when stash itself fails")
	}
	msg := err.Error()
	if !strings.Contains(msg, "uncommitted changes") {
		t.Errorf("error should mention uncommitted changes: %s", msg)
	}
	if !strings.Contains(msg, "stash failed") {
		t.Errorf("error should mention stash failure: %s", msg)
	}
	if !strings.Contains(msg, "dirty.txt") {
		t.Errorf("error should list the dirty file: %s", msg)
	}
	// No comment should have been posted — stash didn't succeed, so there's
	// nothing to tell the operator about restoring.
	if len(*comments) != 0 {
		t.Errorf("expected no comment on stash failure, got %d: %+v", len(*comments), *comments)
	}
}

// =============================================================================
// Boundary cases — no-ops and handle normalization.
// =============================================================================

// TestWorkspaceValidate_CleanWorkspace verifies the happy path: a clean
// worktree on the right branch produces no log lines and returns nil.
func TestWorkspaceValidate_CleanWorkspace(t *testing.T) {
	repo := newValidateTestRepo(t)

	e, logs, _ := testExecutor(t, "spi-bead")
	handle := &WorkspaceHandle{
		Kind:   WorkspaceKindOwnedWorktree,
		Branch: "main",
		Path:   repo,
	}
	if err := e.validateWorkspaceForDispatch(repo, "implement", "step-impl", handle); err != nil {
		t.Fatalf("validateWorkspaceForDispatch on clean workspace: %v", err)
	}
	for _, l := range *logs {
		if strings.Contains(l, "workspace_recovered") {
			t.Errorf("expected no recovery event on clean workspace, got %q", l)
		}
	}
}

// TestWorkspaceValidate_NilHandle — validation must not crash on a nil handle;
// the caller may legitimately have no workspace (e.g. repo-kind with path
// already populated upstream).
func TestWorkspaceValidate_NilHandle(t *testing.T) {
	e, _, _ := testExecutor(t, "spi-bead")
	if err := e.validateWorkspaceForDispatch("/tmp/anywhere", "step", "", nil); err != nil {
		t.Errorf("validateWorkspaceForDispatch nil handle: %v", err)
	}
}

// TestWorkspaceValidate_RepoKindSkipped — the main repo path is never
// validated (the executor never mutates it). Validation returns nil without
// touching disk.
func TestWorkspaceValidate_RepoKindSkipped(t *testing.T) {
	e, logs, _ := testExecutor(t, "spi-bead")
	handle := &WorkspaceHandle{
		Kind:   WorkspaceKindRepo,
		Branch: "main",
		Path:   "/does/not/exist",
	}
	// Path doesn't exist, but repo-kind skips the check entirely.
	if err := e.validateWorkspaceForDispatch("/tmp", "step", "", handle); err != nil {
		t.Errorf("expected repo-kind to skip validation, got: %v", err)
	}
	for _, l := range *logs {
		if strings.Contains(l, "workspace_recovered") {
			t.Errorf("expected no recovery event for repo-kind, got %q", l)
		}
	}
}

// containsEvent returns true if any log line contains a workspace_recovered
// event with the given condition. Centralizes the log-inspection idiom so
// assertions read uniformly.
func containsEvent(logs []string, condition string) bool {
	for _, l := range logs {
		if strings.Contains(l, "event=workspace_recovered") &&
			strings.Contains(l, "condition="+condition) {
			return true
		}
	}
	return false
}
