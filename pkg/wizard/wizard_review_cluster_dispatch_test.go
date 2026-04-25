package wizard

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/steward/intent"
)

// fakeClusterChildDispatcher is the wizard-side test fake for
// executor.ClusterChildDispatcher. The same shape lives in
// pkg/executor's own cluster_dispatch_test.go; the duplication is
// intentional — both packages exercise the seam directly with their
// own fake rather than threading a shared test harness.
type fakeClusterChildDispatcher struct {
	mu          sync.Mutex
	calls       []intent.WorkloadIntent
	dispatchErr error
}

func (f *fakeClusterChildDispatcher) Dispatch(_ context.Context, wi intent.WorkloadIntent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, wi)
	return f.dispatchErr
}

func (f *fakeClusterChildDispatcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeClusterChildDispatcher) lastCall() intent.WorkloadIntent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return intent.WorkloadIntent{}
	}
	return f.calls[len(f.calls)-1]
}

// TestReviewHandleRequestChanges_ClusterNative_EmitsIntent verifies the
// review-fix re-entry in cluster-native mode emits a WorkloadIntent
// with Role=apprentice / Phase=review-fix and never calls
// backend.Spawn. This is the load-bearing assertion for the wizard
// side of migration track 1: cluster-native review-fix routes through
// the operator intent plane, not the local registry.
func TestReviewHandleRequestChanges_ClusterNative_EmitsIntent(t *testing.T) {
	stubSendMessage(t)
	t.Setenv("SPIRE_AGENT_IMAGE", "ghcr.io/example/spire-agent:test")

	disp := &fakeClusterChildDispatcher{}
	backend := &spyBackend{}
	deps := &Deps{
		AddComment:             func(_, _ string) error { return nil },
		AddLabel:               func(_, _ string) error { return nil },
		RegistryUpdate:         func(_ string, _ func(*Entry)) error { return nil },
		ResolveBackend:         func(_ string) Backend { return backend },
		ClusterChildDispatcher: disp,
		ResolveRepo: func(_ string) (string, string, string, error) {
			return "/tmp/repo", "git@github.com:example/spire.git", "main", nil
		},
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return &TowerConfig{
				Name:           "tower-awell",
				DeploymentMode: config.DeploymentModeClusterNative,
				Apprentice:     config.ApprenticeConfig{Transport: config.ApprenticeTransportBundle},
			}, nil
		},
		GetBead:       func(_ string) (Bead, error) { return Bead{ID: "spi-cob04b"}, nil },
		HasLabel:      func(_ Bead, _ string) string { return "" },
		DoltGlobalDir: func() string { return t.TempDir() },
	}

	review := &Review{Verdict: "request_changes", Summary: "please add tests"}
	if err := ReviewHandleRequestChanges("spi-cob04b", "reviewer-1", review, 2, RevisionPolicy{MaxRounds: 3}, deps, noopLogf); err != nil {
		t.Fatalf("ReviewHandleRequestChanges: %v", err)
	}

	if len(backend.spawns) != 0 {
		t.Errorf("backend.Spawn was called %d times in cluster-native review-fix, want 0", len(backend.spawns))
	}
	if disp.callCount() != 1 {
		t.Fatalf("ClusterChildDispatcher.Dispatch called %d times, want 1", disp.callCount())
	}
	got := disp.lastCall()
	if got.TaskID != "spi-cob04b" {
		t.Errorf("intent.TaskID = %q, want %q (review-fix re-entry preserves the task root)", got.TaskID, "spi-cob04b")
	}
	if got.Role != intent.RoleApprentice {
		t.Errorf("intent.Role = %q, want %q", got.Role, intent.RoleApprentice)
	}
	if got.Phase != intent.PhaseReviewFix {
		t.Errorf("intent.Phase = %q, want %q (review-fix MUST NOT collapse to PhaseFix)", got.Phase, intent.PhaseReviewFix)
	}
	if got.RepoIdentity.URL != "git@github.com:example/spire.git" {
		t.Errorf("intent.RepoIdentity.URL = %q, want repo URL", got.RepoIdentity.URL)
	}
	if got.RepoIdentity.BaseBranch != "main" {
		t.Errorf("intent.RepoIdentity.BaseBranch = %q, want main", got.RepoIdentity.BaseBranch)
	}
	if got.RepoIdentity.Prefix != "spi" {
		t.Errorf("intent.RepoIdentity.Prefix = %q, want spi", got.RepoIdentity.Prefix)
	}
	if got.Runtime.Image != "ghcr.io/example/spire-agent:test" {
		t.Errorf("intent.Runtime.Image = %q, want ghcr.io/example/spire-agent:test", got.Runtime.Image)
	}
	if got.HandoffMode == "" {
		t.Errorf("intent.HandoffMode is empty — review-fix produces commits and must declare bundle delivery")
	}
	// The Reason field carries the round number for log/metric continuity
	// across the seam; the steward and operator log all derive review-round
	// correlation from this string.
	if got.Reason == "" {
		t.Errorf("intent.Reason is empty — review-fix should stamp round number for log continuity")
	}
}

