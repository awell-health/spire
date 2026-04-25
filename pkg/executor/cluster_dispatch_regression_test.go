// Cluster-native dispatch regression scaffolding for pkg/executor.
//
// The convergence epic spi-5bzu9r enforces "no direct Spawner.Spawn in
// cluster-native dispatch paths" through a layered defense:
//
//  1. The static AST invariant in cluster_dispatch_invariant_test.go
//     pins the boundary today by file-path allowlist. New Spawn call
//     sites in unauthorized files fail the test. This is the primary
//     enforcement mechanism while the executor's child-dispatch sites
//     are migrating onto the operator seam under spi-5bzu9r.2.
//
//  2. Steward-side runtime regression coverage in
//     pkg/steward/cluster_dispatch_regression_test.go and
//     pkg/steward/cluster_dispatch_phase_emit_test.go pins the
//     dispatchPhase seam: cluster-native emits intent, never calls
//     Spawn.
//
//  3. This file holds the scaffolding the future executor-side
//     runtime regression tests will compose against — a failing
//     spawner that fails the test if Spawn is called, and a fake
//     intent sink that captures emitted intents for assertion.
//     Cluster-native runtime regression tests at the executor's
//     child-dispatch entry points are added in the same PR as
//     spi-5bzu9r.2, where the seam first exists. Adding them before
//     the seam exists would either repeat the local-native spawn
//     assertion or fabricate behaviour against code that does not yet
//     branch on deployment mode.

package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/steward/intent"
)

// failingSpawnBackend is the cluster-native regression sentinel: any
// Spawn call increments the counter AND fails the test. Cluster-native
// dispatch regression tests wire this into executor / wizard Deps to
// assert that the dispatch path emits intent rather than calling
// Spawn.
//
// Renamed from the spec's "failingSpawner" to avoid colliding with
// the existing test type fakeSpawner in
// recovery_actions_agentic_test.go (different intent: that fake is a
// non-nil guard, this one fails the test if used).
type failingSpawnBackend struct {
	t      *testing.T
	mu     sync.Mutex
	calls  int
	reason string
}

func newFailingSpawnBackend(t *testing.T, reason string) *failingSpawnBackend {
	t.Helper()
	return &failingSpawnBackend{t: t, reason: reason}
}

func (f *failingSpawnBackend) Spawn(_ agent.SpawnConfig) (agent.Handle, error) {
	f.mu.Lock()
	f.calls++
	calls := f.calls
	f.mu.Unlock()
	if f.t != nil {
		f.t.Errorf("Spawn called in cluster-native dispatch path (call #%d): %s", calls, f.reason)
	}
	return nil, errors.New("Spawn forbidden in cluster-native dispatch path")
}
func (f *failingSpawnBackend) List() ([]agent.Info, error)         { return nil, nil }
func (f *failingSpawnBackend) Logs(string) (io.ReadCloser, error)  { return nil, os.ErrNotExist }
func (f *failingSpawnBackend) Kill(string) error                   { return nil }

// fakeChildIntentSink captures emitted intents for assertion in
// cluster-native regression tests. Renamed from the spec's
// "fakeIntentSink" so the test type is unambiguously the sink for
// child intents rather than the broader IntentPublisher abstraction.
//
// Today the canonical child-intent contract type is
// intent.WorkloadIntent (carrying the (role, phase, runtime) triple
// in FormulaPhase + RepoIdentity + HandoffMode + the SpawnRole
// implicitly via the intent's downstream consumer). When pkg/executor
// gains its own ChildIntent type under spi-5bzu9r.1 / .2 the
// underlying field type swaps, but the surface (Calls slice + Publish
// method) stays the same so existing test bodies survive.
type fakeChildIntentSink struct {
	mu    sync.Mutex
	Calls []intent.WorkloadIntent
}

// Publish satisfies intent.IntentPublisher and records the intent.
// Returns nil so test bodies can assert exclusively on the captured
// shape rather than threading errors.
func (s *fakeChildIntentSink) Publish(_ context.Context, i intent.WorkloadIntent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, i)
	return nil
}

// TestFailingSpawnBackend_FailsOnSpawn verifies that the cluster-
// native regression sentinel actually fails when Spawn is called.
// Without this sanity-check, a regression test that miswires the
// sentinel could silently pass even when Spawn was invoked.
func TestFailingSpawnBackend_FailsOnSpawn(t *testing.T) {
	// Use an inner *testing.T that we don't pass our own t into, so
	// we can capture the failure signal without failing this test.
	inner := &testing.T{}
	b := newFailingSpawnBackend(inner, "sanity-check")
	_, err := b.Spawn(agent.SpawnConfig{Name: "x", BeadID: "spi-x"})
	if err == nil {
		t.Errorf("Spawn returned nil error; sentinel must error so callers see the violation")
	}
	if !inner.Failed() {
		t.Errorf("Spawn did not mark the inner *testing.T as failed; sentinel is silent")
	}
	if b.calls != 1 {
		t.Errorf("calls = %d, want 1", b.calls)
	}
}

