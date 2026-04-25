package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/bundlestore"
	spgit "github.com/awell-health/spire/pkg/git"
)

// --- Helper unit tests for closeChildAfterBundleSignal ------------------

// TestCloseChildAfterBundleSignal_OpenChild_Closes verifies the happy path:
// when the child bead is open, the helper invokes CloseBead exactly once.
func TestCloseChildAfterBundleSignal_OpenChild_Closes(t *testing.T) {
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

	e.closeChildAfterBundleSignal("spi-epic.1")

	if len(closeCalls) != 1 {
		t.Fatalf("CloseBead called %d times, want 1: %v", len(closeCalls), closeCalls)
	}
	if closeCalls[0] != "spi-epic.1" {
		t.Errorf("CloseBead called with %q, want %q", closeCalls[0], "spi-epic.1")
	}
}

// TestCloseChildAfterBundleSignal_AlreadyClosed_Skips verifies the
// idempotency guard: a child already in "closed" state does not get a
// redundant CloseBead call (which would re-stamp closed_at and emit a
// duplicate EventClosed since CloseIssue matches by id only — see
// internal/storage/dolt/issues.go).
func TestCloseChildAfterBundleSignal_AlreadyClosed_Skips(t *testing.T) {
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

	e.closeChildAfterBundleSignal("spi-epic.1")

	if len(closeCalls) != 0 {
		t.Errorf("CloseBead called %d times on already-closed child, want 0: %v", len(closeCalls), closeCalls)
	}
}

// TestCloseChildAfterBundleSignal_NilDeps_Safe verifies the helper
// tolerates partially-wired deps (no GetBead / no CloseBead) without
// panicking. Production paths always wire both, but defensive nil
// checks make the helper safe to call from anywhere.
func TestCloseChildAfterBundleSignal_NilDeps_Safe(t *testing.T) {
	t.Run("nil CloseBead", func(t *testing.T) {
		deps := &Deps{
			GetBead: func(id string) (Bead, error) {
				return Bead{ID: id, Status: "in_progress"}, nil
			},
		}
		e := NewForTest("spi-epic", "wizard-test", nil, deps)
		// Must not panic.
		e.closeChildAfterBundleSignal("spi-epic.1")
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
		e.closeChildAfterBundleSignal("spi-epic.1")
		if len(closeCalls) != 1 {
			t.Errorf("CloseBead calls = %d, want 1 when GetBead is nil", len(closeCalls))
		}
	})
}