// TestReviewHandleRequestChanges_ClusterNative_DispatchErrorPropagates
// confirms that a Dispatch failure surfaces as the function's returned
// error rather than a silent fallback to backend.Spawn. Cluster-native
// MUST fail closed when the dispatcher errors — that's the contract
// .5 will pin with an invariant test.
func TestReviewHandleRequestChanges_ClusterNative_DispatchErrorPropagates(t *testing.T) {
	stubSendMessage(t)
	t.Setenv("SPIRE_AGENT_IMAGE", "ghcr.io/example/spire-agent:test")

	publishErr := errors.New("publish failed")
	disp := &fakeClusterChildDispatcher{dispatchErr: publishErr}
	backend := &spyBackend{}
	deps := &Deps{
		AddComment:             func(_, _ string) error { return nil },
		AddLabel:               func(_, _ string) error { return nil },
		RegistryUpdate:         func(_ string, _ func(*Entry)) error { return nil },
		ResolveBackend:         func(_ string) Backend { return backend },
		ClusterChildDispatcher: disp,
		ResolveRepo: func(_ string) (string, string, string, error) {
			return "/tmp/repo", "git@github.com:example/spire.git", "main", nil
		},
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return &TowerConfig{
				Name:           "tower-awell",
				DeploymentMode: config.DeploymentModeClusterNative,
				Apprentice:     config.ApprenticeConfig{Transport: config.ApprenticeTransportBundle},
			}, nil
		},
		GetBead:       func(_ string) (Bead, error) { return Bead{ID: "spi-cob04b"}, nil },
		HasLabel:      func(_ Bead, _ string) string { return "" },
		DoltGlobalDir: func() string { return t.TempDir() },
	}

	review := &Review{Verdict: "request_changes", Summary: "please add tests"}
	err := ReviewHandleRequestChanges("spi-cob04b", "reviewer-1", review, 1, RevisionPolicy{MaxRounds: 3}, deps, noopLogf)
	if err == nil {
		t.Fatalf("want error from publish failure, got nil")
	}
	if !errors.Is(err, publishErr) {
		t.Errorf("returned error %v does not wrap %v", err, publishErr)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("backend.Spawn called %d times despite cluster-native dispatch error — must fail closed", len(backend.spawns))
	}
}

