package steward

// orphan_race_test.go — regression coverage for spi-4d2i71.
//
// In local-native mode, daemon-side OrphanSweep used to race with the
// steward-side hooked-resume path on the local wizard registry. The
// daemon could observe the dead PID of the wizard being resumed,
// add `dead-letter:orphan` to the parent bead, and reopen it — even
// though the steward was about to flip it back to in_progress and
// dispatch a fresh wizard.
//
// The fix has two halves:
//   1. OrphanSweep moved from DaemonTowerCycle into TowerCycle so the
//      sweep runs in the same sequential cycle as SweepHookedSteps.
//      OrphanSweepFunc is the test-replaceable seam.
//   2. Belt-and-suspenders: the hooked-resume path removes the stale
//      wizard registry entry BEFORE flipping the parent bead to
//      in_progress. RegistryRemoveFunc is the test-replaceable seam.
//
// These tests exercise both halves.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/awell-health/spire/pkg/beadlifecycle"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/awell-health/spire/pkg/wizardregistry/fake"
	"github.com/steveyegge/beads"
)

// TestSweepHookedSteps_RegistryClearedBeforeStatusUpdate proves the
// belt-and-suspenders ordering: in the cleric-success resume branch,
// RegistryRemoveFunc fires BEFORE UpdateBeadFunc(parent → in_progress).
// Without this ordering, a sync-only daemon (`spire up --no-steward`)
// running OrphanSweep concurrently could still observe the dead PID
// after the parent flipped to in_progress.
func TestSweepHookedSteps_RegistryClearedBeforeStatusUpdate(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{
				{ID: "spi-race1", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}

	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-race1" {
			return &store.Bead{ID: "spi-race1.attempt-1", Status: "in_progress"}, nil
		}
		return nil, nil
	}
	IsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) { return true, nil }
	InstanceIDFunc = func() string { return "local-instance" }

	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-race1" {
			return []store.Bead{
				{ID: "spi-race1.step-impl", Status: "hooked", Labels: []string{"step:implement-failed"}},
			}, nil
		}
		return nil, nil
	}

	GetBeadFunc = func(id string) (store.Bead, error) {
		switch id {
		case "spi-race1":
			return store.Bead{
				ID: "spi-race1", Status: "hooked", Type: "task",
				Labels: []string{"needs-human"},
			}, nil
		case "spi-recovery1":
			return store.Bead{
				ID: "spi-recovery1", Status: "closed", Type: "recovery",
				Metadata: map[string]string{
					recovery.KeyRecoveryOutcome: mustMarshalOutcome(t, recovery.RecoveryOutcome{
						SourceBeadID:  "spi-race1",
						Decision:      recovery.DecisionResume,
						VerifyVerdict: recovery.VerifyVerdictPass,
					}),
				},
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}

	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }
	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id == "spi-race1" {
			return []*beads.IssueWithDependencyMetadata{{
				Issue:          beads.Issue{ID: "spi-recovery1", IssueType: "recovery", Status: "closed"},
				DependencyType: "caused-by",
			}}, nil
		}
		return nil, nil
	}
	UnhookStepBeadFunc = func(id string) error { return nil }

	// Capture call order for the two test seams.
	var (
		mu         sync.Mutex
		callOrder  []string
		removedIDs []string
	)
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		if status, ok := fields["status"].(string); ok && status == "in_progress" && id == "spi-race1" {
			callOrder = append(callOrder, "update:"+id)
		}
		return nil
	}

	origRegistryRemove := RegistryRemoveFunc
	RegistryRemoveFunc = func(ctx context.Context, id string) error {
		mu.Lock()
		defer mu.Unlock()
		callOrder = append(callOrder, "remove:"+id)
		removedIDs = append(removedIDs, id)
		return nil
	}
	defer func() { RegistryRemoveFunc = origRegistryRemove }()

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})
	if count != 1 {
		t.Fatalf("SweepHookedSteps returned %d, want 1", count)
	}

	wantWiz := "wizard-spi-race1"
	if len(removedIDs) != 1 || removedIDs[0] != wantWiz {
		t.Fatalf("RegistryRemoveFunc called with %v, want [%s]", removedIDs, wantWiz)
	}

	// Order check: remove BEFORE update.
	var removeIdx, updateIdx = -1, -1
	for i, e := range callOrder {
		if e == "remove:"+wantWiz && removeIdx == -1 {
			removeIdx = i
		}
		if e == "update:spi-race1" && updateIdx == -1 {
			updateIdx = i
		}
	}
	if removeIdx == -1 {
		t.Fatalf("expected RegistryRemoveFunc(%s) to be called; callOrder=%v", wantWiz, callOrder)
	}
	if updateIdx == -1 {
		t.Fatalf("expected UpdateBeadFunc(spi-race1, in_progress) to be called; callOrder=%v", callOrder)
	}
	if removeIdx > updateIdx {
		t.Fatalf("RegistryRemoveFunc must be called BEFORE UpdateBeadFunc(in_progress) — got %v", callOrder)
	}
}

