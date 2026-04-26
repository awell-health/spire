package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// stubReadyDeps replaces every ready_test.go-relevant seam with safe stubs:
// - readyGetBeadFunc serves beads from the supplied map
// - readyUpdateBeadFunc is a no-op
// - readyGetChildrenFunc returns no children (no active attempts)
// - readyFindLiveWizardFunc returns no binding
// - readyActiveTowerFunc returns a local-native tower
// - readyStewardRunningFunc returns true (steward present)
//
// Tests that need a different topology (cluster-native, gateway,
// missing steward, active attempt, live wizard) override the relevant
// seam after calling stubReadyDeps. Returns a cleanup func.
func stubReadyDeps(t *testing.T, beads map[string]Bead) func() {
	t.Helper()
	origGet := readyGetBeadFunc
	origUpdate := readyUpdateBeadFunc
	origGetChildren := readyGetChildrenFunc
	origFindLive := readyFindLiveWizardFunc
	origActiveTower := readyActiveTowerFunc
	origStewardRunning := readyStewardRunningFunc

	readyGetBeadFunc = func(id string) (Bead, error) {
		b, ok := beads[id]
		if !ok {
			return Bead{}, fmt.Errorf("not found: %s", id)
		}
		return b, nil
	}
	readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		return nil
	}
	readyGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return nil, nil
	}
	readyFindLiveWizardFunc = func(beadID string) *localWizard {
		return nil
	}
	readyActiveTowerFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "test", DeploymentMode: config.DeploymentModeLocalNative}, nil
	}
	readyStewardRunningFunc = func() bool {
		return true
	}

	return func() {
		readyGetBeadFunc = origGet
		readyUpdateBeadFunc = origUpdate
		readyGetChildrenFunc = origGetChildren
		readyFindLiveWizardFunc = origFindLive
		readyActiveTowerFunc = origActiveTower
		readyStewardRunningFunc = origStewardRunning
	}
}

// TestReady_OpenToReady verifies the happy path: open bead transitions to ready.
func TestReady_OpenToReady(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "open"},
	})
	defer cleanup()

	updated := false
	readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		if id != "spi-test" {
			t.Errorf("expected update for spi-test, got %s", id)
		}
		if updates["status"] != "ready" {
			t.Errorf("expected status=ready, got %v", updates["status"])
		}
		updated = true
		return nil
	}

	err := runReady(nil, []string{"spi-test"})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if !updated {
		t.Fatal("expected storeUpdateBead to be called")
	}
}

// TestReady_AlreadyReady verifies idempotent skip when bead is already ready.
func TestReady_AlreadyReady(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "ready"},
	})
	defer cleanup()

	updated := false
	readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		updated = true
		return nil
	}

	err := runReady(nil, []string{"spi-test"})
	if err != nil {
		t.Fatalf("expected success (skip), got: %v", err)
	}
	if updated {
		t.Fatal("should not have called update for already-ready bead")
	}
}

// TestReady_RejectsInProgress verifies in_progress beads are rejected.
func TestReady_RejectsInProgress(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "in_progress"},
	})
	defer cleanup()

	err := runReady(nil, []string{"spi-test"})
	if err == nil {
		t.Fatal("expected error for in_progress bead")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestReady_RejectsDispatched verifies dispatched beads are rejected
// with the same "work in flight" guidance — the operator should not be
// able to redrive a steward-claimed bead through `ready`. (spi-v1hcrs)
func TestReady_RejectsDispatched(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "dispatched"},
	})
	defer cleanup()

	err := runReady(nil, []string{"spi-test"})
	if err == nil {
		t.Fatal("expected error for dispatched bead")
	}
	if !strings.Contains(err.Error(), "dispatched") {
		t.Errorf("error must mention status=dispatched, got: %v", err)
	}
	if !strings.Contains(err.Error(), "spire roster") {
		t.Errorf("error should suggest spire roster, got: %v", err)
	}
}

// TestReady_RejectsHooked verifies hooked beads are rejected. Hooked is
// the post-dispatch state where the steward has handed off to the
// workflow/recovery surface — re-readying breaks that handoff. (spi-v1hcrs)
func TestReady_RejectsHooked(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "hooked"},
	})
	defer cleanup()

	err := runReady(nil, []string{"spi-test"})
	if err == nil {
		t.Fatal("expected error for hooked bead")
	}
	if !strings.Contains(err.Error(), "hooked") {
		t.Errorf("error must mention status=hooked, got: %v", err)
	}
}

// TestReady_RejectsClosed verifies closed beads are rejected.
func TestReady_RejectsClosed(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "closed"},
	})
	defer cleanup()

	err := runReady(nil, []string{"spi-test"})
	if err == nil {
		t.Fatal("expected error for closed bead")
	}
	if !strings.Contains(err.Error(), "is closed") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestReady_RejectsDeferred verifies deferred beads are rejected with guidance.
