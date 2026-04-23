package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

// stubUpdateDeps replaces all store/identity funcs used by cmdUpdate with safe stubs.
// Returns a cleanup func that restores originals.
func stubUpdateDeps(t *testing.T, bead Bead) func() {
	t.Helper()
	origGetBead := updateGetBeadFunc
	origUpdate := updateUpdateBeadFunc
	origAddLabel := updateAddLabelFunc
	origRemoveLabel := updateRemoveLabelFunc
	origIdentity := updateIdentityFunc
	origAddDepTyped := updateAddDepTypedFunc
	origRemoveDep := updateRemoveDepFunc
	origGetDepsWithMeta := updateGetDepsWithMetaFunc

	updateGetBeadFunc = func(id string) (Bead, error) { return bead, nil }
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error { return nil }
	updateAddLabelFunc = func(id, label string) error { return nil }
	updateRemoveLabelFunc = func(id, label string) error { return nil }
	updateIdentityFunc = func(asFlag string) (string, error) { return "wizard-test", nil }
	updateAddDepTypedFunc = func(issueID, dependsOnID, depType string) error { return nil }
	updateRemoveDepFunc = func(issueID, dependsOnID string) error { return nil }
	updateGetDepsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) { return nil, nil }

	return func() {
		updateGetBeadFunc = origGetBead
		updateUpdateBeadFunc = origUpdate
		updateAddLabelFunc = origAddLabel
		updateRemoveLabelFunc = origRemoveLabel
		updateIdentityFunc = origIdentity
		updateAddDepTypedFunc = origAddDepTyped
		updateRemoveDepFunc = origRemoveDep
		updateGetDepsWithMetaFunc = origGetDepsWithMeta
	}
}

// executeUpdateCmd creates a fresh cobra command and runs it with the given args.
func executeUpdateCmd(args []string) error {
	cmd := &cobra.Command{Use: "update <bead-id> [flags]", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return cmdUpdate(cmd, args) }}
	cmd.Flags().String("status", "", "")
	cmd.Flags().String("title", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().IntP("priority", "p", 0, "")
	cmd.Flags().String("assignee", "", "")
	cmd.Flags().String("owner", "", "")
	cmd.Flags().Bool("claim", false, "")
	cmd.Flags().Bool("defer", false, "")
	cmd.Flags().String("add-label", "", "")
	cmd.Flags().String("remove-label", "", "")
	cmd.Flags().String("parent", "", "")
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestUpdate_BasicFieldUpdate(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	var capturedUpdates map[string]interface{}
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		capturedUpdates = updates
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-test", "--title", "new title", "--priority", "2"}); err != nil {
		t.Fatalf("expected update to succeed, got: %v", err)
	}

	if capturedUpdates["title"] != "new title" {
		t.Errorf("expected title='new title', got %v", capturedUpdates["title"])
	}
	if capturedUpdates["priority"] != 2 {
		t.Errorf("expected priority=2, got %v", capturedUpdates["priority"])
	}
}

func TestUpdate_ClaimResolvesIdentity(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	var capturedUpdates map[string]interface{}
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		capturedUpdates = updates
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-test", "--claim"}); err != nil {
		t.Fatalf("expected update to succeed, got: %v", err)
	}

	if capturedUpdates["assignee"] != "wizard-test" {
		t.Errorf("expected assignee='wizard-test', got %v", capturedUpdates["assignee"])
	}
	if capturedUpdates["status"] != "in_progress" {
		t.Errorf("expected status='in_progress', got %v", capturedUpdates["status"])
	}
}

func TestUpdate_ClaimWithExplicitStatus(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	var capturedUpdates map[string]interface{}
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		capturedUpdates = updates
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-test", "--claim", "--status", "ready"}); err != nil {
		t.Fatalf("expected update to succeed, got: %v", err)
	}

	if capturedUpdates["assignee"] != "wizard-test" {
		t.Errorf("expected assignee='wizard-test', got %v", capturedUpdates["assignee"])
	}
	if capturedUpdates["status"] != "ready" {
		t.Errorf("expected status='ready' (explicit), got %v", capturedUpdates["status"])
	}
}

func TestUpdate_DeferSetsStatus(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	var capturedUpdates map[string]interface{}
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		capturedUpdates = updates
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-test", "--defer"}); err != nil {
		t.Fatalf("expected update to succeed, got: %v", err)
	}

	if capturedUpdates["status"] != "deferred" {
		t.Errorf("expected status='deferred', got %v", capturedUpdates["status"])
	}
}