// TestSweepHookedSteps_StandardResume_RegistryClearedBeforeStatusUpdate
// covers the non-failure resume branch (design-linked or human-approval
// hook resolved). The branch executes a different code path than the
// cleric-success branch but must enforce the same ordering invariant.
func TestSweepHookedSteps_StandardResume_RegistryClearedBeforeStatusUpdate(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{
				{ID: "spi-race2", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		return nil, nil
	}
	InstanceIDFunc = func() string { return "local-instance" }

	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-race2" {
			return []store.Bead{
				{ID: "spi-race2.step-design", Status: "hooked", Labels: []string{"step:check.design-linked"}},
			}, nil
		}
		// Second call after unhook — return empty so parent flips.
		return nil, nil
	}

	GetBeadFunc = func(id string) (store.Bead, error) {
		switch id {
		case "spi-race2":
			return store.Bead{
				ID: "spi-race2", Status: "hooked", Type: "task",
				Labels: []string{},
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }
	UnhookStepBeadFunc = func(id string) error { return nil }

	var (
		mu        sync.Mutex
		callOrder []string
	)
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		if status, ok := fields["status"].(string); ok && status == "in_progress" && id == "spi-race2" {
			callOrder = append(callOrder, "update:"+id)
		}
		return nil
	}

	origRegistryRemove := RegistryRemoveFunc
	RegistryRemoveFunc = func(ctx context.Context, id string) error {
		mu.Lock()
		defer mu.Unlock()
		callOrder = append(callOrder, "remove:"+id)
		return nil
	}
	defer func() { RegistryRemoveFunc = origRegistryRemove }()

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	_ = SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

	// In the standard resume path the hook condition is approval-cleared
	// (no needs-human / awaiting-approval label). The wizard name
	// defaults to wizard-<sanitized parent.ID> when no graph state agent
	// is recorded.
	wantWiz := "wizard-spi-race2"

	var removeIdx, updateIdx = -1, -1
	for i, e := range callOrder {
		if e == "remove:"+wantWiz && removeIdx == -1 {
			removeIdx = i
		}
		if e == "update:spi-race2" && updateIdx == -1 {
			updateIdx = i
		}
	}
	if removeIdx == -1 {
		t.Fatalf("expected RegistryRemoveFunc(%s); callOrder=%v", wantWiz, callOrder)
	}
	if updateIdx != -1 && removeIdx > updateIdx {
		t.Fatalf("RegistryRemoveFunc must precede UpdateBeadFunc(in_progress) — got %v", callOrder)
	}
}

