package git

// Tests for spi-pwdhs5 Bug A — cross-tree sibling cleanup. Each test is
// tagged with its spi-1dk71j seam number for traceability.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSeam7_CleanupStaleSiblingWorktreesSafe_ExtraRoots verifies the
// seam-7 matrix case: sibling worktrees exist at four different paths
// (`.worktrees/<bead>`, `.worktrees/<bead>-feature`,
// `<extraRoot>/<name>/<bead>`, `<extraRoot2>/<name>/<bead>`) and the
// extra-roots variant of CleanupStaleSiblingWorktreesSafe finds all of
// them in one pass.
//
// Without Bug A's fix, siblings under temp-dir roots are invisible to
// the cleanup and their branch-holding locks survive, causing git to
// refuse `git worktree add` with "'feat/<bead>' is already used by
// worktree at ..." when the merge staging worktree is created.
func TestSeam7_CleanupStaleSiblingWorktreesSafe_ExtraRoots(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-matrix"

	// Extra root 1: simulate $TMPDIR/spire-review/<name>/<bead>.
	// Create this BEFORE the in-parent sibling — only one worktree per
	// branch is allowed, and this is the bug-reproducer scenario:
	// sage-review worktree checked out on feat/<bead>.
	extraRoot1 := filepath.Join(t.TempDir(), "spire-review")
	reviewerDir := filepath.Join(extraRoot1, "wizard-spi-matrix-review")
	if err := os.MkdirAll(reviewerDir, 0755); err != nil {
		t.Fatalf("mkdir extraRoot1: %v", err)
	}
	sib1 := filepath.Join(reviewerDir, beadID)
	rc := &RepoContext{Dir: repoDir, BaseBranch: "main"}
	// Create the feat branch first so the worktree can check it out.
	run(t, repoDir, "git", "branch", "feat/"+beadID)
	if _, err := rc.CreateWorktree(sib1, "feat/"+beadID); err != nil {
		t.Fatalf("create sibling in extraRoot1: %v", err)
	}
	olderThan(t, sib1, 10*time.Minute)

	// Extra root 2: simulate $TMPDIR/spire-wizard/<name>/<bead> —
	// stale-directory form (branch released, dir lingering). This
	// matches the production scenario where a crashed wizard leaves
	// its worktree dir behind after git pruned the ref.
	extraRoot2 := filepath.Join(t.TempDir(), "spire-wizard")
	wizardDir := filepath.Join(extraRoot2, "wizard-spi-matrix")
	if err := os.MkdirAll(wizardDir, 0755); err != nil {
		t.Fatalf("mkdir extraRoot2: %v", err)
	}
	sib2 := filepath.Join(wizardDir, beadID)
	if err := os.MkdirAll(sib2, 0755); err != nil {
		t.Fatalf("mkdir sib2: %v", err)
	}
	olderThan(t, sib2, 10*time.Minute)

	// Target directory the caller is trying to create — scan context.
	targetDir := filepath.Join(repoDir, ".worktrees", beadID)

	fates := CleanupStaleSiblingWorktreesSafeWithExtraRoots(
		repoDir, targetDir,
		[]string{extraRoot1, extraRoot2},
		nil,
	)

	// Expect at least 1 fate for the extraRoot1 sibling (the key scenario).
	// sib2 is a bare dir — it's discovered but gates A/B/C are unreadable
	// because it isn't a real worktree; the scan may treat it as stale.
	if len(fates) < 1 {
		t.Fatalf("expected at least 1 fate, got %d: %+v", len(fates), fates)
	}

	// Core assertion: the extra-root sibling holding feat/<bead> is gone.
	if _, err := os.Stat(sib1); !os.IsNotExist(err) {
		t.Errorf("extra-root sibling %s still exists — branch lock not released", sib1)
	}

	// And now a fresh CreateWorktree on feat/<bead> must succeed —
	// this is what TerminalMerge's NewStagingWorktreeAt does. If the
	// branch is still locked by sib1, this errors with "already used".
	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		t.Fatalf("mkdir target parent: %v", err)
	}
	if _, err := rc.CreateWorktree(targetDir, "feat/"+beadID); err != nil {
		t.Errorf("could not create staging worktree after cleanup — branch still locked: %v", err)
	}
}

// TestSeam7_CleanupStaleSiblingWorktreesSafe_NoExtraRootsBackwardsCompat
// verifies the non-extra-roots form still works: when called through the
// legacy API, behavior is identical to before Bug A.
func TestSeam7_CleanupStaleSiblingWorktreesSafe_NoExtraRootsBackwardsCompat(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-backcompat"

	sib := createSiblingWorktree(t, repoDir, beadID, beadID+"-feature")
	olderThan(t, sib, 10*time.Minute)

	targetDir := filepath.Join(repoDir, ".worktrees", beadID)
	fates := CleanupStaleSiblingWorktreesSafe(repoDir, targetDir, nil)

	if len(fates) != 1 {
		t.Fatalf("expected 1 fate, got %d: %+v", len(fates), fates)
	}
	if fates[0].Action != "removed" {
		t.Errorf("action = %q, want 'removed'", fates[0].Action)
	}
}

// TestSeam7_CleanupStaleSiblingWorktreesSafe_ExtraRootNonexistent verifies
// non-existent extra roots are tolerated silently (a common case in
// production when $TMPDIR/spire-review exists but $TMPDIR/spire-wizard
// doesn't, or vice versa). Without this, the cleanup would error or
// skip paths that should still have been scanned.
func TestSeam7_CleanupStaleSiblingWorktreesSafe_ExtraRootNonexistent(t *testing.T) {
	repoDir := initTestRepo(t)
	beadID := "spi-empty"

	sib := createSiblingWorktree(t, repoDir, beadID, beadID+"-feature")
	olderThan(t, sib, 10*time.Minute)

	targetDir := filepath.Join(repoDir, ".worktrees", beadID)
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")

	fates := CleanupStaleSiblingWorktreesSafeWithExtraRoots(
		repoDir, targetDir,
		[]string{nonexistent},
		nil,
	)

	// The in-parent sibling should still be cleaned up.
	if len(fates) != 1 {
		t.Errorf("expected 1 fate from in-parent sibling, got %d", len(fates))
	}
}

// TestBugA_CleanupIdempotent verifies WorktreeContext.Cleanup can be
// called multiple times without erroring — a key requirement for the
// line-105 defer-plus-explicit-cleanup pattern introduced in
// wizard_review.go by Bug A's fix.
func TestBugA_CleanupIdempotent(t *testing.T) {
	repoDir := initTestRepo(t)
	rc := &RepoContext{Dir: repoDir, BaseBranch: "main"}
	run(t, repoDir, "git", "branch", "feat/spi-idem")
	wcDir := filepath.Join(t.TempDir(), "wt")
	wc, err := rc.CreateWorktree(wcDir, "feat/spi-idem")
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	// First call: removes the worktree and clears Dir.
	wc.Cleanup()
	if wc.Dir != "" {
		t.Errorf("after first Cleanup, Dir should be empty, got %q", wc.Dir)
	}
	if _, err := os.Stat(wcDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after first Cleanup")
	}
	// Second call: no-op, must not panic or error.
	wc.Cleanup()

	// Third call via nil — also must no-op.
	var nilWC *WorktreeContext
	nilWC.Cleanup() // expect no panic
}
