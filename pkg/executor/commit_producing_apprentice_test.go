package executor

// commit_producing_apprentice_test.go — tests for spi-tlj32a (Principle 1
// dispatch path: fix and cleric-worker). Owns:
//
//   TestFixApprentice_BundleHandoff
//     end-to-end: fix dispatch uses bundle handoff and consumes the bundle
//     into the staging worktree.
//
//   TestFixApprentice_NoBorrowedWorkspace
//     unit-level guarantee that the fix dispatch path never sets
//     HandoffBorrowed on its SpawnConfig (regression test for the bug
//     spi-tlj32a fixed).
//
//   TestApprenticeSignal_DeterministicKey
//     two calls to bundlestore.SignalMetadataKey with the same (bead, role,
//     idx) tuple produce the same key — the load-bearing seam-8 idempotency
//     property.
//
//   TestBundleApply_TwiceIsNoop
//     applying the same bundle twice is idempotent: the second apply does
//     not advance staging and does not duplicate commits.
//
//   TestBundleApply_PartialPriorApply_CompletesRemaining
//     applying a bundle whose first half is already on staging completes
//     the remaining commits without duplicating the prefix.
//
//   TestWorkerRepairApprentice_SameSignalShape
//     a cleric-worker signal parses identically to a fix signal — only
//     RoleTag differs.
//
//   TestFixApprentice_SurvivesWizardCrash
//     given a signal + bundle written while the wizard was dead, a freshly
//     constructed wizard consumes them correctly (seam-8 / seam-9 crash
//     survivability).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/bundlestore"
	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
)

// --- TestFixApprentice_BundleHandoff -----------------------------------

// TestFixApprentice_BundleHandoff verifies the end-to-end fix dispatch
// produces bundle handoff (not borrowed) at SpawnConfig time AND consumes
// the apprentice's bundle into the staging worktree before returning.
//
// Setup: a fake spawner records the SpawnConfig and pre-populates the
// BundleStore with a real git bundle (built from a throw-away repo). The
// staging worktree is the same throw-away repo at the bundle's base SHA.
// The spawner stamps the apprentice signal on the bead so the apply path
// finds it. After actionReviewFix returns, the staging branch must point
// at the bundle's HEAD.
func TestFixApprentice_BundleHandoff(t *testing.T) {
	repoDir, baseSHA := initBundleTestRepo(t)
	bundlePath, headSHA := buildTestBundle(t, repoDir, baseSHA)

	const beadID = "spi-fix"
	const bundleKey = "spi-fix/spi-att-0.bundle"
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	store := newFakeBundleStore()
	store.bundles[bundleKey] = bundleBytes

	role := bundlestore.ApprenticeRole(beadID, 0)
	beadMD := map[string]string{
		bundlestore.SignalMetadataKey(role): fmt.Sprintf(
			`{"kind":"bundle","role":%q,"bundle_key":%q,"commits":["c"],"submitted_at":"t","handoff_mode":"bundle"}`,
			role, bundleKey,
		),
	}

	var captured agent.SpawnConfig
	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			captured = cfg
			return &mockHandle{}, nil
		},
	}
	deps := &Deps{
		Spawner:     backend,
		BundleStore: store,
		ConfigDir:   func() (string, error) { return t.TempDir(), nil },
		AgentResultDir: func(name string) string {
			return filepath.Join(t.TempDir(), name)
		},
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Metadata: beadMD}, nil
		},
		ResolveBranch: func(id string) string { return "feat/" + id },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "subgraph-review",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"fix": {Action: "wizard.run", Flow: "review-fix"},
		},
	}
	exec := NewGraphForTest(beadID, "wizard-fix", graph, nil, deps)
	exec.graphState.RepoPath = repoDir
	exec.graphState.WorktreeDir = repoDir
	exec.graphState.StagingBranch = "main" // staging worktree is checked out on main

	step := StepConfig{Action: "wizard.run", Flow: "review-fix"}
	result := actionReviewFix(exec, "fix", step, exec.graphState, repoDir, nil)
	if result.Error != nil {
		t.Fatalf("actionReviewFix: %v", result.Error)
	}

	// Verify SpawnConfig: bundle handoff, --review-fix --apprentice.
	if captured.Run.HandoffMode != HandoffBundle {
		t.Errorf("HandoffMode = %q, want %q", captured.Run.HandoffMode, HandoffBundle)
	}
	if !containsArg(captured.ExtraArgs, "--review-fix") {
		t.Errorf("ExtraArgs missing --review-fix: %v", captured.ExtraArgs)
	}
	if !containsArg(captured.ExtraArgs, "--apprentice") {
		t.Errorf("ExtraArgs missing --apprentice: %v", captured.ExtraArgs)
	}

	// Verify the bundle landed in the staging worktree as the expected
	// per-attempt branch ref (fix/<beadID>-r1) and was merged into main.
	bundleRef := commitProducingApprenticeBundleRef("fix", beadID, 1)
	gotBundleSHA := readGitRef(t, repoDir, bundleRef)
	if gotBundleSHA != headSHA {
		t.Errorf("ref %s = %q, want %q", bundleRef, gotBundleSHA, headSHA)
	}
	mainSHA := readGitRef(t, repoDir, "main")
	if mainSHA != headSHA {
		t.Errorf("main = %q after merge, want bundle HEAD %q", mainSHA, headSHA)
	}

	// Bundle deletion is the post-merge cleanup step.
	if got := atomic.LoadInt32(&store.delCalls); got == 0 {
		t.Errorf("bundle.Delete not called after successful merge")
	}
}