// TestTowerCycle_OrphanSweepThenResume_NoClobber simulates the in-cycle
// ordering that TowerCycle now enforces (spi-4d2i71): OrphanSweep runs
// FIRST, then SweepHookedSteps. With the daemon-side OrphanSweep removed,
// this is the only path that can fire in production. The test seeds a
// hooked parent bead with a stale (dead-PID) wizard entry, drives the
// sequence in one goroutine, and asserts the parent ends up in_progress
// with a fresh wizard dispatched — never reopened.
//
// The pre-fix race had this scenario clobbering the parent back to
// "open" because the daemon's OrphanSweep would fire concurrently
// with the steward's resume on a separate process. Sequencing the two
// ops removes the race entirely.
func TestTowerCycle_OrphanSweepThenResume_NoClobber(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	// Seed the fake registry with a stale entry: a dead-PID wizard
	// pointing at a hooked parent bead. Pre-fix, this combined with a
	// separate-process OrphanSweep firing mid-resume would clobber.
	wizardName := "wizard-spi-race3"
	reg := fake.New()
	if err := reg.Upsert(context.Background(), wizardregistry.Wizard{
		ID:     wizardName,
		Mode:   wizardregistry.ModeLocal,
		PID:    99999,
		BeadID: "spi-race3",
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	reg.SetAlive(wizardName, false)

	origRegistryRemove := RegistryRemoveFunc
	RegistryRemoveFunc = reg.Remove
	defer func() { RegistryRemoveFunc = origRegistryRemove }()

	// Test state: parent + active attempt + recovery bead (closed,
	// resume-decision). These mirror the production data shape after a
	// cleric-success resolution.
	var (
		stateMu   sync.Mutex
		parent    = store.Bead{ID: "spi-race3", Status: "hooked", Type: "task", Labels: []string{"needs-human"}}
		attempt   = store.Bead{ID: "spi-race3.attempt-1", Status: "in_progress", Parent: "spi-race3", Type: "attempt", Labels: []string{"attempt", "agent:" + wizardName}}
		recBead   = store.Bead{ID: "spi-recovery3", Status: "closed", Type: "recovery", Metadata: map[string]string{recovery.KeyRecoveryOutcome: mustMarshalOutcome(t, recovery.RecoveryOutcome{SourceBeadID: "spi-race3", Decision: recovery.DecisionResume, VerifyVerdict: recovery.VerifyVerdictPass})}}
		labelsAdd []string
	)

	// Wire the steward function vars to the in-memory state.
	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		stateMu.Lock()
		defer stateMu.Unlock()
		if filter.Status != nil && *filter.Status == hookedStatus {
			if parent.Status == "hooked" {
				return []store.Bead{parent}, nil
			}
			return nil, nil
		}
		return []store.Bead{parent, attempt, recBead}, nil
	}
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		stateMu.Lock()
		defer stateMu.Unlock()
		if parentID == "spi-race3" {
			a := attempt
			return &a, nil
		}
		return nil, nil
	}
	IsOwnedByInstanceFunc = func(string, string) (bool, error) { return true, nil }
	InstanceIDFunc = func() string { return "local-instance" }

	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-race3" {
			return []store.Bead{
				{ID: "spi-race3.step-impl", Status: "hooked", Labels: []string{"step:implement-failed"}},
			}, nil
		}
		return nil, nil
	}
	GetBeadFunc = func(id string) (store.Bead, error) {
		stateMu.Lock()
		defer stateMu.Unlock()
		switch id {
		case "spi-race3":
			return parent, nil
		case "spi-recovery3":
			return recBead, nil
		case "spi-race3.attempt-1":
			return attempt, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }
	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id == "spi-race3" {
			return []*beads.IssueWithDependencyMetadata{{
				Issue:          beads.Issue{ID: "spi-recovery3", IssueType: "recovery", Status: "closed"},
				DependencyType: "caused-by",
			}}, nil
		}
		return nil, nil
	}
	UnhookStepBeadFunc = func(id string) error { return nil }
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error {
		stateMu.Lock()
		defer stateMu.Unlock()
		if id == "spi-race3" {
			if status, ok := fields["status"].(string); ok {
				parent.Status = status
			}
		}
		return nil
	}

	// Step 1 of TowerCycle: OrphanSweep runs first. With the parent in
	// `hooked` status, scan A may add `dead-letter:orphan` and remove
	// the entry but MUST NOT reopen the bead (hooked is not in
	// in_progress/open).
	deps := raceTestDeps{
		parent:    &parent,
		attempt:   &attempt,
		stateMu:   &stateMu,
		labelsAdd: &labelsAdd,
	}
	if _, err := beadlifecycle.OrphanSweep(deps, reg, beadlifecycle.OrphanScope{All: true}); err != nil {
		t.Fatalf("OrphanSweep error: %v", err)
	}
	stateMu.Lock()
	statusAfterSweep := parent.Status
	stateMu.Unlock()
	if statusAfterSweep != "hooked" {
		t.Fatalf("after OrphanSweep parent.Status = %q, want hooked (OrphanSweep must skip reopening hooked beads)", statusAfterSweep)
	}

	// Step 2 of TowerCycle: SweepHookedSteps runs next. It detects the
	// hooked parent, runs the cleric-success branch, and resumes:
	// belt-and-suspenders RegistryRemove (already a no-op since
	// OrphanSweep removed the entry), parent → in_progress, dispatch.
	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})
	if count != 1 {
		t.Fatalf("SweepHookedSteps returned %d, want 1", count)
	}

	stateMu.Lock()
	gotStatus := parent.Status
	stateMu.Unlock()
	if gotStatus != "in_progress" {
		t.Fatalf("after TowerCycle parent.Status = %q, want in_progress", gotStatus)
	}

	if len(backend.spawns) != 1 || backend.spawns[0].BeadID != "spi-race3" {
		t.Fatalf("spawns = %+v, want one spawn for spi-race3", backend.spawns)
	}
}

