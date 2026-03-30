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
