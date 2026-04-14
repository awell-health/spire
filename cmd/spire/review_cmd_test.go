package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo creates a temporary bare-minimum git repo with the given number
// of commits. Returns the repo path and a slice of commit SHAs (oldest first).
func initTestRepo(t *testing.T, numCommits int) (string, []string) {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")

	var shas []string
	for i := 1; i <= numCommits; i++ {
		fname := filepath.Join(dir, "file.txt")
		content := strings.Repeat("line\n", i)
		if err := os.WriteFile(fname, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		run("add", "file.txt")
		run("commit", "-m", "commit "+string(rune('0'+i)))
		sha := run("rev-parse", "HEAD")
		shas = append(shas, sha)
	}
	return dir, shas
}

// --- reviewDiff tests ---

func TestReviewDiff_SingleCommit(t *testing.T) {
	repoPath, shas := initTestRepo(t, 1)

	out, err := reviewDiff(repoPath, shas[:1], false)
	if err != nil {
		t.Fatalf("reviewDiff error: %v", err)
	}
	// Single commit should produce a diff without a "# commit" header.
	if strings.Contains(out, "# commit") {
		t.Error("single commit should not have a commit header prefix")
	}
	if !strings.Contains(out, "file.txt") {
		t.Error("diff output should reference file.txt")
	}
}

func TestReviewDiff_SingleCommit_Stats(t *testing.T) {
	repoPath, shas := initTestRepo(t, 1)

	out, err := reviewDiff(repoPath, shas[:1], true)
	if err != nil {
		t.Fatalf("reviewDiff error: %v", err)
	}
	if !strings.Contains(out, "file.txt") {
		t.Error("stats output should reference file.txt")
	}
	// Stats output has change summary like "1 file changed"
	if !strings.Contains(out, "changed") {
		t.Error("stats output should contain change summary")
	}
}

func TestReviewDiff_MultipleCommits(t *testing.T) {
	repoPath, shas := initTestRepo(t, 3)

	out, err := reviewDiff(repoPath, shas, false)
	if err != nil {
		t.Fatalf("reviewDiff error: %v", err)
	}
	// Multiple commits should have per-commit headers.
	for _, sha := range shas {
		if !strings.Contains(out, "# commit "+sha) {
			t.Errorf("expected header for commit %s", sha[:8])
		}
	}
}

func TestReviewDiff_MultipleCommits_Stats(t *testing.T) {
	repoPath, shas := initTestRepo(t, 2)

	out, err := reviewDiff(repoPath, shas, true)
	if err != nil {
		t.Fatalf("reviewDiff error: %v", err)
	}
	// Stats mode should not have commit headers.
	if strings.Contains(out, "# commit") {
		t.Error("stats mode should not have commit headers")
	}
	if !strings.Contains(out, "file.txt") {
		t.Error("stats output should reference file.txt")
	}
}

func TestReviewDiff_UnreachableCommit(t *testing.T) {
	repoPath, _ := initTestRepo(t, 1)

	fakeSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	out, err := reviewDiff(repoPath, []string{fakeSHA}, false)
	// Should not return an error — gracefully degrades per commit.
	if err != nil {
		t.Fatalf("reviewDiff should not error: %v", err)
	}
	if !strings.Contains(out, "git show failed") {
		t.Error("expected failure message for unreachable commit")
	}
}

func TestReviewDiff_MixedReachableUnreachable(t *testing.T) {
	repoPath, shas := initTestRepo(t, 2)

	fakeSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	mixed := []string{shas[0], fakeSHA, shas[1]}
	out, err := reviewDiff(repoPath, mixed, false)
	if err != nil {
		t.Fatalf("reviewDiff should not error: %v", err)
	}
	// Should have output for real commits and failure for fake one.
	if !strings.Contains(out, "# commit "+shas[0]) {
		t.Error("expected header for first real commit")
	}
	if !strings.Contains(out, "git show failed") {
		t.Error("expected failure for fake commit")
	}
	if !strings.Contains(out, "# commit "+shas[1]) {
		t.Error("expected header for second real commit")
	}
}

// --- filterReachableCommits tests ---

func TestFilterReachableCommits_AllReachable(t *testing.T) {
	repoPath, shas := initTestRepo(t, 3)

	got := filterReachableCommits(repoPath, shas)
	if len(got) != 3 {
		t.Fatalf("expected 3 reachable, got %d", len(got))
	}
	for i, s := range shas {
		if got[i] != s {
			t.Errorf("got[%d] = %s, want %s", i, got[i], s)
		}
	}
}

func TestFilterReachableCommits_NoneReachable(t *testing.T) {
	repoPath, _ := initTestRepo(t, 1)

	fakes := []string{
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"0000000000000000000000000000000000000000",
	}
	got := filterReachableCommits(repoPath, fakes)
	if len(got) != 0 {
		t.Fatalf("expected 0 reachable, got %d", len(got))
	}
}

func TestFilterReachableCommits_Mixed(t *testing.T) {
	repoPath, shas := initTestRepo(t, 2)

	mixed := []string{shas[0], "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", shas[1]}
	got := filterReachableCommits(repoPath, mixed)
	if len(got) != 2 {
		t.Fatalf("expected 2 reachable, got %d", len(got))
	}
	if got[0] != shas[0] || got[1] != shas[1] {
		t.Errorf("unexpected reachable commits: %v", got)
	}
}

func TestFilterReachableCommits_Empty(t *testing.T) {
	repoPath, _ := initTestRepo(t, 1)

	got := filterReachableCommits(repoPath, nil)
	if len(got) != 0 {
		t.Fatalf("expected 0 reachable for nil input, got %d", len(got))
	}
}
