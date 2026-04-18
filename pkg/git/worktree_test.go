package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// =============================================================================
// fileExists / readFileTrim helpers
// =============================================================================

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.txt")
	writeFile(t, existing, "hi\n")

	if !fileExists(existing) {
		t.Errorf("fileExists(%q) = false, want true", existing)
	}
	if fileExists(filepath.Join(dir, "absent.txt")) {
		t.Error("fileExists on missing file returned true")
	}
	// Directories also exist — stat does not distinguish.
	if !fileExists(dir) {
		t.Error("fileExists on dir returned false")
	}
}

func TestReadFileTrim(t *testing.T) {
	dir := t.TempDir()
	trimmed := filepath.Join(dir, "trim.txt")
	writeFile(t, trimmed, "   abc123   \n\n")
	if s, ok := readFileTrim(trimmed); !ok || s != "abc123" {
		t.Errorf("readFileTrim(%q) = (%q, %v), want (\"abc123\", true)", trimmed, s, ok)
	}

	whitespace := filepath.Join(dir, "ws.txt")
	writeFile(t, whitespace, "   \n\t  ")
	if s, ok := readFileTrim(whitespace); ok {
		t.Errorf("readFileTrim(whitespace) = (%q, true), want (\"\", false)", s)
	}

	if _, ok := readFileTrim(filepath.Join(dir, "missing.txt")); ok {
		t.Error("readFileTrim on missing file returned ok=true")
	}
}

// =============================================================================
// WorktreeContext.resolveGitDir
// =============================================================================

// TestResolveGitDir_MainRepo verifies resolveGitDir for a normal (non-linked)
// repo — .git is a directory inside wc.Dir.
func TestResolveGitDir_MainRepo(t *testing.T) {
	dir := initTestRepo(t)
	wc := &WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}

	gitDir := wc.resolveGitDir()
	if gitDir == "" {
		t.Fatal("resolveGitDir returned empty")
	}
	if !filepath.IsAbs(gitDir) {
		t.Errorf("resolveGitDir returned non-absolute path: %q", gitDir)
	}
	// For a main repo the git dir should be <dir>/.git.
	want := filepath.Join(dir, ".git")
	// resolveGitDir may return a path with a trailing slash or not — normalize.
	if filepath.Clean(gitDir) != filepath.Clean(want) {
		t.Errorf("resolveGitDir = %q, want %q", gitDir, want)
	}
}

// TestResolveGitDir_LinkedWorktree verifies that for a linked worktree,
// resolveGitDir returns the per-worktree gitdir (inside the main repo's
// .git/worktrees/<name>/), not the main .git.
func TestResolveGitDir_LinkedWorktree(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}
	rc.CreateBranch("feat/linked")

	wtDir := filepath.Join(t.TempDir(), "wt-linked")
	wc, err := rc.CreateWorktree(wtDir, "feat/linked")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	gitDir := wc.resolveGitDir()
	if gitDir == "" {
		t.Fatal("resolveGitDir returned empty for linked worktree")
	}
	// Per-worktree gitdir lives under the main repo's .git/worktrees/.
	if !strings.Contains(gitDir, filepath.Join(".git", "worktrees")) {
		t.Errorf("expected linked worktree gitdir to contain .git/worktrees, got %q", gitDir)
	}
}

func TestResolveGitDir_NotARepo(t *testing.T) {
	dir := t.TempDir()
	wc := &WorktreeContext{Dir: dir}
	if gd := wc.resolveGitDir(); gd != "" {
		t.Errorf("resolveGitDir on non-repo = %q, want empty", gd)
	}
}

// =============================================================================
// WorktreeContext.DetectConflictState
// =============================================================================