func TestUpdate_DeferWithStatusConflict(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	err := executeUpdateCmd([]string{"spi-test", "--defer", "--status", "open"})
	if err == nil {
		t.Fatal("expected error for --defer + --status, got nil")
	}
	if !strings.Contains(err.Error(), "cannot use --defer with --status") {
		t.Errorf("expected conflict error, got: %v", err)
	}
}

func TestUpdate_ReopenClosedBead(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "closed"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	var capturedUpdates map[string]interface{}
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		capturedUpdates = updates
		return nil
	}

	// --status open on a closed bead should reopen it via the store.
	if err := executeUpdateCmd([]string{"spi-test", "--status", "open"}); err != nil {
		t.Fatalf("expected reopen to succeed, got: %v", err)
	}
	if capturedUpdates["status"] != "open" {
		t.Errorf("expected status='open', got %v", capturedUpdates["status"])
	}
}

func TestUpdate_ReopenClosedBeadToInProgress(t *testing.T) {
	// Reopen must work for every non-closed target status, not just 'open'.
	for _, status := range []string{"open", "ready", "in_progress", "deferred"} {
		t.Run(status, func(t *testing.T) {
			bead := Bead{ID: "spi-test", Title: "some task", Status: "closed"}
			cleanup := stubUpdateDeps(t, bead)
			defer cleanup()

			var capturedUpdates map[string]interface{}
			updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
				capturedUpdates = updates
				return nil
			}

			if err := executeUpdateCmd([]string{"spi-test", "--status", status}); err != nil {
				t.Fatalf("expected reopen to %q to succeed, got: %v", status, err)
			}
			if capturedUpdates["status"] != status {
				t.Errorf("expected status=%q, got %v", status, capturedUpdates["status"])
			}
		})
	}
}

func TestUpdate_FieldEditOnClosedBead(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "closed"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	var capturedUpdates map[string]interface{}
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		capturedUpdates = updates
		return nil
	}

	// Non-status field edits on a closed bead are allowed (historical correction).
	if err := executeUpdateCmd([]string{"spi-test", "--title", "new title"}); err != nil {
		t.Fatalf("expected title edit on closed bead to succeed, got: %v", err)
	}
	if capturedUpdates["title"] != "new title" {
		t.Errorf("expected title='new title', got %v", capturedUpdates["title"])
	}
	if _, hasStatus := capturedUpdates["status"]; hasStatus {
		t.Errorf("expected no status in updates, got %v", capturedUpdates["status"])
	}
}

func TestUpdate_ClosedToClosedIsNoop(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "closed"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	updateCalled := false
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		updateCalled = true
		return nil
	}

	// --status closed on an already-closed bead must succeed without writing.
	if err := executeUpdateCmd([]string{"spi-test", "--status", "closed"}); err != nil {
		t.Fatalf("expected closed→closed no-op to succeed, got: %v", err)
	}
	if updateCalled {
		t.Error("expected updateUpdateBeadFunc NOT to be called for closed→closed no-op")
	}
}

func TestUpdate_ClosedToClosedWithFieldEditStillWritesField(t *testing.T) {
	// Mixing --status=closed with a real field change must still write the field;
	// only the redundant status update is stripped.
	bead := Bead{ID: "spi-test", Title: "some task", Status: "closed"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	var capturedUpdates map[string]interface{}
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		capturedUpdates = updates
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-test", "--status", "closed", "--title", "renamed"}); err != nil {
		t.Fatalf("expected mixed update on closed bead to succeed, got: %v", err)
	}
	if capturedUpdates["title"] != "renamed" {
		t.Errorf("expected title='renamed', got %v", capturedUpdates["title"])
	}
	if _, hasStatus := capturedUpdates["status"]; hasStatus {
		t.Errorf("expected redundant status=closed to be stripped, got %v", capturedUpdates["status"])
	}
}

func TestUpdate_NoopWhenNoFlags(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	updateCalled := false
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		updateCalled = true
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-test"}); err != nil {
		t.Fatalf("expected no-op to succeed, got: %v", err)
	}
	if updateCalled {
		t.Error("expected updateUpdateBeadFunc NOT to be called when no flags set")
	}
}

func TestUpdate_AddLabel(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	var capturedLabel string
	updateAddLabelFunc = func(id, label string) error {
		capturedLabel = label
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-test", "--add-label", "urgent"}); err != nil {
		t.Fatalf("expected update to succeed, got: %v", err)
	}
	if capturedLabel != "urgent" {
		t.Errorf("expected label='urgent', got %q", capturedLabel)
	}
}