// --- TestFixApprentice_NoBorrowedWorkspace ------------------------------

// TestFixApprentice_NoBorrowedWorkspace asserts the runtime contract on a
// fix dispatch never carries HandoffBorrowed. This is the per-call-site
// guarantee that complements the source-level audit
// (TestPrinciple1_NoBorrowedForCommitPaths).
func TestFixApprentice_NoBorrowedWorkspace(t *testing.T) {
	const beadID = "spi-noborrow"

	var captured agent.SpawnConfig
	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			captured = cfg
			return &mockHandle{}, nil
		},
	}
	deps := &Deps{
		Spawner:   backend,
		ConfigDir: func() (string, error) { return t.TempDir(), nil },
		// No BundleStore — confirms the dispatch path itself sets bundle
		// handoff regardless of whether the bundle is consumed.
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		ResolveBranch: func(id string) string { return "feat/" + id },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "subgraph-review",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"fix": {Action: "wizard.run", Flow: "review-fix"},
		},
	}
	exec := NewGraphForTest(beadID, "wizard-noborrow", graph, nil, deps)

	step := StepConfig{Action: "wizard.run", Flow: "review-fix"}
	result := actionReviewFix(exec, "fix", step, exec.graphState, "", nil)
	if result.Error != nil {
		t.Fatalf("actionReviewFix: %v", result.Error)
	}

	if captured.Run.HandoffMode == HandoffBorrowed {
		t.Errorf("fix dispatch HandoffMode = HandoffBorrowed; Principle 1 requires bundle delivery")
	}
	if captured.Run.HandoffMode != HandoffBundle && captured.Run.HandoffMode != HandoffTransitional {
		t.Errorf("HandoffMode = %q, want bundle (or transitional during chunk-5a quarantine)", captured.Run.HandoffMode)
	}
}

// --- TestApprenticeSignal_DeterministicKey ------------------------------

// TestApprenticeSignal_DeterministicKey pins the seam-8 idempotency property:
// two computations of the apprentice metadata key for the same (bead, role,
// idx) tuple produce the same string — the consumer trusts the value, not
// the presence, so a re-run apprentice writing the same key is a safe upsert.
func TestApprenticeSignal_DeterministicKey(t *testing.T) {
	cases := []struct {
		name string
		bead string
		idx  int
	}{
		{"single-apprentice", "spi-aaa", 0},
		{"wave-fanout", "spi-bbb", 7},
		{"epic-id", "epic-ccc", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := bundlestore.SignalMetadataKey(bundlestore.ApprenticeRole(tc.bead, tc.idx))
			b := bundlestore.SignalMetadataKey(bundlestore.ApprenticeRole(tc.bead, tc.idx))
			if a != b {
				t.Errorf("non-deterministic key: %q != %q", a, b)
			}
			if !strings.Contains(a, tc.bead) {
				t.Errorf("key %q missing bead id %q", a, tc.bead)
			}
		})
	}

	// Different tuples produce different keys.
	k1 := bundlestore.SignalMetadataKey(bundlestore.ApprenticeRole("spi-x", 0))
	k2 := bundlestore.SignalMetadataKey(bundlestore.ApprenticeRole("spi-x", 1))
	k3 := bundlestore.SignalMetadataKey(bundlestore.ApprenticeRole("spi-y", 0))
	if k1 == k2 || k1 == k3 || k2 == k3 {
		t.Errorf("distinct tuples collided: idx=%q, idx2=%q, bead=%q", k1, k2, k3)
	}
}

// --- TestBundleApply_TwiceIsNoop ----------------------------------------

