package git

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResumeWorktreeContext_CapturesStartSHA(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create a worktree on a feature branch.
	rc.CreateBranch("feat/resume")
	wtDir := filepath.Join(t.TempDir(), "resume-wt")
	_, err := rc.CreateWorktree(wtDir, "feat/resume")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// Get the HEAD SHA of the worktree before resuming.
	expectedSHA := trimNewline(run(t, wtDir, "git", "rev-parse", "HEAD"))

	// Resume the worktree.
	wc, err := ResumeWorktreeContext(wtDir, "feat/resume", "main", dir, nil)
	if err != nil {
		t.Fatalf("ResumeWorktreeContext: %v", err)
	}

	if wc.StartSHA != expectedSHA {
		t.Errorf("StartSHA = %q, want %q", wc.StartSHA, expectedSHA)
	}
	if wc.Dir != wtDir {
		t.Errorf("Dir = %q, want %q", wc.Dir, wtDir)
	}
	if wc.Branch != "feat/resume" {
		t.Errorf("Branch = %q, want feat/resume", wc.Branch)
	}
	if wc.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", wc.BaseBranch)
	}
	if wc.RepoPath != dir {
		t.Errorf("RepoPath = %q, want %q", wc.RepoPath, dir)
	}
}

func TestResumeWorktreeContext_EmptyDirErrors(t *testing.T) {
	_, err := ResumeWorktreeContext("", "branch", "main", "/repo", nil)
	if err == nil {
		t.Error("expected error for empty dir, got nil")
	}
}

func TestResumeWorktreeContext_InvalidDirErrors(t *testing.T) {
	_, err := ResumeWorktreeContext("/nonexistent/path", "branch", "main", "/repo", nil)
	if err == nil {
		t.Error("expected error for invalid dir, got nil")
	}
}

func TestHasNewCommitsSinceStart_SessionBaseline(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create a worktree on a feature branch.
	rc.CreateBranch("feat/session")
	wtDir := filepath.Join(t.TempDir(), "session-wt")
	wc, err := rc.CreateWorktree(wtDir, "feat/session")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	wc.ConfigureUser("Test", "test@test.com")

	// Add a commit BEFORE resuming (simulates pre-existing staging commits).
	writeFile(t, filepath.Join(wtDir, "pre-existing.txt"), "pre\n")
	run(t, wtDir, "git", "add", "-A")
	run(t, wtDir, "git", "commit", "-m", "pre-existing commit")

	// Now BaseBranch..HEAD is non-empty.
	hasNew, err := wc.HasNewCommits()
	if err != nil {
		t.Fatalf("HasNewCommits error: %v", err)
	}
	if !hasNew {
		t.Fatal("precondition: expected HasNewCommits=true after pre-existing commit")
	}

	// Resume the worktree — captures HEAD after the pre-existing commit.
	resumed, err := ResumeWorktreeContext(wtDir, "feat/session", "main", dir, nil)
	if err != nil {
		t.Fatalf("ResumeWorktreeContext: %v", err)
	}

	// StartSHA..HEAD should have NO new commits (we just captured HEAD).
	hasNewSession, err := resumed.HasNewCommitsSinceStart()
	if err != nil {
		t.Fatalf("HasNewCommitsSinceStart error: %v", err)
	}
	if hasNewSession {
		t.Error("expected HasNewCommitsSinceStart=false immediately after resume (no new commits in this session)")
	}

	// Now add a commit IN THIS SESSION.
	resumed.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "session-work.txt"), "session\n")
	run(t, wtDir, "git", "add", "-A")
	run(t, wtDir, "git", "commit", "-m", "session commit")

	// StartSHA..HEAD should now show the session commit.
	hasNewSession, err = resumed.HasNewCommitsSinceStart()
	if err != nil {
		t.Fatalf("HasNewCommitsSinceStart error: %v", err)
	}
	if !hasNewSession {
		t.Error("expected HasNewCommitsSinceStart=true after session commit")
	}
}

