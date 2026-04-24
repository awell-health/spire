package wizard

// Tests for spi-pwdhs5 Bug A seam 2: sage worktree release before
// terminal merge dispatch.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	spgit "github.com/awell-health/spire/pkg/git"
)

// TestSeam2_ReviewCreateWorktreeAndCleanup_RemovesWorktreeDir asserts the
// Bug A contract: after explicit wc.Cleanup() is called on a review
// worktree, the temp-dir path is gone. This is the invariant
// CmdWizardReview depends on — the sage worktree must not outlive the
// verdict dispatch that follows.
func TestSeam2_ReviewCreateWorktreeAndCleanup_RemovesWorktreeDir(t *testing.T) {
	// Set up a minimal test repo.
	repoDir := t.TempDir()
	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "config", "user.email", "t@t.com")
	writeFile(t, filepath.Join(repoDir, "README.md"), "hi\n")
	run(t, repoDir, "git", "add", "-A")
	run(t, repoDir, "git", "commit", "-m", "init")
	run(t, repoDir, "git", "branch", "-M", "main")

	beadID := "spi-sgcleanup"
	reviewer := "test-sage"

	// Create a feat branch for the review.
	run(t, repoDir, "git", "branch", "feat/"+beadID)

	// Redirect TMPDIR to a test-owned temp so the review worktree lives
	// under t.TempDir() and is auto-cleaned even on test failure.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	noopLog := func(string, ...any) {}
	wc, err := ReviewCreateWorktree(repoDir, beadID, reviewer, "main", "feat/"+beadID, noopLog)
	if err != nil {
		t.Fatalf("ReviewCreateWorktree: %v", err)
	}

	// The worktree directory must exist immediately after creation.
	expected := filepath.Join(os.TempDir(), "spire-review", reviewer, beadID)
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("worktree dir missing after create: %v", err)
	}

	// Explicit cleanup — this is the call CmdWizardReview makes before
	// dispatching the terminal merge.
	wc.Cleanup()

	// Contract: directory gone.
	if _, err := os.Stat(expected); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists after Cleanup: %v (path: %s)", err, expected)
	}

	// Contract: the feat branch is no longer held by the review worktree —
	// a fresh CreateWorktree on the same branch must succeed. This is
	// the exact sequence TerminalMerge performs after sage approval; the
	// bug-reproducer asserts no "already used by worktree" error.
	stagingDir := filepath.Join(repoDir, ".worktrees", beadID)
	if err := os.MkdirAll(filepath.Dir(stagingDir), 0755); err != nil {
		t.Fatalf("mkdir staging parent: %v", err)
	}
	rc := &spgit.RepoContext{Dir: repoDir, BaseBranch: "main"}
	if _, err := rc.CreateWorktree(stagingDir, "feat/"+beadID); err != nil {
		t.Errorf("could not create staging worktree after sage cleanup — branch still locked: %v", err)
	}

	// Idempotency: second Cleanup on same wc must be a no-op.
	wc.Cleanup() // expect no panic
}

// --- Test helpers mirrored from other test files ---

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
