package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveGraphStateFiles_ParentAndNested verifies that doRemoveGraphStateFiles
// removes both the parent graph_state.json and nested sub-executor graph states.
func TestRemoveGraphStateFiles_ParentAndNested(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	wizardName := "wizard-spi-test1"

	// Create parent graph state.
	parentDir := filepath.Join(tmp, "runtime", wizardName)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		t.Fatal(err)
	}
	parentGS := filepath.Join(parentDir, "graph_state.json")
	if err := os.WriteFile(parentGS, []byte(`{"step":"implement"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Create nested sub-executor graph states.
	nestedNames := []string{
		wizardName + "-apprentice",
		wizardName + "-sage-review",
	}
	for _, n := range nestedNames {
		d := filepath.Join(tmp, "runtime", n)
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "graph_state.json"), []byte(`{}`), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Also create a nested v2 state.json.
	nestedV2Dir := filepath.Join(tmp, "runtime", wizardName+"-apprentice")
	if err := os.WriteFile(filepath.Join(nestedV2Dir, "state.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Run quiet variant — should return true.
	removed := removeGraphStateFilesQuiet(wizardName)
	if !removed {
		t.Error("expected removeGraphStateFilesQuiet to return true")
	}

	// Verify all graph state files are gone.
	if _, err := os.Stat(parentGS); !os.IsNotExist(err) {
		t.Error("parent graph_state.json should have been removed")
	}
	for _, n := range nestedNames {
		gs := filepath.Join(tmp, "runtime", n, "graph_state.json")
		if _, err := os.Stat(gs); !os.IsNotExist(err) {
			t.Errorf("nested graph_state.json for %s should have been removed", n)
		}
	}
	// Nested v2 state.json should also be removed.
	v2State := filepath.Join(nestedV2Dir, "state.json")
	if _, err := os.Stat(v2State); !os.IsNotExist(err) {
		t.Error("nested v2 state.json should have been removed")
	}
}

// TestRemoveGraphStateFiles_NoFiles verifies graceful handling when no state files exist.
func TestRemoveGraphStateFiles_NoFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	removed := removeGraphStateFilesQuiet("wizard-spi-nonexistent")
	if removed {
		t.Error("expected removeGraphStateFilesQuiet to return false when no files exist")
	}
}

// TestRemoveGraphStateFiles_ParentOnly verifies that only the parent is removed
// when no nested sub-executors exist.
func TestRemoveGraphStateFiles_ParentOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	wizardName := "wizard-spi-solo"
	parentDir := filepath.Join(tmp, "runtime", wizardName)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parentDir, "graph_state.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	removed := removeGraphStateFilesQuiet(wizardName)
	if !removed {
		t.Error("expected removeGraphStateFilesQuiet to return true")
	}

	gs := filepath.Join(parentDir, "graph_state.json")
	if _, err := os.Stat(gs); !os.IsNotExist(err) {
		t.Error("parent graph_state.json should have been removed")
	}
}

// TestResetV3_ChildProcessing verifies that resetV3 correctly processes children:
// - step beads are closed
// - attempt beads are closed
// - in_progress subtask children are reopened
// - closed subtask children are left alone
func TestResetV3_ChildProcessing(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	// Create the parent epic bead.
	epicID := createTestBead(t, createOpts{
		Title:    "test-resetv3-epic",
		Priority: 1,
		Type:     parseIssueType("epic"),
		Labels:   []string{"feat-branch:feat/test", "interrupted:implement", "needs-human"},
	})

	// Create a step bead (should be closed by resetV3).
	stepID := createTestBead(t, createOpts{
		Title:    "step:implement",
		Priority: 3,
		Type:     parseIssueType("task"),
		Labels:   []string{"step:implement", "workflow-step"},
		Parent:   epicID,
	})
	// Set step to in_progress.
	if err := storeUpdateBead(stepID, map[string]interface{}{"status": "in_progress"}); err != nil {
		t.Fatalf("update step: %v", err)
	}

	// Create an attempt bead (should be closed by resetV3).
	attemptID := createTestBead(t, createOpts{
		Title:    "attempt: wizard-test",
		Priority: 3,
		Type:     parseIssueType("task"),
		Labels:   []string{"attempt", "agent:wizard-test"},
		Parent:   epicID,
	})
	if err := storeUpdateBead(attemptID, map[string]interface{}{"status": "in_progress"}); err != nil {
		t.Fatalf("update attempt: %v", err)
	}

	// Create an in_progress subtask (should be reopened to open).
	inProgSubID := createTestBead(t, createOpts{
		Title:    "subtask-in-progress",
		Priority: 2,
		Type:     parseIssueType("task"),
		Parent:   epicID,
	})
	if err := storeUpdateBead(inProgSubID, map[string]interface{}{"status": "in_progress"}); err != nil {
		t.Fatalf("update subtask: %v", err)
	}

	// Create a closed subtask (should be left alone).
	closedSubID := createTestBead(t, createOpts{
		Title:    "subtask-closed",
		Priority: 2,
		Type:     parseIssueType("task"),
		Parent:   epicID,
	})
	if err := storeCloseBead(closedSubID); err != nil {
		t.Fatalf("close subtask: %v", err)
	}

	// Set epic to in_progress.
	if err := storeUpdateBead(epicID, map[string]interface{}{"status": "in_progress"}); err != nil {
		t.Fatalf("update epic: %v", err)
	}

	// Run resetV3 (soft reset — no git cleanup).
	wizardName := "wizard-test-resetv3"
	if err := resetV3(epicID, false, wizardName, ""); err != nil {
		t.Fatalf("resetV3: %v", err)
	}

	// Verify: step bead should be closed.
	stepBead, err := storeGetBead(stepID)
	if err != nil {
		t.Fatalf("get step bead: %v", err)
	}
	if stepBead.Status != "closed" {
		t.Errorf("step bead status = %q, want closed", stepBead.Status)
	}

	// Verify: attempt bead should be closed.
	attemptBead, err := storeGetBead(attemptID)
	if err != nil {
		t.Fatalf("get attempt bead: %v", err)
	}
	if attemptBead.Status != "closed" {
		t.Errorf("attempt bead status = %q, want closed", attemptBead.Status)
	}

	// Verify: in_progress subtask should be reopened.
	inProgSub, err := storeGetBead(inProgSubID)
	if err != nil {
		t.Fatalf("get in_progress subtask: %v", err)
	}
	if inProgSub.Status != "open" {
		t.Errorf("in_progress subtask status = %q, want open", inProgSub.Status)
	}

	// Verify: closed subtask should remain closed.
	closedSub, err := storeGetBead(closedSubID)
	if err != nil {
		t.Fatalf("get closed subtask: %v", err)
	}
	if closedSub.Status != "closed" {
		t.Errorf("closed subtask status = %q, want closed (should be left alone)", closedSub.Status)
	}

	// Verify: epic bead should be set to open.
	epic, err := storeGetBead(epicID)
	if err != nil {
		t.Fatalf("get epic: %v", err)
	}
	if epic.Status != "open" {
		t.Errorf("epic status = %q, want open", epic.Status)
	}

	// Verify: labels should be stripped.
	for _, l := range epic.Labels {
		if l == "feat-branch:feat/test" || l == "interrupted:implement" || l == "needs-human" {
			t.Errorf("label %q should have been stripped by resetV3", l)
		}
	}
}

// TestResetV3_GraphStateCleanup verifies that resetV3 removes graph state files.
func TestResetV3_GraphStateCleanup(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	wizardName := "wizard-spi-gscleanup"

	// Create parent and nested graph state files.
	parentDir := filepath.Join(tmp, "runtime", wizardName)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parentDir, "graph_state.json"), []byte(`{"step":"implement"}`), 0644); err != nil {
		t.Fatal(err)
	}
	nestedDir := filepath.Join(tmp, "runtime", wizardName+"-apprentice")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "graph_state.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a minimal epic for resetV3.
	epicID := createTestBead(t, createOpts{
		Title:    "test-resetv3-gscleanup",
		Priority: 1,
		Type:     parseIssueType("epic"),
	})

	if err := resetV3(epicID, false, wizardName, ""); err != nil {
		t.Fatalf("resetV3: %v", err)
	}

	// Verify both graph state files are gone.
	if _, err := os.Stat(filepath.Join(parentDir, "graph_state.json")); !os.IsNotExist(err) {
		t.Error("parent graph_state.json should have been removed by resetV3")
	}
	if _, err := os.Stat(filepath.Join(nestedDir, "graph_state.json")); !os.IsNotExist(err) {
		t.Error("nested graph_state.json should have been removed by resetV3")
	}
}
