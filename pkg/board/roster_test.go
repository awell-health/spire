package board

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/config"
)

// TestLiveRoster_DispatchByMode pins the deployment-mode switch in
// LiveRoster: each mode routes to a distinct source / typed error.
// The legacy cascade (k8s → local → beads) is gone; modes never bleed
// into each other (spi-rx6bf6).
func TestLiveRoster_DispatchByMode(t *testing.T) {
	t.Run("local-native consults LoadWizardRegistry deps", func(t *testing.T) {
		called := false
		deps := RosterDeps{
			LoadWizardRegistry: func() ([]LocalAgent, error) {
				called = true
				return nil, nil
			},
			CleanDeadWizards: func(a []LocalAgent) []LocalAgent { return a },
			ProcessAlive:     func(int) bool { return true },
		}
		_, err := LiveRoster(context.Background(), config.DeploymentModeLocalNative, time.Minute, deps)
		if err != nil {
			t.Fatalf("local-native returned error: %v", err)
		}
		if !called {
			t.Error("LoadWizardRegistry was not called for local-native mode")
		}
	})

	t.Run("local-native surfaces registry read errors", func(t *testing.T) {
		wantErr := errors.New("transient parse failure")
		deps := RosterDeps{
			LoadWizardRegistry: func() ([]LocalAgent, error) { return nil, wantErr },
			CleanDeadWizards:   func(a []LocalAgent) []LocalAgent { return a },
			ProcessAlive:       func(int) bool { return true },
		}
		_, err := LiveRoster(context.Background(), config.DeploymentModeLocalNative, time.Minute, deps)
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want wrapping %v", err, wantErr)
		}
	})

	t.Run("attached-reserved returns typed not-implemented", func(t *testing.T) {
		deps := RosterDeps{
			LoadWizardRegistry: func() ([]LocalAgent, error) {
				t.Fatal("LoadWizardRegistry must NOT be called for attached-reserved")
				return nil, nil
			},
			CleanDeadWizards: func(a []LocalAgent) []LocalAgent { return a },
			ProcessAlive:     func(int) bool { return true },
		}
		_, err := LiveRoster(context.Background(), config.DeploymentModeAttachedReserved, time.Minute, deps)
		if !errors.Is(err, ErrAttachedRosterNotImplemented) {
			t.Fatalf("err = %v, want ErrAttachedRosterNotImplemented", err)
		}
	})

	t.Run("unknown mode returns named error", func(t *testing.T) {
		deps := RosterDeps{
			LoadWizardRegistry: func() ([]LocalAgent, error) {
				t.Fatal("LoadWizardRegistry must NOT be called for unknown mode")
				return nil, nil
			},
			CleanDeadWizards: func(a []LocalAgent) []LocalAgent { return a },
			ProcessAlive:     func(int) bool { return true },
		}
		_, err := LiveRoster(context.Background(), config.DeploymentMode("nope"), time.Minute, deps)
		if err == nil {
			t.Fatal("expected error for unknown mode")
		}
		if !strings.Contains(err.Error(), "nope") {
			t.Errorf("error = %q, want it to contain mode name %q", err.Error(), "nope")
		}
	})
}

// TestLiveRoster_LocalNative_NoFallbackToLegacyBeads is the regression
// pin for spi-rx6bf6: an empty wizards.json must NOT silently surface
// LegacyAgentRegistrationBeads as a substitute. Empty is empty.
func TestLiveRoster_LocalNative_NoFallbackToLegacyBeads(t *testing.T) {
	deps := RosterDeps{
		LoadWizardRegistry: func() ([]LocalAgent, error) { return nil, nil },
		CleanDeadWizards:   func(a []LocalAgent) []LocalAgent { return a },
		ProcessAlive:       func(int) bool { return true },
	}
	got, err := LiveRoster(context.Background(), config.DeploymentModeLocalNative, time.Minute, deps)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty registry must produce empty roster, got %d agents (%+v)", len(got), got)
	}
}

// TestRosterFromLocalWizards_PropagatesRegistryError verifies the
// migration off agent.LoadRegistry's error-swallowing variant
// (spi-rx6bf6): a transient JSON parse / FS error reaches the caller
// instead of silently degrading to "no wizards".
func TestRosterFromLocalWizards_PropagatesRegistryError(t *testing.T) {
	wantErr := errors.New("read-only filesystem")
	deps := RosterDeps{
		LoadWizardRegistry: func() ([]LocalAgent, error) { return nil, wantErr },
		CleanDeadWizards:   func(a []LocalAgent) []LocalAgent { return a },
		ProcessAlive:       func(int) bool { return true },
	}
	_, err := RosterFromLocalWizards(time.Minute, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", err, wantErr)
	}
}

// TestRosterFromLocalWizards_StampsArchmageFromDeps verifies the new
// per-archmage origin field on RosterAgent: when ResolveArchmage is
// supplied, each row carries the archmage attribution returned for its
// LocalAgent. Missing closure leaves the field empty (no attribution),
// preserving backward compatibility with callers that don't supply it.
func TestRosterFromLocalWizards_StampsArchmageFromDeps(t *testing.T) {
	in := []LocalAgent{
		{Name: "wizard-spi-aaa", PID: 1001, BeadID: "spi-aaa", StartedAt: "2026-04-26T10:00:00Z"},
		{Name: "wizard-spi-bbb", PID: 1002, BeadID: "spi-bbb", StartedAt: "2026-04-26T10:00:00Z"},
	}
	deps := RosterDeps{
		LoadWizardRegistry: func() ([]LocalAgent, error) { return in, nil },
		CleanDeadWizards:   func(a []LocalAgent) []LocalAgent { return a },
		ProcessAlive:       func(int) bool { return true },
		ResolveArchmage:    func(a LocalAgent) string { return "Bob" },
	}
	got, err := RosterFromLocalWizards(time.Minute, deps)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (got=%+v)", len(got), got)
	}
	for _, a := range got {
		if a.Archmage != "Bob" {
			t.Errorf("agent %s archmage = %q, want Bob", a.Name, a.Archmage)
		}
	}

	// No closure: Archmage stays empty, surfaced in JSON via the
	// `archmage,omitempty` tag.
	deps2 := RosterDeps{
		LoadWizardRegistry: func() ([]LocalAgent, error) { return in, nil },
		CleanDeadWizards:   func(a []LocalAgent) []LocalAgent { return a },
		ProcessAlive:       func(int) bool { return true },
	}
	got2, err := RosterFromLocalWizards(time.Minute, deps2)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, a := range got2 {
		if a.Archmage != "" {
			t.Errorf("agent %s archmage = %q, want empty when no resolver", a.Name, a.Archmage)
		}
	}
}
