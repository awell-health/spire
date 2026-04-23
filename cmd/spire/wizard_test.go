package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// wizardClaimHarness captures every mutation cmdWizardClaim performs so tests
// can assert on the full effect set without touching a live dolt server.
type wizardClaimHarness struct {
	createErr      error
	createdParent  string
	createdAgent   string
	createdModel   string
	createdBranch  string
	createdAttempt string
	updatedBead    string
	updatedStatus  string
	identity       string
}

func newWizardClaimHarness(t *testing.T) (*wizardClaimHarness, func()) {
	t.Helper()

	h := &wizardClaimHarness{
		identity:       "wizard-self",
		createdAttempt: "spi-task.attempt-1",
	}

	origCreate := wizardClaimCreateAttempt
	origUpdate := wizardClaimUpdateBead
	origIdentity := wizardClaimIdentity
	origGet := wizardClaimGetBead

	// Default: bead exists in a claimable status. Tests that need a
	// different status (closed, unknown) can override after construction.
	wizardClaimGetBead = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "ready"}, nil
	}

	wizardClaimCreateAttempt = func(parentID, agent, model, branch string) (string, error) {
		if h.createErr != nil {
			return "", h.createErr
		}
		h.createdParent = parentID
		h.createdAgent = agent
		h.createdModel = model
		h.createdBranch = branch
		return h.createdAttempt, nil
	}
	wizardClaimUpdateBead = func(id string, updates map[string]interface{}) error {
		h.updatedBead = id
		if s, ok := updates["status"].(string); ok {
			h.updatedStatus = s
		}
		return nil
	}
	wizardClaimIdentity = func(asFlag string) (string, error) { return h.identity, nil }

	return h, func() {
		wizardClaimCreateAttempt = origCreate
		wizardClaimUpdateBead = origUpdate
		wizardClaimIdentity = origIdentity
		wizardClaimGetBead = origGet
	}
}

// --- Registration --------------------------------------------------------

func TestWizardCmdRegistered(t *testing.T) {
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Name() == "wizard" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("wizardCmd not registered on rootCmd")
	}

	var hasClaim, hasSeal bool
	for _, c := range wizardCmd.Commands() {
		switch c.Name() {
		case "claim":
			hasClaim = true
		case "seal":
			hasSeal = true
		}
	}
	if !hasClaim {
		t.Error("wizardCmd missing 'claim' subcommand")
	}
	if !hasSeal {
		t.Error("wizardCmd missing 'seal' subcommand")
	}
}

// --- wizard claim ---------------------------------------------------------

func TestWizardClaim_HappyPath(t *testing.T) {
	h, cleanup := newWizardClaimHarness(t)
	defer cleanup()

	// Capture stdout so the "claimed" line doesn't pollute test output.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := cmdWizardClaim("spi-task")

	w.Close()
	os.Stdout = oldStdout
	_, _ = io.Copy(io.Discard, r)

	if err != nil {
		t.Fatalf("expected claim to succeed, got: %v", err)
	}
	if h.createdParent != "spi-task" {
		t.Errorf("createdParent = %q, want spi-task", h.createdParent)
	}
	if h.createdAgent != "wizard-self" {
		t.Errorf("createdAgent = %q, want wizard-self", h.createdAgent)
	}
	if h.updatedBead != "spi-task" {
		t.Errorf("updatedBead = %q, want spi-task", h.updatedBead)
	}
	if h.updatedStatus != "in_progress" {
		t.Errorf("updatedStatus = %q, want in_progress", h.updatedStatus)
	}
}

// TestWizardClaim_AcceptsDispatchedSource verifies Seam 3 for the wizard
// path: cmdWizardClaim accepts `dispatched` as a valid source status (the
// cluster-native pod startup path, where the steward flipped
// ready→dispatched at emit time).
func TestWizardClaim_AcceptsDispatchedSource(t *testing.T) {
	_, cleanup := newWizardClaimHarness(t)
	defer cleanup()
	wizardClaimGetBead = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "dispatched"}, nil
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := cmdWizardClaim("spi-task")
	w.Close()
	os.Stdout = oldStdout
	_, _ = io.Copy(io.Discard, r)

	if err != nil {
		t.Fatalf("expected claim from dispatched to succeed, got: %v", err)
	}
}

