package executor

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
)

// capturedLog captures formatted log lines for assertion. Safe for
// concurrent use — the wave-dispatch path fans out goroutines.
type capturedLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturedLog) log(format string, args ...interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *capturedLog) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// TestRecordHandoffSelection covers the four outcomes required by spi-yif4t:
//
//	a) same-owner transitions → HandoffBorrowed — no log, no counter
//	b) cross-owner default → HandoffBundle — no log, no counter
//	c) legacy push → HandoffTransitional — deprecation log AND counter bump
//	d) SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1 promotes (c) to a hard error
func TestRecordHandoffSelection(t *testing.T) {
	cases := []struct {
		name         string
		mode         HandoffMode
		failOnEnv    string // when non-empty, set SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF
		wantErr      bool
		wantCounter  uint64 // spire_handoff_transitional_total after the call
		wantLogMatch string // substring expected in the log; empty = no log
	}{
		{
			name:        "same_owner_borrowed_silent",
			mode:        HandoffBorrowed,
			wantErr:     false,
			wantCounter: 0,
		},
		{
			name:        "cross_owner_bundle_silent",
			mode:        HandoffBundle,
			wantErr:     false,
			wantCounter: 0,
		},
		{
			name:         "transitional_logs_and_counts",
			mode:         HandoffTransitional,
			wantErr:      false,
			wantCounter:  1,
			wantLogMatch: DeprecationMessageTransitional,
		},
		{
			name:         "transitional_with_env_gate_errors",
			mode:         HandoffTransitional,
			failOnEnv:    "1",
			wantErr:      true,
			wantCounter:  1, // counter still bumps — the deprecation happened
			wantLogMatch: DeprecationMessageTransitional,
		},
		{
			name:        "terminal_none_silent",
			mode:        HandoffNone,
			wantErr:     false,
			wantCounter: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ResetHandoffTransitionalCounters()
			if tc.failOnEnv != "" {
				t.Setenv(EnvFailOnTransitionalHandoff, tc.failOnEnv)
			} else {
				t.Setenv(EnvFailOnTransitionalHandoff, "")
			}

			cl := &capturedLog{}
			run := RunContext{
				TowerName: "tower-test",
				Prefix:    "spi",
				BeadID:    "spi-yif4t",
				AttemptID: "att-1",
				RunID:     "run-1",
				Role:      agent.RoleApprentice,
				Backend:   "process",
			}
			err := recordHandoffSelection(cl.log, tc.mode, run)

			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			got := HandoffTransitionalTotal()
			if got != tc.wantCounter {
				t.Errorf("counter = %d, want %d", got, tc.wantCounter)
			}

			// Per-label bucketing sanity check: when we expect a bump, the
			// per-label count must match the total (single-label test).
			if tc.wantCounter > 0 {
				perLabel := HandoffTransitionalCount(run.TowerName, run.Prefix, string(run.Role), run.Backend)
				if perLabel != tc.wantCounter {
					t.Errorf("per-label counter = %d, want %d", perLabel, tc.wantCounter)
				}
			}

			if tc.wantLogMatch == "" {
				for _, line := range cl.all() {
					if strings.Contains(line, DeprecationMessageTransitional) {
						t.Errorf("unexpected deprecation log for mode %q: %s", tc.mode, line)
					}
				}
			} else {
				found := false
				for _, line := range cl.all() {
					if strings.Contains(line, tc.wantLogMatch) {
						found = true
						// Assert the identity fields we DO put on the log.
						// The counter labels are (tower, prefix, role, backend);
						// the log itself carries the full identity including
						// the high-cardinality bead/attempt/run IDs.
						if !strings.Contains(line, "tower=tower-test") {
							t.Errorf("log missing tower label: %s", line)
						}
						if !strings.Contains(line, "prefix=spi") {
							t.Errorf("log missing prefix label: %s", line)
						}
						if !strings.Contains(line, "bead_id=spi-yif4t") {
							t.Errorf("log missing bead_id label: %s", line)
						}
						if !strings.Contains(line, "role=apprentice") {
							t.Errorf("log missing role label: %s", line)
						}
						break
					}
				}
				if !found {
					t.Errorf("expected deprecation log, got lines: %v", cl.all())
				}
			}
		})
	}
}