func TestHasNewCommitsSinceStart_FallbackToBaseBranch(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create a worktree on a feature branch.
	rc.CreateBranch("feat/fallback")
	wtDir := filepath.Join(t.TempDir(), "fallback-wt")
	wc, err := rc.CreateWorktree(wtDir, "feat/fallback")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// No StartSHA — should fall back to BaseBranch.
	wc.StartSHA = ""

	// Initially no new commits.
	hasNew, err := wc.HasNewCommitsSinceStart()
	if err != nil {
		t.Fatalf("HasNewCommitsSinceStart error: %v", err)
	}
	if hasNew {
		t.Error("expected no new commits initially with fallback to BaseBranch")
	}

	// Add a commit.
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "new.txt"), "new\n")
	_, commitErr := wc.Commit("new commit")
	if commitErr != nil {
		t.Fatalf("Commit: %v", commitErr)
	}

	hasNew, err = wc.HasNewCommitsSinceStart()
	if err != nil {
		t.Fatalf("HasNewCommitsSinceStart error: %v", err)
	}
	if !hasNew {
		t.Error("expected new commits after committing with fallback to BaseBranch")
	}
}

func TestHasNewCommitsSinceStart_NoBaselineNoBaseBranch(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("feat/nobase")
	wtDir := filepath.Join(t.TempDir(), "nobase-wt")
	wc, err := rc.CreateWorktree(wtDir, "feat/nobase")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// Clear both StartSHA and BaseBranch.
	wc.StartSHA = ""
	wc.BaseBranch = ""

	_, err = wc.HasNewCommitsSinceStart()
	if err == nil {
		t.Error("expected error when neither StartSHA nor BaseBranch is set")
	}
}

// TestResumeStagingWorktree_CapturesStartSHA verifies that ResumeStagingWorktree
// sets StartSHA from the current HEAD.
func TestResumeStagingWorktree_CapturesStartSHA(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create a feature branch and worktree.
	rc.CreateBranch("staging/test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	_, err := rc.CreateWorktree(wtDir, "staging/test")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// Add a pre-existing commit in the staging worktree.
	run(t, wtDir, "git", "config", "user.name", "Test")
	run(t, wtDir, "git", "config", "user.email", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "stage.txt"), "staged\n")
	run(t, wtDir, "git", "add", "-A")
	run(t, wtDir, "git", "commit", "-m", "staging commit")

	expectedSHA := trimNewline(run(t, wtDir, "git", "rev-parse", "HEAD"))

	// Resume the staging worktree.
	sw := ResumeStagingWorktree(dir, wtDir, "staging/test", "main", nil)

	if sw.StartSHA != expectedSHA {
		t.Errorf("StartSHA = %q, want %q", sw.StartSHA, expectedSHA)
	}

	// No new commits in this session yet.
	hasNew, err := sw.HasNewCommitsSinceStart()
	if err != nil {
		t.Fatalf("HasNewCommitsSinceStart error: %v", err)
	}
	if hasNew {
		t.Error("expected no new commits after resume")
	}
}

