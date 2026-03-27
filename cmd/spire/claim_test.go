package main

import (
	"fmt"
	"strings"
	"testing"
)

// stubClaimDeps replaces all store/identity funcs used by cmdClaim with safe stubs.
// Returns a cleanup func that restores originals.
func stubClaimDeps(t *testing.T, bead Bead, attemptErr error, identity string) func() {
	t.Helper()
	origGetBead := claimGetBeadFunc
	origUpdate := claimUpdateBeadFunc
	origIdentity := claimIdentityFunc
	origCreate := claimCreateAttemptFunc

	claimGetBeadFunc = func(id string) (Bead, error) { return bead, nil }
	claimUpdateBeadFunc = func(id string, updates map[string]interface{}) error { return nil }
	claimIdentityFunc = func(asFlag string) (string, error) { return identity, nil }
	claimCreateAttemptFunc = func(parentID, agentName, model, branch string) (string, error) {
		if attemptErr != nil {
			return "", attemptErr
		}
		return parentID + ".attempt", nil
	}

	return func() {
		claimGetBeadFunc = origGetBead
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

// TestClaim_RejectsWhenAttemptBeadOpen verifies cmdClaim is rejected when the
// atomic attempt creation returns an error (e.g. active attempt by another agent).
func TestClaim_RejectsWhenAttemptBeadOpen(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	alreadyClaimed := fmt.Errorf("active attempt spi-test.1 already exists (agent: wizard-other)")
	cleanup := stubClaimDeps(t, bead, alreadyClaimed, "wizard-self")
	defer cleanup()

	err := cmdClaim([]string{"spi-test"})
	if err == nil {
		t.Fatal("expected claim to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' in error, got: %v", err)
	}
}

// TestClaim_IgnoresClosedAttemptBead verifies cmdClaim succeeds when only closed attempt
// beads exist (storeCreateAttemptBeadAtomic sees no active attempt, creates a new one).
func TestClaim_IgnoresClosedAttemptBead(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	// Closed attempts are invisible — atomic create sees no active attempt.
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	if err := cmdClaim([]string{"spi-test"}); err != nil {
		t.Fatalf("expected claim to succeed when only closed attempts exist, got: %v", err)
	}
}

// TestClaim_CreatesAttemptBeadAtomically verifies that cmdClaim creates an attempt
// bead as part of the claim via storeCreateAttemptBeadAtomic, and passes empty
// model (unknown at claim time — the executor fills it in later).
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
		if model != "" {
			t.Errorf("expected model=\"\" (unknown at claim time), got %q", model)
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

// TestClaim_ReclaimReusesExistingAttempt verifies that reclaiming (same identity)
// reuses the existing attempt bead. The atomic create func returns the existing
// attempt's ID when the same agent reclaims.
func TestClaim_ReclaimReusesExistingAttempt(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "in_progress"}
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	// Simulate atomic create returning an existing attempt ID (reclaim path).
	claimCreateAttemptFunc = func(parentID, agentName, model, branch string) (string, error) {
		return "spi-test.existing", nil
	}

	if err := cmdClaim([]string{"spi-test"}); err != nil {
		t.Fatalf("expected reclaim to succeed, got error: %v", err)
	}
}
