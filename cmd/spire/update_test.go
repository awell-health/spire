package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

	updateGetBeadFunc = func(id string) (Bead, error) { return bead, nil }
	updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error { return nil }
	updateAddLabelFunc = func(id, label string) error { return nil }
	updateRemoveLabelFunc = func(id, label string) error { return nil }
	updateIdentityFunc = func(asFlag string) (string, error) { return "wizard-test", nil }

	return func() {
		updateGetBeadFunc = origGetBead
		updateUpdateBeadFunc = origUpdate
		updateAddLabelFunc = origAddLabel
		updateRemoveLabelFunc = origRemoveLabel
		updateIdentityFunc = origIdentity
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

func TestUpdate_RejectsClosedBead(t *testing.T) {
	bead := Bead{ID: "spi-test", Title: "some task", Status: "closed"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	err := executeUpdateCmd([]string{"spi-test", "--title", "new title"})
	if err == nil {
		t.Fatal("expected error for closed bead, got nil")
	}
	if !strings.Contains(err.Error(), "already closed") {
		t.Errorf("expected 'already closed' error, got: %v", err)
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
