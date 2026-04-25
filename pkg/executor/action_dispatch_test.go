package executor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	spgit "github.com/awell-health/spire/pkg/git"
)

// --- Helper unit tests for closeChildAfterStagingMerge ------------------

// TestCloseChildAfterStagingMerge_OpenChild_Closes verifies the happy path:
// when the child bead is open, the helper invokes CloseBead exactly once.
func TestCloseChildAfterStagingMerge_OpenChild_Closes(t *testing.T) {
	var closeCalls []string
	deps := &Deps{
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			return nil
		},
	}
	e := NewForTest("spi-epic", "wizard-test", nil, deps)

	e.closeChildAfterStagingMerge("spi-epic.1")

	if len(closeCalls) != 1 {
		t.Fatalf("CloseBead called %d times, want 1: %v", len(closeCalls), closeCalls)
	}
	if closeCalls[0] != "spi-epic.1" {
		t.Errorf("CloseBead called with %q, want %q", closeCalls[0], "spi-epic.1")
	}
}

// TestCloseChildAfterStagingMerge_AlreadyClosed_Skips verifies the
// idempotency guard: a child already in "closed" state does not get a
// redundant CloseBead call (which would re-stamp closed_at and emit a
// duplicate EventClosed since CloseIssue matches by id only — see
// internal/storage/dolt/issues.go).
func TestCloseChildAfterStagingMerge_AlreadyClosed_Skips(t *testing.T) {
	var closeCalls []string
	deps := &Deps{
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "closed"}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			return nil
		},
	}
	e := NewForTest("spi-epic", "wizard-test", nil, deps)

	e.closeChildAfterStagingMerge("spi-epic.1")

	if len(closeCalls) != 0 {
		t.Errorf("CloseBead called %d times on already-closed child, want 0: %v", len(closeCalls), closeCalls)
	}
}

// TestCloseChildAfterStagingMerge_NilDeps_Safe verifies the helper
// tolerates partially-wired deps (no GetBead / no CloseBead) without
// panicking. Production paths always wire both, but defensive nil
// checks make the helper safe to call from anywhere.
func TestCloseChildAfterStagingMerge_NilDeps_Safe(t *testing.T) {
	t.Run("nil CloseBead", func(t *testing.T) {
		deps := &Deps{
			GetBead: func(id string) (Bead, error) {
				return Bead{ID: id, Status: "in_progress"}, nil
			},
		}
		e := NewForTest("spi-epic", "wizard-test", nil, deps)
		// Must not panic.
		e.closeChildAfterStagingMerge("spi-epic.1")
	})

	t.Run("nil GetBead falls through to CloseBead", func(t *testing.T) {
		var closeCalls []string
		deps := &Deps{
			CloseBead: func(id string) error {
				closeCalls = append(closeCalls, id)
				return nil
			},
		}
		e := NewForTest("spi-epic", "wizard-test", nil, deps)
		e.closeChildAfterStagingMerge("spi-epic.1")
		if len(closeCalls) != 1 {
			t.Errorf("CloseBead calls = %d, want 1 when GetBead is nil", len(closeCalls))
		}
	})
}

// TestCloseChildAfterStagingMerge_CloseError_NonFatal verifies that a
// CloseBead error is logged but not propagated. The defensive cascade
// in actionBeadFinish is the safety net for any survivors.
func TestCloseChildAfterStagingMerge_CloseError_NonFatal(t *testing.T) {
	deps := &Deps{
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		CloseBead: func(id string) error {
			return errors.New("transient store error")
		},
	}
	e := NewForTest("spi-epic", "wizard-test", nil, deps)

	// Must not panic; nothing to assert other than no panic / no return.
	e.closeChildAfterStagingMerge("spi-epic.1")
}

// --- Integration tests: dispatch paths trigger eager close --------------