// TestApprenticeDeliveryHandoff verifies the transport → handoff-mode
// mapping that every cross-owner dispatch site uses.
func TestApprenticeDeliveryHandoff(t *testing.T) {
	cases := []struct {
		name      string
		tower     *TowerConfig
		wantMode  HandoffMode
	}{
		{
			name:     "nil_tower_defaults_to_bundle",
			tower:    nil,
			wantMode: HandoffBundle,
		},
		{
			name: "push_transport_is_transitional",
			tower: &TowerConfig{
				Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportPush},
			},
			wantMode: HandoffTransitional,
		},
		{
			name: "bundle_transport_is_bundle",
			tower: &TowerConfig{
				Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportBundle},
			},
			wantMode: HandoffBundle,
		},
		{
			name: "empty_transport_defaults_to_bundle",
			tower: &TowerConfig{
				Apprentice: config.ApprenticeConfig{Transport: ""},
			},
			wantMode: HandoffBundle,
		},
		{
			name: "unknown_transport_falls_back_to_bundle",
			tower: &TowerConfig{
				Apprentice: config.ApprenticeConfig{Transport: "warp-drive"},
			},
			wantMode: HandoffBundle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := apprenticeDeliveryHandoff(tc.tower)
			if got != tc.wantMode {
				t.Errorf("mode = %q, want %q", got, tc.wantMode)
			}
		})
	}
}

// TestResolveApprenticeHandoff verifies the executor's helper picks up the
// tower config from Deps and translates it correctly. Missing deps or an
// errored tower lookup falls back to bundle (wizard-side validator surfaces
// misconfiguration when it tries to deliver).
func TestResolveApprenticeHandoff(t *testing.T) {
	cases := []struct {
		name     string
		deps     *Deps
		wantMode HandoffMode
	}{
		{
			name:     "nil_deps_defaults_to_bundle",
			deps:     nil,
			wantMode: HandoffBundle,
		},
		{
			name:     "missing_accessor_defaults_to_bundle",
			deps:     &Deps{},
			wantMode: HandoffBundle,
		},
		{
			name: "accessor_error_defaults_to_bundle",
			deps: &Deps{
				ActiveTowerConfig: func() (*TowerConfig, error) {
					return nil, errFake{}
				},
			},
			wantMode: HandoffBundle,
		},
		{
			name: "push_tower_resolves_transitional",
			deps: &Deps{
				ActiveTowerConfig: func() (*TowerConfig, error) {
					return &TowerConfig{
						Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportPush},
					}, nil
				},
			},
			wantMode: HandoffTransitional,
		},
		{
			name: "bundle_tower_resolves_bundle",
			deps: &Deps{
				ActiveTowerConfig: func() (*TowerConfig, error) {
					return &TowerConfig{
						Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportBundle},
					}, nil
				},
			},
			wantMode: HandoffBundle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewForTest("spi-test", "wizard-test", nil, tc.deps)
			got := e.resolveApprenticeHandoff()
			if got != tc.wantMode {
				t.Errorf("mode = %q, want %q", got, tc.wantMode)
			}
		})
	}
}

// TestWithRuntimeContract_HandoffModePopulates verifies that withRuntimeContract
// stamps the explicit mode onto cfg.Run.HandoffMode and exercises the
// deprecation-log + counter side-effects via the shared recordHandoffSelection
// choke point.
func TestWithRuntimeContract_HandoffModePopulates(t *testing.T) {
	// Clear the env gate up front; the parity lane's transitional-gate job
	// runs this package under SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1, and the
	// ungated sub-case below must see the gate off before we flip it on.
	t.Setenv(EnvFailOnTransitionalHandoff, "")
	ResetHandoffTransitionalCounters()

	e := NewForTest("spi-target", "wizard-test", nil, &Deps{})
	cl := &capturedLog{}
	e.log = cl.log

	cfg := agent.SpawnConfig{
		BeadID: "spi-target",
		Role:   agent.RoleApprentice,
	}

	out, err := e.withRuntimeContract(cfg, "tower-a", "/tmp/repo", "main", "implement", "", nil, HandoffBundle)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Run.HandoffMode != HandoffBundle {
		t.Errorf("HandoffMode = %q, want HandoffBundle", out.Run.HandoffMode)
	}
	if HandoffTransitionalTotal() != 0 {
		t.Errorf("counter should stay zero for bundle; got %d", HandoffTransitionalTotal())
	}

	out, err = e.withRuntimeContract(cfg, "tower-a", "/tmp/repo", "main", "implement", "", nil, HandoffTransitional)
	if err != nil {
		t.Fatalf("unexpected error without env gate: %v", err)
	}
	if out.Run.HandoffMode != HandoffTransitional {
		t.Errorf("HandoffMode = %q, want HandoffTransitional", out.Run.HandoffMode)
	}
	if HandoffTransitionalTotal() != 1 {
		t.Errorf("counter = %d, want 1", HandoffTransitionalTotal())
	}

	// With the env gate ON, the same call must return an error.
	t.Setenv(EnvFailOnTransitionalHandoff, "1")
	_, err = e.withRuntimeContract(cfg, "tower-a", "/tmp/repo", "main", "implement", "", nil, HandoffTransitional)
	if err == nil {
		t.Fatal("expected error with SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1, got nil")
	}
	if !strings.Contains(err.Error(), EnvFailOnTransitionalHandoff) {
		t.Errorf("error should name the env var, got: %v", err)
	}

	// Counter still bumped on the failing call — the deprecation happened.
	if HandoffTransitionalTotal() != 2 {
		t.Errorf("counter after env-gated call = %d, want 2", HandoffTransitionalTotal())
	}

	// Double check at least one log line contains the deprecation marker.
	found := false
	for _, line := range cl.all() {
		if strings.Contains(line, DeprecationMessageTransitional) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected at least one deprecation log line, got: %v", cl.all())
	}
}