func TestDetectConflictState_NoOp(t *testing.T) {
	dir := initTestRepo(t)
	wc := &WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}

	state := wc.DetectConflictState()
	if state.InProgressOp != "" {
		t.Errorf("InProgressOp = %q, want empty when nothing in progress", state.InProgressOp)
	}
	if state.HeadSHA == "" {
		t.Error("HeadSHA should still resolve even with no in-progress op")
	}
	if state.IncomingSHA != "" {
		t.Errorf("IncomingSHA = %q, want empty", state.IncomingSHA)
	}
}

// TestDetectConflictState_Rebase creates a real paused rebase with a conflict,
// then asserts DetectConflictState reports op=rebase and resolves HEAD +
// IncomingSHA to real commit SHAs.
func TestDetectConflictState_Rebase(t *testing.T) {
	dir := initTestRepo(t)

	// Create two divergent commits touching the same file.
	writeFile(t, filepath.Join(dir, "conflict.txt"), "base\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "base")

	// Branch, modify, commit.
	run(t, dir, "git", "checkout", "-b", "feat/conflict")
	writeFile(t, filepath.Join(dir, "conflict.txt"), "branch-side\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "feat(spi-abc12): branch change")
	branchSHA := strings.TrimSpace(run(t, dir, "git", "rev-parse", "HEAD"))

	// Back to main and make a conflicting change.
	run(t, dir, "git", "checkout", "main")
	writeFile(t, filepath.Join(dir, "conflict.txt"), "main-side\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "feat(spi-xyz99): main change")
	mainSHA := strings.TrimSpace(run(t, dir, "git", "rev-parse", "HEAD"))

	// Rebase feat/conflict onto main — expected to pause on conflict.
	run(t, dir, "git", "checkout", "feat/conflict")
	// rebase is expected to fail (return nonzero) — use the raw exec rather
	// than run() which fails the test.
	_ = runAllow(t, dir, "git", "rebase", "main")

	wc := &WorktreeContext{Dir: dir, RepoPath: dir, Branch: "feat/conflict", BaseBranch: "main"}
	state := wc.DetectConflictState()
	if state.InProgressOp != "rebase" {
		t.Errorf("InProgressOp = %q, want %q", state.InProgressOp, "rebase")
	}
	if state.HeadSHA == "" {
		t.Error("HeadSHA empty during paused rebase")
	}
	if state.IncomingSHA == "" {
		t.Error("IncomingSHA empty during paused rebase")
	}
	// During rebase, HEAD is the rebased-onto main tip (mainSHA) and the
	// incoming commit being replayed is branchSHA. Don't assert exact order —
	// git rebase implementations vary — just assert both show up among the
	// real SHAs.
	seen := map[string]bool{state.HeadSHA: true, state.IncomingSHA: true}
	if !seen[mainSHA] && !seen[branchSHA] {
		t.Errorf("neither HeadSHA=%s nor IncomingSHA=%s match mainSHA=%s or branchSHA=%s",
			state.HeadSHA, state.IncomingSHA, mainSHA, branchSHA)
	}
}

// TestDetectConflictState_Merge creates a paused merge conflict and asserts op=merge.
func TestDetectConflictState_Merge(t *testing.T) {
	dir := initTestRepo(t)

	writeFile(t, filepath.Join(dir, "m.txt"), "base\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "base")

	run(t, dir, "git", "checkout", "-b", "feat/merge-conflict")
	writeFile(t, filepath.Join(dir, "m.txt"), "branch\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "branch change")
	branchSHA := strings.TrimSpace(run(t, dir, "git", "rev-parse", "HEAD"))

	run(t, dir, "git", "checkout", "main")
	writeFile(t, filepath.Join(dir, "m.txt"), "main\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main change")

	_ = runAllow(t, dir, "git", "merge", "--no-edit", "feat/merge-conflict")

	wc := &WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}
	state := wc.DetectConflictState()
	if state.InProgressOp != "merge" {
		t.Errorf("InProgressOp = %q, want merge", state.InProgressOp)
	}
	if state.IncomingSHA != branchSHA {
		t.Errorf("IncomingSHA = %q, want %q (branch tip)", state.IncomingSHA, branchSHA)
	}
}

// TestDetectConflictState_CherryPick exercises the cherry-pick path.
func TestDetectConflictState_CherryPick(t *testing.T) {
	dir := initTestRepo(t)

	writeFile(t, filepath.Join(dir, "c.txt"), "base\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "base")

	run(t, dir, "git", "checkout", "-b", "feat/cp")
	writeFile(t, filepath.Join(dir, "c.txt"), "branch\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "branch")
	branchSHA := strings.TrimSpace(run(t, dir, "git", "rev-parse", "HEAD"))

	run(t, dir, "git", "checkout", "main")
	writeFile(t, filepath.Join(dir, "c.txt"), "main\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "main")

	_ = runAllow(t, dir, "git", "cherry-pick", branchSHA)

	wc := &WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}
	state := wc.DetectConflictState()
	if state.InProgressOp != "cherry-pick" {
		t.Errorf("InProgressOp = %q, want cherry-pick", state.InProgressOp)
	}
	if state.IncomingSHA != branchSHA {
		t.Errorf("IncomingSHA = %q, want %q", state.IncomingSHA, branchSHA)
	}
}

// =============================================================================
// WorktreeContext.ShowCommit
// =============================================================================

func TestShowCommit_BasicFields(t *testing.T) {
	dir := initTestRepo(t)
	writeFile(t, filepath.Join(dir, "f.txt"), "hi\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "feat(spi-abc12): add f.txt\n\nbody line 1\nbody line 2")
	sha := strings.TrimSpace(run(t, dir, "git", "rev-parse", "HEAD"))

	wc := &WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}
	md, err := wc.ShowCommit(sha)
	if err != nil {
		t.Fatalf("ShowCommit: %v", err)
	}
	if md.SHA != sha {
		t.Errorf("SHA = %q, want %q", md.SHA, sha)
	}
	if md.Subject != "feat(spi-abc12): add f.txt" {
		t.Errorf("Subject = %q, want %q", md.Subject, "feat(spi-abc12): add f.txt")
	}
	if !strings.Contains(md.Author, "Test") || !strings.Contains(md.Author, "test@test.com") {
		t.Errorf("Author = %q, want to contain name and email", md.Author)
	}
	if md.Date == "" {
		t.Error("Date empty")
	}
	if !strings.Contains(md.Body, "body line 1") {
		t.Errorf("Body = %q, want to contain body lines", md.Body)
	}
}

func TestShowCommit_EmptySHA(t *testing.T) {
	wc := &WorktreeContext{Dir: "/tmp"}
	md, err := wc.ShowCommit("")
	if err == nil {
		t.Fatal("expected error for empty SHA")
	}
	if md != nil {
		t.Errorf("expected nil metadata on error, got %+v", md)
	}
}

func TestShowCommit_UnknownSHA(t *testing.T) {
	dir := initTestRepo(t)
	wc := &WorktreeContext{Dir: dir}
	// 40 zero chars — valid hex length but almost certainly not a real SHA.
	_, err := wc.ShowCommit("0000000000000000000000000000000000000000")
	if err == nil {
		t.Error("expected error for nonexistent SHA")
	}
}

// =============================================================================
// WorktreeContext.FileLog
// =============================================================================

func TestFileLog_ReturnsRecentHistory(t *testing.T) {
	dir := initTestRepo(t)
	path := filepath.Join(dir, "tracked.txt")

	// Three commits, each touching tracked.txt with distinct subjects.
	for i, msg := range []string{"first", "second", "third"} {
		writeFile(t, path, msg+"\n")
		run(t, dir, "git", "add", "-A")
		run(t, dir, "git", "commit", "-m", msg+" commit")
		_ = i
	}

	wc := &WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}
	out, err := wc.FileLog("tracked.txt", 10)
	if err != nil {
		t.Fatalf("FileLog: %v", err)
	}
	for _, want := range []string{"first commit", "second commit", "third commit"} {
		if !strings.Contains(out, want) {
			t.Errorf("FileLog output missing %q; got:\n%s", want, out)
		}
	}
	// --pretty=fuller emits AuthorDate + CommitDate headers.
	if !strings.Contains(out, "AuthorDate:") {
		t.Errorf("FileLog output missing AuthorDate header (pretty=fuller); got:\n%s", out)
	}
}

func TestFileLog_ZeroLimitFallsBackToDefault(t *testing.T) {
	dir := initTestRepo(t)
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "hi\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "only")

	wc := &WorktreeContext{Dir: dir}
	out, err := wc.FileLog("a.txt", 0)
	if err != nil {
		t.Fatalf("FileLog with zero limit: %v", err)
	}
	if !strings.Contains(out, "only") {
		t.Errorf("FileLog output missing expected commit; got:\n%s", out)
	}
}

func TestFileLog_UnknownPath(t *testing.T) {
	dir := initTestRepo(t)
	wc := &WorktreeContext{Dir: dir}
	// git log on a never-tracked file returns zero output but also zero exit.
	out, err := wc.FileLog("never-existed.txt", 5)
	if err != nil {
		t.Fatalf("FileLog on untracked path should not error, got: %v (out=%q)", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("FileLog on untracked path returned content: %q", out)
	}
}

// =============================================================================
// WorktreeContext.DiffCheck
// =============================================================================

func TestDiffCheck_Clean(t *testing.T) {
	dir := initTestRepo(t)
	wc := &WorktreeContext{Dir: dir}
	out, err := wc.DiffCheck()
	if err != nil {
		t.Fatalf("DiffCheck on clean tree: %v (out=%q)", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("DiffCheck output non-empty on clean tree: %q", out)
	}
}

// TestDiffCheck_FlagsConflictMarkers writes a file with literal conflict
// markers, stages nothing (diff --check works against the index diff), and
// verifies DiffCheck flags it.
func TestDiffCheck_FlagsConflictMarkers(t *testing.T) {
	dir := initTestRepo(t)
	path := filepath.Join(dir, "c.txt")
	writeFile(t, path, "baseline\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "baseline")

	// Overwrite with marker content — stays unstaged so diff --check sees it.
	content := "line1\n<<<<<<< HEAD\nleft\n=======\nright\n>>>>>>> other\nline2\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	wc := &WorktreeContext{Dir: dir}
	out, err := wc.DiffCheck()
	if err == nil {
		t.Fatal("DiffCheck returned nil error with conflict markers present")
	}
	if !strings.Contains(out, "conflict") && !strings.Contains(out, "marker") {
		t.Errorf("DiffCheck output lacks conflict/marker mention: %q", out)
	}
}

// =============================================================================
// WorktreeContext.readRefFile
// =============================================================================

func TestReadRefFile_MissingRef(t *testing.T) {
	dir := initTestRepo(t)
	wc := &WorktreeContext{Dir: dir}
	// No rebase/merge/cherry-pick in progress — MERGE_HEAD won't resolve.
	if _, ok := wc.readRefFile("MERGE_HEAD"); ok {
		t.Error("readRefFile returned ok=true for absent MERGE_HEAD")
	}
}

func TestReadRefFile_ResolvesHEAD(t *testing.T) {
	dir := initTestRepo(t)
	wc := &WorktreeContext{Dir: dir}
	sha, ok := wc.readRefFile("HEAD")
	if !ok {
		t.Fatal("readRefFile(HEAD) returned ok=false")
	}
	if len(sha) != 40 {
		t.Errorf("readRefFile(HEAD) = %q, want a 40-char SHA", sha)
	}
}

// =============================================================================
// Test helpers local to this file
// =============================================================================

// runAllow is like run() but does not fail the test on nonzero exit — used
// when we intentionally trigger a git operation that pauses (e.g. rebase with
// an expected conflict).
func runAllow(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	return string(out)
}
