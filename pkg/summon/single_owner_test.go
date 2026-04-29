package summon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/awell-health/spire/pkg/wizardregistry/fake"
	"github.com/steveyegge/beads"
)

// TestSpawnWizard_RefusesWhileRecoveryOpen pins the cleric runtime
// (spi-hhkozk) single-owner invariant: the wizard summon path refuses
// when a non-closed recovery bead has caused-by → the candidate source
// bead. Without this, the wizard and cleric could both run on the same
// source simultaneously — the design's hard rule.
func TestSpawnWizard_RefusesWhileRecoveryOpen(t *testing.T) {
	calls := installStubs(t, stubOpts{bead: store.Bead{Status: "in_progress", Type: "task"}})

	prevDeps := GetDependentsWithMetaFunc
	defer func() { GetDependentsWithMetaFunc = prevDeps }()
	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id != "spi-src" {
			return nil, nil
		}
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue: beads.Issue{
					ID:        "spi-rec",
					IssueType: beads.IssueType("recovery"),
					Status:    beads.Status("in_progress"),
				},
				DependencyType: beads.DependencyType(store.DepCausedBy),
			},
		}, nil
	}

	bead := store.Bead{ID: "spi-src", Status: "in_progress", Type: "task"}
	_, err := SpawnWizard(bead, "")
	if err == nil {
		t.Fatal("expected ErrRecoveryInFlight")
	}
	if !errors.Is(err, ErrRecoveryInFlight) {
		t.Fatalf("err = %v, want wraps ErrRecoveryInFlight", err)
	}
	if !strings.Contains(err.Error(), "spi-rec") {
		t.Errorf("error should mention recovery bead ID; got %v", err)
	}
	if len(calls.spawns) != 0 {
		t.Errorf("expected 0 spawns, got %d", len(calls.spawns))
	}
}

// TestSpawnWizard_AllowsWhenRecoveryClosed pins that closed recovery
// beads do NOT block summon — they're historical evidence, not active
// owners.
func TestSpawnWizard_AllowsWhenRecoveryClosed(t *testing.T) {
	calls := installStubs(t, stubOpts{bead: store.Bead{Status: "in_progress", Type: "task"}})

	prevDeps := GetDependentsWithMetaFunc
	defer func() { GetDependentsWithMetaFunc = prevDeps }()
	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue: beads.Issue{
					ID:        "spi-rec",
					IssueType: beads.IssueType("recovery"),
					Status:    beads.Status("closed"),
				},
				DependencyType: beads.DependencyType(store.DepCausedBy),
			},
		}, nil
	}

	bead := store.Bead{ID: "spi-src", Status: "in_progress", Type: "task"}
	_, err := SpawnWizard(bead, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(calls.spawns) != 1 {
		t.Errorf("expected 1 spawn, got %d", len(calls.spawns))
	}
}

// TestSpawnWizard_AllowsWhenNoRecovery pins the no-recovery happy path.
// Used as a regression sentinel — making sure the single-owner check
// doesn't accidentally block normal summon flows.
func TestSpawnWizard_AllowsWhenNoRecovery(t *testing.T) {
	calls := installStubs(t, stubOpts{bead: store.Bead{Status: "in_progress", Type: "task"}})

	prevDeps := GetDependentsWithMetaFunc
	defer func() { GetDependentsWithMetaFunc = prevDeps }()
	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil
	}

	bead := store.Bead{ID: "spi-src", Status: "in_progress", Type: "task"}
	_, err := SpawnWizard(bead, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(calls.spawns) != 1 {
		t.Errorf("expected 1 spawn, got %d", len(calls.spawns))
	}
}

// TestRun_RecoveryGuardLeavesHookedSourceUnchanged pins spi-skfsia
// finding 5: when a hooked source bead has an open recovery, Run must
// surface ErrRecoveryInFlight without touching the bead's status. Pre-
// fix, Run flipped hooked → in_progress before checking guards, so a
// guard rejection left the bead stranded in_progress.
func TestRun_RecoveryGuardLeavesHookedSourceUnchanged(t *testing.T) {
	calls := installStubs(t, stubOpts{bead: store.Bead{Status: "hooked", Type: "task"}})

	prevDeps := GetDependentsWithMetaFunc
	defer func() { GetDependentsWithMetaFunc = prevDeps }()
	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue: beads.Issue{
					ID:        "spi-rec",
					IssueType: beads.IssueType("recovery"),
					Status:    beads.Status("in_progress"),
				},
				DependencyType: beads.DependencyType(store.DepCausedBy),
			},
		}, nil
	}

	_, err := Run("spi-src", "")
	if err == nil {
		t.Fatal("expected ErrRecoveryInFlight")
	}
	if !errors.Is(err, ErrRecoveryInFlight) {
		t.Fatalf("err = %v, want wraps ErrRecoveryInFlight", err)
	}
	if len(calls.statusUpdates) != 0 {
		t.Errorf("statusUpdates = %+v, want empty (guard must run before status mutation)",
			calls.statusUpdates)
	}
	if len(calls.spawns) != 0 {
		t.Errorf("expected 0 spawns, got %d", len(calls.spawns))
	}
}

// TestRun_DuplicateLiveWizardLeavesHookedSourceUnchanged pins spi-skfsia
// finding 5: when a duplicate live wizard already owns the source, Run
// must surface ErrAlreadyRunning without flipping the bead's status.
// Pre-fix, the duplicate check ran after the hooked → in_progress
// transition, leaving the bead in_progress with no spawn happening.
func TestRun_DuplicateLiveWizardLeavesHookedSourceUnchanged(t *testing.T) {
	reg := fake.New()
	// Pre-populate the registry with a live wizard for spi-src so the
	// duplicate guard fires.
	reg.Upsert(context.Background(), wizardregistry.Wizard{
		ID:        "wizard-spi-src",
		Mode:      wizardregistry.ModeLocal,
		PID:       1234,
		BeadID:    "spi-src",
		StartedAt: time.Now().UTC(),
	})
	reg.SetAlive("wizard-spi-src", true)
	calls := installStubs(t, stubOpts{
		bead:     store.Bead{Status: "hooked", Type: "task"},
		registry: reg,
	})

	_, err := Run("spi-src", "")
	if err == nil {
		t.Fatal("expected ErrAlreadyRunning")
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err = %v, want wraps ErrAlreadyRunning", err)
	}
	if len(calls.statusUpdates) != 0 {
		t.Errorf("statusUpdates = %+v, want empty (guard must run before status mutation)",
			calls.statusUpdates)
	}
	if len(calls.spawns) != 0 {
		t.Errorf("expected 0 spawns, got %d", len(calls.spawns))
	}
}
