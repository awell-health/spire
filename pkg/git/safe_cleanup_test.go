package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// createSiblingWorktree creates `.worktrees/<siblingBaseName>` on feat/<beadID>
// and returns its path. The sibling is fully checked out on the bead's
// feature branch, which is the scenario CleanupStaleSiblingWorktreesSafe is
// designed to clean up.
func createSiblingWorktree(t *testing.T, repoDir, beadID, siblingBaseName string) string {
	t.Helper()
	rc := &RepoContext{Dir: repoDir, BaseBranch: "main"}
	// Ensure feat/<beadID> exists.
	featBranch := "feat/" + beadID
	if !rc.BranchExists(featBranch) {
		run(t, repoDir, "git", "branch", featBranch)
	}
	siblingPath := filepath.Join(repoDir, ".worktrees", siblingBaseName)
	if err := os.MkdirAll(filepath.Dir(siblingPath), 0755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if _, err := rc.CreateWorktree(siblingPath, featBranch); err != nil {
		t.Fatalf("create sibling worktree: %v", err)
	}
	return siblingPath
}

// olderThan sets the mtime of a path to "well before 5 minutes ago" so the
// mtime gate (gate D) no longer treats it as fresh. Without this, newly-
// created siblings are always treated as live by gate D.
func olderThan(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// TestCleanupStaleSiblingWorktreesSafe_RemovesClean verifies that a sibling
// that passes ALL gates (clean worktree, no in-flight rebase, correct branch,
// mtime > 5m) is force-removed.
func TestCleanupStaleSiblingWorktreesSafe_RemovesClean(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-abc123"

	sibling := createSiblingWorktree(t, repoDir, beadID, beadID+"-feature")
	olderThan(t, sibling, 10*time.Minute)

	targetDir := filepath.Join(repoDir, ".worktrees", beadID)
	fates := CleanupStaleSiblingWorktreesSafe(repoDir, targetDir, nil)

	if len(fates) != 1 {
		t.Fatalf("expected 1 fate, got %d: %+v", len(fates), fates)
	}
	if fates[0].Action != "removed" {
		t.Errorf("action = %q, want 'removed' (reason: %s)", fates[0].Action, fates[0].Reason)
	}
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Errorf("sibling still exists after cleanup: %s", sibling)
	}
}

// TestCleanupStaleSiblingWorktreesSafe_PreservesDirty verifies Gate A: a
// sibling with uncommitted changes (`git status --porcelain` non-empty) is
// quarantined to `.abandoned-*`, not destroyed.
func TestCleanupStaleSiblingWorktreesSafe_PreservesDirty(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-dirty"

	sibling := createSiblingWorktree(t, repoDir, beadID, beadID+"-feature")
	// Make the sibling dirty: add a staged-but-uncommitted file.
	writeFile(t, filepath.Join(sibling, "workinprogress.txt"), "dirty content\n")
	olderThan(t, sibling, 10*time.Minute)

	targetDir := filepath.Join(repoDir, ".worktrees", beadID)
	fates := CleanupStaleSiblingWorktreesSafe(repoDir, targetDir, nil)

	if len(fates) != 1 {
		t.Fatalf("expected 1 fate, got %d", len(fates))
	}
	if fates[0].Action != "renamed" {
		t.Errorf("action = %q, want 'renamed' (reason: %s)", fates[0].Action, fates[0].Reason)
	}
	if !strings.Contains(fates[0].Reason, "gate A") {
		t.Errorf("reason = %q, want to mention gate A", fates[0].Reason)
	}
	// Original path should be gone.
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Errorf("original sibling still exists after rename: %s", sibling)
	}
	// Quarantine path should exist with the dirty file intact.
	if fates[0].NewPath == "" {
		t.Fatalf("NewPath is empty — rename didn't record destination")
	}
	if _, err := os.Stat(fates[0].NewPath); err != nil {
		t.Errorf("quarantine path does not exist: %v", err)
	}
	// The dirty file content must survive the rename.
	data, err := os.ReadFile(filepath.Join(fates[0].NewPath, "workinprogress.txt"))
	if err != nil {
		t.Errorf("dirty file not present in quarantine: %v", err)
	}
	if string(data) != "dirty content\n" {
		t.Errorf("dirty content lost: got %q", string(data))
	}
}

// TestCleanupStaleSiblingWorktreesSafe_PreservesInProgressRebase verifies
// Gate B: a sibling with a `.git/rebase-merge` directory is quarantined.
func TestCleanupStaleSiblingWorktreesSafe_PreservesInProgressRebase(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-rebase"

	sibling := createSiblingWorktree(t, repoDir, beadID, beadID+"-feature")
	olderThan(t, sibling, 10*time.Minute)

	// Worktrees keep their admin data under <repo>/.git/worktrees/<name>.
	// Write a rebase-merge marker there; the safe cleanup checks both the
	// worktree's local .git path and the admin dir.
	adminDir := resolveWorktreeGitDir(sibling)
	if adminDir == "" {
		t.Fatalf("could not resolve worktree admin dir for %s", sibling)
	}
	if err := os.MkdirAll(filepath.Join(adminDir, "rebase-merge"), 0755); err != nil {
		t.Fatalf("create rebase-merge marker: %v", err)
	}

	targetDir := filepath.Join(repoDir, ".worktrees", beadID)
	fates := CleanupStaleSiblingWorktreesSafe(repoDir, targetDir, nil)

	if len(fates) != 1 {
		t.Fatalf("expected 1 fate, got %d", len(fates))
	}
	if fates[0].Action != "renamed" {
		t.Errorf("action = %q, want 'renamed' (reason: %s)", fates[0].Action, fates[0].Reason)
	}
	if !strings.Contains(fates[0].Reason, "gate B") {
		t.Errorf("reason = %q, want to mention gate B", fates[0].Reason)
	}
}

