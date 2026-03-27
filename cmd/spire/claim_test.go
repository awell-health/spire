package main

import (
	"strings"
	"testing"
)

// stubClaimDeps replaces all store/identity funcs used by cmdClaim with safe stubs.
// Returns a cleanup func that restores originals.
func stubClaimDeps(t *testing.T, bead Bead, attempt *Bead, identity string) func() {
	t.Helper()
	origGetBead := claimGetBeadFunc
	origAttempt := claimGetActiveAttemptFunc
	origUpdate := claimUpdateBeadFunc
	origIdentity := claimIdentityFunc
	origCreate := claimCreateAttemptFunc

	claimGetBeadFunc = func(id string) (Bead, error) { return bead, nil }
	claimGetActiveAttemptFunc = func(parentID string) (*Bead, error) { return attempt, nil }
	claimUpdateBeadFunc = func(id string, updates map[string]interface{}) error { return nil }
	claimIdentityFunc = func(asFlag string) (string, error) { return identity, nil }
	claimCreateAttemptFunc = func(parentID, agentName, model, branch string) (string, error) {
		return parentID + ".attempt", nil
	}

	return func() {
		claimGetBeadFunc = origGetBead
		claimGetActiveAttemptFunc = origAttempt
		claimUpdateBeadFunc = origUpdate
		claimIdentityFunc = origIdentity
		claimCreateAttemptFunc = origCreate
	}
}

// TestClaim_NoAttemptBead verifies cmdClaim succeeds when no attempt bead exists.
func TestClaim_NoAttemptBead(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	if err := cmdClaim([]string{"spi-test"}); err != nil {
		t.Fatalf("expected claim to succeed, got error: %v", err)
	}
}

// TestClaim_RejectsWhenAttemptBeadOpen verifies cmdClaim is rejected when an open attempt
// belonging to a different agent exists.
func TestClaim_RejectsWhenAttemptBeadOpen(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	activeAttempt := &Bead{
		ID:     "spi-test.1",
		Title:  "attempt: wizard-other",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-other"},
	}
	cleanup := stubClaimDeps(t, bead, activeAttempt, "wizard-self")
	defer cleanup()

	err := cmdClaim([]string{"spi-test"})
	if err == nil {
		t.Fatal("expected claim to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("expected 'already claimed' in error, got: %v", err)
	}
}

// TestClaim_IgnoresClosedAttemptBead verifies cmdClaim succeeds when only closed attempt
// beads exist (storeGetActiveAttempt filters closed beads, returning nil).
func TestClaim_IgnoresClosedAttemptBead(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	// Closed attempts are invisible — storeGetActiveAttempt returns nil for them.
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	if err := cmdClaim([]string{"spi-test"}); err != nil {
		t.Fatalf("expected claim to succeed when only closed attempts exist, got: %v", err)
	}
}

// TestClaim_CreatesAttemptBeadAtomically verifies that cmdClaim creates an attempt
// bead as part of the claim, closing the race window where two actors could both
// see no attempt and claim the same bead.
func TestClaim_CreatesAttemptBeadAtomically(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	attemptCreated := false
	claimCreateAttemptFunc = func(parentID, agentName, model, branch string) (string, error) {
		attemptCreated = true
		if parentID != "spi-test" {
			t.Errorf("expected parentID=spi-test, got %s", parentID)
		}
		if agentName != "wizard-self" {
			t.Errorf("expected agentName=wizard-self, got %s", agentName)
		}
		if branch != "feat/spi-test" {
			t.Errorf("expected branch=feat/spi-test, got %s", branch)
		}
		return "spi-test.attempt", nil
	}

	if err := cmdClaim([]string{"spi-test"}); err != nil {
		t.Fatalf("expected claim to succeed, got error: %v", err)
	}
	if !attemptCreated {
		t.Fatal("expected attempt bead to be created during claim")
	}
}

// TestClaim_ReclaimSkipsAttemptCreation verifies that reclaiming (same identity)
// reuses the existing attempt bead without creating a new one.
func TestClaim_ReclaimSkipsAttemptCreation(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "in_progress"}
	existingAttempt := &Bead{
		ID:     "spi-test.1",
		Title:  "attempt: wizard-self",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-self"},
	}
	cleanup := stubClaimDeps(t, bead, existingAttempt, "wizard-self")
	defer cleanup()

	attemptCreated := false
	claimCreateAttemptFunc = func(parentID, agentName, model, branch string) (string, error) {
		attemptCreated = true
		return "spi-test.new", nil
	}

	if err := cmdClaim([]string{"spi-test"}); err != nil {
		t.Fatalf("expected reclaim to succeed, got error: %v", err)
	}
	if attemptCreated {
		t.Fatal("expected reclaim to reuse existing attempt, not create a new one")
	}
}
