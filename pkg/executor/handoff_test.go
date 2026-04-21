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
						if !strings.Contains(line, "bead=spi-yif4t") {
							t.Errorf("log missing bead label: %s", line)
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

// errFake is a non-nil error sentinel used in ActiveTowerConfig stubs.
type errFake struct{}

func (errFake) Error() string { return "fake" }