// TestWizardClaim_RejectsClosedSource makes the refusal path explicit.
func TestWizardClaim_RejectsClosedSource(t *testing.T) {
	_, cleanup := newWizardClaimHarness(t)
	defer cleanup()
	wizardClaimGetBead = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "closed"}, nil
	}

	err := cmdWizardClaim("spi-task")
	if err == nil || !strings.Contains(err.Error(), "already closed") {
		t.Fatalf("expected 'already closed' error, got: %v", err)
	}
}

// TestWizardClaim_AlreadyHasAttempt verifies that a foreign-agent conflict
// surfaced by CreateAttemptBeadAtomic is wrapped with "claim <bead>" and
// preserves the underlying "already exists (agent: ...)" identifiers, and
// that no status flip happens on conflict.
func TestWizardClaim_AlreadyHasAttempt(t *testing.T) {
	h, cleanup := newWizardClaimHarness(t)
	defer cleanup()

	h.createErr = fmt.Errorf("active attempt spi-task.attempt-old already exists (agent: wizard-other)")

	err := cmdWizardClaim("spi-task")
	if err == nil {
		t.Fatal("expected error when active attempt exists, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "claim spi-task") {
		t.Errorf("error %q missing 'claim spi-task' wrap", msg)
	}
	if !strings.Contains(msg, "already exists") {
		t.Errorf("error %q missing 'already exists' phrase", msg)
	}
	if !strings.Contains(msg, "spi-task.attempt-old") {
		t.Errorf("error %q missing foreign attempt id", msg)
	}
	if !strings.Contains(msg, "wizard-other") {
		t.Errorf("error %q missing foreign agent identity", msg)
	}

	// No status flip on conflict.
	if h.updatedBead != "" {
		t.Errorf("expected no bead update on conflict; got updatedBead = %q", h.updatedBead)
	}
	if h.updatedStatus != "" {
		t.Errorf("expected no status flip on conflict; got updatedStatus = %q", h.updatedStatus)
	}
}

// TestWizardClaim_ReclaimReusesExistingAttempt verifies the same-agent
// reclaim path: CreateAttemptBeadAtomic returns the existing attempt ID with
// nil error, and cmdWizardClaim treats that as success — flipping the bead
// to in_progress.
func TestWizardClaim_ReclaimReusesExistingAttempt(t *testing.T) {
	h, cleanup := newWizardClaimHarness(t)
	defer cleanup()

	// Atomic create stub returns an existing attempt ID (same-agent reclaim).
	h.createdAttempt = "spi-task.attempt-existing"

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := cmdWizardClaim("spi-task")

	w.Close()
	os.Stdout = oldStdout
	_, _ = io.Copy(io.Discard, r)

	if err != nil {
		t.Fatalf("expected reclaim to succeed, got: %v", err)
	}
	if h.createdParent != "spi-task" {
		t.Errorf("createdParent = %q, want spi-task", h.createdParent)
	}
	if h.updatedBead != "spi-task" {
		t.Errorf("updatedBead = %q, want spi-task", h.updatedBead)
	}
	if h.updatedStatus != "in_progress" {
		t.Errorf("updatedStatus = %q, want in_progress", h.updatedStatus)
	}
}

// --- wizard seal ----------------------------------------------------------

type wizardSealHarness struct {
	active       *Bead
	activeErr    error
	metaCalls    []sealMetaCall
	metaErr      error
	closedID     string
	closedResult string
	closeErr     error
	resolvedSHA  string
	resolveErr   error
	now          time.Time
}

type sealMetaCall struct {
	beadID string
	meta   map[string]string
}

func newWizardSealHarness(t *testing.T) (*wizardSealHarness, func()) {
	t.Helper()

	h := &wizardSealHarness{
		resolvedSHA: "deadbeefcafe1234567890abcdef1234567890ab",
		now:         time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
	}

	origGetActive := wizardSealGetActiveAttempt
	origSetMeta := wizardSealSetBeadMetadata
	origClose := wizardSealCloseAttempt
	origResolve := wizardSealResolveHead
	origNow := wizardSealNow

	wizardSealGetActiveAttempt = func(parentID string) (*Bead, error) {
		return h.active, h.activeErr
	}
	wizardSealSetBeadMetadata = func(id string, meta map[string]string) error {
		copied := make(map[string]string, len(meta))
		for k, v := range meta {
			copied[k] = v
		}
		h.metaCalls = append(h.metaCalls, sealMetaCall{beadID: id, meta: copied})
		return h.metaErr
	}
	wizardSealCloseAttempt = func(attemptID, result string) error {
		h.closedID = attemptID
		h.closedResult = result
		return h.closeErr
	}
	wizardSealResolveHead = func() (string, error) {
		return h.resolvedSHA, h.resolveErr
	}
	wizardSealNow = func() time.Time { return h.now }

	return h, func() {
		wizardSealGetActiveAttempt = origGetActive
		wizardSealSetBeadMetadata = origSetMeta
		wizardSealCloseAttempt = origClose
		wizardSealResolveHead = origResolve
		wizardSealNow = origNow
	}
}

func TestWizardSeal_HappyPath_WithFlag(t *testing.T) {
	h, cleanup := newWizardSealHarness(t)
	defer cleanup()

	h.active = &Bead{ID: "spi-task.attempt-1"}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := cmdWizardSeal("spi-task", "abc123")

	w.Close()
	os.Stdout = oldStdout
	_, _ = io.Copy(io.Discard, r)

	if err != nil {
		t.Fatalf("expected seal to succeed, got: %v", err)
	}

	if len(h.metaCalls) != 1 {
		t.Fatalf("expected 1 metadata call, got %d", len(h.metaCalls))
	}
	call := h.metaCalls[0]
	if call.beadID != "spi-task" {
		t.Errorf("metadata bead = %q, want spi-task", call.beadID)
	}
	if call.meta["merge_commit"] != "abc123" {
		t.Errorf("merge_commit = %q, want abc123", call.meta["merge_commit"])
	}
	if call.meta["sealed_at"] == "" {
		t.Errorf("sealed_at empty")
	}
	want := h.now.Format(time.RFC3339)
	if call.meta["sealed_at"] != want {
		t.Errorf("sealed_at = %q, want %q", call.meta["sealed_at"], want)
	}

	if h.closedID != "spi-task.attempt-1" {
		t.Errorf("closedID = %q, want spi-task.attempt-1", h.closedID)
	}
}

func TestWizardSeal_HappyPath_DefaultsToHEAD(t *testing.T) {
	h, cleanup := newWizardSealHarness(t)
	defer cleanup()

	h.active = &Bead{ID: "spi-task.attempt-1"}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := cmdWizardSeal("spi-task", "")

	w.Close()
	os.Stdout = oldStdout
	_, _ = io.Copy(io.Discard, r)

	if err != nil {
		t.Fatalf("expected seal to succeed, got: %v", err)
	}
	if len(h.metaCalls) != 1 {
		t.Fatalf("expected 1 metadata call, got %d", len(h.metaCalls))
	}
	if h.metaCalls[0].meta["merge_commit"] != h.resolvedSHA {
		t.Errorf("merge_commit = %q, want resolved HEAD %q",
			h.metaCalls[0].meta["merge_commit"], h.resolvedSHA)
	}
}

func TestWizardSeal_NoOpenAttempt(t *testing.T) {
	h, cleanup := newWizardSealHarness(t)
	defer cleanup()

	h.active = nil

	err := cmdWizardSeal("spi-task", "abc123")
	if err == nil {
		t.Fatal("expected error when no open attempt exists, got nil")
	}
	if !strings.Contains(err.Error(), "no open attempt") {
		t.Errorf("error %q missing 'no open attempt' phrase", err.Error())
	}
	if len(h.metaCalls) != 0 {
		t.Errorf("expected no metadata writes, got %d", len(h.metaCalls))
	}
	if h.closedID != "" {
		t.Errorf("expected no attempt close, got %q", h.closedID)
	}
}