// TestNoOpInStaging_ReportsNoChanges is the core regression test:
// On a staging worktree with pre-existing commits, a no-op apprentice run
// (no new commits in THIS session) must report no_changes, not success.
func TestNoOpInStaging_ReportsNoChanges(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create and populate a staging branch with pre-existing work.
	rc.CreateBranch("staging/noop")
	wtDir := filepath.Join(t.TempDir(), "noop-wt")
	_, err := rc.CreateWorktree(wtDir, "staging/noop")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	run(t, wtDir, "git", "config", "user.name", "Test")
	run(t, wtDir, "git", "config", "user.email", "test@test.com")

	// Pre-existing commit (from a previous wave merge).
	writeFile(t, filepath.Join(wtDir, "wave1.txt"), "wave 1 work\n")
	run(t, wtDir, "git", "add", "-A")
	run(t, wtDir, "git", "commit", "-m", "wave merge: task 1")

	// Resume the worktree — this is what the fix apprentice would do.
	resumed, err := ResumeWorktreeContext(wtDir, "staging/noop", "main", dir, nil)
	if err != nil {
		t.Fatalf("ResumeWorktreeContext: %v", err)
	}

	// Verify: no uncommitted changes, no NEW session commits.
	if resumed.HasUncommittedChanges() {
		t.Fatal("precondition: expected no uncommitted changes")
	}
	hasNew, err := resumed.HasNewCommitsSinceStart()
	if err != nil {
		t.Fatalf("HasNewCommitsSinceStart: %v", err)
	}
	if hasNew {
		t.Fatal("precondition: expected no new session commits")
	}

	// The OLD behavior: BaseBranch..HEAD would show the pre-existing commit
	// and incorrectly report "Claude already committed." Verify this:
	hasOld, err := resumed.HasNewCommits()
	if err != nil {
		t.Fatalf("HasNewCommits: %v", err)
	}
	if !hasOld {
		t.Fatal("precondition: HasNewCommits (BaseBranch..HEAD) should be true for pre-existing commits")
	}

	// The correct behavior: HasNewCommitsSinceStart returns false,
	// so any commit check logic should report no_changes.
	if hasNew {
		t.Error("HasNewCommitsSinceStart should return false for a no-op session in staging")
	}
}

// TestResumeWorktreeContext_DetectsBranch verifies that passing "" for branch
// causes ResumeWorktreeContext to read the checked-out branch from the worktree.
func TestResumeWorktreeContext_DetectsBranch(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("staging/detect-me")
	wtDir := filepath.Join(t.TempDir(), "detect-wt")
	_, err := rc.CreateWorktree(wtDir, "staging/detect-me")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// Resume with "" for branch — should detect "staging/detect-me".
	wc, err := ResumeWorktreeContext(wtDir, "", "main", dir, nil)
	if err != nil {
		t.Fatalf("ResumeWorktreeContext: %v", err)
	}

	if wc.Branch != "staging/detect-me" {
		t.Errorf("Branch = %q, want %q", wc.Branch, "staging/detect-me")
	}
}

// TestResumeWorktreeContext_ExplicitBranchPreserved verifies that an explicit
// branch argument is used as-is (no detection override).
func TestResumeWorktreeContext_ExplicitBranchPreserved(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("staging/explicit")
	wtDir := filepath.Join(t.TempDir(), "explicit-wt")
	_, err := rc.CreateWorktree(wtDir, "staging/explicit")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// Resume with explicit branch — should use it, not detect.
	wc, err := ResumeWorktreeContext(wtDir, "my-override", "main", dir, nil)
	if err != nil {
		t.Fatalf("ResumeWorktreeContext: %v", err)
	}

	if wc.Branch != "my-override" {
		t.Errorf("Branch = %q, want %q", wc.Branch, "my-override")
	}
}

// =============================================================================
// Fresh worktree session baseline tests (spi-6y22y)
// =============================================================================

// TestCreateWorktree_CapturesStartSHA verifies that CreateWorktree sets StartSHA
// on the returned WorktreeContext.
func TestCreateWorktree_CapturesStartSHA(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("feat/fresh-baseline")
	wtDir := filepath.Join(t.TempDir(), "fresh-wt")
	wc, err := rc.CreateWorktree(wtDir, "feat/fresh-baseline")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	if wc.StartSHA == "" {
		t.Fatal("expected StartSHA to be set on fresh worktree")
	}

	// StartSHA should match the worktree's HEAD.
	expectedSHA := trimNewline(run(t, wtDir, "git", "rev-parse", "HEAD"))
	if wc.StartSHA != expectedSHA {
		t.Errorf("StartSHA = %q, want %q", wc.StartSHA, expectedSHA)
	}
}

