package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStashUncommitted_TrackedFile asserts that a modified tracked file gets
// stashed, the worktree returns to clean, and the result carries the
// stash@{0} ref + a non-empty SHA + the changed path.
func TestStashUncommitted_TrackedFile(t *testing.T) {
	dir := initTestRepo(t)

	// Modify the tracked README so `git status` shows a dirty tree.
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# modified\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	res, err := StashUncommitted(dir, "spire-autoStash:spi-test:implement")
	if err != nil {
		t.Fatalf("StashUncommitted: %v", err)
	}
	if !res.Created {
		t.Fatal("expected Created=true for dirty tree")
	}
	if res.Ref != "stash@{0}" {
		t.Errorf("Ref = %q, want stash@{0}", res.Ref)
	}
	if res.SHA == "" || len(res.SHA) < 7 {
		t.Errorf("SHA = %q, want non-empty commit sha", res.SHA)
	}
	if !sliceContains(res.Files, "README.md") {
		t.Errorf("Files = %v, want to contain README.md", res.Files)
	}

	// Worktree should be clean again.
	status := run(t, dir, "git", "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("worktree not clean after stash: %q", status)
	}

	// Stash list should have an entry whose subject matches our message.
	list := run(t, dir, "git", "stash", "list")
	if !strings.Contains(list, "spire-autoStash:spi-test:implement") {
		t.Errorf("git stash list does not show our stash entry: %q", list)
	}
}

// TestStashUncommitted_UntrackedFile asserts that `-u` is in effect: a brand-
// new file with no tracked history is included in the stash.
func TestStashUncommitted_UntrackedFile(t *testing.T) {
	dir := initTestRepo(t)

	untracked := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(untracked, []byte("apprentice half-edit\n"), 0644); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	res, err := StashUncommitted(dir, "spire-autoStash:spi-test:implement")
	if err != nil {
		t.Fatalf("StashUncommitted: %v", err)
	}
	if !res.Created {
		t.Fatal("expected Created=true for untracked file (verifies -u flag)")
	}
	if !sliceContains(res.Files, "new.txt") {
		t.Errorf("Files = %v, want to contain new.txt (verifies -u captured untracked)", res.Files)
	}

	// The untracked file must be gone after stashing — `-u` moves it into
	// the stash, not just the index. If it's still here, the flag isn't
	// being applied.
	if _, err := os.Stat(untracked); !os.IsNotExist(err) {
		t.Errorf("expected untracked file removed after stash, stat=%v", err)
	}
}

// TestStashUncommitted_CleanTree asserts a no-op-and-no-error on a clean
// tree. The validator's policy is to gate this call on dirtyState.Dirty,
// but the primitive should still return a benign zero-value rather than
// fail or "succeed" with a misleading SHA.
func TestStashUncommitted_CleanTree(t *testing.T) {
	dir := initTestRepo(t)

	res, err := StashUncommitted(dir, "spire-autoStash:spi-test:implement")
	if err != nil {
		t.Fatalf("StashUncommitted on clean tree: %v", err)
	}
	if res.Created {
		t.Errorf("expected Created=false on clean tree, got %+v", res)
	}
	if res.SHA != "" {
		t.Errorf("expected empty SHA on clean tree, got %q", res.SHA)
	}

	// And no entry should appear on `git stash list`.
	list := run(t, dir, "git", "stash", "list")
	if strings.TrimSpace(list) != "" {
		t.Errorf("expected empty stash list, got %q", list)
	}
}

// TestStashUncommitted_EmptyPath returns an error rather than running git
// against the current process working dir.
func TestStashUncommitted_EmptyPath(t *testing.T) {
	if _, err := StashUncommitted("", "msg"); err == nil {
		t.Error("expected error for empty path")
	}
}

// TestStashUncommitted_EmptyMessage returns an error rather than producing
// a stash with no subject — operators trace back from the message.
func TestStashUncommitted_EmptyMessage(t *testing.T) {
	dir := initTestRepo(t)
	if _, err := StashUncommitted(dir, ""); err == nil {
		t.Error("expected error for empty message")
	}
}

func sliceContains(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}
