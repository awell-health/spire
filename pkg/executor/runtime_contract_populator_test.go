package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
)

// TestPopulateRuntimeContract_FillsAllFields verifies the happy path: every
// input is propagated to the correct Identity/Workspace/Run field and the
// workspace is normalized to the default kind=repo handle when no explicit
// handle is supplied.
//
// This is the contract the three review-spawn sites (DetectReviewReady,
// WizardReviewHandoff, ReviewHandleRequestChanges) depend on for cluster
// backend compatibility (spi-cob04b).
func TestPopulateRuntimeContract_FillsAllFields(t *testing.T) {
	cfg := agent.SpawnConfig{
		Name:   "reviewer-spi-cob04b",
		BeadID: "spi-cob04b",
		Role:   agent.RoleSage,
	}
	inputs := RuntimeContractInputs{
		TowerName:   "tower-awell",
		RepoURL:     "git@github.com:awell-health/spire.git",
		RepoPath:    "/home/dev/awell/spire",
		BaseBranch:  "main",
		RunStep:     "review",
		Backend:     "process",
		RunID:       "run-42",
		HandoffMode: HandoffBorrowed,
	}
	out, err := PopulateRuntimeContract(cfg, inputs)
	if err != nil {
		t.Fatalf("PopulateRuntimeContract: %v", err)
	}

	// Identity — every required cluster-backend field must be non-empty.
	if out.Identity == (RepoIdentity{}) {
		t.Fatalf("Identity is zero")
	}
	if out.Identity.TowerName != inputs.TowerName {
		t.Errorf("Identity.TowerName = %q, want %q", out.Identity.TowerName, inputs.TowerName)
	}
	if out.Identity.Prefix != "spi" {
		t.Errorf("Identity.Prefix = %q, want spi (derived from bead ID)", out.Identity.Prefix)
	}
	if out.Identity.RepoURL != inputs.RepoURL {
		t.Errorf("Identity.RepoURL = %q, want %q", out.Identity.RepoURL, inputs.RepoURL)
	}
	if out.Identity.BaseBranch != inputs.BaseBranch {
		t.Errorf("Identity.BaseBranch = %q, want %q", out.Identity.BaseBranch, inputs.BaseBranch)
	}

	// Workspace — cluster backend requires a non-nil pointer.
	if out.Workspace == nil {
		t.Fatalf("Workspace is nil")
	}
	if out.Workspace.Kind != WorkspaceKindRepo {
		t.Errorf("Workspace.Kind = %q, want %q (default for nil input)", out.Workspace.Kind, WorkspaceKindRepo)
	}
	if out.Workspace.Path != inputs.RepoPath {
		t.Errorf("Workspace.Path = %q, want %q", out.Workspace.Path, inputs.RepoPath)
	}
	if out.Workspace.Origin != WorkspaceOriginLocalBind {
		t.Errorf("Workspace.Origin = %q, want %q", out.Workspace.Origin, WorkspaceOriginLocalBind)
	}

	// Run — observability identity must include a non-empty HandoffMode.
	if out.Run.HandoffMode == "" {
		t.Errorf("Run.HandoffMode is empty, want %q", HandoffBorrowed)
	}
	if out.Run.HandoffMode != HandoffBorrowed {
		t.Errorf("Run.HandoffMode = %q, want %q", out.Run.HandoffMode, HandoffBorrowed)
	}
	if out.Run.TowerName != inputs.TowerName {
		t.Errorf("Run.TowerName = %q, want %q", out.Run.TowerName, inputs.TowerName)
	}
	if out.Run.Prefix != "spi" {
		t.Errorf("Run.Prefix = %q, want spi", out.Run.Prefix)
	}
	if out.Run.BeadID != "spi-cob04b" {
		t.Errorf("Run.BeadID = %q, want spi-cob04b", out.Run.BeadID)
	}
	if out.Run.FormulaStep != inputs.RunStep {
		t.Errorf("Run.FormulaStep = %q, want %q", out.Run.FormulaStep, inputs.RunStep)
	}
	if out.Run.Backend != inputs.Backend {
		t.Errorf("Run.Backend = %q, want %q", out.Run.Backend, inputs.Backend)
	}
	if out.Run.RunID != inputs.RunID {
		t.Errorf("Run.RunID = %q, want %q", out.Run.RunID, inputs.RunID)
	}
	if out.Run.Role != agent.RoleSage {
		t.Errorf("Run.Role = %q, want %q (propagated from cfg.Role)", out.Run.Role, agent.RoleSage)
	}
	if out.Run.WorkspaceKind != WorkspaceKindRepo {
		t.Errorf("Run.WorkspaceKind = %q, want %q (from normalized workspace)", out.Run.WorkspaceKind, WorkspaceKindRepo)
	}
}

// TestPopulateRuntimeContract_RepoURLFallsBackToCfg verifies that when
// inputs.RepoURL is empty the helper falls back to cfg.RepoURL so legacy
// call sites that threaded RepoURL through the SpawnConfig keep working.
func TestPopulateRuntimeContract_RepoURLFallsBackToCfg(t *testing.T) {
	cfg := agent.SpawnConfig{
		Name:    "apprentice-spi-x",
		BeadID:  "spi-x",
		Role:    agent.RoleApprentice,
		RepoURL: "https://fallback.example/spire.git",
	}
	out, err := PopulateRuntimeContract(cfg, RuntimeContractInputs{
		TowerName:   "tower-x",
		HandoffMode: HandoffBorrowed,
		// RepoURL intentionally empty
	})
	if err != nil {
		t.Fatalf("PopulateRuntimeContract: %v", err)
	}
	if out.Identity.RepoURL != "https://fallback.example/spire.git" {
		t.Errorf("Identity.RepoURL = %q, want cfg.RepoURL fallback", out.Identity.RepoURL)
	}
}