// initEagerCloseTestRepo creates a real git repo with main + a staging
// worktree on stageBranch + N feature branches "feat/<beadID>" each
// carrying a unique commit on top of baseSHA. Returns the staging
// worktree, the list of bead IDs prepared, and a cleanup func.
//
// The legacy fetch+merge path is exercised: FetchBranch from "origin"
// fails (no remote configured), but it's best-effort and the local
// branch already exists, so MergeBranch succeeds via fast-forward.
func initEagerCloseTestRepo(t *testing.T, beadIDs []string) (*spgit.StagingWorktree, string) {
	t.Helper()
	repoDir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "t@t.com")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# init\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "initial")
	runGit("branch", "-M", "main")
	baseSHA := runGit("rev-parse", "HEAD")

	// Create one feature branch per bead, each with its own commit.
	for i, beadID := range beadIDs {
		branch := "feat/" + beadID
		runGit("checkout", "-q", "-b", branch, baseSHA)
		fname := filepath.Join(repoDir, "feat-"+beadID+".txt")
		if err := os.WriteFile(fname, []byte("feat-"+beadID+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", fname, err)
		}
		runGit("add", "-A")
		runGit("commit", "-q", "-m", "feat "+beadID)
		runGit("checkout", "-q", "main")
		_ = i
	}

	// Create a staging branch + a separate worktree checked out at it.
	const stagingBranch = "stage/spi-epic"
	runGit("branch", stagingBranch, baseSHA)

	stagingDir := filepath.Join(t.TempDir(), "staging-wt")
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", stagingDir, stagingBranch).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	// Configure user identity in the staging worktree (for any merge commits).
	for _, args := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "t@t.com"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", stagingDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("staging git %v: %v\n%s", args, err, out)
		}
	}

	stagingWt := &spgit.StagingWorktree{
		WorktreeContext: spgit.WorktreeContext{
			Dir:      stagingDir,
			Branch:   stagingBranch,
			RepoPath: repoDir,
		},
	}
	return stagingWt, baseSHA
}