// TestWithRuntimeContract_ResolvesRepoURLFromTower is the spi-x7fus
// regression. Pre-fix, withRuntimeContract copied cfg.RepoURL — which
// every same-bead dispatch site (graph_actions.go:583 and
// action_dispatch.go:236/363/455) left empty — into Identity.RepoURL,
// causing buildSubstratePod to reject k8s spawns with ErrIdentityRequired
// before the worker started. Post-fix, RepoURL is resolved from the
// active tower's LocalBindings map keyed by bead prefix.
//
// To reproduce the pre-fix failure locally:
//
//  1. Revert withRuntimeContract to RepoURL: cfg.RepoURL
//  2. Run: go test ./pkg/executor -run TestWithRuntimeContract_ResolvesRepoURLFromTower
//  3. Observe a failure on the "cfg_repo_url_empty_tower_bound" case:
//     Identity.RepoURL = "", want "https://github.com/example/repo.git"
//  4. Re-apply the fix and the test passes.
func TestWithRuntimeContract_ResolvesRepoURLFromTower(t *testing.T) {
	const wantURL = "https://github.com/example/repo.git"

	cases := []struct {
		name          string
		cfgRepoURL    string
		tower         *TowerConfig
		towerErr      error
		deps          *Deps // when nil, constructed from tower/towerErr
		wantRepoURL   string
	}{
		{
			// The load-bearing case: a same-bead dispatch site that did NOT
			// populate cfg.RepoURL (the production bug) must still produce
			// a SpawnConfig that buildSubstratePod will accept.
			name:       "cfg_repo_url_empty_tower_bound",
			cfgRepoURL: "",
			tower: &TowerConfig{
				LocalBindings: map[string]*config.LocalRepoBinding{
					"spi": {Prefix: "spi", RepoURL: wantURL},
				},
			},
			wantRepoURL: wantURL,
		},
		{
			// Back-compat: a caller that does thread cfg.RepoURL through is
			// still honored when the tower has no binding (e.g. unit tests
			// with a stub Deps that returns a minimal tower).
			name:       "cfg_repo_url_wins_when_tower_has_no_binding",
			cfgRepoURL: wantURL,
			tower:      &TowerConfig{LocalBindings: nil},
			wantRepoURL: wantURL,
		},
		{
			// Tower binding wins over any legacy cfg.RepoURL — the executor
			// state is now authoritative.
			name:       "tower_binding_wins_over_cfg_repo_url",
			cfgRepoURL: "https://github.com/legacy/stale.git",
			tower: &TowerConfig{
				LocalBindings: map[string]*config.LocalRepoBinding{
					"spi": {Prefix: "spi", RepoURL: wantURL},
				},
			},
			wantRepoURL: wantURL,
		},
		{
			// No tower accessor: fall back to cfg.RepoURL if set. Preserves
			// existing test setups that construct an empty Deps{}.
			name:       "no_deps_accessor_falls_back_to_cfg",
			cfgRepoURL: wantURL,
			deps:       &Deps{},
			wantRepoURL: wantURL,
		},
		{
			// Tower accessor errors: fall back to cfg.RepoURL.
			name:       "tower_error_falls_back_to_cfg",
			cfgRepoURL: wantURL,
			towerErr:   errFake{},
			wantRepoURL: wantURL,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ResetHandoffTransitionalCounters()

			deps := tc.deps
			if deps == nil {
				tower := tc.tower
				err := tc.towerErr
				deps = &Deps{
					ActiveTowerConfig: func() (*TowerConfig, error) {
						return tower, err
					},
				}
			}

			e := NewForTest("spi-target", "wizard-test", nil, deps)

			cfg := agent.SpawnConfig{
				BeadID:  "spi-target",
				Role:    agent.RoleApprentice,
				RepoURL: tc.cfgRepoURL,
			}

			out, err := e.withRuntimeContract(cfg, "tower-a", "/tmp/repo", "main", "implement", "", nil, HandoffBorrowed)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if out.Identity.RepoURL != tc.wantRepoURL {
				t.Errorf("Identity.RepoURL = %q, want %q", out.Identity.RepoURL, tc.wantRepoURL)
			}

			// Defense in depth: the downstream buildSubstratePod check is
			// `ident.RepoURL == ""`. Assert the exact condition that the
			// k8s backend will see.
			if tc.wantRepoURL != "" && out.Identity.RepoURL == "" {
				t.Errorf("Identity.RepoURL empty — buildSubstratePod would reject this spawn with ErrIdentityRequired")
			}
		})
	}
}

