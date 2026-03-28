package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initTestRepo creates a temporary git repo with an initial commit.
// Returns the repo directory path.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Initialize a git repo.
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.name", "Test")
	run(t, dir, "git", "config", "user.email", "test@test.com")

	// Create an initial commit on main.
	writeFile(t, filepath.Join(dir, "README.md"), "# Test Repo\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "initial commit")

	// Ensure we're on the "main" branch.
	run(t, dir, "git", "branch", "-M", "main")

	return dir
}

// run executes a command in the given dir and fails the test on error.
func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
	return string(out)
}

// writeFile writes content to a file, creating directories as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0755)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// =============================================================================
// RepoContext tests
// =============================================================================

func TestRepoContext_CreateBranch(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	err := rc.CreateBranch("feature/test")
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	if !rc.BranchExists("feature/test") {
		t.Error("expected branch feature/test to exist")
	}
}

func TestRepoContext_BranchExists(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	if !rc.BranchExists("main") {
		t.Error("expected main branch to exist")
	}
	if rc.BranchExists("nonexistent-branch") {
		t.Error("nonexistent branch should not exist")
	}
}

func TestRepoContext_ListBranches(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("feat/one")
	rc.CreateBranch("feat/two")
	rc.CreateBranch("other/branch")

	branches := rc.ListBranches("feat/*")
	if len(branches) != 2 {
		t.Errorf("expected 2 branches matching feat/*, got %d: %v", len(branches), branches)
	}
}

func TestRepoContext_ForceBranch(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// ForceBranch creates a new branch at the given start point.
	err := rc.ForceBranch("staging/test", "HEAD")
	if err != nil {
		t.Fatalf("ForceBranch: %v", err)
	}

	if !rc.BranchExists("staging/test") {
		t.Error("expected staging/test branch to exist after ForceBranch")
	}

	// Verify the branch points to the same commit as HEAD.
	headSHA := rc.HeadSHA()
	branchSHA := run(t, dir, "git", "rev-parse", "staging/test")
	if headSHA == "" || headSHA != trimNewline(branchSHA) {
		t.Errorf("ForceBranch branch SHA %q != HEAD SHA %q", trimNewline(branchSHA), headSHA)
	}
}

// TestRepoContext_ForceBranch_AnchorToBaseBranch verifies that ForceBranch
// anchors to the explicit start-point parameter, fixing the old bug where
// staging branches were always created from HEAD regardless of intent.
// (Fixed in spi-mswj8: ForceBranch now takes an explicit startPoint.)
func TestRepoContext_ForceBranch_AnchorToBaseBranch(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create and switch to a feature branch with a new commit.
	rc.CreateBranch("feature/other")
	run(t, dir, "git", "checkout", "feature/other")
	writeFile(t, filepath.Join(dir, "feature.txt"), "feature work\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "feature commit")

	// Now HEAD is on feature/other, not main.
	// With the explicit startPoint, ForceBranch anchors to BaseBranch.
	err := rc.ForceBranch("staging/test", rc.BaseBranch)
	if err != nil {
		t.Fatalf("ForceBranch: %v", err)
	}

	stagingSHA := trimNewline(run(t, dir, "git", "rev-parse", "staging/test"))
	mainSHA := trimNewline(run(t, dir, "git", "rev-parse", "main"))

	// With explicit startPoint=BaseBranch, staging should match main.
	if stagingSHA != mainSHA {
		t.Errorf("ForceBranch(startPoint=%q) SHA %q != main SHA %q", rc.BaseBranch, stagingSHA, mainSHA)
	}
}

func TestRepoContext_CurrentBranch(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	branch := rc.CurrentBranch()
	if branch != "main" {
		t.Errorf("CurrentBranch = %q, want main", branch)
	}
}

func TestRepoContext_HeadSHA(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	sha := rc.HeadSHA()
	if sha == "" {
		t.Error("HeadSHA returned empty string")
	}
	if len(sha) < 40 {
		t.Errorf("HeadSHA returned short SHA: %q", sha)
	}
}

func TestRepoContext_Checkout(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("feature/checkout")
	err := rc.Checkout("feature/checkout")
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	if rc.CurrentBranch() != "feature/checkout" {
		t.Errorf("CurrentBranch = %q, want feature/checkout", rc.CurrentBranch())
	}
}

func TestRepoContext_DeleteBranch(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("temp-branch")
	if !rc.BranchExists("temp-branch") {
		t.Fatal("temp-branch should exist before delete")
	}

	err := rc.DeleteBranch("temp-branch")
	if err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}

	if rc.BranchExists("temp-branch") {
		t.Error("temp-branch should not exist after delete")
	}
}

