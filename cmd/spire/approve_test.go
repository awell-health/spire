package main

import (
	"strings"
	"testing"
)

// stubApproveDeps replaces test-replaceable vars used by approve.go.
// Returns a cleanup func that restores originals.
func stubApproveDeps(t *testing.T) func() {
	t.Helper()
	origGetBead := approveGetBeadFunc
	origGetStepBeads := approveGetStepBeadsFunc
	origUnhookStepBead := approveUnhookStepBeadFunc
	origUpdateBead := approveUpdateBeadFunc
	origAddComment := approveAddCommentFunc
	origIdentity := approveIdentityFunc
	origSummon := approveSummonFunc

	// Default stub: no-op summon to avoid hitting the store.
	approveSummonFunc = func(beadID string) error { return nil }

	return func() {
		approveGetBeadFunc = origGetBead
		approveGetStepBeadsFunc = origGetStepBeads
		approveUnhookStepBeadFunc = origUnhookStepBead
		approveUpdateBeadFunc = origUpdateBead
		approveAddCommentFunc = origAddComment
		approveIdentityFunc = origIdentity
		approveSummonFunc = origSummon
	}
}

// TestCmdApprove_RequiresHookedStep verifies that cmdApprove rejects
// beads with no hooked step beads.
func TestCmdApprove_RequiresHookedStep(t *testing.T) {
	cleanup := stubApproveDeps(t)
	defer cleanup()

	approveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "in_progress"}, nil
	}
	approveGetStepBeadsFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-test.1", Status: "open", Type: "step", Labels: []string{"workflow-step", "step:implement"}},
		}, nil
	}

	err := cmdApprove("spi-test", "")
	if err == nil {
		t.Fatal("expected error for bead with no hooked approval step")
	}
	if !strings.Contains(err.Error(), "no hooked approval step") {
		t.Errorf("expected 'no hooked approval step' in error, got: %v", err)
	}
}

// TestCmdApprove_UnhooksStep verifies that cmdApprove unhooks the hooked
// approval step and transitions the parent to in_progress.
func TestCmdApprove_UnhooksStep(t *testing.T) {
	cleanup := stubApproveDeps(t)
	defer cleanup()

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	approveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "hooked"}, nil
	}
	approveGetStepBeadsFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-test.1", Status: "closed", Type: "step", Labels: []string{"workflow-step", "step:implement"}},
			{ID: "spi-test.2", Status: "hooked", Type: "step", Labels: []string{"workflow-step", "step:human.approve"}},
		}, nil
	}

	var unhooked []string
	approveUnhookStepBeadFunc = func(stepID string) error {
		unhooked = append(unhooked, stepID)
		return nil
	}

	var updates []map[string]interface{}
	approveUpdateBeadFunc = func(id string, u map[string]interface{}) error {
		updates = append(updates, u)
		return nil
	}

	var addedComments []string
	approveAddCommentFunc = func(id, text string) error {
		addedComments = append(addedComments, text)
		return nil
	}

	approveIdentityFunc = func() (string, error) { return "JB", nil }

	err := cmdApprove("spi-test", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The hooked step should be unhooked.
	if len(unhooked) != 1 || unhooked[0] != "spi-test.2" {
		t.Errorf("expected spi-test.2 to be unhooked, got: %v", unhooked)
	}

	// Parent should be set to in_progress (no other hooked steps).
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0]["status"] != "in_progress" {
		t.Errorf("expected parent set to in_progress, got: %v", updates[0])
	}

	// Approval comment should include identity.
	if len(addedComments) == 0 {
		t.Fatal("expected approval comment to be added")
	}
	if !strings.Contains(addedComments[0], "Approved by JB") {
		t.Errorf("expected comment to contain 'Approved by JB', got: %s", addedComments[0])
	}
}

// TestCmdApprove_WithComment verifies that an optional comment is appended.
func TestCmdApprove_WithComment(t *testing.T) {
	cleanup := stubApproveDeps(t)
	defer cleanup()

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	approveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "hooked"}, nil
	}
	approveGetStepBeadsFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-test.1", Status: "hooked", Type: "step", Labels: []string{"workflow-step", "step:human.approve"}},
		}, nil
	}
	approveUnhookStepBeadFunc = func(stepID string) error { return nil }
	approveUpdateBeadFunc = func(id string, u map[string]interface{}) error { return nil }

	var addedComments []string
	approveAddCommentFunc = func(id, text string) error {
		addedComments = append(addedComments, text)
		return nil
	}

	approveIdentityFunc = func() (string, error) { return "JB", nil }

	err := cmdApprove("spi-test", "looks good, ship it")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(addedComments) == 0 {
		t.Fatal("expected approval comment")
	}
	if !strings.Contains(addedComments[0], "looks good, ship it") {
		t.Errorf("expected comment to contain user comment, got: %s", addedComments[0])
	}
}

// TestCmdApprove_FallsBackToAnyHookedStep verifies that when no step:human.approve
// step exists, cmdApprove falls back to the first hooked step.
func TestCmdApprove_FallsBackToAnyHookedStep(t *testing.T) {
	cleanup := stubApproveDeps(t)
	defer cleanup()

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	approveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "hooked"}, nil
	}
	approveGetStepBeadsFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-test.1", Status: "closed", Type: "step", Labels: []string{"workflow-step", "step:implement"}},
			{ID: "spi-test.2", Status: "hooked", Type: "step", Labels: []string{"workflow-step", "step:review"}},
		}, nil
	}

	var unhooked []string
	approveUnhookStepBeadFunc = func(stepID string) error {
		unhooked = append(unhooked, stepID)
		return nil
	}
	approveUpdateBeadFunc = func(id string, u map[string]interface{}) error { return nil }
	approveAddCommentFunc = func(id, text string) error { return nil }
	approveIdentityFunc = func() (string, error) { return "JB", nil }

	err := cmdApprove("spi-test", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should fall back to spi-test.2 (the only hooked step).
	if len(unhooked) != 1 || unhooked[0] != "spi-test.2" {
		t.Errorf("expected spi-test.2 to be unhooked, got: %v", unhooked)
	}
}

// TestCmdApprove_MultipleHookedSteps verifies that when other steps remain hooked,
// the parent is NOT transitioned to in_progress.
func TestCmdApprove_MultipleHookedSteps(t *testing.T) {
	cleanup := stubApproveDeps(t)
	defer cleanup()

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	approveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "hooked"}, nil
	}
	approveGetStepBeadsFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-test.1", Status: "hooked", Type: "step", Labels: []string{"workflow-step", "step:human.approve"}},
			{ID: "spi-test.2", Status: "hooked", Type: "step", Labels: []string{"workflow-step", "step:review"}},
		}, nil
	}

	approveUnhookStepBeadFunc = func(stepID string) error { return nil }

	var updates []map[string]interface{}
	approveUpdateBeadFunc = func(id string, u map[string]interface{}) error {
		updates = append(updates, u)
		return nil
	}
	approveAddCommentFunc = func(id, text string) error { return nil }
	approveIdentityFunc = func() (string, error) { return "JB", nil }

	err := cmdApprove("spi-test", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Parent should NOT be set to in_progress (other step still hooked).
	if len(updates) != 0 {
		t.Errorf("expected no parent updates (other steps still hooked), got %d: %v", len(updates), updates)
	}
}
