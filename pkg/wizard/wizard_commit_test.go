package wizard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	spgit "github.com/awell-health/spire/pkg/git"
)

// gitRun executes a git command in the given dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

// setupWorktree creates a temp git repo with an initial commit and a worktree
// on a feature branch. Returns the worktree context and a cleanup function.
func setupWorktree(t *testing.T, branch string) *spgit.WorktreeContext {
	t.Helper()
	repoDir := t.TempDir()

	gitRun(t, repoDir, "init")
	gitRun(t, repoDir, "config", "user.name", "Test")
	gitRun(t, repoDir, "config", "user.email", "test@test.com")

	os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Test\n"), 0644)
	gitRun(t, repoDir, "add", "-A")
	gitRun(t, repoDir, "commit", "-m", "initial commit")
	gitRun(t, repoDir, "branch", "-M", "main")

	// Create feature branch and worktree
	gitRun(t, repoDir, "branch", branch)
	wtDir := filepath.Join(t.TempDir(), "wt")
	gitRun(t, repoDir, "worktree", "add", wtDir, branch)

	wc := &spgit.WorktreeContext{
		Dir:        wtDir,
		Branch:     branch,
		BaseBranch: "main",
		RepoPath:   repoDir,
	}
	wc.ConfigureUser("Test", "test@test.com")
	return wc
}

func noopLog(string, ...interface{}) {}

func TestWizardCommit_NoChanges(t *testing.T) {
	wc := setupWorktree(t, "feat/no-changes")

	sha, committed := WizardCommit(wc, "test-001", "no changes test", noopLog)
	if committed {
		t.Error("expected committed=false when nothing changed")
	}
	if sha != "" {
		t.Errorf("expected empty SHA, got %q", sha)
	}
}

func TestWizardCommit_ClaudeAlreadyCommitted(t *testing.T) {
	wc := setupWorktree(t, "feat/already-committed")

	// Simulate Claude committing directly
	os.WriteFile(filepath.Join(wc.Dir, "feature.go"), []byte("package main\n"), 0644)
	gitRun(t, wc.Dir, "add", "-A")
	gitRun(t, wc.Dir, "commit", "-m", "feat: claude commit")
	expectedSHA := gitRun(t, wc.Dir, "rev-parse", "HEAD")

	sha, committed := WizardCommit(wc, "test-002", "already committed test", noopLog)
	if !committed {
		t.Error("expected committed=true when Claude already committed")
	}
	if sha != expectedSHA {
		t.Errorf("expected SHA %q, got %q", expectedSHA, sha)
	}
}

func TestWizardCommit_NormalCommit(t *testing.T) {
	wc := setupWorktree(t, "feat/normal-commit")

	// Write uncommitted changes
	os.WriteFile(filepath.Join(wc.Dir, "new.go"), []byte("package main\n"), 0644)

	sha, committed := WizardCommit(wc, "test-003", "Normal commit test", noopLog)
	if !committed {
		t.Error("expected committed=true")
	}
	if sha == "" {
		t.Error("expected non-empty SHA")
	}
}

// TestWizardCommit_FallbackAfterEmptyCommit is the regression test for
// spi-czyx5: Claude committed real changes AND leftover prompt files make
// HasUncommittedChanges true. The commit attempt cleans prompt files and
// finds nothing to stage, returning empty SHA. The fallback should detect
// Claude's prior commit and return success.
func TestWizardCommit_FallbackAfterEmptyCommit(t *testing.T) {
	wc := setupWorktree(t, "feat/fallback")

	// Step 1: Simulate Claude committing real changes directly
	os.WriteFile(filepath.Join(wc.Dir, "feature.go"), []byte("package main\n"), 0644)
	gitRun(t, wc.Dir, "add", "-A")
	gitRun(t, wc.Dir, "commit", "-m", "feat: claude work")
	expectedSHA := gitRun(t, wc.Dir, "rev-parse", "HEAD")

	// Step 2: Leave a prompt file in the worktree (makes HasUncommittedChanges true)
	os.WriteFile(filepath.Join(wc.Dir, ".spire-prompt.txt"), []byte("leftover prompt\n"), 0644)

	// Verify preconditions: uncommitted changes exist AND new commits exist
	if !wc.HasUncommittedChanges() {
		t.Fatal("precondition: expected uncommitted changes from prompt file")
	}
	hasNew, err := wc.HasNewCommits()
	if err != nil {
		t.Fatalf("precondition: HasNewCommits error: %v", err)
	}
	if !hasNew {
		t.Fatal("precondition: expected new commits from Claude's commit")
	}

	// Step 3: WizardCommit should hit the fallback path:
	// - HasUncommittedChanges=true → enters commit path
	// - Commit() cleans .spire-prompt.txt, nothing else to stage → empty SHA
	// - Fallback re-checks HasNewCommits → true → returns Claude's commit SHA
	sha, committed := WizardCommit(wc, "test-004", "Fallback test", noopLog)
	if !committed {
		t.Error("expected committed=true from fallback path")
	}
	if sha != expectedSHA {
		t.Errorf("expected SHA %q from fallback, got %q", expectedSHA, sha)
	}

	// Verify prompt file was cleaned up
	if _, err := os.Stat(filepath.Join(wc.Dir, ".spire-prompt.txt")); !os.IsNotExist(err) {
		t.Error(".spire-prompt.txt should have been removed")
	}
}