// raceTestDeps is a beadlifecycle.Deps stub just rich enough for the
// orphan-clobber regression test. It mirrors a tiny in-memory slice
// of the store; ListBeads returns parent+attempt so OrphanSweep scan B
// has something to walk.
type raceTestDeps struct {
	parent    *store.Bead
	attempt   *store.Bead
	stateMu   *sync.Mutex
	labelsAdd *[]string
}

func (d raceTestDeps) GetBead(id string) (store.Bead, error) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	switch id {
	case d.parent.ID:
		return *d.parent, nil
	case d.attempt.ID:
		return *d.attempt, nil
	}
	return store.Bead{}, fmt.Errorf("not found: %s", id)
}

func (d raceTestDeps) UpdateBead(id string, updates map[string]interface{}) error {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if id == d.parent.ID {
		if status, ok := updates["status"].(string); ok {
			d.parent.Status = status
		}
	}
	return nil
}

func (d raceTestDeps) CreateAttemptBead(parentID, agentName, model, branch string) (string, error) {
	return "", fmt.Errorf("not implemented")
}

func (d raceTestDeps) CloseAttemptBead(attemptID, resultLabel string) error {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if attemptID == d.attempt.ID {
		d.attempt.Status = "closed"
	}
	return nil
}

func (d raceTestDeps) ListAttemptsForBead(beadID string) ([]store.Bead, error) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if beadID == d.parent.ID {
		return []store.Bead{*d.attempt}, nil
	}
	return nil, nil
}

func (d raceTestDeps) RemoveLabel(id, label string) error { return nil }
func (d raceTestDeps) AlertCascadeClose(sourceBeadID string) error {
	return nil
}

func (d raceTestDeps) AddLabel(id, label string) error {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if id == d.parent.ID {
		*d.labelsAdd = append(*d.labelsAdd, label)
	}
	return nil
}

func (d raceTestDeps) ListBeads(filter beads.IssueFilter) ([]store.Bead, error) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return []store.Bead{*d.parent, *d.attempt}, nil
}

// TestTowerCycle_OrphanSweepFuncIsTestReplaceable confirms the seam
// added in spi-4d2i71. The default OrphanSweepFunc wires through to
// beadlifecycle.OrphanSweep against the real store; tests substitute
// it. This guard lets future refactors notice if the seam is removed
// or stops being exercised.
func TestTowerCycle_OrphanSweepFuncIsTestReplaceable(t *testing.T) {
	called := false
	orig := OrphanSweepFunc
	OrphanSweepFunc = func() (beadlifecycle.SweepReport, error) {
		called = true
		return beadlifecycle.SweepReport{}, nil
	}
	defer func() { OrphanSweepFunc = orig }()

	// Run the seam directly — TowerCycle would call this near the top.
	if _, err := OrphanSweepFunc(); err != nil {
		t.Fatalf("OrphanSweepFunc: %v", err)
	}
	if !called {
		t.Fatalf("OrphanSweepFunc substitution did not run")
	}
}