// TestCreateWorktreeNewBranch_CapturesStartSHA verifies that CreateWorktreeNewBranch
// sets StartSHA on the returned WorktreeContext.
func TestCreateWorktreeNewBranch_CapturesStartSHA(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	wtDir := filepath.Join(t.TempDir(), "newbranch-wt")
	wc, err := rc.CreateWorktreeNewBranch(wtDir, "feat/new-baseline", "main")
	if err != nil {
		t.Fatalf("CreateWorktreeNewBranch: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	if wc.StartSHA == "" {
		t.Fatal("expected StartSHA to be set on fresh worktree (new branch)")
	}

	expectedSHA := trimNewline(run(t, wtDir, "git", "rev-parse", "HEAD"))
	if wc.StartSHA != expectedSHA {
		t.Errorf("StartSHA = %q, want %q", wc.StartSHA, expectedSHA)
	}
}

// TestFreshWorktree_NonBaseStart_NoOpDetection is the core regression test for
// spi-6y22y: a fresh worktree created from a non-base start point (e.g. a
// staging branch with extra commits) must report HasNewCommitsSinceStart=false
// when no work is done in the session.
func TestFreshWorktree_NonBaseStart_NoOpDetection(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create a staging branch with extra commits not on main.
	rc.CreateBranch("staging/epic")
	run(t, dir, "git", "checkout", "staging/epic")
	writeFile(t, filepath.Join(dir, "wave1.txt"), "wave 1 work\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "wave merge: prior work")
	run(t, dir, "git", "checkout", "main")

	// Create a fresh worktree from the staging branch (non-base start).
	wtDir := filepath.Join(t.TempDir(), "fresh-nonbase-wt")
	wc, err := rc.CreateWorktreeNewBranch(wtDir, "feat/child-task", "staging/epic")
	if err != nil {
		t.Fatalf("CreateWorktreeNewBranch: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// BaseBranch..HEAD would show commits (the wave merge), but
	// StartSHA..HEAD should show NO commits (no work done this session).
	hasOld, err := wc.HasNewCommits()
	if err != nil {
		t.Fatalf("HasNewCommits: %v", err)
	}
	if !hasOld {
		t.Fatal("precondition: HasNewCommits (BaseBranch..HEAD) should be true for non-base start")
	}

	hasNew, err := wc.HasNewCommitsSinceStart()
	if err != nil {
		t.Fatalf("HasNewCommitsSinceStart: %v", err)
	}
	if hasNew {
		t.Error("expected HasNewCommitsSinceStart=false on fresh worktree with no session work")
	}

	// Now make a commit — should be detected.
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "session-work.txt"), "session\n")
	if _, cerr := wc.Commit("session commit"); cerr != nil {
		t.Fatalf("Commit: %v", cerr)
	}

	hasNew, err = wc.HasNewCommitsSinceStart()
	if err != nil {
		t.Fatalf("HasNewCommitsSinceStart after commit: %v", err)
	}
	if !hasNew {
		t.Error("expected HasNewCommitsSinceStart=true after session commit")
	}
}

// TestNewStagingWorktree_InheritsStartSHA verifies that staging worktrees
// created via NewStagingWorktree inherit StartSHA from CreateWorktree.
func TestNewStagingWorktree_InheritsStartSHA(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create a branch for the staging worktree.
	rc.CreateBranch("staging/inherit")

	sw, err := NewStagingWorktree(dir, "staging/inherit", "main", "spire-test", "Test", "test@test.com", nil)
	if err != nil {
		t.Fatalf("NewStagingWorktree: %v", err)
	}
	defer sw.Close()

	if sw.StartSHA == "" {
		t.Fatal("expected StartSHA to be set on staging worktree created via NewStagingWorktree")
	}

	expectedSHA := trimNewline(run(t, sw.Dir, "git", "rev-parse", "HEAD"))
	if sw.StartSHA != expectedSHA {
		t.Errorf("StartSHA = %q, want %q", sw.StartSHA, expectedSHA)
	}
}

// writeFile helper is already defined in repo_test.go in this package.
// initTestRepo, run, trimNewline are also reused from repo_test.go.
// These are in the same package so they're available here.

// init verifies test helpers are accessible (compile-time check).
var _ = os.Stat
