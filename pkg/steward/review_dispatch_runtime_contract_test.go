package steward

import (
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// TestDetectReviewReady_PopulatesRuntimeContract verifies the reviewer
// SpawnConfig dispatched by DetectReviewReady carries a non-empty
// Identity, Workspace, and Run — the three fields the cluster backend's
// buildSubstratePod rejects with ErrIdentityRequired /
// ErrWorkspaceRequired when missing (spi-cob04b). Without this, every
// cluster-mode review would fail at pod construction.
func TestDetectReviewReady_PopulatesRuntimeContract(t *testing.T) {
	origList := ListBeadsFunc
	origSteps := GetStepBeadsFunc
	origChildren := GetChildrenFunc
	t.Cleanup(func() {
		ListBeadsFunc = origList
		GetStepBeadsFunc = origSteps
		GetChildrenFunc = origChildren
	})

	parentBead := store.Bead{
		ID:     "spi-cob04b",
		Status: "in_progress",
		Type:   "bug",
	}
	ListBeadsFunc = func(_ beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{parentBead}, nil
	}
	// Return a single closed implement step bead so DetectReviewReady's
	// eligibility check passes.
	GetStepBeadsFunc = func(_ string) ([]store.Bead, error) {
		return []store.Bead{{
			ID:     "spi-cob04b.implement",
			Status: "closed",
			Labels: []string{"step:implement", "workflow-step"},
		}}, nil
	}
	// No review-round child beads — first review.
	GetChildrenFunc = func(_ string) ([]store.Bead, error) {
		return nil, nil
	}

	backend := &phaseSpyBackend{}
	towerBindings := map[string]*config.LocalRepoBinding{
		"spi": {
			Prefix:       "spi",
			LocalPath:    "/home/dev/awell/spire",
			RepoURL:      "git@github.com:awell-health/spire.git",
			SharedBranch: "main",
			State:        "bound",
		},
	}

	DetectReviewReady(false, backend, "tower-awell", towerBindings, PhaseDispatch{})

	if len(backend.spawns) != 1 {
		t.Fatalf("backend.Spawn called %d time(s), want 1", len(backend.spawns))
	}
	cfg := backend.spawns[0]

	if cfg.BeadID != "spi-cob04b" {
		t.Errorf("BeadID = %q, want spi-cob04b", cfg.BeadID)
	}
	if cfg.Role != agent.RoleSage {
		t.Errorf("Role = %q, want %q", cfg.Role, agent.RoleSage)
	}

	// Runtime contract — every required cluster-backend field must be set.
	if cfg.Identity == (runtime.RepoIdentity{}) {
		t.Fatalf("Identity is zero — cluster backend rejects with ErrIdentityRequired")
	}
	if cfg.Identity.RepoURL != "git@github.com:awell-health/spire.git" {
		t.Errorf("Identity.RepoURL = %q, want git@github.com:awell-health/spire.git", cfg.Identity.RepoURL)
	}
	if cfg.Identity.BaseBranch != "main" {
		t.Errorf("Identity.BaseBranch = %q, want main", cfg.Identity.BaseBranch)
	}
	if cfg.Identity.Prefix != "spi" {
		t.Errorf("Identity.Prefix = %q, want spi", cfg.Identity.Prefix)
	}
	if cfg.Identity.TowerName != "tower-awell" {
		t.Errorf("Identity.TowerName = %q, want tower-awell", cfg.Identity.TowerName)
	}
	if cfg.Workspace == nil {
		t.Fatalf("Workspace is nil — cluster backend rejects with ErrWorkspaceRequired")
	}
	if cfg.Workspace.Path != "/home/dev/awell/spire" {
		t.Errorf("Workspace.Path = %q, want /home/dev/awell/spire", cfg.Workspace.Path)
	}

	if cfg.Run.HandoffMode == "" {
		t.Errorf("Run.HandoffMode is empty — review MUST pass an explicit mode")
	}
	if cfg.Run.HandoffMode != runtime.HandoffBorrowed {
		t.Errorf("Run.HandoffMode = %q, want borrowed (sage review is same-owner)", cfg.Run.HandoffMode)
	}
	if cfg.Run.TowerName != "tower-awell" {
		t.Errorf("Run.TowerName = %q, want tower-awell", cfg.Run.TowerName)
	}
	if cfg.Run.FormulaStep != "review" {
		t.Errorf("Run.FormulaStep = %q, want review", cfg.Run.FormulaStep)
	}
}

// TestDetectReviewReady_NilTowerBindings verifies the helper is resilient
// to a nil tower-bindings map (single-tower legacy mode, or a tower
// whose bindings couldn't be loaded). The contract still needs to fill
// Identity/Workspace/Run with safe defaults so ProcessBackend works
// unchanged — the cluster path is by definition off when there's no
// tower config to name the repo.
func TestDetectReviewReady_NilTowerBindings(t *testing.T) {
	origList := ListBeadsFunc
	origSteps := GetStepBeadsFunc
	origChildren := GetChildrenFunc
	t.Cleanup(func() {
		ListBeadsFunc = origList
		GetStepBeadsFunc = origSteps
		GetChildrenFunc = origChildren
	})

	ListBeadsFunc = func(_ beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{{ID: "spi-x", Status: "in_progress", Type: "task"}}, nil
	}
	GetStepBeadsFunc = func(_ string) ([]store.Bead, error) {
		return []store.Bead{{
			ID:     "spi-x.implement",
			Status: "closed",
			Labels: []string{"step:implement", "workflow-step"},
		}}, nil
	}
	GetChildrenFunc = func(_ string) ([]store.Bead, error) {
		return nil, nil
	}

	backend := &phaseSpyBackend{}
	DetectReviewReady(false, backend, "tower-x", nil, PhaseDispatch{})

	if len(backend.spawns) != 1 {
		t.Fatalf("backend.Spawn called %d time(s), want 1", len(backend.spawns))
	}
	cfg := backend.spawns[0]

	// Even without a binding, the populator fills kind=repo defaults
	// and stamps a non-empty HandoffMode — nothing in the path that
	// leads to the backend should be left unpopulated.
	if cfg.Workspace == nil {
		t.Fatalf("Workspace is nil")
	}
	if cfg.Run.HandoffMode == "" {
		t.Errorf("Run.HandoffMode is empty")
	}
	if cfg.Identity.Prefix != "spi" {
		t.Errorf("Identity.Prefix = %q, want spi (derived from bead ID)", cfg.Identity.Prefix)
	}
}