// TestCleanupStaleSiblingWorktreesSafe_PreservesBranchMismatch verifies Gate
// C: a sibling checked out on a branch other than feat/<beadID> or
// staging/<beadID> is quarantined.
func TestCleanupStaleSiblingWorktreesSafe_PreservesBranchMismatch(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-branchmismatch"

	// Build a sibling on an unrelated branch.
	otherBranch := "feat/spi-other"
	run(t, repoDir, "git", "branch", otherBranch)
	siblingBase := beadID + "-feature"
	siblingPath := filepath.Join(repoDir, ".worktrees", siblingBase)
	if err := os.MkdirAll(filepath.Dir(siblingPath), 0755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	rc := &RepoContext{Dir: repoDir, BaseBranch: "main"}
	if _, err := rc.CreateWorktree(siblingPath, otherBranch); err != nil {
		t.Fatalf("create sibling on other branch: %v", err)
	}
	olderThan(t, siblingPath, 10*time.Minute)

	targetDir := filepath.Join(repoDir, ".worktrees", beadID)
	fates := CleanupStaleSiblingWorktreesSafe(repoDir, targetDir, nil)

	if len(fates) != 1 {
		t.Fatalf("expected 1 fate, got %d", len(fates))
	}
	if fates[0].Action != "renamed" {
		t.Errorf("action = %q, want 'renamed' (reason: %s)", fates[0].Action, fates[0].Reason)
	}
	if !strings.Contains(fates[0].Reason, "gate C") {
		t.Errorf("reason = %q, want to mention gate C", fates[0].Reason)
	}
}

// TestCleanupStaleSiblingWorktreesSafe_PreservesFreshMtime verifies Gate D:
// a sibling whose mtime is < 5 minutes old is treated as live and
// quarantined rather than destroyed.
func TestCleanupStaleSiblingWorktreesSafe_PreservesFreshMtime(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-fresh"

	sibling := createSiblingWorktree(t, repoDir, beadID, beadID+"-feature")
	// Leave mtime at creation time (recent) so gate D fails.

	targetDir := filepath.Join(repoDir, ".worktrees", beadID)
	fates := CleanupStaleSiblingWorktreesSafe(repoDir, targetDir, nil)

	if len(fates) != 1 {
		t.Fatalf("expected 1 fate, got %d", len(fates))
	}
	if fates[0].Action != "renamed" {
		t.Errorf("action = %q, want 'renamed' (reason: %s)", fates[0].Action, fates[0].Reason)
	}
	if !strings.Contains(fates[0].Reason, "gate D") {
		t.Errorf("reason = %q, want to mention gate D", fates[0].Reason)
	}
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Errorf("original sibling still exists after rename")
	}
}

// TestCleanupStaleSiblingWorktreesSafe_Idempotent verifies a second call
// with no un-quarantined siblings is a no-op.
func TestCleanupStaleSiblingWorktreesSafe_Idempotent(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-idemp"

	sibling := createSiblingWorktree(t, repoDir, beadID, beadID+"-feature")
	olderThan(t, sibling, 10*time.Minute)

	targetDir := filepath.Join(repoDir, ".worktrees", beadID)
	first := CleanupStaleSiblingWorktreesSafe(repoDir, targetDir, nil)
	if len(first) != 1 || first[0].Action != "removed" {
		t.Fatalf("first call unexpected: %+v", first)
	}
	// Second call — the sibling is gone, so nothing to do.
	second := CleanupStaleSiblingWorktreesSafe(repoDir, targetDir, nil)
	if len(second) != 0 {
		t.Errorf("second call produced fates (expected idempotent no-op): %+v", second)
	}
}

// TestCleanupStaleSiblingWorktreesSafe_SkipsAlreadyQuarantined verifies that
// the cleanup does not re-quarantine a `.abandoned-*` directory on every
// invocation (would build up nested `.abandoned-<ts>-.abandoned-<ts>-...`
// chains).
func TestCleanupStaleSiblingWorktreesSafe_SkipsAlreadyQuarantined(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-quar"

	// Manually create a `.abandoned-*` directory that names the bead.
	quarantine := filepath.Join(repoDir, ".worktrees", fmt.Sprintf(".abandoned-1234567890-%s-feature", beadID))
	if err := os.MkdirAll(quarantine, 0755); err != nil {
		t.Fatalf("mkdir quarantine: %v", err)
	}
	targetDir := filepath.Join(repoDir, ".worktrees", beadID)
	fates := CleanupStaleSiblingWorktreesSafe(repoDir, targetDir, nil)
	if len(fates) != 0 {
		t.Errorf("expected already-quarantined dirs to be skipped, got fates: %+v", fates)
	}
	if _, err := os.Stat(quarantine); err != nil {
		t.Errorf("quarantine dir was unexpectedly touched: %v", err)
	}
}

// TestCleanupStaleSiblingWorktreesSafe_PreservesUnrelatedBead verifies the
// bead-prefix match gate: worktrees for other beads are never inspected.
func TestCleanupStaleSiblingWorktreesSafe_PreservesUnrelatedBead(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-mine"
	other := "spi-other"

	otherSibling := createSiblingWorktree(t, repoDir, other, other+"-feature")

	targetDir := filepath.Join(repoDir, ".worktrees", beadID)
	fates := CleanupStaleSiblingWorktreesSafe(repoDir, targetDir, nil)
	if len(fates) != 0 {
		t.Errorf("expected unrelated siblings to be skipped, got fates: %+v", fates)
	}
	if _, err := os.Stat(otherSibling); err != nil {
		t.Errorf("unrelated sibling was touched: %v", err)
	}
}
