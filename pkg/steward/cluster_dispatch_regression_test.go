// Cluster-native dispatch regression coverage for pkg/steward.
//
// Pins the runtime side of the spi-5bzu9r convergence: in cluster-
// native mode the steward never invokes pkg/agent.Backend.Spawn, and
// every per-phase dispatch site emits a WorkloadIntent stamped with a
// supported (role, phase, runtime) triple — never the legacy
// formula_phase=recovery value the operator would misroute.
//
// Companion files:
//   - cluster_dispatch_phase_emit_test.go pins dispatchPhase /
//     dispatchPhaseClusterNative on review and hooked-step resume.
//   - cluster_dispatch_test.go exercises dispatchClusterNative (the
//     bead-level loop).
//   - This file adds the regression scenarios that complete the
//     boundary: a failing-spawner harness that fails the test on any
//     Spawn call, intent capture for cleric dispatch, and a
//     classifier-driven sanity check against the legacy
//     formula_phase=recovery anti-pattern.

package steward

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"
)

// failingSpawnBackend is the cluster-native regression sentinel: any
// Spawn call records the offending config AND fails the test. Wired
// into dispatchPhase calls in cluster-native mode to assert the
// dispatch path never falls through to backend.Spawn.
type failingSpawnBackend struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []agent.SpawnConfig
	reason string
}

func newFailingSpawnBackend(t *testing.T, reason string) *failingSpawnBackend {
	t.Helper()
	return &failingSpawnBackend{t: t, reason: reason}
}

func (f *failingSpawnBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cfg)
	calls := len(f.calls)
	f.mu.Unlock()
	if f.t != nil {
		f.t.Errorf("Spawn called in cluster-native dispatch path (call #%d, role=%q, bead=%q): %s",
			calls, cfg.Role, cfg.BeadID, f.reason)
	}
	return nil, errors.New("Spawn forbidden in cluster-native dispatch path")
}
func (f *failingSpawnBackend) List() ([]agent.Info, error)        { return nil, nil }
func (f *failingSpawnBackend) Logs(string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
func (f *failingSpawnBackend) Kill(string) error                  { return nil }
func (f *failingSpawnBackend) TerminateBead(_ context.Context, _ string) error {
	return nil
}

// fakeChildIntentSink captures emitted intents so cluster-native
// regression tests can assert on the captured shape — TaskID, phase,
// HandoffMode — without needing a live operator. Mirrors the
// pkg/executor sink so future cross-package tests can share patterns.
type fakeChildIntentSink struct {
	mu    sync.Mutex
	Calls []intent.WorkloadIntent
}

func (s *fakeChildIntentSink) Publish(_ context.Context, i intent.WorkloadIntent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, i)
	return nil
}

// TestFailingSpawnBackend_FailsOnSpawn is the sanity check on the
// regression sentinel — without it, a misconfigured test could
// silently pass while Spawn was being invoked.
func TestFailingSpawnBackend_FailsOnSpawn(t *testing.T) {
	inner := &testing.T{}
	b := newFailingSpawnBackend(inner, "sanity-check")
	_, err := b.Spawn(agent.SpawnConfig{Name: "x", BeadID: "spi-x", Role: agent.RoleApprentice})
	if err == nil {
		t.Errorf("Spawn returned nil error; sentinel must error so callers see the violation")
	}
	if !inner.Failed() {
		t.Errorf("Spawn did not mark the inner *testing.T as failed; sentinel is silent")
	}
	if len(b.calls) != 1 {
		t.Errorf("calls = %d, want 1", len(b.calls))
	}
}

