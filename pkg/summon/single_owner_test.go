package summon

import (
	"errors"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/store"
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
