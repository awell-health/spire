package wizard

import (
	"io"
	"os"
	"sync"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/runtime"
)

// spyBackend captures every SpawnConfig handed to Spawn. The existing
// mockBackend in wizard tests is process-oriented; the runtime-contract
// checks need a minimal, isolated fake focused on SpawnConfig inspection.
type spyBackend struct {
	mu     sync.Mutex
	spawns []SpawnConfig
}

func (s *spyBackend) Spawn(cfg SpawnConfig) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spawns = append(s.spawns, cfg)
	return spyHandle{name: cfg.Name}, nil
}

func (s *spyBackend) List() ([]agent.Info, error)             { return nil, nil }
func (s *spyBackend) Logs(_ string) (io.ReadCloser, error)    { return nil, os.ErrNotExist }
func (s *spyBackend) Kill(_ string) error                     { return nil }
func (s *spyBackend) last() SpawnConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.spawns) == 0 {
		return SpawnConfig{}
	}
	return s.spawns[len(s.spawns)-1]
}

type spyHandle struct{ name string }

func (h spyHandle) Wait() error              { return nil }
func (h spyHandle) Signal(_ os.Signal) error { return nil }
func (h spyHandle) Alive() bool              { return true }
func (h spyHandle) Name() string             { return h.name }
func (h spyHandle) Identifier() string       { return "0" }

// TestWizardReviewHandoff_PopulatesRuntimeContract verifies that the
// reviewer SpawnConfig handed to the backend includes Identity,
// Workspace, and Run fields — the non-optional shape the cluster
// backend validates at buildSubstratePod. Also confirms Tower and
// InstanceID are set on the SpawnConfig, since the bug description
// calls out both as missing in the pre-fix path.
func TestWizardReviewHandoff_PopulatesRuntimeContract(t *testing.T) {
	backend := &spyBackend{}
	deps := &Deps{
		AddLabel:       func(id, label string) error { return nil },
		RegistryAdd:    func(_ Entry) error { return nil },
		RegistryRemove: func(_ string) error { return nil },
		RegistryUpdate: func(_ string, _ func(*Entry)) error { return nil },
		AddComment:     func(_, _ string) error { return nil },
		ResolveBackend: func(_ string) Backend { return backend },
		ResolveRepo: func(_ string) (string, string, string, error) {
			return "/tmp/repo", "git@github.com:example/spire.git", "main", nil
		},
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return &TowerConfig{Name: "tower-awell"}, nil
		},
		GetBead:  func(_ string) (Bead, error) { return Bead{}, nil },
		HasLabel: func(_ Bead, _ string) string { return "" },
	}

	WizardReviewHandoff("spi-cob04b", "wizard-spi-cob04b", "feat/spi-cob04b", deps, noopLogf)

	cfg := backend.last()
	if cfg.Name != "wizard-spi-cob04b-review" {
		t.Errorf("Name = %q, want wizard-spi-cob04b-review", cfg.Name)
	}
	if cfg.Role != RoleSage {
		t.Errorf("Role = %q, want %q", cfg.Role, RoleSage)
	}
	if cfg.Tower == "" {
		t.Errorf("Tower is empty — cluster backend needs it set")
	}
	if cfg.InstanceID == "" {
		t.Errorf("InstanceID is empty — bug description calls this out as missing")
	}
	if cfg.Identity == (runtime.RepoIdentity{}) {
		t.Fatalf("Identity is zero — cluster backend rejects with ErrIdentityRequired")
	}
	if cfg.Identity.RepoURL == "" {
		t.Errorf("Identity.RepoURL is empty — buildSubstratePod requires it")
	}
	if cfg.Identity.BaseBranch == "" {
		t.Errorf("Identity.BaseBranch is empty — buildSubstratePod requires it")
	}
	if cfg.Identity.Prefix == "" {
		t.Errorf("Identity.Prefix is empty — buildSubstratePod requires it")
	}
	if cfg.Workspace == nil {
		t.Fatalf("Workspace is nil — cluster backend rejects with ErrWorkspaceRequired")
	}
	if cfg.Run.HandoffMode == "" {
		t.Errorf("Run.HandoffMode is empty — review dispatch MUST pass an explicit mode")
	}
	if cfg.Run.HandoffMode != "borrowed" {
		// HandoffBorrowed is the sage-review canonical selection — matches
		// the executor's own wizardRunSpawn default. Using the literal
		// avoids dragging the executor constant into a wizard test import.
		t.Errorf("Run.HandoffMode = %q, want borrowed (sage review is same-owner)", cfg.Run.HandoffMode)
	}
	if cfg.Run.TowerName == "" {
		t.Errorf("Run.TowerName is empty — observability identity requires it")
	}
}