// TestDispatchPhase_ClusterNativeSentinelNeverSpawns wires the
// failing-spawn sentinel into dispatchPhase under cluster-native and
// asserts that the dispatch path emits an intent without calling the
// sentinel. Complements
// TestDispatchPhase_ClusterNativeEmitsIntentAndSkipsSpawn in
// cluster_dispatch_phase_emit_test.go (which uses a counting spy);
// this version's sentinel fails the test directly so the violation
// surfaces immediately.
func TestDispatchPhase_ClusterNativeSentinelNeverSpawns(t *testing.T) {
	withStubbedNextDispatchSeq(t, 11)

	sink := &fakeChildIntentSink{}
	sentinel := newFailingSpawnBackend(t, "cluster-native dispatchPhase must emit intent, not spawn")

	pd := PhaseDispatch{
		Mode: config.DeploymentModeClusterNative,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  fakeResolver{},
			Publisher: sink,
		},
	}

	cases := []struct {
		name   string
		role   agent.SpawnRole
		phase  string
		beadID string
	}{
		{"sage_review", agent.RoleSage, intent.PhaseReview, "spi-rev-1"},
		{"sage_review_fix_via_review_phase", agent.RoleSage, intent.PhaseReview, "spi-rev-2"},
		{"apprentice_implement", agent.RoleApprentice, intent.PhaseImplement, "spi-impl-1"},
		{"apprentice_fix", agent.RoleApprentice, intent.PhaseFix, "spi-fix-1"},
		{"wizard_resume_task", agent.RoleWizard, "task", "spi-task-1"},
		{"wizard_resume_epic", agent.RoleWizard, "epic", "spi-epic-1"},
		{"cleric_resume_task", agent.RoleExecutor, "task", "spi-rec-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handle, err := dispatchPhase(context.Background(), pd, sentinel, agent.SpawnConfig{
				Name:   "agent-" + tc.beadID,
				BeadID: tc.beadID,
				Role:   tc.role,
			}, tc.phase)
			if err != nil {
				t.Fatalf("dispatchPhase: %v", err)
			}
			if handle != nil {
				t.Errorf("handle = %v, want nil — cluster-native emits intent, no local handle", handle)
			}
		})
	}

	// Aggregate assertions on the captured intents — every entry
	// classifies under exactly one of the three intent.IsXxxLevelPhase
	// helpers so the operator's pod-builder can route it.
	if len(sink.Calls) != len(cases) {
		t.Fatalf("captured %d intent(s), want %d (one per dispatch case)", len(sink.Calls), len(cases))
	}
	for i, got := range sink.Calls {
		if got.TaskID != cases[i].beadID {
			t.Errorf("Calls[%d].TaskID = %q, want %q", i, got.TaskID, cases[i].beadID)
		}
		if got.FormulaPhase != cases[i].phase {
			t.Errorf("Calls[%d].FormulaPhase = %q, want %q", i, got.FormulaPhase, cases[i].phase)
		}
		if got.HandoffMode == "" {
			t.Errorf("Calls[%d].HandoffMode empty — dispatchPhase should set the default", i)
		}
		if got.RepoIdentity.URL == "" {
			t.Errorf("Calls[%d].RepoIdentity incomplete: %+v", i, got.RepoIdentity)
		}

		classified := intent.IsBeadLevelPhase(got.FormulaPhase) ||
			intent.IsStepLevelPhase(got.FormulaPhase) ||
			intent.IsReviewLevelPhase(got.FormulaPhase)
		if !classified {
			t.Errorf("Calls[%d].FormulaPhase = %q does not classify under bead/step/review-level — operator would misroute",
				i, got.FormulaPhase)
		}
	}
}