func TestUpdate_RemoveLabel(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	var capturedLabel string
	updateRemoveLabelFunc = func(id, label string) error {
		capturedLabel = label
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-test", "--remove-label", "old-tag"}); err != nil {
		t.Fatalf("expected update to succeed, got: %v", err)
	}
	if capturedLabel != "old-tag" {
		t.Errorf("expected label='old-tag', got %q", capturedLabel)
	}
}

func TestUpdate_SetParent(t *testing.T) {
	bead := Bead{ID: "spi-child", Title: "child task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	// getBead resolves both the child and the parent.
	updateGetBeadFunc = func(id string) (Bead, error) {
		switch id {
		case "spi-child":
			return bead, nil
		case "spi-parent":
			return Bead{ID: "spi-parent", Title: "parent epic", Status: "open"}, nil
		}
		return Bead{}, fmt.Errorf("not found")
	}

	// No existing deps — this is a fresh parent assignment.
	updateGetDepsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil
	}

	var addedDep struct{ issueID, dependsOnID, depType string }
	updateAddDepTypedFunc = func(issueID, dependsOnID, depType string) error {
		addedDep.issueID = issueID
		addedDep.dependsOnID = dependsOnID
		addedDep.depType = depType
		return nil
	}

	removeCalled := false
	updateRemoveDepFunc = func(issueID, dependsOnID string) error {
		removeCalled = true
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-child", "--parent", "spi-parent"}); err != nil {
		t.Fatalf("expected update to succeed, got: %v", err)
	}

	if addedDep.issueID != "spi-child" || addedDep.dependsOnID != "spi-parent" || addedDep.depType != string(beads.DepParentChild) {
		t.Errorf("expected parent-child dep spi-child→spi-parent, got %+v", addedDep)
	}
	if removeCalled {
		t.Error("expected no remove call when no existing parent")
	}
}

func TestUpdate_ChangeParent(t *testing.T) {
	bead := Bead{ID: "spi-child", Title: "child task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	updateGetBeadFunc = func(id string) (Bead, error) {
		switch id {
		case "spi-child":
			return bead, nil
		case "spi-new-parent":
			return Bead{ID: "spi-new-parent", Title: "new parent", Status: "open"}, nil
		}
		return Bead{}, fmt.Errorf("not found")
	}

	// Existing parent-child dep to spi-old-parent.
	updateGetDepsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-old-parent"},
				DependencyType: beads.DepParentChild,
			},
		}, nil
	}

	var removedDep struct{ issueID, dependsOnID string }
	updateRemoveDepFunc = func(issueID, dependsOnID string) error {
		removedDep.issueID = issueID
		removedDep.dependsOnID = dependsOnID
		return nil
	}

	var addedDep struct{ issueID, dependsOnID, depType string }
	updateAddDepTypedFunc = func(issueID, dependsOnID, depType string) error {
		addedDep.issueID = issueID
		addedDep.dependsOnID = dependsOnID
		addedDep.depType = depType
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-child", "--parent", "spi-new-parent"}); err != nil {
		t.Fatalf("expected update to succeed, got: %v", err)
	}

	if removedDep.issueID != "spi-child" || removedDep.dependsOnID != "spi-old-parent" {
		t.Errorf("expected old parent dep removed (spi-child, spi-old-parent), got %+v", removedDep)
	}
	if addedDep.issueID != "spi-child" || addedDep.dependsOnID != "spi-new-parent" || addedDep.depType != string(beads.DepParentChild) {
		t.Errorf("expected new parent-child dep spi-child→spi-new-parent, got %+v", addedDep)
	}
}

func TestUpdate_ParentNotFound(t *testing.T) {
	bead := Bead{ID: "spi-child", Title: "child task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	updateGetBeadFunc = func(id string) (Bead, error) {
		if id == "spi-child" {
			return bead, nil
		}
		return Bead{}, fmt.Errorf("not found")
	}

	err := executeUpdateCmd([]string{"spi-child", "--parent", "spi-ghost"})
	if err == nil {
		t.Fatal("expected error for nonexistent parent, got nil")
	}
	if !strings.Contains(err.Error(), "parent bead spi-ghost not found") {
		t.Errorf("expected 'parent bead not found' error, got: %v", err)
	}
}

func TestUpdate_SelfParentRejected(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	err := executeUpdateCmd([]string{"spi-test", "--parent", "spi-test"})
	if err == nil {
		t.Fatal("expected error for self-parent, got nil")
	}
	if !strings.Contains(err.Error(), "cannot set a bead as its own parent") {
		t.Errorf("expected self-parent error, got: %v", err)
	}
}