// TestWizardCommit_FallbackNotTriggeredWithoutClaudeCommit verifies that when
// there are only prompt files and no Claude commit, the fallback does NOT fire
// and WizardCommit correctly returns "nothing staged".
func TestWizardCommit_FallbackNotTriggeredWithoutClaudeCommit(t *testing.T) {
	wc := setupWorktree(t, "feat/no-fallback")

	// Only leave a prompt file — no real Claude commit
	os.WriteFile(filepath.Join(wc.Dir, ".spire-prompt.txt"), []byte("orphan prompt\n"), 0644)

	sha, committed := WizardCommit(wc, "test-005", "No fallback test", noopLog)
	if committed {
		t.Error("expected committed=false when only prompt files exist")
	}
	if sha != "" {
		t.Errorf("expected empty SHA, got %q", sha)
	}
}

// setupStagingWorktree creates a worktree with pre-existing commits (simulating
// a staging branch) and returns a resumed WorktreeContext with StartSHA set.
func setupStagingWorktree(t *testing.T, branch string) *spgit.WorktreeContext {
	t.Helper()
	repoDir := t.TempDir()

	gitRun(t, repoDir, "init")
	gitRun(t, repoDir, "config", "user.name", "Test")
	gitRun(t, repoDir, "config", "user.email", "test@test.com")

	os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Test\n"), 0644)
	gitRun(t, repoDir, "add", "-A")
	gitRun(t, repoDir, "commit", "-m", "initial commit")
	gitRun(t, repoDir, "branch", "-M", "main")

	// Create feature branch and worktree
	gitRun(t, repoDir, "branch", branch)
	wtDir := filepath.Join(t.TempDir(), "wt")
	gitRun(t, repoDir, "worktree", "add", wtDir, branch)

	// Configure user in worktree
	gitRun(t, wtDir, "config", "user.name", "Test")
	gitRun(t, wtDir, "config", "user.email", "test@test.com")

	// Add a pre-existing commit (simulates previous wave merge)
	os.WriteFile(filepath.Join(wtDir, "wave1.go"), []byte("package main\n"), 0644)
	gitRun(t, wtDir, "add", "-A")
	gitRun(t, wtDir, "commit", "-m", "wave merge: prior work")

	// Resume with session baseline (captures HEAD after pre-existing commit)
	wc, err := spgit.ResumeWorktreeContext(wtDir, branch, "main", repoDir, nil)
	if err != nil {
		t.Fatalf("ResumeWorktreeContext: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")
	return wc
}

// TestWizardCommit_NoOpBuildFixInStaging is the regression test for spi-8de7f:
// A no-op build-fix in a staging worktree with pre-existing commits must report
// no_changes (committed=false), not success.
//
// Before this fix, the bare WorktreeContext had no StartSHA, so HasNewCommits
// did BaseBranch..HEAD which included pre-existing commits, causing WizardCommit
// to report "Claude already committed."
func TestWizardCommit_NoOpBuildFixInStaging(t *testing.T) {
	wc := setupStagingWorktree(t, "staging/noop-build")

	// No changes made — the "build fix" did nothing.
	sha, committed := WizardCommit(wc, "test-staging-001", "fix build errors", noopLog)
	if committed {
		t.Error("expected committed=false for no-op build-fix in staging")
	}
	if sha != "" {
		t.Errorf("expected empty SHA, got %q", sha)
	}
}

// TestWizardCommit_NoOpReviewFixInStaging is the regression test for spi-8de7f:
// A no-op review-fix in a staging worktree must report no_changes.
func TestWizardCommit_NoOpReviewFixInStaging(t *testing.T) {
	wc := setupStagingWorktree(t, "staging/noop-review")

	// No changes made — the "review fix" did nothing.
	sha, committed := WizardCommit(wc, "test-staging-002", "review fix test", noopLog)
	if committed {
		t.Error("expected committed=false for no-op review-fix in staging")
	}
	if sha != "" {
		t.Errorf("expected empty SHA, got %q", sha)
	}
}

// TestWizardCommit_RealCommitInStaging verifies that a real commit in a staging
// worktree with pre-existing commits is correctly detected as committed.
func TestWizardCommit_RealCommitInStaging(t *testing.T) {
	wc := setupStagingWorktree(t, "staging/real-commit")

	// Make a real change (simulates successful fix).
	os.WriteFile(filepath.Join(wc.Dir, "fix.go"), []byte("package main\n\nfunc fix() {}\n"), 0644)

	sha, committed := WizardCommit(wc, "test-staging-003", "Fix build errors", noopLog)
	if !committed {
		t.Error("expected committed=true for real changes in staging")
	}
	if sha == "" {
		t.Error("expected non-empty SHA for real commit")
	}
}

// TestWizardCommit_ClaudeCommittedInStaging verifies that Claude's own commit
// in a staging session is correctly detected (Claude committed, nothing
// uncommitted left).
func TestWizardCommit_ClaudeCommittedInStaging(t *testing.T) {
	wc := setupStagingWorktree(t, "staging/claude-commit")

	// Simulate Claude committing directly during this session.
	os.WriteFile(filepath.Join(wc.Dir, "claude-fix.go"), []byte("package main\n"), 0644)
	gitRun(t, wc.Dir, "add", "-A")
	gitRun(t, wc.Dir, "commit", "-m", "feat: claude fix")
	expectedSHA := gitRun(t, wc.Dir, "rev-parse", "HEAD")

	sha, committed := WizardCommit(wc, "test-staging-004", "staging claude commit", noopLog)
	if !committed {
		t.Error("expected committed=true when Claude committed in staging session")
	}
	if sha != expectedSHA {
		t.Errorf("expected SHA %q, got %q", expectedSHA, sha)
	}
}

// setupFreshWorktreeFromNonBase creates a repo with a staging branch that has
// extra commits, then creates a FRESH worktree (using pkg/git constructors)
// from that staging branch. Unlike setupStagingWorktree which uses
// ResumeWorktreeContext, this uses CreateWorktreeNewBranch — testing the
// spi-6y22y fix where fresh worktrees now capture StartSHA.
func setupFreshWorktreeFromNonBase(t *testing.T, childBranch string) *spgit.WorktreeContext {
	t.Helper()
	repoDir := t.TempDir()

	gitRun(t, repoDir, "init")
	gitRun(t, repoDir, "config", "user.name", "Test")
	gitRun(t, repoDir, "config", "user.email", "test@test.com")

	os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Test\n"), 0644)
	gitRun(t, repoDir, "add", "-A")
	gitRun(t, repoDir, "commit", "-m", "initial commit")
	gitRun(t, repoDir, "branch", "-M", "main")

	// Create a staging branch with extra commits not on main.
	gitRun(t, repoDir, "branch", "staging/epic")
	gitRun(t, repoDir, "checkout", "staging/epic")
	os.WriteFile(filepath.Join(repoDir, "wave1.go"), []byte("package main\n"), 0644)
	gitRun(t, repoDir, "add", "-A")
	gitRun(t, repoDir, "commit", "-m", "wave merge: prior work")
	gitRun(t, repoDir, "checkout", "main")

	// Create a fresh worktree from the staging branch using the constructor.
	rc := &spgit.RepoContext{Dir: repoDir, BaseBranch: "main"}
	wtDir := filepath.Join(t.TempDir(), "wt")
	wc, err := rc.CreateWorktreeNewBranch(wtDir, childBranch, "staging/epic")
	if err != nil {
		t.Fatalf("CreateWorktreeNewBranch: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")
	return wc
}

// TestWizardCommit_NoOpFreshWorktreeFromNonBase is the regression test for
// spi-6y22y: a fresh worktree (not resumed) created from a non-base branch
// (staging/epic) with no session work must report committed=false.
//
// Before spi-6y22y, fresh worktrees had no StartSHA, so HasNewCommitsSinceStart
// fell back to BaseBranch..HEAD which included the staging commits and
// incorrectly reported "Claude already committed."
func TestWizardCommit_NoOpFreshWorktreeFromNonBase(t *testing.T) {
	wc := setupFreshWorktreeFromNonBase(t, "feat/fresh-noop")

	sha, committed := WizardCommit(wc, "test-fresh-001", "no-op from staging", noopLog)
	if committed {
		t.Error("expected committed=false for no-op fresh worktree from non-base start")
	}
	if sha != "" {
		t.Errorf("expected empty SHA, got %q", sha)
	}
}

// TestWizardCommit_RealCommitFreshWorktreeFromNonBase verifies that real work
// in a fresh worktree from a non-base start is correctly detected.
func TestWizardCommit_RealCommitFreshWorktreeFromNonBase(t *testing.T) {
	wc := setupFreshWorktreeFromNonBase(t, "feat/fresh-real")

	os.WriteFile(filepath.Join(wc.Dir, "new-feature.go"), []byte("package main\n\nfunc feature() {}\n"), 0644)

	sha, committed := WizardCommit(wc, "test-fresh-002", "real work from staging", noopLog)
	if !committed {
		t.Error("expected committed=true for real changes in fresh worktree from non-base start")
	}
	if sha == "" {
		t.Error("expected non-empty SHA")
	}
}

// TestWizardCommit_ClaudeCommittedFreshWorktreeFromNonBase verifies that Claude's
// own commit in a fresh worktree from a non-base start is correctly detected.
func TestWizardCommit_ClaudeCommittedFreshWorktreeFromNonBase(t *testing.T) {
	wc := setupFreshWorktreeFromNonBase(t, "feat/fresh-claude")

	// Simulate Claude committing directly during this session.
	os.WriteFile(filepath.Join(wc.Dir, "claude-work.go"), []byte("package main\n"), 0644)
	gitRun(t, wc.Dir, "add", "-A")
	gitRun(t, wc.Dir, "commit", "-m", "feat: claude work")
	expectedSHA := gitRun(t, wc.Dir, "rev-parse", "HEAD")

	sha, committed := WizardCommit(wc, "test-fresh-003", "claude commit from staging", noopLog)
	if !committed {
		t.Error("expected committed=true when Claude committed in fresh worktree from non-base start")
	}
	if sha != expectedSHA {
		t.Errorf("expected SHA %q, got %q", expectedSHA, sha)
	}
}

// TestWizardCommit_ErrorDoesNotAssumeCommits verifies that comparison errors
// do not cause WizardCommit to falsely report "Claude already committed."
// This was a bug: the old code did `hasNewCommits = true` on error.
func TestWizardCommit_ErrorDoesNotAssumeCommits(t *testing.T) {
	repoDir := t.TempDir()
	gitRun(t, repoDir, "init")
	gitRun(t, repoDir, "config", "user.name", "Test")
	gitRun(t, repoDir, "config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Test\n"), 0644)
	gitRun(t, repoDir, "add", "-A")
	gitRun(t, repoDir, "commit", "-m", "initial commit")
	gitRun(t, repoDir, "branch", "-M", "main")

	gitRun(t, repoDir, "branch", "feat/error-test")
	wtDir := filepath.Join(t.TempDir(), "wt")
	gitRun(t, repoDir, "worktree", "add", wtDir, "feat/error-test")

	// Construct WorktreeContext with an invalid StartSHA to trigger an error.
	wc := &spgit.WorktreeContext{
		Dir:        wtDir,
		Branch:     "feat/error-test",
		BaseBranch: "",      // empty
		StartSHA:   "bogus", // invalid SHA — will cause git log error
		RepoPath:   repoDir,
	}

	sha, committed := WizardCommit(wc, "test-error", "error test", noopLog)
	if committed {
		t.Error("expected committed=false when comparison errors occur (should not assume commits exist)")
	}
	if sha != "" {
		t.Errorf("expected empty SHA, got %q", sha)
	}
}