// TestClericDispatchPhase_NeverEmitsLegacyRecoveryString pins the
// behaviour spi-5bzu9r.3 enforces: cleric dispatch in cluster-native
// MUST emit a bead-level FormulaPhase, never the literal
// "recovery". The operator's pod-builder allowlist (driven by
// intent.IsBeadLevelPhase) only routes bead-level values; emitting
// "recovery" would silently misroute or drop the workload.
//
// Today this is a property of beadDispatchPhase: when the recovery
// bead's type is non-empty it returns the type as-is, but every type
// the steward sets on a recovery bead in cluster-native code paths
// is bead-level (task / bug / epic / feature / chore) by
// construction. Pinning it as a regression test catches any future
// change that lets "recovery" leak through.
func TestClericDispatchPhase_NeverEmitsLegacyRecoveryString(t *testing.T) {
	cases := []struct {
		name           string
		recoveryType   string
		wantPhase      string
		wantBeadLevel  bool
	}{
		{"task_parent", "task", "task", true},
		{"bug_parent", "bug", "bug", true},
		{"epic_parent", "epic", "epic", true},
		{"feature_parent", "feature", "feature", true},
		{"chore_parent", "chore", "chore", true},
		{"empty_type_falls_back_to_wizard", "", intent.PhaseWizard, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := beadDispatchPhase("", tc.recoveryType)
			if got != tc.wantPhase {
				t.Errorf("beadDispatchPhase(%q) = %q, want %q", tc.recoveryType, got, tc.wantPhase)
			}
			if tc.wantBeadLevel && !intent.IsBeadLevelPhase(got) {
				t.Errorf("beadDispatchPhase(%q) = %q, which is NOT bead-level — operator would misroute",
					tc.recoveryType, got)
			}
		})
	}

	// The legacy "recovery" string must NOT be a bead-level phase —
	// that is what makes it unsupported on the operator side. If
	// future code begins classifying "recovery" as bead-level
	// (intentionally or accidentally), this test fails so the change
	// is reviewed against the convergence contract.
	if intent.IsBeadLevelPhase("recovery") {
		t.Errorf(`IsBeadLevelPhase("recovery") = true — operator would route an unsupported triple`)
	}
}

// TestHookedResumePhase_ClassifiesUnderBeadLevel pins the rule that
// hookedResumePhase always returns a bead-level phase for known parent
// types and the wizard fallback. Combined with
// TestClericDispatchPhase_NeverEmitsLegacyRecoveryString, this closes
// the loop: every cluster-native phase the steward emits classifies
// under intent.IsBeadLevelPhase / IsStepLevelPhase /
// IsReviewLevelPhase, so the operator's allowlist matches.
func TestHookedResumePhase_ClassifiesUnderBeadLevel(t *testing.T) {
	cases := []string{"task", "bug", "epic", "feature", "chore", ""}
	for _, parentType := range cases {
		got := hookedResumePhase(parentType)
		if !intent.IsBeadLevelPhase(got) {
			t.Errorf("hookedResumePhase(%q) = %q, which is NOT bead-level — operator would misroute the resume",
				parentType, got)
		}
	}
}

// TestDispatchClusterNative_NeverSpawns wires a failing-spawn
// sentinel into a steward configured for cluster-native and runs a
// dry-run-friendly slice of bead-level dispatch through
// dispatchClusterNative. It complements TestDispatchClusterNative_*
// in cluster_dispatch_test.go (which assert on emit shape) by
// asserting on the negative invariant — Spawn is never called.
//
// dispatchClusterNative does not take a backend argument; the
// invariant is enforced by the file-level guard at the top of
// cluster_dispatch.go and by the absence of a Spawn call site in the
// helper itself. This test pins the contract by configuring a
// StewardConfig whose Backend is the failing sentinel, exercising
// dispatchClusterNative through DryRun=true (so the publisher is
// never actually called), and confirming the sentinel's call slice
// stays empty.
func TestDispatchClusterNative_NeverSpawns(t *testing.T) {
	sink := &fakeChildIntentSink{}
	sentinel := newFailingSpawnBackend(t, "dispatchClusterNative must emit intents, not spawn")

	// fakeClaimer + fakeResolver are defined in cluster_dispatch_test.go
	// (same package) and reused here so the test surface stays narrow.
	cfg := StewardConfig{
		DryRun:  true, // skip the publisher call to keep the test hermetic
		Backend: sentinel,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  fakeResolver{},
			Claimer:   &fakeClaimer{},
			Publisher: sink,
		},
	}

	candidates := []store.Bead{{ID: "spi-x", Type: "task"}}
	emitted := dispatchClusterNative(context.Background(), "[regression] ", candidates, cfg)
	if emitted != 1 {
		t.Errorf("emitted = %d, want 1 (dry-run still increments the counter)", emitted)
	}
	if len(sentinel.calls) != 0 {
		t.Errorf("sentinel.calls = %d, want 0 — cluster-native must not spawn", len(sentinel.calls))
	}
	if len(sink.Calls) != 0 {
		t.Errorf("sink.Calls = %d, want 0 (DryRun bypasses Publish)", len(sink.Calls))
	}
}