func TestRepoContext_MergeFFOnly(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create a feature branch with a commit.
	rc.CreateBranch("feature/merge-test")
	run(t, dir, "git", "checkout", "feature/merge-test")
	writeFile(t, filepath.Join(dir, "merged.txt"), "merged content\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "feature work")
	featureSHA := rc.HeadSHA()

	// Go back to main and ff-merge.
	run(t, dir, "git", "checkout", "main")

	err := rc.MergeFFOnly("feature/merge-test", nil)
	if err != nil {
		t.Fatalf("MergeFFOnly: %v", err)
	}

	// Verify main is at the same commit as the feature branch.
	if rc.HeadSHA() != featureSHA {
		t.Error("after ff-merge, main should be at feature branch HEAD")
	}
}

func TestRepoContext_MergeFFOnly_FailsOnDiverge(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	// Create a feature branch.
	rc.CreateBranch("feature/diverge")
	run(t, dir, "git", "checkout", "feature/diverge")
	writeFile(t, filepath.Join(dir, "feature.txt"), "feature\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "feature commit")

	// Go back to main and add a different commit (diverge).
	run(t, dir, "git", "checkout", "main")
	writeFile(t, filepath.Join(dir, "main.txt"), "main\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main commit")

	// ff-only should fail because branches have diverged.
	err := rc.MergeFFOnly("feature/diverge", nil)
	if err == nil {
		t.Fatal("expected error for diverged ff-merge, got nil")
	}
}

// =============================================================================
// Worktree lifecycle tests
// =============================================================================

func TestRepoContext_CreateWorktree(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("wt-branch")
	wtDir := filepath.Join(t.TempDir(), "my-worktree")

	wc, err := rc.CreateWorktree(wtDir, "wt-branch")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	if wc.Dir != wtDir {
		t.Errorf("worktree Dir = %q, want %q", wc.Dir, wtDir)
	}
	if wc.Branch != "wt-branch" {
		t.Errorf("worktree Branch = %q, want wt-branch", wc.Branch)
	}

	// Verify the worktree directory exists and has the README.
	if _, err := os.Stat(filepath.Join(wtDir, "README.md")); os.IsNotExist(err) {
		t.Error("README.md should exist in worktree")
	}
}

func TestRepoContext_CreateWorktreeNewBranch(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	wtDir := filepath.Join(t.TempDir(), "new-branch-wt")
	wc, err := rc.CreateWorktreeNewBranch(wtDir, "new-feat", "main")
	if err != nil {
		t.Fatalf("CreateWorktreeNewBranch: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	if wc.Branch != "new-feat" {
		t.Errorf("worktree Branch = %q, want new-feat", wc.Branch)
	}

	// The new branch should exist in the main repo.
	if !rc.BranchExists("new-feat") {
		t.Error("new-feat branch should exist after CreateWorktreeNewBranch")
	}
}

func TestRepoContext_RemoveWorktree(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("wt-remove")
	wtDir := filepath.Join(t.TempDir(), "remove-wt")
	_, err := rc.CreateWorktree(wtDir, "wt-remove")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	err = rc.RemoveWorktree(wtDir)
	if err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
}

// =============================================================================
// WorktreeContext tests
// =============================================================================

func TestWorktreeContext_Commit(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("wt-commit")
	wtDir := filepath.Join(t.TempDir(), "commit-wt")
	wc, err := rc.CreateWorktree(wtDir, "wt-commit")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// Configure user for the worktree.
	wc.ConfigureUser("Test", "test@test.com")

	// Write a file and commit.
	writeFile(t, filepath.Join(wtDir, "new-file.txt"), "content\n")
	sha, err := wc.Commit("test commit")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if sha == "" {
		t.Error("Commit should return non-empty SHA")
	}
}

func TestWorktreeContext_CommitEmpty(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("wt-empty")
	wtDir := filepath.Join(t.TempDir(), "empty-wt")
	wc, err := rc.CreateWorktree(wtDir, "wt-empty")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// Commit with no changes should return ("", nil).
	sha, err := wc.Commit("empty commit")
	if err != nil {
		t.Fatalf("Commit error: %v", err)
	}
	if sha != "" {
		t.Errorf("Commit on clean tree should return empty SHA, got %q", sha)
	}
}

func TestWorktreeContext_HasNewCommits(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("wt-new-commits")
	wtDir := filepath.Join(t.TempDir(), "newcommits-wt")
	wc, err := rc.CreateWorktree(wtDir, "wt-new-commits")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// Initially no new commits.
	if wc.HasNewCommits() {
		t.Error("should have no new commits initially")
	}

	// Add a commit.
	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "new.txt"), "new\n")
	_, err = wc.Commit("new commit")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if !wc.HasNewCommits() {
		t.Error("should have new commits after committing")
	}
}

func TestWorktreeContext_HasUncommittedChanges(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("wt-uncommitted")
	wtDir := filepath.Join(t.TempDir(), "uncommitted-wt")
	wc, err := rc.CreateWorktree(wtDir, "wt-uncommitted")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	// Clean tree — no uncommitted changes.
	if wc.HasUncommittedChanges() {
		t.Error("should have no uncommitted changes on clean tree")
	}

	// Write a file without committing.
	writeFile(t, filepath.Join(wtDir, "dirty.txt"), "dirty\n")

	if !wc.HasUncommittedChanges() {
		t.Error("should have uncommitted changes after writing a file")
	}
}

func TestWorktreeContext_Diff(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("wt-diff")
	wtDir := filepath.Join(t.TempDir(), "diff-wt")
	wc, err := rc.CreateWorktree(wtDir, "wt-diff")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	wc.ConfigureUser("Test", "test@test.com")
	writeFile(t, filepath.Join(wtDir, "diff-file.txt"), "content\n")
	_, err = wc.Commit("diff commit")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	diff, err := wc.Diff("main")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff == "" {
		t.Error("diff should be non-empty after a commit")
	}
}

func TestWorktreeContext_CommitCleanFiles(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	rc.CreateBranch("wt-clean")
	wtDir := filepath.Join(t.TempDir(), "clean-wt")
	wc, err := rc.CreateWorktree(wtDir, "wt-clean")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	defer rc.ForceRemoveWorktree(wtDir)

	wc.ConfigureUser("Test", "test@test.com")

	// Create a temp file that should be cleaned before commit.
	writeFile(t, filepath.Join(wtDir, "prompt.txt"), "temp prompt\n")
	writeFile(t, filepath.Join(wtDir, "real.txt"), "real content\n")

	sha, err := wc.Commit("commit with cleanup", "prompt.txt")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if sha == "" {
		t.Error("Commit should return non-empty SHA (real.txt should be committed)")
	}

	// Verify prompt.txt was removed.
	if _, err := os.Stat(filepath.Join(wtDir, "prompt.txt")); !os.IsNotExist(err) {
		t.Error("prompt.txt should have been removed before commit")
	}
}

// Helper to trim trailing newlines from command output.
func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
