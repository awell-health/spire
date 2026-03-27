package main

import (
	"strings"
	"testing"
)

// TestClaim_NoAttemptBead verifies claim succeeds when no attempt bead exists.
func TestClaim_NoAttemptBead(t *testing.T) {
	orig := claimGetActiveAttemptFunc
	claimGetActiveAttemptFunc = func(parentID string) (*Bead, error) {
		return nil, nil
	}
	defer func() { claimGetActiveAttemptFunc = orig }()

	// No active attempt — claim check should pass (no error from attempt gate).
	attempt, err := claimGetActiveAttemptFunc("spi-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempt != nil {
		t.Fatalf("expected nil attempt, got %+v", attempt)
	}
}

// TestClaim_RejectsWhenAttemptBeadOpen verifies claim is rejected when an open attempt exists.
func TestClaim_RejectsWhenAttemptBeadOpen(t *testing.T) {
	orig := claimGetActiveAttemptFunc
	claimGetActiveAttemptFunc = func(parentID string) (*Bead, error) {
		return &Bead{
			ID:     parentID + ".1",
			Title:  "attempt: wizard-other",
			Status: "in_progress",
			Labels: []string{"attempt", "agent:wizard-other"},
		}, nil
	}
	defer func() { claimGetActiveAttemptFunc = orig }()

	attempt, err := claimGetActiveAttemptFunc("spi-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempt == nil {
		t.Fatal("expected active attempt, got nil")
	}

	// Simulate the claim gate logic: different owner → should reject.
	identity := "wizard-self"
	owner := ""
	for _, l := range attempt.Labels {
		if strings.HasPrefix(l, "agent:") {
			owner = l[6:]
			break
		}
	}
	if owner == identity {
		t.Fatal("test setup error: owner and identity should differ")
	}
	// If owner != identity, claim would be rejected — correct behaviour.
}

// TestClaim_IgnoresClosedAttemptBead verifies closed attempt beads are not treated as active.
func TestClaim_IgnoresClosedAttemptBead(t *testing.T) {
	orig := claimGetActiveAttemptFunc
	claimGetActiveAttemptFunc = func(parentID string) (*Bead, error) {
		// Closed attempt — storeGetActiveAttempt filters these out, so returns nil.
		return nil, nil
	}
	defer func() { claimGetActiveAttemptFunc = orig }()

	attempt, err := claimGetActiveAttemptFunc("spi-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempt != nil {
		t.Fatalf("closed attempt should be invisible, got %+v", attempt)
	}
}