// TestReviewHandleRequestChanges_LocalNative_StillSpawns is the parity
// counterpart: local-native review-fix must keep using backend.Spawn
// even when ClusterChildDispatcher is wired (e.g. shared deps across
// modes). The 2-condition truth table lives in
// useClusterChildDispatchForReviewFix; this test pins it.
func TestReviewHandleRequestChanges_LocalNative_StillSpawns(t *testing.T) {
	stubSendMessage(t)

	disp := &fakeClusterChildDispatcher{}
	backend := &spyBackend{}
	deps := &Deps{
		AddComment:             func(_, _ string) error { return nil },
		AddLabel:               func(_, _ string) error { return nil },
		RegistryUpdate:         func(_ string, _ func(*Entry)) error { return nil },
		ResolveBackend:         func(_ string) Backend { return backend },
		ClusterChildDispatcher: disp, // wired but should be ignored in local-native
		ResolveRepo: func(_ string) (string, string, string, error) {
			return "/tmp/repo", "git@github.com:example/spire.git", "main", nil
		},
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return &TowerConfig{
				Name:           "tower-awell",
				DeploymentMode: config.DeploymentModeLocalNative,
				Apprentice:     config.ApprenticeConfig{Transport: config.ApprenticeTransportBundle},
			}, nil
		},
		GetBead:       func(_ string) (Bead, error) { return Bead{ID: "spi-cob04b"}, nil },
		HasLabel:      func(_ Bead, _ string) string { return "" },
		DoltGlobalDir: func() string { return t.TempDir() },
	}

	review := &Review{Verdict: "request_changes", Summary: "please add tests"}
	if err := ReviewHandleRequestChanges("spi-cob04b", "reviewer-1", review, 1, RevisionPolicy{MaxRounds: 3}, deps, noopLogf); err != nil {
		t.Fatalf("ReviewHandleRequestChanges: %v", err)
	}

	if disp.callCount() != 0 {
		t.Errorf("ClusterChildDispatcher.Dispatch called %d times in local-native, want 0", disp.callCount())
	}
	if len(backend.spawns) != 1 {
		t.Fatalf("backend.Spawn called %d times in local-native, want 1", len(backend.spawns))
	}
	cfg := backend.last()
	if cfg.BeadID != "spi-cob04b" {
		t.Errorf("Spawn cfg.BeadID = %q, want spi-cob04b", cfg.BeadID)
	}
	if cfg.Role != RoleApprentice {
		t.Errorf("Spawn cfg.Role = %q, want apprentice", cfg.Role)
	}
}

// TestUseClusterChildDispatchForReviewFix locks the helper truth
// table: cluster-native AND non-nil dispatcher → true; everything
// else → false. Keeps the wizard-side check in lockstep with the
// executor-side useClusterChildDispatch helper so neither path drifts
// into accidental fallback.
func TestUseClusterChildDispatchForReviewFix(t *testing.T) {
	t.Run("cluster + dispatcher", func(t *testing.T) {
		deps := &Deps{ClusterChildDispatcher: &fakeClusterChildDispatcher{}}
		tower := &TowerConfig{DeploymentMode: config.DeploymentModeClusterNative}
		if !useClusterChildDispatchForReviewFix(deps, tower) {
			t.Errorf("useClusterChildDispatchForReviewFix(cluster + dispatcher) = false, want true")
		}
	})
	t.Run("cluster + nil dispatcher", func(t *testing.T) {
		deps := &Deps{ClusterChildDispatcher: nil}
		tower := &TowerConfig{DeploymentMode: config.DeploymentModeClusterNative}
		if useClusterChildDispatchForReviewFix(deps, tower) {
			t.Errorf("useClusterChildDispatchForReviewFix(cluster + nil) = true, want false (must fail closed)")
		}
	})
	t.Run("local + dispatcher", func(t *testing.T) {
		deps := &Deps{ClusterChildDispatcher: &fakeClusterChildDispatcher{}}
		tower := &TowerConfig{DeploymentMode: config.DeploymentModeLocalNative}
		if useClusterChildDispatchForReviewFix(deps, tower) {
			t.Errorf("useClusterChildDispatchForReviewFix(local + dispatcher) = true, want false")
		}
	})
	t.Run("nil tower", func(t *testing.T) {
		deps := &Deps{ClusterChildDispatcher: &fakeClusterChildDispatcher{}}
		if useClusterChildDispatchForReviewFix(deps, nil) {
			t.Errorf("useClusterChildDispatchForReviewFix(nil tower) = true, want false")
		}
	})
}