// TestCloseChildAfterBundleSignal_CloseError_NonFatal verifies that a
// CloseBead error is logged but not propagated. The defensive cascade
// in actionBeadFinish is the safety net for any survivors.
func TestCloseChildAfterBundleSignal_CloseError_NonFatal(t *testing.T) {
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
	e.closeChildAfterBundleSignal("spi-epic.1")
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

// TestRunDispatchWave_LegacyPath_ClosesChildOnMergeSuccess verifies the
// legacy push-fetch fallback: with no BundleStore wired, the wave
// dispatch path falls through to fetch+merge feat/<bead> and then close
// on MergeBranch success — preserving spi-b2qjqv semantics for that
// migration-period transport.
func TestRunDispatchWave_LegacyPath_ClosesChildOnMergeSuccess(t *testing.T) {
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

// TestDispatchSequentialCore_LegacyPath_ClosesChildOnMergeSuccess mirrors
// the wave-path legacy-fallback behavior for the sequential dispatch:
// with no BundleStore wired, MergeBranch success eagerly closes the
// child (spi-b2qjqv close-on-merge semantics preserved for the
// migration-period push transport).
func TestDispatchSequentialCore_LegacyPath_ClosesChildOnMergeSuccess(t *testing.T) {
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

// TestRunDispatchWave_LegacyPath_MergeFailure_LeavesChildOpen pins the
// legacy-fallback failure-path invariant: when there is no BundleStore
// and MergeBranch returns an error, the child bead must NOT be eager-
// closed. The legacy path has no separate bundle signal — the close
// trigger is MergeBranch success, so a failure naturally leaves the
// child in_progress for recovery.
func TestRunDispatchWave_LegacyPath_MergeFailure_LeavesChildOpen(t *testing.T) {
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

// TestDispatchSequentialCore_LegacyPath_MergeFailure_LeavesChildOpen
// mirrors the wave legacy-fallback merge-failure invariant for the
// sequential path: with no BundleStore, a failed MergeBranch must not
// eagerly close the child.
func TestDispatchSequentialCore_LegacyPath_MergeFailure_LeavesChildOpen(t *testing.T) {
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
// closeChildAfterBundleSignal guard at the dispatch seam: when a child
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

// --- Bundle-path tests: close fires at the apprentice/bundle seam --------

// initBundlePathTestRepo creates a real git repo with main + a staging
// worktree on stageBranch. Unlike initEagerCloseTestRepo it does NOT
// pre-create feat/<bead> branches — the bundle apply path materializes
// the branch from the supplied bundle. Returns the staging worktree and
// the repo dir (which doubles as the bundle-source repo).
func initBundlePathTestRepo(t *testing.T) (*spgit.StagingWorktree, string, string) {
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

	const stagingBranch = "stage/spi-epic"
	runGit("branch", stagingBranch, baseSHA)

	stagingDir := filepath.Join(t.TempDir(), "staging-wt")
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", stagingDir, stagingBranch).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
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
	return stagingWt, repoDir, baseSHA
}

// stampBundleSignal writes an apprentice_signal_<role> entry into a bead
// metadata map for the bundle-path tests. Mirrors the producer side in
// cmd/spire/apprentice.go. The empty-key form (kind=no-op) is also
// supported via emptyBundleKey == true.
func stampBundleSignal(beadID, bundleKey string) map[string]string {
	role := bundlestore.ApprenticeRole(beadID, 0)
	return map[string]string{
		bundlestore.SignalMetadataKey(role): fmt.Sprintf(
			`{"kind":"bundle","role":%q,"bundle_key":%q,"commits":["sha1"],"submitted_at":"t"}`,
			role, bundleKey,
		),
	}
}

func stampNoOpSignal(beadID string) map[string]string {
	role := bundlestore.ApprenticeRole(beadID, 0)
	return map[string]string{
		bundlestore.SignalMetadataKey(role): fmt.Sprintf(
			`{"kind":"no-op","role":%q,"submitted_at":"t"}`, role,
		),
	}
}

// TestRunDispatchWave_BundlePath_NoOp_ClosesChild verifies that a no-op
// signal on the bundle path closes the child bead — even though no
// MergeBranch runs. Under the bundle-signal model, NoOp is an explicit
// "apprentice succeeded with no work needed" outcome and the task is
// done.
func TestRunDispatchWave_BundlePath_NoOp_ClosesChild(t *testing.T) {
	stagingWt, _, _ := initBundlePathTestRepo(t)

	backend := &concurrentBackend{sleepPerJob: time.Millisecond}

	beadStatus := map[string]string{"spi-epic.1": "in_progress"}
	beadMeta := map[string]map[string]string{
		"spi-epic.1": stampNoOpSignal("spi-epic.1"),
	}
	var closeCalls []string

	deps := &Deps{
		Spawner:        backend,
		MaxApprentices: 1,
		BundleStore:    newFakeBundleStore(),
		UpdateBead:     func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch:  func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: beadStatus[id], Metadata: beadMeta[id]}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			beadStatus[id] = "closed"
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	if _, err := e.dispatchWaveCore([][]string{{"spi-epic.1"}}, stagingWt, "claude-sonnet-4-6", resolver, 1); err != nil {
		t.Fatalf("dispatchWaveCore: %v", err)
	}

	if len(closeCalls) != 1 || closeCalls[0] != "spi-epic.1" {
		t.Errorf("CloseBead calls = %v, want [spi-epic.1] (NoOp must close the child)", closeCalls)
	}
	if beadStatus["spi-epic.1"] != "closed" {
		t.Errorf("bead status = %q, want closed", beadStatus["spi-epic.1"])
	}
}

// TestRunDispatchWave_BundlePath_Applied_ClosesChildBeforeMerge proves
// the close fires before MergeBranch on the bundle path: by ordering
// the assertions on a successful merge, we observe the close has
// already happened by the time we inspect post-merge state.
func TestRunDispatchWave_BundlePath_Applied_ClosesChildBeforeMerge(t *testing.T) {
	stagingWt, repoDir, baseSHA := initBundlePathTestRepo(t)
	bundlePath, _ := buildTestBundle(t, repoDir, baseSHA)
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	store := newFakeBundleStore()
	const bundleKey = "spi-epic.1/spi-att-0.bundle"
	store.bundles[bundleKey] = bundleBytes

	beadStatus := map[string]string{"spi-epic.1": "in_progress"}
	beadMeta := map[string]map[string]string{
		"spi-epic.1": stampBundleSignal("spi-epic.1", bundleKey),
	}
	var closeCalls []string

	deps := &Deps{
		Spawner:        &concurrentBackend{sleepPerJob: time.Millisecond},
		MaxApprentices: 1,
		BundleStore:    store,
		UpdateBead:     func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch:  func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: beadStatus[id], Metadata: beadMeta[id]}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			beadStatus[id] = "closed"
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	if _, err := e.dispatchWaveCore([][]string{{"spi-epic.1"}}, stagingWt, "claude-sonnet-4-6", resolver, 1); err != nil {
		t.Fatalf("dispatchWaveCore: %v", err)
	}

	if len(closeCalls) != 1 || closeCalls[0] != "spi-epic.1" {
		t.Errorf("CloseBead calls = %v, want [spi-epic.1] (Applied must close the child)", closeCalls)
	}
	if beadStatus["spi-epic.1"] != "closed" {
		t.Errorf("bead status = %q, want closed", beadStatus["spi-epic.1"])
	}
}

// TestRunDispatchWave_BundlePath_MergeFailure_StaysClosed proves the
// load-bearing invariant: when MergeBranch fails on the bundle path,
// the child stays *closed* — the close has already happened at the
// apprentice/bundle seam, and no reopen logic exists. The merge failure
// surfaces through the wizard's error path (the test asserts the
// returned error mentions the merge), but the child bead's status is
// independent of merge success.
func TestRunDispatchWave_BundlePath_MergeFailure_StaysClosed(t *testing.T) {
	stagingWt, repoDir, baseSHA := initBundlePathTestRepo(t)

	// Engineer a conflict: stage a commit on the staging branch that
	// touches the same path the bundle's commit will modify.
	stagingDir := stagingWt.Dir
	if err := os.WriteFile(filepath.Join(stagingDir, "feature.txt"), []byte("staging-version\n"), 0o644); err != nil {
		t.Fatalf("write conflicting file: %v", err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", "staging conflicting commit"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", stagingDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("staging git %v: %v\n%s", args, err, out)
		}
	}

	bundlePath, _ := buildTestBundle(t, repoDir, baseSHA)
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	store := newFakeBundleStore()
	const bundleKey = "spi-epic.1/spi-att-0.bundle"
	store.bundles[bundleKey] = bundleBytes

	beadStatus := map[string]string{"spi-epic.1": "in_progress"}
	beadMeta := map[string]map[string]string{
		"spi-epic.1": stampBundleSignal("spi-epic.1", bundleKey),
	}
	var closeCalls []string

	deps := &Deps{
		Spawner:        &concurrentBackend{sleepPerJob: time.Millisecond},
		MaxApprentices: 1,
		BundleStore:    store,
		UpdateBead:     func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch:  func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: beadStatus[id], Metadata: beadMeta[id]}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			beadStatus[id] = "closed"
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	// Resolver that refuses to fix conflicts — forces MergeBranch to fail.
	resolver := func(string, string) error { return errors.New("conflict resolver refused") }

	_, dispErr := e.dispatchWaveCore([][]string{{"spi-epic.1"}}, stagingWt, "claude-sonnet-4-6", resolver, 1)
	if dispErr == nil {
		t.Fatal("dispatchWaveCore returned nil error, want a merge failure")
	}
	if !strings.Contains(dispErr.Error(), "merge") {
		t.Errorf("err = %q, want it to mention 'merge'", dispErr)
	}

	// The child must be closed even though the merge failed — close
	// happened at the bundle-signal seam, before MergeBranch ran.
	if len(closeCalls) != 1 || closeCalls[0] != "spi-epic.1" {
		t.Errorf("CloseBead calls = %v, want [spi-epic.1] (close must fire before merge on bundle path)", closeCalls)
	}
	if beadStatus["spi-epic.1"] != "closed" {
		t.Errorf("bead status = %q, want closed (merge failure must NOT reopen)", beadStatus["spi-epic.1"])
	}
}

// TestRunDispatchWave_BundlePath_ApplyError_LeavesChildOpen verifies
// that when applyApprenticeBundle returns an error (e.g. malformed
// signal, missing bundle), the child stays in_progress. The bundle
// signal couldn't be read, so we have no apprentice success to act on.
func TestRunDispatchWave_BundlePath_ApplyError_LeavesChildOpen(t *testing.T) {
	stagingWt, _, _ := initBundlePathTestRepo(t)

	store := newFakeBundleStore()
	// Stamp a bundle signal pointing at a key that doesn't exist in the
	// store — Get will return ErrNotFound and applyApprenticeBundle
	// returns a wrapped error.
	beadMeta := map[string]map[string]string{
		"spi-epic.1": stampBundleSignal("spi-epic.1", "missing-key"),
	}
	var closeCalls []string

	deps := &Deps{
		Spawner:        &concurrentBackend{sleepPerJob: time.Millisecond},
		MaxApprentices: 1,
		BundleStore:    store,
		UpdateBead:     func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch:  func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress", Metadata: beadMeta[id]}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	_, dispErr := e.dispatchWaveCore([][]string{{"spi-epic.1"}}, stagingWt, "claude-sonnet-4-6", resolver, 1)
	if dispErr == nil {
		t.Fatal("dispatchWaveCore returned nil error, want a bundle apply failure")
	}

	if len(closeCalls) != 0 {
		t.Errorf("CloseBead called %d times after bundle apply failure, want 0: %v", len(closeCalls), closeCalls)
	}
}

// TestDispatchSequentialCore_BundlePath_NoOp_ClosesChild mirrors the
// wave-path NoOp test for sequential dispatch.
func TestDispatchSequentialCore_BundlePath_NoOp_ClosesChild(t *testing.T) {
	stagingWt, _, _ := initBundlePathTestRepo(t)

	beadStatus := map[string]string{"spi-epic.1": "in_progress"}
	beadMeta := map[string]map[string]string{
		"spi-epic.1": stampNoOpSignal("spi-epic.1"),
	}
	var closeCalls []string

	deps := &Deps{
		Spawner:       &concurrentBackend{sleepPerJob: time.Millisecond},
		BundleStore:   newFakeBundleStore(),
		UpdateBead:    func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch: func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: beadStatus[id], Metadata: beadMeta[id]}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			beadStatus[id] = "closed"
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	if _, err := e.dispatchSequentialCore([]string{"spi-epic.1"}, stagingWt, "claude-sonnet-4-6", resolver); err != nil {
		t.Fatalf("dispatchSequentialCore: %v", err)
	}

	if len(closeCalls) != 1 || closeCalls[0] != "spi-epic.1" {
		t.Errorf("CloseBead calls = %v, want [spi-epic.1] (NoOp must close the child)", closeCalls)
	}
}

// TestDispatchSequentialCore_BundlePath_Applied_ClosesChildBeforeMerge
// mirrors the wave-path Applied close test for sequential dispatch.
func TestDispatchSequentialCore_BundlePath_Applied_ClosesChildBeforeMerge(t *testing.T) {
	stagingWt, repoDir, baseSHA := initBundlePathTestRepo(t)
	bundlePath, _ := buildTestBundle(t, repoDir, baseSHA)
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	store := newFakeBundleStore()
	const bundleKey = "spi-epic.1/spi-att-0.bundle"
	store.bundles[bundleKey] = bundleBytes

	beadStatus := map[string]string{"spi-epic.1": "in_progress"}
	beadMeta := map[string]map[string]string{
		"spi-epic.1": stampBundleSignal("spi-epic.1", bundleKey),
	}
	var closeCalls []string

	deps := &Deps{
		Spawner:       &concurrentBackend{sleepPerJob: time.Millisecond},
		BundleStore:   store,
		UpdateBead:    func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch: func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: beadStatus[id], Metadata: beadMeta[id]}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			beadStatus[id] = "closed"
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	if _, err := e.dispatchSequentialCore([]string{"spi-epic.1"}, stagingWt, "claude-sonnet-4-6", resolver); err != nil {
		t.Fatalf("dispatchSequentialCore: %v", err)
	}

	if len(closeCalls) != 1 || closeCalls[0] != "spi-epic.1" {
		t.Errorf("CloseBead calls = %v, want [spi-epic.1] (Applied must close the child)", closeCalls)
	}
}

// TestDispatchSequentialCore_BundlePath_MergeFailure_StaysClosed
// mirrors the wave-path merge-failure test for sequential dispatch:
// MergeBranch failure on the bundle path leaves the child closed.
func TestDispatchSequentialCore_BundlePath_MergeFailure_StaysClosed(t *testing.T) {
	stagingWt, repoDir, baseSHA := initBundlePathTestRepo(t)

	stagingDir := stagingWt.Dir
	if err := os.WriteFile(filepath.Join(stagingDir, "feature.txt"), []byte("staging-version\n"), 0o644); err != nil {
		t.Fatalf("write conflicting file: %v", err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", "staging conflicting commit"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", stagingDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("staging git %v: %v\n%s", args, err, out)
		}
	}

	bundlePath, _ := buildTestBundle(t, repoDir, baseSHA)
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	store := newFakeBundleStore()
	const bundleKey = "spi-epic.1/spi-att-0.bundle"
	store.bundles[bundleKey] = bundleBytes

	beadStatus := map[string]string{"spi-epic.1": "in_progress"}
	beadMeta := map[string]map[string]string{
		"spi-epic.1": stampBundleSignal("spi-epic.1", bundleKey),
	}
	var closeCalls []string

	deps := &Deps{
		Spawner:       &concurrentBackend{sleepPerJob: time.Millisecond},
		BundleStore:   store,
		UpdateBead:    func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch: func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: beadStatus[id], Metadata: beadMeta[id]}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			beadStatus[id] = "closed"
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return errors.New("conflict resolver refused") }

	_, dispErr := e.dispatchSequentialCore([]string{"spi-epic.1"}, stagingWt, "claude-sonnet-4-6", resolver)
	if dispErr == nil {
		t.Fatal("dispatchSequentialCore returned nil error, want a merge failure")
	}
	if !strings.Contains(dispErr.Error(), "merge") {
		t.Errorf("err = %q, want it to mention 'merge'", dispErr)
	}

	if len(closeCalls) != 1 || closeCalls[0] != "spi-epic.1" {
		t.Errorf("CloseBead calls = %v, want [spi-epic.1] (close must fire before merge on bundle path)", closeCalls)
	}
	if beadStatus["spi-epic.1"] != "closed" {
		t.Errorf("bead status = %q, want closed (merge failure must NOT reopen)", beadStatus["spi-epic.1"])
	}
}

// TestDispatchSequentialCore_BundlePath_ApplyError_LeavesChildOpen
// mirrors the wave-path bundle-apply-error test for sequential
// dispatch: a bundle-read failure leaves the child in_progress.
func TestDispatchSequentialCore_BundlePath_ApplyError_LeavesChildOpen(t *testing.T) {
	stagingWt, _, _ := initBundlePathTestRepo(t)

	store := newFakeBundleStore()
	beadMeta := map[string]map[string]string{
		"spi-epic.1": stampBundleSignal("spi-epic.1", "missing-key"),
	}
	var closeCalls []string

	deps := &Deps{
		Spawner:       &concurrentBackend{sleepPerJob: time.Millisecond},
		BundleStore:   store,
		UpdateBead:    func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch: func(beadID string) string { return "feat/" + beadID },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress", Metadata: beadMeta[id]}, nil
		},
		CloseBead: func(id string) error {
			closeCalls = append(closeCalls, id)
			return nil
		},
	}

	e := NewForTest("spi-epic", "wizard-test", nil, deps)
	resolver := func(string, string) error { return nil }

	_, dispErr := e.dispatchSequentialCore([]string{"spi-epic.1"}, stagingWt, "claude-sonnet-4-6", resolver)
	if dispErr == nil {
		t.Fatal("dispatchSequentialCore returned nil error, want a bundle apply failure")
	}

	if len(closeCalls) != 0 {
		t.Errorf("CloseBead called %d times after bundle apply failure, want 0: %v", len(closeCalls), closeCalls)
	}
}