// TestRunDispatchWave_EagerlyClosesChildOnSuccessfulMerge verifies the
// wave dispatch path: each successful MergeBranch is followed by an
// eager CloseBead on the corresponding child bead.
func TestRunDispatchWave_EagerlyClosesChildOnSuccessfulMerge(t *testing.T) {
	beadIDs := []string{"spi-epic.1", "spi-epic.2"}
	stagingWt, _ := initEagerCloseTestRepo(t, beadIDs)

	backend := &concurrentBackend{sleepPerJob: time.Millisecond}

	beadStatus := map[string]string{
		"spi-epic.1": "in_progress",
		"spi-epic.2": "in_progress",
	}
	var closeCalls []string

	deps := &Deps{
		Spawner:        backend,
		MaxApprentices: 2,
		UpdateBead:     func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch:  func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: beadStatus[id]}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			beadStatus[id] = "closed"
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	results, err := e.dispatchWaveCore([][]string{beadIDs}, stagingWt, "claude-sonnet-4-6", resolver, 2)
	if err != nil {
		t.Fatalf("dispatchWaveCore: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("results count = %d, want 2", len(results))
	}

	if len(closeCalls) != 2 {
		t.Fatalf("CloseBead calls = %d, want 2 (eager close per child): %v", len(closeCalls), closeCalls)
	}
	for _, id := range beadIDs {
		if beadStatus[id] != "closed" {
			t.Errorf("bead %s status = %q, want closed", id, beadStatus[id])
		}
	}
}

// TestDispatchSequentialCore_EagerlyClosesChildOnSuccessfulMerge verifies
// the sequential dispatch path mirrors the wave path's eager-close
// behavior.
func TestDispatchSequentialCore_EagerlyClosesChildOnSuccessfulMerge(t *testing.T) {
	beadIDs := []string{"spi-epic.1", "spi-epic.2"}
	stagingWt, _ := initEagerCloseTestRepo(t, beadIDs)

	backend := &concurrentBackend{sleepPerJob: time.Millisecond}

	beadStatus := map[string]string{
		"spi-epic.1": "in_progress",
		"spi-epic.2": "in_progress",
	}
	var closeCalls []string

	deps := &Deps{
		Spawner:       backend,
		UpdateBead:    func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch: func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: beadStatus[id]}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			beadStatus[id] = "closed"
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	results, err := e.dispatchSequentialCore(beadIDs, stagingWt, "claude-sonnet-4-6", resolver)
	if err != nil {
		t.Fatalf("dispatchSequentialCore: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("results count = %d, want 2", len(results))
	}

	if len(closeCalls) != 2 {
		t.Fatalf("CloseBead calls = %d, want 2 (eager close per child): %v", len(closeCalls), closeCalls)
	}
	// Sequential preserves order: child 1 closes before child 2 starts.
	if closeCalls[0] != "spi-epic.1" || closeCalls[1] != "spi-epic.2" {
		t.Errorf("close order = %v, want [spi-epic.1 spi-epic.2]", closeCalls)
	}
}

// TestRunDispatchWave_MergeFailure_LeavesChildOpen pins the failure-path
// invariant: when MergeBranch returns an error, the child bead must NOT
// be eager-closed — its work hasn't landed and retry/recovery semantics
// require it to remain in_progress.
func TestRunDispatchWave_MergeFailure_LeavesChildOpen(t *testing.T) {
	// Set up a real staging worktree, but DO NOT create the feat branch
	// in git. The legacy fetch+merge path will then fail at MergeBranch
	// (the local branch ref doesn't exist).
	stagingWt, _ := initEagerCloseTestRepo(t, nil)

	backend := &concurrentBackend{sleepPerJob: time.Millisecond}

	var closeCalls []string

	deps := &Deps{
		Spawner:        backend,
		MaxApprentices: 1,
		UpdateBead:     func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch:  func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	_, err := e.dispatchWaveCore([][]string{{"spi-epic.99"}}, stagingWt, "claude-sonnet-4-6", resolver, 1)
	if err == nil {
		t.Fatal("dispatchWaveCore returned nil error, want a merge failure")
	}
	if !strings.Contains(err.Error(), "merge") {
		t.Errorf("err = %q, want it to mention 'merge'", err)
	}

	if len(closeCalls) != 0 {
		t.Errorf("CloseBead called %d times after merge failure, want 0: %v", len(closeCalls), closeCalls)
	}
}

// TestDispatchSequentialCore_MergeFailure_LeavesChildOpen mirrors the
// wave merge-failure invariant for the sequential path: a failed
// MergeBranch must not eagerly close the child.
func TestDispatchSequentialCore_MergeFailure_LeavesChildOpen(t *testing.T) {
	stagingWt, _ := initEagerCloseTestRepo(t, nil)

	backend := &concurrentBackend{sleepPerJob: time.Millisecond}

	var closeCalls []string

	deps := &Deps{
		Spawner:       backend,
		UpdateBead:    func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch: func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	_, err := e.dispatchSequentialCore([]string{"spi-epic.99"}, stagingWt, "claude-sonnet-4-6", resolver)
	if err == nil {
		t.Fatal("dispatchSequentialCore returned nil error, want a merge failure")
	}
	if !strings.Contains(err.Error(), "merge") {
		t.Errorf("err = %q, want it to mention 'merge'", err)
	}

	if len(closeCalls) != 0 {
		t.Errorf("CloseBead called %d times after merge failure, want 0: %v", len(closeCalls), closeCalls)
	}
}

// TestRunDispatchWave_EagerCloseIsIdempotent verifies the
// closeChildAfterStagingMerge guard at the dispatch seam: when a child
// is already closed (e.g. from a prior partial run that crashed after
// CloseBead but before reporting success), the eager close path must
// not re-invoke CloseBead and produce a duplicate EventClosed.
func TestRunDispatchWave_EagerCloseIsIdempotent(t *testing.T) {
	beadIDs := []string{"spi-epic.1"}
	stagingWt, _ := initEagerCloseTestRepo(t, beadIDs)

	backend := &concurrentBackend{sleepPerJob: time.Millisecond}

	var closeCalls int32

	deps := &Deps{
		Spawner:        backend,
		MaxApprentices: 1,
		UpdateBead:     func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch:  func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			// Simulate a child that was already closed before the merge.
			return Bead{ID: id, Status: "closed"}, nil
		},
		CloseBead: func(id string) error {
			atomic.AddInt32(&closeCalls, 1)
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	if _, err := e.dispatchWaveCore([][]string{beadIDs}, stagingWt, "claude-sonnet-4-6", resolver, 1); err != nil {
		t.Fatalf("dispatchWaveCore: %v", err)
	}

	if got := atomic.LoadInt32(&closeCalls); got != 0 {
		t.Errorf("CloseBead calls = %d, want 0 (helper must short-circuit on already-closed child)", got)
	}
}

// TestDispatchDirectCore_DoesNotEagerCloseParent verifies that the
// direct dispatch path (single apprentice working the parent bead's own
// branch) does NOT eager-close. Eager close is a child-task concept;
// closing the parent mid-flight would be wrong because the parent's own
// formula owns its terminal close-step.
func TestDispatchDirectCore_DoesNotEagerCloseParent(t *testing.T) {
	stagingWt, _ := initEagerCloseTestRepo(t, []string{"spi-task"})

	backend := &concurrentBackend{sleepPerJob: time.Millisecond}

	var closeCalls []string

	deps := &Deps{
		Spawner:       backend,
		UpdateBead:    func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch: func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			return nil
		},
	}

	e := NewForTest("spi-task", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	if err := e.dispatchDirectCore(stagingWt, "claude-sonnet-4-6", resolver); err != nil {
		t.Fatalf("dispatchDirectCore: %v", err)
	}

	if len(closeCalls) != 0 {
		t.Errorf("CloseBead called %d times in direct dispatch, want 0 (parent must not be eager-closed): %v", len(closeCalls), closeCalls)
	}
}