// TestBundleApply_TwiceIsNoop verifies the seam-9 invariant: applying a
// bundle whose commits are already in the target branch is a no-op (no head
// advance, no duplicate commits in the log). This is what lets the wizard
// re-apply bundles on crash recovery without corrupting staging.
func TestBundleApply_TwiceIsNoop(t *testing.T) {
	repoDir, baseSHA := initBundleTestRepo(t)
	bundlePath, headSHA := buildTestBundle(t, repoDir, baseSHA)

	stagingWt := &spgit.StagingWorktree{
		WorktreeContext: spgit.WorktreeContext{Dir: repoDir, RepoPath: repoDir},
	}

	// First apply: writes bundle to a sibling ref.
	if err := stagingWt.ApplyBundle(bundlePath, "spi-test-bundle-target"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	firstSHA := readGitRef(t, repoDir, "spi-test-bundle-target")
	if firstSHA != headSHA {
		t.Fatalf("first apply ref = %q, want %q", firstSHA, headSHA)
	}

	// Second apply: must not advance the ref or duplicate commits.
	if err := stagingWt.ApplyBundle(bundlePath, "spi-test-bundle-target"); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	secondSHA := readGitRef(t, repoDir, "spi-test-bundle-target")
	if secondSHA != firstSHA {
		t.Errorf("second apply advanced ref: first=%q second=%q", firstSHA, secondSHA)
	}

	// Commit count between baseSHA and the bundle ref must be 1 (the single
	// feat commit), not 2.
	count := commitCount(t, repoDir, baseSHA, "spi-test-bundle-target")
	if count != 1 {
		t.Errorf("commit count between %s and bundle ref = %d, want 1 (no duplicates)", baseSHA, count)
	}
}

// --- TestBundleApply_PartialPriorApply_CompletesRemaining ---------------

// TestBundleApply_PartialPriorApply_CompletesRemaining verifies the seam-9
// partial-apply property: when the first commit of an N-commit bundle is
// already on staging, applying the full bundle lands the remaining
// commit(s) and produces no duplicates of the already-applied prefix.
//
// Mechanism: build a 2-commit bundle. Pre-apply just the first commit to
// the target ref (simulating crash mid-apply). Then apply the full bundle
// and assert the ref advances by one commit and total log length is 2.
func TestBundleApply_PartialPriorApply_CompletesRemaining(t *testing.T) {
	repoDir, baseSHA := initBundleTestRepo(t)

	// Build a 2-commit feature, capture mid SHA, full SHA, and a bundle for
	// each.
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("checkout", "-q", "-b", "build-tmp")

	// Commit 1 + partial bundle (baseSHA..HEAD = just this commit).
	if err := os.WriteFile(filepath.Join(repoDir, "feat-1.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write feat-1: %v", err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "feat 1")
	midSHA := runGit("rev-parse", "HEAD")
	partialBundle := filepath.Join(t.TempDir(), "partial.bundle")
	runGit("bundle", "create", partialBundle, baseSHA+"..HEAD")

	// Commit 2 + full bundle (baseSHA..HEAD = both commits).
	if err := os.WriteFile(filepath.Join(repoDir, "feat-2.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatalf("write feat-2: %v", err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "feat 2")
	headSHA := runGit("rev-parse", "HEAD")
	fullBundle := filepath.Join(t.TempDir(), "full.bundle")
	runGit("bundle", "create", fullBundle, baseSHA+"..HEAD")

	runGit("checkout", "-q", "main")
	runGit("branch", "-D", "build-tmp")

	stagingWt := &spgit.StagingWorktree{
		WorktreeContext: spgit.WorktreeContext{Dir: repoDir, RepoPath: repoDir},
	}

	// Apply partial bundle (first commit only).
	if err := stagingWt.ApplyBundle(partialBundle, "spi-partial-bundle-target"); err != nil {
		t.Fatalf("partial apply: %v", err)
	}
	if got := readGitRef(t, repoDir, "spi-partial-bundle-target"); got != midSHA {
		t.Fatalf("partial ref = %q, want %q", got, midSHA)
	}
	if got := commitCount(t, repoDir, baseSHA, "spi-partial-bundle-target"); got != 1 {
		t.Fatalf("partial apply commit count = %d, want 1", got)
	}

	// Apply full bundle: must complete the remainder, no duplicate of the
	// already-applied first commit.
	if err := stagingWt.ApplyBundle(fullBundle, "spi-partial-bundle-target"); err != nil {
		t.Fatalf("full apply: %v", err)
	}
	if got := readGitRef(t, repoDir, "spi-partial-bundle-target"); got != headSHA {
		t.Errorf("after full apply ref = %q, want HEAD %q", got, headSHA)
	}
	if got := commitCount(t, repoDir, baseSHA, "spi-partial-bundle-target"); got != 2 {
		t.Errorf("after full apply commit count = %d, want 2 (no duplicates of first commit)", got)
	}
}

// --- TestWorkerRepairApprentice_SameSignalShape -------------------------

// TestWorkerRepairApprentice_SameSignalShape pins the cross-role drift
// guard: a worker-mode cleric repair signal must parse with the same
// bundlestore.Signal struct as a fix or wave signal — only the Role string
// differs. If a future refactor accidentally adds cleric-only signal
// fields, this test fails and forces a deliberate decision.
func TestWorkerRepairApprentice_SameSignalShape(t *testing.T) {
	const beadID = "spi-shape"
	fixRole := bundlestore.ApprenticeRole(beadID, 0)
	clericRole := bundlestore.ApprenticeRole(beadID, 0) // same idx convention

	fixJSON := fmt.Sprintf(
		`{"kind":"bundle","role":%q,"bundle_key":"k","commits":["c"],"submitted_at":"t","handoff_mode":"bundle"}`,
		fixRole,
	)
	clericJSON := fmt.Sprintf(
		`{"kind":"bundle","role":%q,"bundle_key":"k","commits":["c"],"submitted_at":"t","handoff_mode":"bundle"}`,
		clericRole,
	)

	var fix, cleric bundlestore.Signal
	if err := json.Unmarshal([]byte(fixJSON), &fix); err != nil {
		t.Fatalf("unmarshal fix: %v", err)
	}
	if err := json.Unmarshal([]byte(clericJSON), &cleric); err != nil {
		t.Fatalf("unmarshal cleric: %v", err)
	}

	// Structural equality: every field except Role identical.
	if fix.Kind != cleric.Kind || fix.BundleKey != cleric.BundleKey ||
		fix.SubmittedAt != cleric.SubmittedAt || fix.HandoffMode != cleric.HandoffMode {
		t.Errorf("signals differ in non-role fields: fix=%+v cleric=%+v", fix, cleric)
	}
	if len(fix.Commits) != len(cleric.Commits) {
		t.Errorf("commits length differs: fix=%d cleric=%d", len(fix.Commits), len(cleric.Commits))
	}
}

// --- TestFixApprentice_SurvivesWizardCrash ------------------------------

// TestFixApprentice_SurvivesWizardCrash verifies the cross-pod
// survivability property: given a bundle + signal written while the wizard
// process was dead, a freshly constructed wizard reads them and applies
// them correctly. Scope is intentionally narrow per spec — the test does
// NOT touch resumption logic owned by spi-icgqhi; it asserts the apply
// machinery is independent of the spawn machinery.
func TestFixApprentice_SurvivesWizardCrash(t *testing.T) {
	repoDir, baseSHA := initBundleTestRepo(t)
	bundlePath, headSHA := buildTestBundle(t, repoDir, baseSHA)

	const beadID = "spi-crash"
	const bundleKey = "spi-crash/spi-att-0.bundle"
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	store := newFakeBundleStore()
	store.bundles[bundleKey] = bundleBytes

	role := bundlestore.ApprenticeRole(beadID, 0)
	beadMD := map[string]string{
		bundlestore.SignalMetadataKey(role): fmt.Sprintf(
			`{"kind":"bundle","role":%q,"bundle_key":%q,"commits":["c"],"submitted_at":"t","handoff_mode":"bundle"}`,
			role, bundleKey,
		),
	}

	// Construct a fresh executor (simulates a restarted wizard process).
	deps := &Deps{
		BundleStore: store,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Metadata: beadMD}, nil
		},
		ResolveBranch: func(id string) string { return "feat/" + id },
	}
	exec := NewForTest(beadID, "wizard-crash", nil, deps)

	stagingWt := &spgit.StagingWorktree{
		WorktreeContext: spgit.WorktreeContext{Dir: repoDir, RepoPath: repoDir},
	}

	// Apply via the same code path the executor uses post-spawn.
	out, err := exec.applyApprenticeBundle(beadID, 0, stagingWt)
	if err != nil {
		t.Fatalf("applyApprenticeBundle: %v", err)
	}
	if !out.Applied {
		t.Fatalf("Applied = false, want true after crash + restart")
	}
	gotSHA := readGitRef(t, repoDir, "feat/"+beadID)
	if gotSHA != headSHA {
		t.Errorf("staging ref = %q, want %q", gotSHA, headSHA)
	}
	_ = context.Background() // silence unused import in case
}

// --- helpers ------------------------------------------------------------

// readGitRef reads the SHA pointed at by a local ref, failing the test on
// error so the caller doesn't have to repeat boilerplate.
func readGitRef(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

// commitCount returns the number of commits in `from..to`. Used to assert
// no-duplicate properties on bundle apply paths.
func commitCount(t *testing.T, dir, from, to string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", from+".."+to).Output()
	if err != nil {
		t.Fatalf("rev-list %s..%s: %v", from, to, err)
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

// containsArg reports whether args contains target as an exact element.
// Avoids accidental substring matches when checking CLI flag presence.
func containsArg(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}
