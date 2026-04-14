package main

import (
	"fmt"
	"strings"
	"testing"
)

// stubReadyDeps replaces readyGetBeadFunc and readyUpdateBeadFunc with safe
// stubs for the duration of a test. Returns a cleanup func.
func stubReadyDeps(t *testing.T, beads map[string]Bead) func() {
	t.Helper()
	origGet := readyGetBeadFunc
	origUpdate := readyUpdateBeadFunc

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

	return func() {
		readyGetBeadFunc = origGet
		readyUpdateBeadFunc = origUpdate
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