func TestReady_RejectsDeferred(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "deferred"},
	})
	defer cleanup()

	err := runReady(nil, []string{"spi-test"})
	if err == nil {
		t.Fatal("expected error for deferred bead")
	}
	if !strings.Contains(err.Error(), "deferred") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "undefer") {
		t.Errorf("expected guidance to undefer, got: %v", err)
	}
}

// TestReady_RejectsUnknownStatus verifies unknown statuses are rejected.
func TestReady_RejectsUnknownStatus(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "bogus"},
	})
	defer cleanup()

	err := runReady(nil, []string{"spi-test"})
	if err == nil {
		t.Fatal("expected error for unknown status")
	}
	if !strings.Contains(err.Error(), "unexpected status") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("expected status value in error, got: %v", err)
	}
}

// TestReady_NotFound verifies missing beads return an error.
func TestReady_NotFound(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{})
	defer cleanup()

	err := runReady(nil, []string{"spi-missing"})
	if err == nil {
		t.Fatal("expected error for missing bead")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestReady_MultipleBeads verifies processing multiple beads in one call.
func TestReady_MultipleBeads(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-a": {ID: "spi-a", Status: "open"},
		"spi-b": {ID: "spi-b", Status: "open"},
	})
	defer cleanup()

	var updatedIDs []string
	readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		updatedIDs = append(updatedIDs, id)
		return nil
	}

	err := runReady(nil, []string{"spi-a", "spi-b"})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(updatedIDs) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updatedIDs))
	}
	if updatedIDs[0] != "spi-a" || updatedIDs[1] != "spi-b" {
		t.Errorf("expected [spi-a, spi-b], got %v", updatedIDs)
	}
}

// TestReady_MultipleMixed verifies that an already-ready bead is skipped
// while open beads still transition, and an error bead stops processing.
func TestReady_MultipleMixed(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-a": {ID: "spi-a", Status: "open"},
		"spi-b": {ID: "spi-b", Status: "ready"},
		"spi-c": {ID: "spi-c", Status: "open"},
	})
	defer cleanup()

	var updatedIDs []string
	readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		updatedIDs = append(updatedIDs, id)
		return nil
	}

	// spi-b is already ready — skipped, but spi-a and spi-c should update.
	err := runReady(nil, []string{"spi-a", "spi-b", "spi-c"})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(updatedIDs) != 2 {
		t.Fatalf("expected 2 updates (skip already-ready), got %d: %v", len(updatedIDs), updatedIDs)
	}
}

// TestReady_UpdateError verifies that a store update error is propagated.
func TestReady_UpdateError(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "open"},
	})
	defer cleanup()

	readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		return fmt.Errorf("database unavailable")
	}

	err := runReady(nil, []string{"spi-test"})
	if err == nil {
		t.Fatal("expected error on update failure")
	}
	if !strings.Contains(err.Error(), "database unavailable") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- New guards (spi-v1hcrs) -----------------------------------------------
//
// These tests pin the fix for the targeted-summon-and-ready ambiguity:
// `spire ready` now refuses to enqueue a bead that already has work in
// flight, and emits mode-aware messaging so the operator can tell whether
// the local steward or the cluster steward/operator owns the dispatch.

// TestReady_RejectsActiveAttempt verifies that an open bead with a
// non-terminal attempt child is refused. The attempt itself indicates a
// wizard or pod is mid-execution; re-queuing through ready would create
// the ambiguous "ready'd while running" state called out in spi-v1hcrs.
func TestReady_RejectsActiveAttempt(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "open"},
	})
	defer cleanup()

	readyGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "att-1", Type: "attempt", Status: "in_progress"},
		}, nil
	}

	err := runReady(nil, []string{"spi-test"})
	if err == nil {
		t.Fatal("expected error for bead with active attempt")
	}
	if !strings.Contains(err.Error(), "active attempt") {
		t.Errorf("error must mention active attempt, got: %v", err)
	}
	if !strings.Contains(err.Error(), "att-1") {
		t.Errorf("error must mention attempt ID att-1, got: %v", err)
	}
}

// TestReady_AllowsTerminalAttempt verifies that closed/merged/done
// attempt children do NOT block ready. A bead with only historical
// attempts is legitimately re-readyable.
func TestReady_AllowsTerminalAttempt(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "open"},
	})
	defer cleanup()

	readyGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "att-old", Type: "attempt", Status: "closed"},
			{ID: "att-old2", Type: "attempt", Status: "merged"},
		}, nil
	}

	if err := runReady(nil, []string{"spi-test"}); err != nil {
		t.Fatalf("terminal attempts must not block ready, got: %v", err)
	}
}

