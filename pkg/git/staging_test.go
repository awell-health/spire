package git

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// =============================================================================
// MergeToMain tests
// =============================================================================

// TestMergeToMain_HappyPath verifies the fast path: staging is already
// ff-able onto main, so a single merge succeeds with no rebase.
func TestMergeToMain_HappyPath(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create a staging branch with a commit ahead of main.
	rc.CreateBranch("staging/test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	wc, err := rc.CreateWorktree(wtDir, "staging/test")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "feature.txt"), "feature work\n")
	wc.Commit("feature commit")

	sw := &StagingWorktree{
		WorktreeContext: *wc,
	}

	// main hasn't moved — ff-only should succeed on first attempt.
	if err := sw.MergeToMain("main", nil, "", ""); err != nil {
		t.Fatalf("MergeToMain: %v", err)
	}

	// Verify main has the feature file.
	mainSHA := rc.HeadSHA()
	stagingSHA := trimNewline(run(t, wtDir, "git", "rev-parse", "HEAD"))
	if mainSHA != stagingSHA {
		t.Errorf("main SHA %s != staging SHA %s", mainSHA, stagingSHA)
	}
}

// TestMergeToMain_RebaseSucceeds verifies the rebase path: main has diverged
// but a single rebase + ff-only merge succeeds.
func TestMergeToMain_RebaseSucceeds(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create a staging branch with a commit.
	rc.CreateBranch("staging/test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	wc, err := rc.CreateWorktree(wtDir, "staging/test")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "feature.txt"), "feature work\n")
	wc.Commit("feature commit")

	// Advance main with a non-conflicting commit.
	writeFile(t, filepath.Join(dir, "main-file.txt"), "main work\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main advance")

	sw := &StagingWorktree{
		WorktreeContext: *wc,
	}

	// ff-only will fail, then rebase + retry should succeed.
	if err := sw.MergeToMain("main", nil, "", ""); err != nil {
		t.Fatalf("MergeToMain: %v", err)
	}

	// Verify main has both files.
	if _, err := exec.Command("git", "-C", dir, "cat-file", "-e", "HEAD:feature.txt").Output(); err != nil {
		t.Error("main should contain feature.txt after merge")
	}
	if _, err := exec.Command("git", "-C", dir, "cat-file", "-e", "HEAD:main-file.txt").Output(); err != nil {
		t.Error("main should contain main-file.txt after merge")
	}
}

// TestMergeToMain_RaceResolvedOnRetry deterministically simulates a merge race:
// main advances during the first retry (via the test command), causing ff-only
// to fail. On the second retry, main doesn't advance, so ff-only succeeds.
//
// Mechanism: testStr is a shell command that commits to main on the first call
// only (tracked via a flag file). This advances main between rebase and ff-only
// in the first retry, but not in subsequent retries.
func TestMergeToMain_RaceResolvedOnRetry(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create staging with a commit.
	rc.CreateBranch("staging/test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	wc, err := rc.CreateWorktree(wtDir, "staging/test")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "feature.txt"), "feature work\n")
	wc.Commit("feature commit")

	// Advance main to force the initial ff-only to fail.
	writeFile(t, filepath.Join(dir, "main1.txt"), "main advance 1\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main advance 1")

	sw := &StagingWorktree{
		WorktreeContext: *wc,
	}

	// testStr: on the first call, commit to main (advancing it during the
	// retry window). On subsequent calls, the flag file exists so no advance.
	flagFile := filepath.Join(t.TempDir(), "race-flag")
	testCmd := fmt.Sprintf(
		`if [ ! -f %q ]; then git -C %q commit --allow-empty -m "race advance" && touch %q; fi`,
		flagFile, dir, flagFile,
	)

	err = sw.MergeToMain("main", nil, "", testCmd)
	if err != nil {
		t.Fatalf("MergeToMain should succeed after race resolved on retry, got: %v", err)
	}

	// Verify main has the feature file.
	if _, err := exec.Command("git", "-C", dir, "cat-file", "-e", "HEAD:feature.txt").Output(); err != nil {
		t.Error("main should contain feature.txt after merge")
	}
}

// TestMergeToMain_RaceExhaustsRetries deterministically exercises the
// ErrMergeRace return path. The test command advances main on EVERY call,
// so every retry's ff-only merge fails — the race is never resolved.
//
// Mechanism: testStr is a shell command that unconditionally commits to main.
// After rebase, the test command runs and advances main, then ff-only fails
// because main has a commit that staging doesn't have. This repeats for
// maxMergeAttempts iterations, then ErrMergeRace is returned.
func TestMergeToMain_RaceExhaustsRetries(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create staging with a commit.
	rc.CreateBranch("staging/test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	wc, err := rc.CreateWorktree(wtDir, "staging/test")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "feature.txt"), "feature work\n")
	wc.Commit("feature commit")

	// Advance main to force the initial ff-only to fail.
	writeFile(t, filepath.Join(dir, "diverge.txt"), "diverge\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "initial divergence")

	sw := &StagingWorktree{
		WorktreeContext: *wc,
	}

	// testStr: unconditionally advance main on every call. This ensures the
	// ff-only merge fails after every rebase because main always has a commit
	// that staging doesn't.
	testCmd := fmt.Sprintf(
		`git -C %q commit --allow-empty -m "race advance"`,
		dir,
	)

	err = sw.MergeToMain("main", nil, "", testCmd)
	if err == nil {
		t.Fatal("expected ErrMergeRace, got nil")
	}
	if !errors.Is(err, ErrMergeRace) {
		t.Fatalf("expected ErrMergeRace, got: %v", err)
	}
	// Verify the error message includes the attempt count.
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", maxMergeAttempts)) {
		t.Errorf("error should mention attempt count %d, got: %s", maxMergeAttempts, err.Error())
	}
}

// TestMergeToMain_RebaseConflictIsTerminal verifies that a genuine rebase
// conflict (not a race) produces a terminal error, not ErrMergeRace.
func TestMergeToMain_RebaseConflictIsTerminal(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create staging with a commit that edits README.md.
	rc.CreateBranch("staging/test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	wc, err := rc.CreateWorktree(wtDir, "staging/test")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "README.md"), "staging version\n")
	wc.Commit("staging edit README")

	// Advance main with a conflicting edit to the same file.
	writeFile(t, filepath.Join(dir, "README.md"), "main version\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main edit README")

	sw := &StagingWorktree{
		WorktreeContext: *wc,
	}

	err = sw.MergeToMain("main", nil, "", "")
	if err == nil {
		t.Fatal("expected error for rebase conflict, got nil")
	}
	if errors.Is(err, ErrMergeRace) {
		t.Errorf("rebase conflict should NOT return ErrMergeRace, got: %v", err)
	}
	if !strings.Contains(err.Error(), "rebase") {
		t.Errorf("expected error to mention rebase, got: %s", err.Error())
	}
}

// TestMergeToMain_WithBuildAndTest verifies that build and test commands
// are re-run after rebase.
func TestMergeToMain_WithBuildAndTest(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create staging with a commit.
	rc.CreateBranch("staging/test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	wc, err := rc.CreateWorktree(wtDir, "staging/test")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "feature.txt"), "feature work\n")
	wc.Commit("feature commit")

	// Advance main to force rebase path.
	writeFile(t, filepath.Join(dir, "main.txt"), "main\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main advance")

	sw := &StagingWorktree{
		WorktreeContext: *wc,
	}

	// Use "true" as both build and test commands (always succeeds).
	if err := sw.MergeToMain("main", nil, "true", "true"); err != nil {
		t.Fatalf("MergeToMain with build/test: %v", err)
	}
}

// TestMergeToMain_BuildFailureIsTerminal verifies that a build failure after
// rebase is a terminal error, not retried.
func TestMergeToMain_BuildFailureIsTerminal(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("staging/test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	wc, err := rc.CreateWorktree(wtDir, "staging/test")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "feature.txt"), "feature work\n")
	wc.Commit("feature commit")

	// Advance main to force rebase.
	writeFile(t, filepath.Join(dir, "main.txt"), "main\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main advance")

	sw := &StagingWorktree{
		WorktreeContext: *wc,
	}

	// Use "false" as build command (always fails).
	err = sw.MergeToMain("main", nil, "false", "")
	if err == nil {
		t.Fatal("expected error for build failure, got nil")
	}
	if errors.Is(err, ErrMergeRace) {
		t.Error("build failure should NOT return ErrMergeRace")
	}
	if !strings.Contains(err.Error(), "build failed") {
		t.Errorf("expected error to mention build failure, got: %s", err.Error())
	}
}