// TestFakeChildIntentSink_CapturesPublish verifies that the fake
// child-intent sink is a working IntentPublisher and captures
// emitted intents in order.
func TestFakeChildIntentSink_CapturesPublish(t *testing.T) {
	sink := &fakeChildIntentSink{}
	want := []intent.WorkloadIntent{
		{TaskID: "spi-1", DispatchSeq: 1, FormulaPhase: intent.PhaseImplement},
		{TaskID: "spi-1", DispatchSeq: 2, FormulaPhase: intent.PhaseFix},
		{TaskID: "spi-1", DispatchSeq: 3, FormulaPhase: intent.PhaseReview},
	}
	for _, w := range want {
		if err := sink.Publish(context.Background(), w); err != nil {
			t.Fatalf("Publish(%+v): %v", w, err)
		}
	}
	if len(sink.Calls) != len(want) {
		t.Fatalf("Calls = %d, want %d", len(sink.Calls), len(want))
	}
	for i, got := range sink.Calls {
		if got != want[i] {
			t.Errorf("Calls[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}

// TestSupportedChildPhasesClassifyCorrectly pins the role/phase
// classification documented in
// docs/VISION-CLUSTER.md → "The role / phase / runtime contract":
// implement and fix are step-level (apprentice pod); review and
// arbiter are review-level (sage pod); the bead-type / wizard
// strings are bead-level (wizard pod). Cleric dispatch reuses the
// bead-level slot via beadDispatchPhase in pkg/steward, so the
// "recovery" string is intentionally NOT a registered bead-level
// phase — that boundary is what stops the operator from materializing
// a pod for an unsupported triple.
func TestSupportedChildPhasesClassifyCorrectly(t *testing.T) {
	beadLevel := []string{intent.PhaseWizard, "task", "bug", "epic", "feature", "chore"}
	for _, p := range beadLevel {
		if !intent.IsBeadLevelPhase(p) {
			t.Errorf("IsBeadLevelPhase(%q) = false, want true", p)
		}
	}

	stepLevel := []string{intent.PhaseImplement, intent.PhaseFix}
	for _, p := range stepLevel {
		if !intent.IsStepLevelPhase(p) {
			t.Errorf("IsStepLevelPhase(%q) = false, want true", p)
		}
	}

	reviewLevel := []string{intent.PhaseReview, intent.PhaseArbiter}
	for _, p := range reviewLevel {
		if !intent.IsReviewLevelPhase(p) {
			t.Errorf("IsReviewLevelPhase(%q) = false, want true", p)
		}
	}

	// "recovery" is NOT a bead-level phase — cleric dispatch must
	// emit a bead-level value (the recovery bead's type, with
	// PhaseWizard fallback). See pkg/steward beadDispatchPhase and
	// the cleric dispatch routing section in pkg/steward/README.md.
	if intent.IsBeadLevelPhase("recovery") {
		t.Errorf(`IsBeadLevelPhase("recovery") = true; cleric dispatch would misroute through the operator`)
	}
	if intent.IsStepLevelPhase("recovery") {
		t.Errorf(`IsStepLevelPhase("recovery") = true; cleric is not a step-level workload`)
	}
	if intent.IsReviewLevelPhase("recovery") {
		t.Errorf(`IsReviewLevelPhase("recovery") = true; cleric is not a review-level workload`)
	}
}

// TestExecutorChildDispatch_EmitsIntent_TODO is the placeholder for
// the executor-side cluster-native runtime regression test landing
// in spi-5bzu9r.2. The test is intentionally skipped today — the
// executor's child-dispatch entry points (graph_actions wizard.run,
// action_dispatch wave/sequential/direct, recovery_phase) do not
// yet branch on deployment mode, so wiring failingSpawnBackend +
// fakeChildIntentSink and asserting "intent emitted, no spawn" would
// fail in a way that has nothing to do with a regression in the
// boundary.
//
// When spi-5bzu9r.2 lands, this skip is removed and the test body
// drives each entry point through a cluster-native config. The
// captured intents are asserted to carry the supported (role, phase,
// runtime) triples enumerated in
// docs/VISION-CLUSTER.md → "The role / phase / runtime contract".
func TestExecutorChildDispatch_EmitsIntent_TODO(t *testing.T) {
	t.Skip("pending spi-5bzu9r.2 — executor child-dispatch sites have not yet been migrated onto the operator intent seam; until then, the AST invariant in cluster_dispatch_invariant_test.go is the load-bearing enforcement for this boundary in pkg/executor")
}

// TestWizardReviewFix_EmitsIntent_TODO mirrors the executor TODO for
// pkg/wizard's review-fix re-entry. Migration also lands under
// spi-5bzu9r.2.
func TestWizardReviewFix_EmitsIntent_TODO(t *testing.T) {
	t.Skip("pending spi-5bzu9r.2 — wizard review-fix re-entry has not yet been migrated onto the operator intent seam; until then, the AST invariant in cluster_dispatch_invariant_test.go is the load-bearing enforcement for this boundary in pkg/wizard")
}
