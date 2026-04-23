package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

// stubClaimDeps replaces all store/identity funcs used by cmdClaim with safe stubs.
// Returns a cleanup func that restores originals.
func stubClaimDeps(t *testing.T, bead Bead, attemptErr error, identity string) func() {
	t.Helper()
	origGetBead := claimGetBeadFunc
	origUpdate := claimUpdateBeadFunc
	origIdentity := claimIdentityFunc
	origCreate := claimCreateAttemptFunc
	origStamp := storeStampAttemptInstanceFunc
	origOwned := storeIsOwnedByInstanceFunc
	origGetInstance := storeGetAttemptInstanceFunc

	claimGetBeadFunc = func(id string) (Bead, error) { return bead, nil }
	claimUpdateBeadFunc = func(id string, updates map[string]interface{}) error { return nil }
	claimIdentityFunc = func(asFlag string) (string, error) { return identity, nil }
	claimCreateAttemptFunc = func(parentID, agentName, model, branch string) (string, error) {
		if attemptErr != nil {
			return "", attemptErr
		}
		return parentID + ".attempt", nil
	}
	storeStampAttemptInstanceFunc = func(attemptID string, m store.InstanceMeta) error { return nil }
	storeIsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) { return true, nil }
	storeGetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) { return nil, nil }

	return func() {
		claimGetBeadFunc = origGetBead
		claimUpdateBeadFunc = origUpdate
		claimIdentityFunc = origIdentity
		claimCreateAttemptFunc = origCreate
		storeStampAttemptInstanceFunc = origStamp
		storeIsOwnedByInstanceFunc = origOwned
		storeGetAttemptInstanceFunc = origGetInstance
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

// TestClaim_StampsInstanceMetadata verifies that cmdClaim stamps instance metadata
// on the attempt bead after creating it.
func TestClaim_StampsInstanceMetadata(t *testing.T) {
	bead := Bead{ID: "spi-stamp", Title: "stamp test", Status: "open"}
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	var stamped *store.InstanceMeta
	var stampedAttemptID string
	storeStampAttemptInstanceFunc = func(attemptID string, m store.InstanceMeta) error {
		stampedAttemptID = attemptID
		stamped = &m
		return nil
	}

	if err := cmdClaim([]string{"spi-stamp"}); err != nil {
		t.Fatalf("expected claim to succeed, got: %v", err)
	}
	if stampedAttemptID != "spi-stamp.attempt" {
		t.Errorf("expected stamp on attempt spi-stamp.attempt, got %s", stampedAttemptID)
	}
	if stamped == nil {
		t.Fatal("expected instance metadata to be stamped")
	}
	if stamped.InstanceID == "" {
		t.Error("expected non-empty InstanceID")
	}
	if stamped.SessionID == "" {
		t.Error("expected non-empty SessionID")
	}
	if stamped.Backend != "process" {
		t.Errorf("expected Backend=process, got %q", stamped.Backend)
	}
	if stamped.StartedAt == "" {
		t.Error("expected non-empty StartedAt")
	}
	if stamped.LastSeenAt == "" {
		t.Error("expected non-empty LastSeenAt")
	}
}

// TestClaim_ReclaimSucceedsSameInstance verifies that reclaiming succeeds when
// the same instance owns the existing attempt.
func TestClaim_ReclaimSucceedsSameInstance(t *testing.T) {
	bead := Bead{ID: "spi-reclaim", Title: "reclaim test", Status: "in_progress"}
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	// IsOwnedByInstance returns true (same instance).
	storeIsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) {
		return true, nil
	}

	if err := cmdClaim([]string{"spi-reclaim"}); err != nil {
		t.Fatalf("expected reclaim to succeed for same instance, got: %v", err)
	}
}

// TestClaim_ReclaimFailsForeignInstance verifies that cmdClaim fails with a clear
// error when a foreign instance owns the active attempt.
func TestClaim_ReclaimFailsForeignInstance(t *testing.T) {
	bead := Bead{ID: "spi-foreign", Title: "foreign test", Status: "in_progress"}
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	// IsOwnedByInstance returns false (foreign instance).
	storeIsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) {
		return false, nil
	}
	storeGetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) {
		return &store.InstanceMeta{
			InstanceID:   "foreign-uuid",
			InstanceName: "other-machine",
		}, nil
	}

	err := cmdClaim([]string{"spi-foreign"})
	if err == nil {
		t.Fatal("expected claim to fail for foreign instance, got nil error")
	}
	if !strings.Contains(err.Error(), "owned by instance") {
		t.Errorf("expected 'owned by instance' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "other-machine") {
		t.Errorf("expected foreign instance name 'other-machine' in error, got: %v", err)
	}
}

// TestClaim_AcceptsReadySource verifies Seam 3: `spire claim` accepts
// status=ready (the local-native path) as a valid source.
func TestClaim_AcceptsReadySource(t *testing.T) {
	bead := Bead{ID: "spi-ready", Title: "ready task", Status: "ready"}
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	if err := cmdClaim([]string{"spi-ready"}); err != nil {
		t.Fatalf("expected claim from ready to succeed, got: %v", err)
	}
}

// TestClaim_AcceptsDispatchedSource verifies Seam 3: `spire claim`
// accepts status=dispatched (the cluster-native path, where the steward
// flipped ready→dispatched at emit time) as a valid source.
func TestClaim_AcceptsDispatchedSource(t *testing.T) {
	bead := Bead{ID: "spi-disp", Title: "dispatched task", Status: "dispatched"}
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	if err := cmdClaim([]string{"spi-disp"}); err != nil {
		t.Fatalf("expected claim from dispatched to succeed, got: %v", err)
	}
}

// TestClaim_RejectsClosedSource is explicit about the one status the
// claim must refuse. Closed is terminal — reclaiming would resurrect
// already-sealed work.
func TestClaim_RejectsClosedSource(t *testing.T) {
	bead := Bead{ID: "spi-closed", Title: "closed task", Status: "closed"}
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	err := cmdClaim([]string{"spi-closed"})
	if err == nil {
		t.Fatal("expected claim from closed to fail, got nil error")
	}
	if !strings.Contains(err.Error(), "already closed") {
		t.Errorf("expected 'already closed' in error, got: %v", err)
	}
}

// TestClaim_OutputIncludesInstanceFields verifies that the JSON output from cmdClaim
// includes instance_name and instance_id fields.
func TestClaim_OutputIncludesInstanceFields(t *testing.T) {
	bead := Bead{ID: "spi-out", Title: "output test", Type: "task", Status: "open"}
	cleanup := stubClaimDeps(t, bead, nil, "wizard-self")
	defer cleanup()

	// Capture stdout.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := cmdClaim([]string{"spi-out"})

	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("expected claim to succeed, got: %v", err)
	}

	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\noutput: %s", err, output)
	}

	if result["instance_id"] == "" {
		t.Error("expected non-empty instance_id in output")
	}
	if result["instance_name"] == "" {
		t.Error("expected non-empty instance_name in output")
	}
	if result["attempt"] == "" {
		t.Error("expected non-empty attempt in output")
	}
}