// TestWithRuntimeContract_ProducesSpawnConfigAcceptedByBuildSubstratePod
// closes the loop on spi-x7fus by asserting that the cfg produced by
// withRuntimeContract without cfg.RepoURL set satisfies every precondition
// that pkg/agent/backend_k8s.go:buildSubstratePod enforces
// (cfg.Workspace != nil, Identity.RepoURL != "", Identity.BaseBranch != "",
// Identity.Prefix != ""). Before the fix, Identity.RepoURL was "" and
// buildSubstratePod returned ErrIdentityRequired; this test encodes that
// exact contract from the executor side so the bug cannot regress.
func TestWithRuntimeContract_ProducesSpawnConfigAcceptedByBuildSubstratePod(t *testing.T) {
	const wantURL = "https://github.com/example/repo.git"

	deps := &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return &TowerConfig{
				LocalBindings: map[string]*config.LocalRepoBinding{
					"spi": {Prefix: "spi", RepoURL: wantURL},
				},
			}, nil
		},
	}
	e := NewForTest("spi-target", "wizard-test", nil, deps)

	// Mimic the same-bead dispatch sites: cfg.RepoURL is intentionally
	// NOT set. Callers only thread repoPath/baseBranch through the
	// withRuntimeContract signature.
	cfg := agent.SpawnConfig{
		Name:   "wizard-test-impl",
		BeadID: "spi-target",
		Role:   agent.RoleApprentice,
	}

	workspace := &WorkspaceHandle{
		Name:       "implement",
		Kind:       WorkspaceKindBorrowedWorktree,
		Branch:     "feat/spi-target",
		BaseBranch: "main",
		Path:       "/workspace/spi",
		Origin:     WorkspaceOriginLocalBind,
		Borrowed:   true,
	}

	out, err := e.withRuntimeContract(cfg, "tower-a", "/tmp/repo", "main", "implement", "implement", workspace, HandoffBorrowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Exact preconditions from pkg/agent/backend_k8s.go:buildSubstratePod.
	if out.Workspace == nil {
		t.Error("cfg.Workspace is nil — buildSubstratePod would reject with ErrWorkspaceRequired")
	}
	if out.Identity.RepoURL == "" {
		t.Error("Identity.RepoURL is empty — buildSubstratePod would reject with ErrIdentityRequired (the spi-x7fus bug)")
	}
	if out.Identity.BaseBranch == "" {
		t.Error("Identity.BaseBranch is empty — buildSubstratePod would reject with ErrIdentityRequired")
	}
	if out.Identity.Prefix == "" {
		t.Error("Identity.Prefix is empty — buildSubstratePod would reject with ErrIdentityRequired")
	}

	// And the positive assertion: the resolved RepoURL came from the tower.
	if out.Identity.RepoURL != wantURL {
		t.Errorf("Identity.RepoURL = %q, want %q (resolved from tower binding)", out.Identity.RepoURL, wantURL)
	}
}

// errFake is a non-nil error sentinel used in ActiveTowerConfig stubs.
type errFake struct{}

func (errFake) Error() string { return "fake" }