// TestReviewHandleRequestChanges_PopulatesRuntimeContract verifies the
// review-fix re-engagement path also produces a SpawnConfig with the
// full runtime contract set. This is the apprentice-for-fix spawn, so
// HandoffMode must be an apprentice delivery mode (bundle by default).
func TestReviewHandleRequestChanges_PopulatesRuntimeContract(t *testing.T) {
	stubSendMessage(t)
	backend := &spyBackend{}
	// Build a deps with bundle transport so HandoffMode resolves to
	// "bundle" (not "transitional"). The review-fix re-engagement
	// path routes through ApprenticeDeliveryHandoff(tower) to stay in
	// lockstep with the executor's review-fix dispatch.
	deps := &Deps{
		AddComment:     func(_, _ string) error { return nil },
		AddLabel:       func(_, _ string) error { return nil },
		RegistryAdd:    func(_ Entry) error { return nil },
		RegistryUpdate: func(_ string, _ func(*Entry)) error { return nil },
		ResolveBackend: func(_ string) Backend { return backend },
		ResolveRepo: func(_ string) (string, string, string, error) {
			return "/tmp/repo", "git@github.com:example/spire.git", "main", nil
		},
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return &TowerConfig{
				Name:       "tower-awell",
				Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportBundle},
			}, nil
		},
		GetBead:       func(_ string) (Bead, error) { return Bead{ID: "spi-cob04b"}, nil },
		HasLabel:      func(_ Bead, _ string) string { return "" },
		DoltGlobalDir: func() string { return t.TempDir() },
	}

	review := &Review{Verdict: "request_changes", Summary: "please add tests"}
	err := ReviewHandleRequestChanges("spi-cob04b", "reviewer-1", review, 1, RevisionPolicy{MaxRounds: 3}, deps, noopLogf)
	if err != nil {
		t.Fatalf("ReviewHandleRequestChanges: %v", err)
	}

	cfg := backend.last()
	if cfg.BeadID != "spi-cob04b" {
		t.Fatalf("BeadID = %q, want spi-cob04b", cfg.BeadID)
	}
	if cfg.Role != RoleApprentice {
		t.Errorf("Role = %q, want %q (review-fix is apprentice)", cfg.Role, RoleApprentice)
	}
	if cfg.Tower == "" {
		t.Errorf("Tower is empty — bug description calls this out as missing")
	}
	if cfg.InstanceID == "" {
		t.Errorf("InstanceID is empty — bug description calls this out as missing")
	}
	if cfg.Identity == (runtime.RepoIdentity{}) {
		t.Fatalf("Identity is zero — cluster backend rejects with ErrIdentityRequired")
	}
	if cfg.Identity.RepoURL == "" {
		t.Errorf("Identity.RepoURL is empty — buildSubstratePod requires it")
	}
	if cfg.Identity.BaseBranch == "" {
		t.Errorf("Identity.BaseBranch is empty — buildSubstratePod requires it")
	}
	if cfg.Identity.Prefix == "" {
		t.Errorf("Identity.Prefix is empty — buildSubstratePod requires it")
	}
	if cfg.Workspace == nil {
		t.Fatalf("Workspace is nil — cluster backend rejects with ErrWorkspaceRequired")
	}
	if cfg.Run.HandoffMode == "" {
		t.Errorf("Run.HandoffMode is empty — review-fix MUST pass an explicit mode")
	}
	// Review-fix must route through ApprenticeDeliveryHandoff so the
	// transport selection matches the executor's commit-producing
	// apprentice dispatch. Bundle is the canonical cross-owner protocol
	// (the executor's apprenticeDeliveryHandoff default when a tower
	// declares bundle transport).
	if cfg.Run.HandoffMode != "bundle" {
		t.Errorf("Run.HandoffMode = %q, want bundle (apprentice delivery for bundle-transport tower)", cfg.Run.HandoffMode)
	}
	if cfg.Run.TowerName == "" {
		t.Errorf("Run.TowerName is empty — observability identity requires it")
	}
}

// TestReviewHandleRequestChanges_FailOnTransitionalPropagates verifies the
// SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF gate is honored end-to-end from the
// wizard's review-fix re-engagement path through PopulateRuntimeContract.
// This is the CI parity surface: if a tower accidentally ships push
// transport, review-fix must surface it as a hard error instead of
// silently spawning.
func TestReviewHandleRequestChanges_FailOnTransitionalPropagates(t *testing.T) {
	stubSendMessage(t)
	t.Setenv("SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF", "1")

	backend := &spyBackend{}
	deps := &Deps{
		AddComment:     func(_, _ string) error { return nil },
		AddLabel:       func(_, _ string) error { return nil },
		RegistryAdd:    func(_ Entry) error { return nil },
		RegistryUpdate: func(_ string, _ func(*Entry)) error { return nil },
		ResolveBackend: func(_ string) Backend { return backend },
		ResolveRepo: func(_ string) (string, string, string, error) {
			return "/tmp/repo", "git@github.com:example/spire.git", "main", nil
		},
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return &TowerConfig{
				Name: "tower-awell",
				// Push transport — transitional HandoffMode under the gate.
				Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportPush},
			}, nil
		},
		GetBead:       func(_ string) (Bead, error) { return Bead{ID: "spi-cob04b"}, nil },
		HasLabel:      func(_ Bead, _ string) string { return "" },
		DoltGlobalDir: func() string { return t.TempDir() },
	}

	review := &Review{Verdict: "request_changes", Summary: "please add tests"}
	err := ReviewHandleRequestChanges("spi-cob04b", "reviewer-1", review, 1, RevisionPolicy{MaxRounds: 3}, deps, noopLogf)
	if err == nil {
		t.Fatalf("want error from transitional gate, got nil")
	}
	if len(backend.spawns) != 0 {
		t.Errorf("backend.Spawn called despite gate — cluster parity lane needs gate to block spawn")
	}
}

func noopLogf(string, ...interface{}) {}

// stubSendMessage replaces reviewFixSendMessage with a no-op for the
// duration of a test. The default implementation invokes os.Executable()
// which is the test binary itself during `go test`; recursive re-execution
// would stall the test.
func stubSendMessage(t *testing.T) {
	t.Helper()
	orig := reviewFixSendMessage
	reviewFixSendMessage = func(_, _, _, _ string) {}
	t.Cleanup(func() { reviewFixSendMessage = orig })
}
