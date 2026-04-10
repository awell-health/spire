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
	origRemoveLabel := approveRemoveLabelFunc
	origAddComment := approveAddCommentFunc
	origIdentity := approveIdentityFunc

	return func() {
		approveGetBeadFunc = origGetBead
		approveRemoveLabelFunc = origRemoveLabel
		approveAddCommentFunc = origAddComment
		approveIdentityFunc = origIdentity
	}
}

// TestCmdApprove_RequiresAwaitingApproval verifies that cmdApprove rejects
// beads without the awaiting-approval label.
func TestCmdApprove_RequiresAwaitingApproval(t *testing.T) {
	cleanup := stubApproveDeps(t)
	defer cleanup()

	approveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "in_progress", Labels: []string{"needs-human"}}, nil
	}

	err := cmdApprove("spi-test", "")
	if err == nil {
		t.Fatal("expected error for bead without awaiting-approval label")
	}
	if !strings.Contains(err.Error(), "not awaiting approval") {
		t.Errorf("expected 'not awaiting approval' in error, got: %v", err)
	}
}

// TestCmdApprove_RemovesLabels verifies that cmdApprove removes both
// awaiting-approval and needs-human labels.
func TestCmdApprove_RemovesLabels(t *testing.T) {
	cleanup := stubApproveDeps(t)
	defer cleanup()

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	approveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{
			ID:     id,
			Status: "in_progress",
			Labels: []string{"needs-human", "awaiting-approval"},
		}, nil
	}

	var removedLabels []string
	approveRemoveLabelFunc = func(id, label string) error {
		removedLabels = append(removedLabels, label)
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

	// Both labels should be removed.
	if len(removedLabels) != 2 {
		t.Fatalf("expected 2 labels removed, got %d: %v", len(removedLabels), removedLabels)
	}
	hasAwaiting := false
	hasNeedsHuman := false
	for _, l := range removedLabels {
		if l == "awaiting-approval" {
			hasAwaiting = true
		}
		if l == "needs-human" {
			hasNeedsHuman = true
		}
	}
	if !hasAwaiting {
		t.Error("expected awaiting-approval label to be removed")
	}
	if !hasNeedsHuman {
		t.Error("expected needs-human label to be removed")
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
		return Bead{
			ID:     id,
			Status: "in_progress",
			Labels: []string{"needs-human", "awaiting-approval"},
		}, nil
	}
	approveRemoveLabelFunc = func(id, label string) error { return nil }

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
