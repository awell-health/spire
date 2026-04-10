package git

import (
	"errors"
	"fmt"
	"os"
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
	if err := sw.MergeToMain("main", nil, "", "", nil); err != nil {
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
	if err := sw.MergeToMain("main", nil, "", "", nil); err != nil {
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

	err = sw.MergeToMain("main", nil, "", testCmd, nil)
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

	err = sw.MergeToMain("main", nil, "", testCmd, nil)
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

	err = sw.MergeToMain("main", nil, "", "", nil)
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
	if err := sw.MergeToMain("main", nil, "true", "true", nil); err != nil {
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
	err = sw.MergeToMain("main", nil, "false", "", nil)
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

// testConflictResolver returns a resolver that removes conflict markers (keeping
// both sides), stages the files, and commits — matching the production resolver's
// contract (resolve + stage + CommitMerge).
func testConflictResolver(t *testing.T) func(string, string) error {
	t.Helper()
	return func(dir, branch string) error {
		wc := &WorktreeContext{Dir: dir}
		files, err := wc.ConflictedFiles()
		if err != nil {
			return err
		}
		for _, f := range files {
			path := filepath.Join(dir, f)
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			resolved := resolveTestConflictMarkers(string(content))
			if err := os.WriteFile(path, []byte(resolved), 0644); err != nil {
				return err
			}
		}
		if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
			return fmt.Errorf("git add: %w\n%s", err, out)
		}
		return wc.CommitMerge()
	}
}

// resolveTestConflictMarkers strips git conflict markers, keeping both sides.
func resolveTestConflictMarkers(content string) string {
	lines := strings.Split(content, "\n")
	var result []string
	for _, line := range lines {
		if strings.HasPrefix(line, "<<<<<<<") ||
			strings.HasPrefix(line, "=======") ||
			strings.HasPrefix(line, ">>>>>>>") {
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

// TestMergeToMain_ConflictResolvedByResolver verifies that when a rebase hits
// conflicts and a resolver is provided, the resolver is called, conflicts are
// resolved, and the merge succeeds.
func TestMergeToMain_ConflictResolvedByResolver(t *testing.T) {
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

	sw := &StagingWorktree{WorktreeContext: *wc}

	resolver := testConflictResolver(t)
	if err := sw.MergeToMain("main", nil, "", "", resolver); err != nil {
		t.Fatalf("MergeToMain with resolver: %v", err)
	}

	// Verify main has content from both branches.
	mainContent := run(t, dir, "git", "show", "HEAD:README.md")
	if !strings.Contains(mainContent, "main version") || !strings.Contains(mainContent, "staging version") {
		t.Errorf("expected both versions in merged README.md, got: %s", mainContent)
	}
}

// TestMergeToMain_ResolverFailureExhaustsRetries verifies that when the resolver
// fails on every attempt, all maxMergeAttempts are tried before returning ErrMergeRace.
func TestMergeToMain_ResolverFailureExhaustsRetries(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("staging/test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	wc, err := rc.CreateWorktree(wtDir, "staging/test")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "README.md"), "staging version\n")
	wc.Commit("staging edit README")

	// Advance main with a conflicting edit.
	writeFile(t, filepath.Join(dir, "README.md"), "main version\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main edit README")

	sw := &StagingWorktree{WorktreeContext: *wc}

	// Resolver that always fails.
	attempts := 0
	failResolver := func(dir, branch string) error {
		attempts++
		return fmt.Errorf("intentional failure")
	}

	err = sw.MergeToMain("main", nil, "", "", failResolver)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrMergeRace) {
		t.Fatalf("expected ErrMergeRace, got: %v", err)
	}
	if attempts != maxMergeAttempts {
		t.Errorf("expected %d resolver attempts, got %d", maxMergeAttempts, attempts)
	}
}

// TestMergeToMain_MultiCommitConflictsResolved verifies that a multi-commit
// rebase where each commit conflicts is resolved by calling the resolver
// multiple times (once per conflicting commit).
func TestMergeToMain_MultiCommitConflictsResolved(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create staging branch with two commits, each editing a different file.
	rc.CreateBranch("staging/test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	wc, err := rc.CreateWorktree(wtDir, "staging/test")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	wc.ConfigureUser("Test", "test@test.com")

	// Commit 1: edit README.md.
	writeFile(t, filepath.Join(wtDir, "README.md"), "staging readme\n")
	run(t, wtDir, "git", "add", "-A")
	run(t, wtDir, "git", "commit", "-m", "staging commit 1: readme")

	// Commit 2: create file2.txt.
	writeFile(t, filepath.Join(wtDir, "file2.txt"), "staging file2\n")
	run(t, wtDir, "git", "add", "-A")
	run(t, wtDir, "git", "commit", "-m", "staging commit 2: file2")

	// Advance main with conflicting edits to BOTH files.
	writeFile(t, filepath.Join(dir, "README.md"), "main readme\n")
	writeFile(t, filepath.Join(dir, "file2.txt"), "main file2\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main conflicting edits")

	sw := &StagingWorktree{WorktreeContext: *wc}

	resolver := testConflictResolver(t)
	resolverCalls := 0
	countingResolver := func(dir, branch string) error {
		resolverCalls++
		return resolver(dir, branch)
	}

	if err := sw.MergeToMain("main", nil, "", "", countingResolver); err != nil {
		t.Fatalf("MergeToMain with multi-commit conflicts: %v", err)
	}

	// Resolver should be called at least twice (once per conflicting commit).
	if resolverCalls < 2 {
		t.Errorf("expected resolver to be called at least 2 times, got %d", resolverCalls)
	}

	// Verify main has content from the staging branch.
	readmeContent := run(t, dir, "git", "show", "HEAD:README.md")
	if !strings.Contains(readmeContent, "staging readme") {
		t.Errorf("expected staging readme in merged content, got: %s", readmeContent)
	}
}

// =============================================================================
// MergeBranch tests
// =============================================================================

// TestMergeBranch_RebaseSucceedsWhileChildBranchIsCheckedOutElsewhere verifies
// that the staging rebase path does not require mutating the child branch ref.
// This mirrors epic wave dispatch, where child feature branches may still be
// checked out in their own worktrees when the staging branch integrates them.
func TestMergeBranch_RebaseSucceedsWhileChildBranchIsCheckedOutElsewhere(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	stageDir := filepath.Join(t.TempDir(), "staging-wt")
	if err := rc.ForceBranch("staging/spi-j12j9", "main"); err != nil {
		t.Fatalf("ForceBranch staging: %v", err)
	}
	stageWC, err := rc.CreateWorktree(stageDir, "staging/spi-j12j9")
	if err != nil {
		t.Fatalf("CreateWorktree staging: %v", err)
	}
	stageWC.ConfigureUser("Test", "test@test.com")
	sw := &StagingWorktree{WorktreeContext: *stageWC}

	wt1Dir := filepath.Join(t.TempDir(), "child-1")
	wt1, err := rc.CreateWorktreeNewBranch(wt1Dir, "feat/spi-j12j9.1", "main")
	if err != nil {
		t.Fatalf("CreateWorktreeNewBranch child 1: %v", err)
	}
	wt1.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wt1Dir, "child1.txt"), "child 1\n")
	if _, err := wt1.Commit("child 1"); err != nil {
		t.Fatalf("Commit child 1: %v", err)
	}
	child1Head := trimNewline(run(t, wt1Dir, "git", "rev-parse", "HEAD"))

	wt2Dir := filepath.Join(t.TempDir(), "child-2")
	wt2, err := rc.CreateWorktreeNewBranch(wt2Dir, "feat/spi-j12j9.2", "main")
	if err != nil {
		t.Fatalf("CreateWorktreeNewBranch child 2: %v", err)
	}
	wt2.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wt2Dir, "child2.txt"), "child 2\n")
	if _, err := wt2.Commit("child 2"); err != nil {
		t.Fatalf("Commit child 2: %v", err)
	}

	// First child merges as a fast-forward. Second child requires the rebase
	// path because staging has advanced, but its branch remains checked out in
	// its own worktree throughout the test.
	if err := sw.MergeBranch("feat/spi-j12j9.2", nil); err != nil {
		t.Fatalf("MergeBranch child 2: %v", err)
	}
	if err := sw.MergeBranch("feat/spi-j12j9.1", nil); err != nil {
		t.Fatalf("MergeBranch child 1 via rebase path: %v", err)
	}

	if _, err := exec.Command("git", "-C", stageDir, "cat-file", "-e", "HEAD:child1.txt").Output(); err != nil {
		t.Error("staging should contain child1.txt after merge")
	}
	if _, err := exec.Command("git", "-C", stageDir, "cat-file", "-e", "HEAD:child2.txt").Output(); err != nil {
		t.Error("staging should contain child2.txt after merge")
	}

	// The child branch remains intact in its original worktree; staging
	// integration should not have to rewrite it in-place.
	if got := trimNewline(run(t, wt1Dir, "git", "rev-parse", "HEAD")); got != child1Head {
		t.Fatalf("child branch HEAD changed in its own worktree: got %s want %s", got, child1Head)
	}
}