// TestReady_IgnoresNonAttemptChildren verifies that step / message /
// review beads (other dependent types) don't trip the active-attempt
// guard — only Type=="attempt" counts.
func TestReady_IgnoresNonAttemptChildren(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "open"},
	})
	defer cleanup()

	readyGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "step-1", Type: "step", Status: "in_progress"},
			{ID: "msg-1", Type: "message", Status: "open"},
		}, nil
	}

	if err := runReady(nil, []string{"spi-test"}); err != nil {
		t.Fatalf("non-attempt children must not block ready, got: %v", err)
	}
}

// TestReady_RejectsBoundWizard verifies that a live local wizard
// claiming the bead refuses ready, with guidance pointing at
// `spire roster` and `spire dismiss --targets`.
func TestReady_RejectsBoundWizard(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "open"},
	})
	defer cleanup()

	readyFindLiveWizardFunc = func(beadID string) *localWizard {
		return &localWizard{Name: "wizard-spi-test", PID: 12345, BeadID: beadID}
	}

	err := runReady(nil, []string{"spi-test"})
	if err == nil {
		t.Fatal("expected error when a live wizard is bound to the bead")
	}
	if !strings.Contains(err.Error(), "wizard-spi-test") {
		t.Errorf("error must name the bound wizard, got: %v", err)
	}
	if !strings.Contains(err.Error(), "12345") {
		t.Errorf("error should report the wizard PID, got: %v", err)
	}
}

// TestReady_LocalNativeWithoutSteward verifies the local-native guard:
// without a running steward, `ready` would sit unconsumed, so the
// command refuses with `spire up` guidance.
func TestReady_LocalNativeWithoutSteward(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "open"},
	})
	defer cleanup()

	readyStewardRunningFunc = func() bool { return false }

	updated := false
	readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		updated = true
		return nil
	}

	err := runReady(nil, []string{"spi-test"})
	if err == nil {
		t.Fatal("expected error when local steward is not running")
	}
	if !strings.Contains(err.Error(), "spire up") {
		t.Errorf("error must suggest `spire up`, got: %v", err)
	}
	if updated {
		t.Fatal("must not flip status when steward isn't running")
	}
}

// TestReady_LocalNativeWithStewardSucceeds is the sanity success path:
// local-native + running steward emits the local-pickup message.
func TestReady_LocalNativeWithStewardSucceeds(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "open"},
	})
	defer cleanup()

	readyStewardRunningFunc = func() bool { return true }

	updated := false
	readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		updated = true
		return nil
	}

	if err := runReady(nil, []string{"spi-test"}); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if !updated {
		t.Fatal("expected status update")
	}
}

// TestReady_ClusterNativeMessage verifies cluster-native mode does NOT
// consult the local steward (cluster steward owns dispatch) and that the
// success message names the cluster path.
func TestReady_ClusterNativeMessage(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "open"},
	})
	defer cleanup()

	readyActiveTowerFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster-tower", DeploymentMode: config.DeploymentModeClusterNative}, nil
	}
	// Cluster mode must not require a local steward.
	readyStewardRunningFunc = func() bool { return false }

	updated := false
	readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		updated = true
		return nil
	}

	if err := runReady(nil, []string{"spi-test"}); err != nil {
		t.Fatalf("cluster-native ready must not require a local steward, got: %v", err)
	}
	if !updated {
		t.Fatal("expected status update in cluster-native mode")
	}
}

// TestReady_ClusterAttachedMessage verifies that gateway-mode towers
// (laptop attached to a remote cluster) succeed without a local
// steward. From the laptop's perspective the cluster steward owns
// dispatch via the gateway. (spi-v1hcrs)
func TestReady_ClusterAttachedMessage(t *testing.T) {
	cleanup := stubReadyDeps(t, map[string]Bead{
		"spi-test": {ID: "spi-test", Status: "open"},
	})
	defer cleanup()

	readyActiveTowerFunc = func() (*TowerConfig, error) {
		return &TowerConfig{
			Name:           "remote",
			DeploymentMode: config.DeploymentModeLocalNative, // laptop's view
			Mode:           config.TowerModeGateway,
			URL:            "https://gateway.example",
		}, nil
	}
	// No local steward — gateway-mode must not block on local.
	readyStewardRunningFunc = func() bool { return false }

	updated := false
	readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		updated = true
		return nil
	}

	if err := runReady(nil, []string{"spi-test"}); err != nil {
		t.Fatalf("gateway-mode (cluster-attached) ready must not require a local steward, got: %v", err)
	}
	if !updated {
		t.Fatal("expected status update for gateway-mode tower")
	}
}