// TestPopulateRuntimeContract_BaseBranchFallsBackToCfg verifies the same
// fallback path for BaseBranch.
func TestPopulateRuntimeContract_BaseBranchFallsBackToCfg(t *testing.T) {
	cfg := agent.SpawnConfig{
		Name:       "apprentice-spi-x",
		BeadID:     "spi-x",
		Role:       agent.RoleApprentice,
		RepoBranch: "trunk",
	}
	out, err := PopulateRuntimeContract(cfg, RuntimeContractInputs{
		TowerName:   "tower-x",
		HandoffMode: HandoffBorrowed,
		// BaseBranch intentionally empty
	})
	if err != nil {
		t.Fatalf("PopulateRuntimeContract: %v", err)
	}
	if out.Identity.BaseBranch != "trunk" {
		t.Errorf("Identity.BaseBranch = %q, want cfg.RepoBranch fallback", out.Identity.BaseBranch)
	}
}

// TestPopulateRuntimeContract_PrefixFromCfgRepoPrefix verifies the prefix
// fallback path — when the bead ID doesn't match the "prefix-..." shape,
// cfg.RepoPrefix is used. This preserves the behavior of the old
// withRuntimeContract.
func TestPopulateRuntimeContract_PrefixFromCfgRepoPrefix(t *testing.T) {
	cfg := agent.SpawnConfig{
		Name:       "x",
		BeadID:     "nonstandardID",
		Role:       agent.RoleApprentice,
		RepoPrefix: "web",
	}
	out, err := PopulateRuntimeContract(cfg, RuntimeContractInputs{
		TowerName:   "tower-x",
		HandoffMode: HandoffBorrowed,
	})
	if err != nil {
		t.Fatalf("PopulateRuntimeContract: %v", err)
	}
	if out.Identity.Prefix != "web" {
		t.Errorf("Identity.Prefix = %q, want web (cfg.RepoPrefix fallback)", out.Identity.Prefix)
	}
}

// TestPopulateRuntimeContract_HandoffTransitional_BumpsCounter verifies
// that the transitional deprecation side-effect still fires through the
// package-level helper. Call sites that configure HandoffTransitional
// must see the same observability surface as executor-internal dispatch.
func TestPopulateRuntimeContract_HandoffTransitional_BumpsCounter(t *testing.T) {
	ResetHandoffTransitionalCounters()
	t.Setenv(EnvFailOnTransitionalHandoff, "")

	var logged []string
	logf := func(format string, args ...interface{}) {
		logged = append(logged, format)
	}

	cfg := agent.SpawnConfig{
		Name:   "apprentice-spi-x",
		BeadID: "spi-x",
		Role:   agent.RoleApprentice,
	}
	_, err := PopulateRuntimeContract(cfg, RuntimeContractInputs{
		TowerName:   "tower-x",
		Backend:     "process",
		HandoffMode: HandoffTransitional,
		Log:         logf,
	})
	if err != nil {
		t.Fatalf("PopulateRuntimeContract: %v", err)
	}
	if got := HandoffTransitionalTotal(); got != 1 {
		t.Errorf("transitional counter = %d, want 1", got)
	}
	if len(logged) != 1 {
		t.Fatalf("log lines = %d, want 1", len(logged))
	}
}

// TestPopulateRuntimeContract_FailOnTransitionalGate verifies the CI parity
// gate fires through the package-level helper when
// SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1.
func TestPopulateRuntimeContract_FailOnTransitionalGate(t *testing.T) {
	ResetHandoffTransitionalCounters()
	t.Setenv(EnvFailOnTransitionalHandoff, "1")

	cfg := agent.SpawnConfig{
		Name:   "apprentice-spi-x",
		BeadID: "spi-x",
		Role:   agent.RoleApprentice,
	}
	_, err := PopulateRuntimeContract(cfg, RuntimeContractInputs{
		TowerName:   "tower-x",
		Backend:     "process",
		HandoffMode: HandoffTransitional,
	})
	if err == nil {
		t.Fatalf("want error when SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1, got nil")
	}
}

// TestApprenticeDeliveryHandoff_ExportMatchesInternal verifies the exported
// wrapper returns the same value as the package-internal selector for the
// two well-known tower-transport configurations. This is the contract
// the wizard's review-fix re-engagement path depends on to stay in
// lockstep with the executor's own review-fix dispatch.
func TestApprenticeDeliveryHandoff_ExportMatchesInternal(t *testing.T) {
	cases := []struct {
		name      string
		transport string
		want      HandoffMode
	}{
		{"push_transitional", config.ApprenticeTransportPush, HandoffTransitional},
		{"bundle_canonical", config.ApprenticeTransportBundle, HandoffBundle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tower := &TowerConfig{
				Apprentice: config.ApprenticeConfig{Transport: tc.transport},
			}
			got := ApprenticeDeliveryHandoff(tower)
			if got != tc.want {
				t.Errorf("ApprenticeDeliveryHandoff(%s) = %q, want %q", tc.transport, got, tc.want)
			}
		})
	}

	// nil tower defaults to bundle so tests without tower config stay aligned.
	if got := ApprenticeDeliveryHandoff(nil); got != HandoffBundle {
		t.Errorf("ApprenticeDeliveryHandoff(nil) = %q, want %q", got, HandoffBundle)
	}
}
