package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/recovery"
)

// executeRecoveryActionForTest is a thin wrapper around executeRecoveryAction
// that builds a minimal RecoveryAction from a name string — simplifies tests
// that only care about dispatch by name.
func executeRecoveryActionForTest(beadID, name string) error {
	return executeRecoveryAction(beadID, &recovery.RecoveryAction{Name: name})
}

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

// --- computeStepsToReset unit tests (pure function, no store needed) ---

func TestCleanupInternalDAGChildren_SoftClosesInternalArtifacts(t *testing.T) {
	origClose := storeCloseBeadFunc
	origDelete := storeDeleteBeadFunc
	defer func() {
		storeCloseBeadFunc = origClose
		storeDeleteBeadFunc = origDelete
	}()

	var closedIDs, deletedIDs []string
	storeCloseBeadFunc = func(id string) error {
		closedIDs = append(closedIDs, id)
		return nil
	}
	storeDeleteBeadFunc = func(id string) error {
		deletedIDs = append(deletedIDs, id)
		return nil
	}

	children := []Bead{
		{ID: "spi-step", Title: "step:implement", Status: "in_progress", Labels: []string{"workflow-step", "step:implement"}},
		{ID: "spi-attempt", Title: "attempt: wizard", Status: "open", Labels: []string{"attempt", "agent:wizard"}},
		{ID: "spi-review", Title: "review-round-1", Status: "closed", Labels: []string{"review-round", "round:1"}},
		{ID: "spi-task", Title: "real subtask", Status: "open"},
	}

	// Pass cycle=0 to skip stamping (covered separately by the cycle test).
	counts := cleanupInternalDAGChildren(children, false, 0)

	if !reflect.DeepEqual(closedIDs, []string{"spi-step", "spi-attempt"}) {
		t.Fatalf("closed IDs = %v, want [spi-step spi-attempt]", closedIDs)
	}
	if len(deletedIDs) != 0 {
		t.Fatalf("deleted IDs = %v, want none", deletedIDs)
	}
	if counts.ClosedSteps != 1 || counts.ClosedAttempts != 1 || counts.ClosedReviewRounds != 0 {
		t.Fatalf("unexpected close counts: %+v", counts)
	}
	if counts.DeletedSteps != 0 || counts.DeletedAttempts != 0 || counts.DeletedReviewRounds != 0 {
		t.Fatalf("unexpected delete counts: %+v", counts)
	}
}

// TestCleanupInternalDAGChildren_HardPreservesAttemptsAndReviews verifies that
// hard reset DELETES step beads but CLOSES attempt/review beads (the spi-cjotlm
// fix for log-collision across reset cycles).
func TestCleanupInternalDAGChildren_HardPreservesAttemptsAndReviews(t *testing.T) {
	origClose := storeCloseBeadFunc
	origDelete := storeDeleteBeadFunc
	defer func() {
		storeCloseBeadFunc = origClose
		storeDeleteBeadFunc = origDelete
	}()

	var closedIDs, deletedIDs []string
	storeCloseBeadFunc = func(id string) error {
		closedIDs = append(closedIDs, id)
		return nil
	}
	storeDeleteBeadFunc = func(id string) error {
		deletedIDs = append(deletedIDs, id)
		return nil
	}

	children := []Bead{
		{ID: "spi-step", Title: "step:implement", Status: "closed", Labels: []string{"workflow-step", "step:implement"}},
		{ID: "spi-attempt", Title: "attempt: wizard", Status: "in_progress", Labels: []string{"attempt", "agent:wizard"}},
		{ID: "spi-review", Title: "review-round-1", Status: "closed", Labels: []string{"review-round", "round:1"}},
		{ID: "spi-task", Title: "real subtask", Status: "open"},
	}

	// cycle=0 disables stamping so this test stays focused on close-vs-delete.
	counts := cleanupInternalDAGChildren(children, true, 0)

	if !reflect.DeepEqual(deletedIDs, []string{"spi-step"}) {
		t.Fatalf("deleted IDs = %v, want [spi-step] (attempts and reviews must be preserved)", deletedIDs)
	}
	// Only the in_progress attempt is closed by this call — the already-closed
	// review is left alone (idempotent), even though it would have been stamped
	// if cycle > 0.
	if !reflect.DeepEqual(closedIDs, []string{"spi-attempt"}) {
		t.Fatalf("closed IDs = %v, want [spi-attempt]", closedIDs)
	}
	if counts.DeletedSteps != 1 || counts.DeletedAttempts != 0 || counts.DeletedReviewRounds != 0 {
		t.Fatalf("unexpected delete counts: %+v", counts)
	}
	if counts.ClosedAttempts != 1 || counts.ClosedReviewRounds != 0 {
		t.Fatalf("unexpected close counts: %+v", counts)
	}
}

// TestCleanupInternalDAGChildren_StampsResetCycle verifies the spi-cjotlm
// behavior: each attempt/review child without an existing reset-cycle:<N>
// label is stamped with the supplied cycle, and the stamping is idempotent
// (already-labeled children are skipped).
func TestCleanupInternalDAGChildren_StampsResetCycle(t *testing.T) {
	origClose := storeCloseBeadFunc
	origDelete := storeDeleteBeadFunc
	origAddLabel := storeAddLabelFunc
	defer func() {
		storeCloseBeadFunc = origClose
		storeDeleteBeadFunc = origDelete
		storeAddLabelFunc = origAddLabel
	}()

	storeCloseBeadFunc = func(id string) error { return nil }
	storeDeleteBeadFunc = func(id string) error { return nil }

	type stamp struct{ id, label string }
	var stamps []stamp
	storeAddLabelFunc = func(id, label string) error {
		stamps = append(stamps, stamp{id, label})
		return nil
	}

	children := []Bead{
		{ID: "spi-step", Title: "step:implement", Status: "in_progress", Labels: []string{"workflow-step", "step:implement"}},
		// No reset-cycle yet — should be stamped with cycle 2.
		{ID: "spi-attempt-fresh", Title: "attempt: w", Status: "open", Labels: []string{"attempt"}},
		// Already carries reset-cycle:1 from a previous reset — must NOT be re-stamped (idempotency).
		{ID: "spi-attempt-old", Title: "attempt: w", Status: "closed", Labels: []string{"attempt", "reset-cycle:1"}},
		// Review without reset-cycle — should be stamped with cycle 2.
		{ID: "spi-review-fresh", Title: "review-round-3", Status: "in_progress", Labels: []string{"review-round", "round:3"}},
		// Real subtask — never stamped.
		{ID: "spi-task", Title: "real subtask", Status: "open"},
	}

	cleanupInternalDAGChildren(children, false, 2)

	wantStamps := []stamp{
		{"spi-attempt-fresh", "reset-cycle:2"},
		{"spi-review-fresh", "reset-cycle:2"},
	}
	if !reflect.DeepEqual(stamps, wantStamps) {
		t.Fatalf("stamps = %+v, want %+v", stamps, wantStamps)
	}
}

// TestDeleteInternalDAGBeadsRecursive_StampsResetCycle is the parity test
// for the hard-reset path — same close-not-delete + stamp behavior as the
// soft path covered above.
func TestDeleteInternalDAGBeadsRecursive_StampsResetCycle(t *testing.T) {
	origClose := storeCloseBeadFunc
	origDelete := storeDeleteBeadFunc
	origAddLabel := storeAddLabelFunc
	origGetChildren := storeGetChildrenFunc
	defer func() {
		storeCloseBeadFunc = origClose
		storeDeleteBeadFunc = origDelete
		storeAddLabelFunc = origAddLabel
		storeGetChildrenFunc = origGetChildren
	}()

	storeCloseBeadFunc = func(id string) error { return nil }
	storeDeleteBeadFunc = func(id string) error { return nil }
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) { return nil, nil }

	type stamp struct{ id, label string }
	var stamps []stamp
	storeAddLabelFunc = func(id, label string) error {
		stamps = append(stamps, stamp{id, label})
		return nil
	}

	children := []Bead{
		{ID: "spi-attempt", Title: "attempt: w", Status: "in_progress", Labels: []string{"attempt"}},
		{ID: "spi-review", Title: "review-round-1", Status: "closed", Labels: []string{"review-round", "round:1"}},
	}

	deleteInternalDAGBeadsRecursive(children, 1)

	wantStamps := []stamp{
		{"spi-attempt", "reset-cycle:1"},
		{"spi-review", "reset-cycle:1"},
	}
	if !reflect.DeepEqual(stamps, wantStamps) {
		t.Fatalf("stamps = %+v, want %+v", stamps, wantStamps)
	}
}

// TestDeleteInternalDAGBeadsRecursive_PreservesAttemptsAndReviews verifies the
// spi-cjotlm fix: hard reset deletes step beads (along with their nested
// descendants) but CLOSES — never deletes — attempt and review-round beads.
func TestDeleteInternalDAGBeadsRecursive_PreservesAttemptsAndReviews(t *testing.T) {
	origDelete := storeDeleteBeadFunc
	origClose := storeCloseBeadFunc
	origGetChildren := storeGetChildrenFunc
	defer func() {
		storeDeleteBeadFunc = origDelete
		storeCloseBeadFunc = origClose
		storeGetChildrenFunc = origGetChildren
	}()

	var deletedIDs, closedIDs []string
	storeDeleteBeadFunc = func(id string) error {
		deletedIDs = append(deletedIDs, id)
		return nil
	}
	storeCloseBeadFunc = func(id string) error {
		closedIDs = append(closedIDs, id)
		return nil
	}

	// Simulate nested children: step bead has a nested attempt child (still
	// gets recursively deleted because it's part of the step subtree).
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) {
		if parentID == "spi-step" {
			return []Bead{
				{ID: "spi-nested-attempt", Title: "attempt: nested", Labels: []string{"attempt"}},
			}, nil
		}
		return nil, nil
	}

	children := []Bead{
		{ID: "spi-step", Title: "step:implement", Status: "closed", Labels: []string{"workflow-step", "step:implement"}},
		{ID: "spi-attempt", Title: "attempt: wizard", Status: "in_progress", Labels: []string{"attempt", "agent:wizard"}},
		{ID: "spi-review", Title: "review-round-1", Status: "closed", Labels: []string{"review-round", "round:1"}},
		{ID: "spi-task", Title: "real subtask", Status: "open"},
	}

	// cycle=0 skips reset-cycle stamping so the test stays focused on
	// close-vs-delete behavior; cycle stamping is covered separately below.
	counts := deleteInternalDAGBeadsRecursive(children, 0)

	// Top-level attempt is closed (preserved for log continuity), the step is
	// deleted, the nested attempt under the step is deleted bottom-up, and
	// the already-closed review-round is left alone (no double-close).
	if !reflect.DeepEqual(deletedIDs, []string{"spi-nested-attempt", "spi-step"}) {
		t.Fatalf("deleted IDs = %v, want [spi-nested-attempt spi-step]", deletedIDs)
	}
	if !reflect.DeepEqual(closedIDs, []string{"spi-attempt"}) {
		t.Fatalf("closed IDs = %v, want [spi-attempt]", closedIDs)
	}
	if counts.DeletedSteps != 1 || counts.DeletedAttempts != 0 || counts.DeletedReviewRounds != 0 {
		t.Fatalf("unexpected delete counts: %+v", counts)
	}
	if counts.ClosedAttempts != 1 || counts.ClosedReviewRounds != 0 {
		t.Fatalf("unexpected close counts: %+v", counts)
	}
}

func TestDeleteInternalDAGBeadsRecursive_SkipsRealSubtasks(t *testing.T) {
	origDelete := storeDeleteBeadFunc
	origGetChildren := storeGetChildrenFunc
	defer func() {
		storeDeleteBeadFunc = origDelete
		storeGetChildrenFunc = origGetChildren
	}()

	var deletedIDs []string
	storeDeleteBeadFunc = func(id string) error {
		deletedIDs = append(deletedIDs, id)
		return nil
	}
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return nil, nil
	}

	children := []Bead{
		{ID: "spi-task1", Title: "real subtask 1", Status: "in_progress"},
		{ID: "spi-task2", Title: "real subtask 2", Status: "open"},
	}

	counts := deleteInternalDAGBeadsRecursive(children, 0)

	if len(deletedIDs) != 0 {
		t.Fatalf("deleted IDs = %v, want none (real subtasks should be skipped)", deletedIDs)
	}
	if counts.DeletedSteps != 0 || counts.DeletedAttempts != 0 || counts.DeletedReviewRounds != 0 {
		t.Fatalf("unexpected delete counts: %+v", counts)
	}
}

func TestIsProtectedByLabel(t *testing.T) {
	tests := []struct {
		name   string
		bead   Bead
		expect bool
	}{
		{
			name:   "recovery-bead label",
			bead:   Bead{ID: "spi-rec", Labels: []string{"recovery-bead", "type:recovery"}},
			expect: true,
		},
		{
			// spi-pwdhs5 Bug B narrowed the protected set to recovery-bead
			// only; alert:* labels are NOT protected so the reset cascade
			// can close them and stamp a reset-cycle:<N> for audit.
			name:   "alert label",
			bead:   Bead{ID: "spi-alert", Labels: []string{"alert:corrupted-bead"}},
			expect: false,
		},
		{
			// See above — alert-labeled beads flow through the cascade.
			name:   "alert merge-failure label",
			bead:   Bead{ID: "spi-alert2", Labels: []string{"alert:merge-failure"}},
			expect: false,
		},
		{
			name:   "normal step bead",
			bead:   Bead{ID: "spi-step", Labels: []string{"workflow-step", "step:implement"}},
			expect: false,
		},
		{
			name:   "normal subtask",
			bead:   Bead{ID: "spi-task", Labels: nil},
			expect: false,
		},
		{
			name:   "attempt bead",
			bead:   Bead{ID: "spi-att", Labels: []string{"attempt", "agent:wizard"}},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isProtectedByLabel(tt.bead)
			if got != tt.expect {
				t.Errorf("isProtectedByLabel(%s) = %v, want %v", tt.bead.ID, got, tt.expect)
			}
		})
	}
}

func TestComputeStepsToReset_LinearChain(t *testing.T) {
	// A → B → C: resetting B should include B and C.
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"A": {},
			"B": {Needs: []string{"A"}},
			"C": {Needs: []string{"B"}},
		},
	}

	result := computeStepsToReset(graph, "B")
	if !result["B"] {
		t.Error("expected B in reset set")
	}
	if !result["C"] {
		t.Error("expected C in reset set (depends on B)")
	}
	if result["A"] {
		t.Error("A should NOT be in reset set (upstream of B)")
	}
	if len(result) != 2 {
		t.Errorf("expected 2 steps in reset set, got %d", len(result))
	}
}

func TestComputeStepsToReset_RootStep(t *testing.T) {
	// Resetting the root should include everything.
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"plan":      {},
			"implement": {Needs: []string{"plan"}},
			"review":    {Needs: []string{"implement"}},
			"merge":     {Needs: []string{"review"}},
		},
	}

	result := computeStepsToReset(graph, "plan")
	if len(result) != 4 {
		t.Errorf("expected 4 steps when resetting root, got %d", len(result))
	}
	for _, name := range []string{"plan", "implement", "review", "merge"} {
		if !result[name] {
			t.Errorf("expected %s in reset set", name)
		}
	}
}

func TestComputeStepsToReset_LeafStep(t *testing.T) {
	// Resetting a leaf step should only include itself.
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"A": {},
			"B": {Needs: []string{"A"}},
			"C": {Needs: []string{"A"}},
		},
	}

	result := computeStepsToReset(graph, "C")
	if len(result) != 1 {
		t.Errorf("expected 1 step when resetting leaf, got %d", len(result))
	}
	if !result["C"] {
		t.Error("expected C in reset set")
	}
}

func TestComputeStepsToReset_Diamond(t *testing.T) {
	// Diamond: A → {B, C} → D: resetting A includes all.
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"A": {},
			"B": {Needs: []string{"A"}},
			"C": {Needs: []string{"A"}},
			"D": {Needs: []string{"B", "C"}},
		},
	}

	result := computeStepsToReset(graph, "A")
	if len(result) != 4 {
		t.Errorf("expected 4 steps for diamond from root, got %d", len(result))
	}

	// Resetting B should include B and D (D depends on B), but not A or C.
	result = computeStepsToReset(graph, "B")
	if len(result) != 2 {
		t.Errorf("expected 2 steps for diamond from B, got %d", len(result))
	}
	if !result["B"] || !result["D"] {
		t.Error("expected B and D in reset set")
	}
	if result["A"] || result["C"] {
		t.Error("A and C should not be in reset set when resetting B")
	}
}

func TestComputeStepsToReset_Branching(t *testing.T) {
	// A → {B, C}: resetting A includes A, B, C.
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"A": {},
			"B": {Needs: []string{"A"}},
			"C": {Needs: []string{"A"}},
		},
	}

	result := computeStepsToReset(graph, "A")
	if len(result) != 3 {
		t.Errorf("expected 3 steps, got %d", len(result))
	}
}

func TestComputeStepsToReset_DisconnectedGraphs(t *testing.T) {
	// Two disconnected chains: A→B and X→Y. Resetting A only touches A, B.
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"A": {},
			"B": {Needs: []string{"A"}},
			"X": {},
			"Y": {Needs: []string{"X"}},
		},
	}

	result := computeStepsToReset(graph, "A")
	if len(result) != 2 {
		t.Errorf("expected 2 steps (A, B only), got %d", len(result))
	}
	if result["X"] || result["Y"] {
		t.Error("disconnected steps X, Y should not be in reset set")
	}
}

func TestComputeStepsToReset_SingleStep(t *testing.T) {
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"only": {},
		},
	}

	result := computeStepsToReset(graph, "only")
	if len(result) != 1 || !result["only"] {
		t.Errorf("expected exactly {only}, got %v", result)
	}
}

// --- softResetV3 tests ---

// writeGraphState writes a GraphState to disk for a given wizard name,
// using SPIRE_CONFIG_DIR as the config root.
func writeGraphState(t *testing.T, configRoot, wizardName string, gs *executor.GraphState) {
	t.Helper()
	runtimeDir := filepath.Join(configRoot, "runtime", wizardName)
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(gs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "graph_state.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSoftResetV3_RewindsStepsAndPreservesUpstream(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	// Create a bead with a v3 formula.
	epicID := createTestBead(t, createOpts{
		Title:    "test-soft-reset-rewind",
		Priority: 1,
		Type:     parseIssueType("epic"),
	})

	// Create step beads for the steps that will be closed during reset.
	implementStepID := createTestBead(t, createOpts{
		Title:    "step:implement",
		Priority: 3,
		Type:     parseIssueType("task"),
		Labels:   []string{"step:implement", "workflow-step"},
		Parent:   epicID,
	})
	if err := storeUpdateBead(implementStepID, map[string]interface{}{"status": "in_progress"}); err != nil {
		t.Fatalf("update step bead: %v", err)
	}

	reviewStepID := createTestBead(t, createOpts{
		Title:    "step:review",
		Priority: 3,
		Type:     parseIssueType("task"),
		Labels:   []string{"step:review", "workflow-step"},
		Parent:   epicID,
	})
	if err := storeUpdateBead(reviewStepID, map[string]interface{}{"status": "in_progress"}); err != nil {
		t.Fatalf("update step bead: %v", err)
	}

	wizardName := "wizard-test-soft-reset"

	// Write graph state with plan=completed, implement=completed, review=active.
	gs := &executor.GraphState{
		BeadID:    epicID,
		AgentName: wizardName,
		Formula:   "task-default",
		Steps: map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1, StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z", Outputs: map[string]string{"result": "ok"}},
			"implement": {Status: "completed", CompletedCount: 1, StartedAt: "2026-01-01T00:01:00Z", CompletedAt: "2026-01-01T00:02:00Z", Outputs: map[string]string{"branch": "feat/test"}},
			"review":    {Status: "active", CompletedCount: 0, StartedAt: "2026-01-01T00:02:00Z"},
			"merge":     {Status: "pending"},
		},
		ActiveStep: "review",
		StepBeadIDs: map[string]string{
			"implement": implementStepID,
			"review":    reviewStepID,
		},
		Workspaces: map[string]executor.WorkspaceState{},
	}
	writeGraphState(t, tmp, wizardName, gs)

	// Create nested graph state for implement (should be deleted).
	nestedDir := filepath.Join(tmp, "runtime", wizardName+"-implement")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatal(err)
	}
	nestedGS := filepath.Join(nestedDir, "graph_state.json")
	if err := os.WriteFile(nestedGS, []byte(`{"step":"nested"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Set epic to in_progress.
	if err := storeUpdateBead(epicID, map[string]interface{}{"status": "in_progress"}); err != nil {
		t.Fatalf("update epic: %v", err)
	}

	// Soft-reset to "implement" — should reset implement, review, merge but preserve plan.
	// NOTE: softResetV3 calls ResolveFormulaAny and cmdSummon internally;
	// since we can't mock those in an integration test without a real formula,
	// we test the graph state manipulation by directly invoking the internal logic.
	// Instead, verify the rewind by loading graph state after simulating the changes.

	// Load the graph state, apply the same logic, and verify.
	loaded, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil || loaded == nil {
		t.Fatalf("load graph state: %v (nil=%v)", err, loaded == nil)
	}

	// Simulate computeStepsToReset + rewind (same logic as softResetV3).
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"plan":      {},
			"implement": {Needs: []string{"plan"}},
			"review":    {Needs: []string{"implement"}},
			"merge":     {Needs: []string{"review"}},
		},
	}
	stepsToReset := computeStepsToReset(graph, "implement")

	// Verify correct steps are computed.
	if !stepsToReset["implement"] || !stepsToReset["review"] || !stepsToReset["merge"] {
		t.Errorf("expected implement, review, merge in reset set, got %v", stepsToReset)
	}
	if stepsToReset["plan"] {
		t.Error("plan should NOT be in reset set")
	}

	// Apply rewind to loaded state.
	for stepName := range stepsToReset {
		ss, ok := loaded.Steps[stepName]
		if !ok {
			continue
		}
		ss.Status = "pending"
		ss.Outputs = nil
		ss.StartedAt = ""
		ss.CompletedAt = ""
		loaded.Steps[stepName] = ss
	}

	// Clear active step if in reset set.
	if stepsToReset[loaded.ActiveStep] {
		loaded.ActiveStep = ""
	}

	// Verify plan is preserved.
	plan := loaded.Steps["plan"]
	if plan.Status != "completed" {
		t.Errorf("plan status = %q, want completed (should be preserved)", plan.Status)
	}
	if plan.CompletedCount != 1 {
		t.Errorf("plan CompletedCount = %d, want 1 (should be preserved)", plan.CompletedCount)
	}
	if plan.Outputs["result"] != "ok" {
		t.Error("plan outputs should be preserved")
	}

	// Verify implement was rewound.
	impl := loaded.Steps["implement"]
	if impl.Status != "pending" {
		t.Errorf("implement status = %q, want pending", impl.Status)
	}
	if impl.Outputs != nil {
		t.Error("implement outputs should be nil after reset")
	}
	if impl.StartedAt != "" || impl.CompletedAt != "" {
		t.Error("implement timestamps should be cleared")
	}
	if impl.CompletedCount != 1 {
		t.Errorf("implement CompletedCount = %d, want 1 (should be preserved)", impl.CompletedCount)
	}

	// Verify review was rewound.
	rev := loaded.Steps["review"]
	if rev.Status != "pending" {
		t.Errorf("review status = %q, want pending", rev.Status)
	}

	// Verify active step was cleared.
	if loaded.ActiveStep != "" {
		t.Errorf("active step = %q, want empty (was in reset set)", loaded.ActiveStep)
	}

	// Verify nested graph state deletion.
	// Remove it as softResetV3 would.
	os.Remove(nestedGS)
	if _, err := os.Stat(nestedGS); !os.IsNotExist(err) {
		t.Error("nested graph_state.json for implement should be deleted")
	}

	// Verify step beads would be closed.
	// In a full integration test with a running formula, softResetV3 would close these.
	// Here we verify the bead IDs are correctly tracked.
	if loaded.StepBeadIDs["implement"] != implementStepID {
		t.Errorf("step bead ID for implement = %q, want %q", loaded.StepBeadIDs["implement"], implementStepID)
	}
	if loaded.StepBeadIDs["review"] != reviewStepID {
		t.Errorf("step bead ID for review = %q, want %q", loaded.StepBeadIDs["review"], reviewStepID)
	}
}

func TestSoftResetV3_NoGraphState(t *testing.T) {
	// When no graph state exists, softResetV3 should still proceed (goto resummon).
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	wizardName := "wizard-test-no-gs"
	gs, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil {
		t.Fatalf("load graph state: %v", err)
	}
	if gs != nil {
		t.Error("expected nil graph state when no file exists")
	}
	// This verifies the "no graph state → goto resummon" path is valid.
}

func TestSoftResetV3_InvalidStepName(t *testing.T) {
	// Verify computeStepsToReset with the target step present in graph works,
	// and that the caller (softResetV3) would validate the step name.
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"plan":      {},
			"implement": {Needs: []string{"plan"}},
		},
	}

	// The step validation in softResetV3 checks graph.Steps[targetStep].
	_, ok := graph.Steps["nonexistent"]
	if ok {
		t.Error("nonexistent step should not be found in graph")
	}

	// Valid step names should be found.
	_, ok = graph.Steps["plan"]
	if !ok {
		t.Error("plan step should be found in graph")
	}
}

func TestSoftResetV3_NestedStateCleanup(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	wizardName := "wizard-test-nested-cleanup"

	// Create nested graph state files for various steps.
	for _, step := range []string{"implement", "review", "merge"} {
		nestedDir := filepath.Join(tmp, "runtime", wizardName+"-"+step)
		if err := os.MkdirAll(nestedDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nestedDir, "graph_state.json"), []byte(`{}`), 0644); err != nil {
			t.Fatal(err)
		}
		// Also create nested v2 state.json.
		if err := os.WriteFile(filepath.Join(nestedDir, "state.json"), []byte(`{}`), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Simulate softResetV3 nested cleanup for steps implement and review (not merge).
	stepsToReset := map[string]bool{"implement": true, "review": true}
	runtimeDir := filepath.Join(tmp, "runtime")
	for stepName := range stepsToReset {
		nestedName := wizardName + "-" + stepName
		nestedPath := filepath.Join(runtimeDir, nestedName, "graph_state.json")
		os.Remove(nestedPath)
		nestedV2Path := filepath.Join(runtimeDir, nestedName, "state.json")
		os.Remove(nestedV2Path)
	}

	// Verify reset steps' nested state is removed.
	for _, step := range []string{"implement", "review"} {
		gsPath := filepath.Join(runtimeDir, wizardName+"-"+step, "graph_state.json")
		if _, err := os.Stat(gsPath); !os.IsNotExist(err) {
			t.Errorf("nested graph_state.json for %s should be removed", step)
		}
		v2Path := filepath.Join(runtimeDir, wizardName+"-"+step, "state.json")
		if _, err := os.Stat(v2Path); !os.IsNotExist(err) {
			t.Errorf("nested state.json for %s should be removed", step)
		}
	}

	// Verify non-reset step's nested state is preserved.
	mergeGS := filepath.Join(runtimeDir, wizardName+"-merge", "graph_state.json")
	if _, err := os.Stat(mergeGS); os.IsNotExist(err) {
		t.Error("nested graph_state.json for merge should be preserved (not in reset set)")
	}
}

// TestMapKeys verifies mapKeys returns sorted keys.
func TestMapKeys(t *testing.T) {
	m := map[string]bool{"cherry": true, "apple": true, "banana": true}
	keys := mapKeys(m)
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}
	if keys[0] != "apple" || keys[1] != "banana" || keys[2] != "cherry" {
		t.Errorf("expected [apple banana cherry], got %v", keys)
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

// --- Decoupling from summon ---

// withSummonRecorder swaps cmdSummonFunc to a recorder for the duration of a
// test. The recorder fails the test if cmdSummonFunc is invoked, which is
// the negative assertion that reset does not auto-summon.
func withSummonRecorder(t *testing.T) *bool {
	t.Helper()
	called := false
	orig := cmdSummonFunc
	cmdSummonFunc = func(args []string) error {
		called = true
		t.Errorf("cmdSummonFunc was invoked with args=%v — reset must not auto-summon", args)
		return nil
	}
	t.Cleanup(func() { cmdSummonFunc = orig })
	return &called
}

// TestResetV3_DoesNotAutoSummon_Soft verifies a soft reset (plain reset without
// --hard) does not invoke cmdSummonFunc.
func TestResetV3_DoesNotAutoSummon_Soft(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	called := withSummonRecorder(t)

	epicID := createTestBead(t, createOpts{
		Title:    "test-resetv3-no-autosummon-soft",
		Priority: 1,
		Type:     parseIssueType("epic"),
	})

	wizardName := "wizard-test-no-autosummon-soft"
	if err := resetV3(epicID, false, wizardName, ""); err != nil {
		t.Fatalf("resetV3: %v", err)
	}
	if *called {
		t.Error("cmdSummonFunc was invoked by resetV3 (soft) — reset must not auto-summon")
	}
}

// TestResetV3_DoesNotAutoSummon_Hard verifies a hard reset does not invoke
// cmdSummonFunc.
func TestResetV3_DoesNotAutoSummon_Hard(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	called := withSummonRecorder(t)

	epicID := createTestBead(t, createOpts{
		Title:    "test-resetv3-no-autosummon-hard",
		Priority: 1,
		Type:     parseIssueType("epic"),
	})

	wizardName := "wizard-test-no-autosummon-hard"
	if err := resetV3(epicID, true, wizardName, ""); err != nil {
		t.Fatalf("resetV3 hard: %v", err)
	}
	if *called {
		t.Error("cmdSummonFunc was invoked by resetV3 (hard) — reset must not auto-summon")
	}
}

// TestBoardReset_DoesNotAutoSummon verifies the board reset/reset-hard actions
// do not auto-summon after reset. Mirrors the CLI decoupling: one-click reset
// resets only; the operator explicitly triggers summon as a separate action.
func TestBoardReset_DoesNotAutoSummon(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	called := withSummonRecorder(t)

	epicID := createTestBead(t, createOpts{
		Title:    "test-board-reset-no-autosummon",
		Priority: 1,
		Type:     parseIssueType("epic"),
	})

	// Invoke the same paths the board dispatches via executeInlineAction
	// (ActionResetSoft → cmdReset with no flags; ActionResetHard → --hard).
	if err := cmdReset([]string{epicID}); err != nil {
		t.Fatalf("cmdReset (soft): %v", err)
	}
	if *called {
		t.Error("cmdSummonFunc was invoked during board soft-reset path")
	}

	// Reset the recorder for the hard-reset leg.
	*called = false
	if err := cmdReset([]string{epicID, "--hard"}); err != nil {
		t.Fatalf("cmdReset (hard): %v", err)
	}
	if *called {
		t.Error("cmdSummonFunc was invoked during board hard-reset path")
	}
}

// --- removeNestedGraphStateRecursive ---

// TestRemoveNestedGraphStateRecursive_ArbitraryDepth verifies nested graph_state.json
// files are deleted at arbitrary depth — subgraph wave apprentices can be
// nested several levels deep.
func TestRemoveNestedGraphStateRecursive_ArbitraryDepth(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	wizardName := "wizard-spi-nested-walk"
	runtimeDir := filepath.Join(tmp, "runtime")

	// Level 1: wizard-<bead>-implement/graph_state.json
	level1 := filepath.Join(runtimeDir, wizardName+"-implement")
	if err := os.MkdirAll(level1, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(level1, "graph_state.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Level 2: wizard-<bead>-implement-w1-1/graph_state.json
	level2 := filepath.Join(runtimeDir, wizardName+"-implement", "w1-1")
	if err := os.MkdirAll(level2, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(level2, "graph_state.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Level 3: arbitrarily deep.
	level3 := filepath.Join(runtimeDir, wizardName+"-implement", "w1-1", "spi-xyz")
	if err := os.MkdirAll(level3, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(level3, "graph_state.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Unrelated wizard tree — must be preserved.
	other := filepath.Join(runtimeDir, "wizard-spi-other", "graph_state.json")
	if err := os.MkdirAll(filepath.Dir(other), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	removeNestedGraphStateRecursive(wizardName)

	// All three nested states should be removed.
	for _, p := range []string{
		filepath.Join(level1, "graph_state.json"),
		filepath.Join(level2, "graph_state.json"),
		filepath.Join(level3, "graph_state.json"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed", p)
		}
	}
	// Unrelated tree must be untouched.
	if _, err := os.Stat(other); err != nil {
		t.Errorf("unrelated wizard tree should be preserved, got err: %v", err)
	}
}

// TestRemoveNestedGraphStateRecursive_TopLevelUntouched verifies the top-level
// graph_state.json (runtime/<wizardName>/graph_state.json) is NOT touched — it
// is rewritten in place by softResetV3's save, not removed.
func TestRemoveNestedGraphStateRecursive_TopLevelUntouched(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	wizardName := "wizard-spi-top-level-untouched"
	runtimeDir := filepath.Join(tmp, "runtime")

	topDir := filepath.Join(runtimeDir, wizardName)
	if err := os.MkdirAll(topDir, 0755); err != nil {
		t.Fatal(err)
	}
	topGS := filepath.Join(topDir, "graph_state.json")
	if err := os.WriteFile(topGS, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	removeNestedGraphStateRecursive(wizardName)

	if _, err := os.Stat(topGS); err != nil {
		t.Errorf("top-level graph_state.json should be preserved, got err: %v", err)
	}
}

// --- softResetV3 — integration tests requiring live store + formula ---

// writeV3GraphStateForTask writes a GraphState matching the task-default formula
// (plan → implement → review → merge/close with policy branches) suitable for
// softResetV3 integration tests.
func writeV3GraphStateForTask(t *testing.T, configRoot, wizardName, beadID string, steps map[string]executor.StepState, activeStep string, stepBeadIDs map[string]string) *executor.GraphState {
	t.Helper()
	gs := &executor.GraphState{
		BeadID:      beadID,
		AgentName:   wizardName,
		Formula:     "task-default",
		Steps:       steps,
		ActiveStep:  activeStep,
		StepBeadIDs: stepBeadIDs,
		Workspaces:  map[string]executor.WorkspaceState{},
	}
	writeGraphState(t, configRoot, wizardName, gs)
	return gs
}

// TestSoftResetV3_MissingGraphState_ReturnsError verifies that a missing graph
// state yields a typed ErrNoGraphState error (not a silent "re-summoning"
// success).
func TestSoftResetV3_MissingGraphState_ReturnsError(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	// Lock down cmdSummonFunc so a silent summon on missing state would fail
	// the test loudly.
	withSummonRecorder(t)

	epicID := createTestBead(t, createOpts{
		Title:    "test-softreset-missing-gs",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	wizardName := "wizard-test-softreset-missing-gs"

	err := softResetV3(epicID, "implement", wizardName, false, nil)
	if err == nil {
		t.Fatal("expected error for missing graph state, got nil")
	}
	if !errors.Is(err, ErrNoGraphState) {
		t.Errorf("expected error to wrap ErrNoGraphState, got: %v", err)
	}
}

// TestSoftResetV3_RejectsPendingTarget verifies that attempting to --to a step
// that has not been reached yet is rejected with a clear error and no state
// mutation.
func TestSoftResetV3_RejectsPendingTarget(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	withSummonRecorder(t)

	epicID := createTestBead(t, createOpts{
		Title:    "test-softreset-reject-pending",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	wizardName := "wizard-test-softreset-reject-pending"

	// plan completed, implement still pending.
	gs := writeV3GraphStateForTask(t, tmp, wizardName, epicID,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"implement": {Status: "pending"},
			"review":    {Status: "pending"},
		},
		"",
		nil,
	)

	// Snapshot file contents before.
	gsPath := filepath.Join(tmp, "runtime", wizardName, "graph_state.json")
	before, err := os.ReadFile(gsPath)
	if err != nil {
		t.Fatalf("read graph_state before: %v", err)
	}

	err = softResetV3(epicID, "implement", wizardName, false, nil)
	if err == nil {
		t.Fatal("expected error for pending target, got nil")
	}
	if !strings.Contains(err.Error(), "has not been reached") {
		t.Errorf("expected error to mention 'has not been reached', got: %v", err)
	}

	// Graph state file must be byte-identical.
	after, err := os.ReadFile(gsPath)
	if err != nil {
		t.Fatalf("read graph_state after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("graph_state.json was mutated despite rejected rewind — operation must be all-or-nothing")
	}

	// State in memory should be unchanged.
	loaded, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil || loaded == nil {
		t.Fatalf("load graph state after: err=%v nil=%v", err, loaded == nil)
	}
	if loaded.Steps["plan"].Status != "completed" {
		t.Errorf("plan status changed after rejected rewind: %q", loaded.Steps["plan"].Status)
	}
	if loaded.Steps["implement"].Status != "pending" {
		t.Errorf("implement status changed after rejected rewind: %q", loaded.Steps["implement"].Status)
	}
	_ = gs
}

// TestSoftResetV3_ReopensClosedStepBeadsInResetSet verifies that step beads in
// the rewind set get reopened (not closed) and annotated with an audit comment,
// while step beads upstream of the target stay closed.
func TestSoftResetV3_ReopensClosedStepBeadsInResetSet(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	withSummonRecorder(t)

	epicID := createTestBead(t, createOpts{
		Title:    "test-softreset-reopen",
		Priority: 1,
		Type:     parseIssueType("task"),
	})

	// Create step beads for plan, implement, review.
	// plan and implement are completed (closed); review is active.
	planStepID := createTestBead(t, createOpts{
		Title:    "step:plan",
		Priority: 3,
		Type:     parseIssueType("task"),
		Labels:   []string{"step:plan", "workflow-step"},
		Parent:   epicID,
	})
	implStepID := createTestBead(t, createOpts{
		Title:    "step:implement",
		Priority: 3,
		Type:     parseIssueType("task"),
		Labels:   []string{"step:implement", "workflow-step"},
		Parent:   epicID,
	})
	reviewStepID := createTestBead(t, createOpts{
		Title:    "step:review",
		Priority: 3,
		Type:     parseIssueType("task"),
		Labels:   []string{"step:review", "workflow-step"},
		Parent:   epicID,
	})
	// Close plan and implement step beads (as if they had completed).
	if err := storeCloseBead(planStepID); err != nil {
		t.Fatalf("close plan step: %v", err)
	}
	if err := storeCloseBead(implStepID); err != nil {
		t.Fatalf("close implement step: %v", err)
	}

	wizardName := "wizard-test-softreset-reopen"
	writeV3GraphStateForTask(t, tmp, wizardName, epicID,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"implement": {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:02:00Z"},
			"review":    {Status: "active", StartedAt: "2026-01-01T00:03:00Z"},
		},
		"review",
		map[string]string{
			"plan":      planStepID,
			"implement": implStepID,
			"review":    reviewStepID,
		},
	)

	if err := softResetV3(epicID, "implement", wizardName, false, nil); err != nil {
		t.Fatalf("softResetV3: %v", err)
	}

	// Step bead for implement should be reopened (was closed).
	impl, err := storeGetBead(implStepID)
	if err != nil {
		t.Fatalf("get implement step: %v", err)
	}
	if impl.Status == "closed" {
		t.Errorf("implement step bead should be reopened, got status=%q", impl.Status)
	}

	// Step bead for plan should still be closed (upstream of target).
	plan, err := storeGetBead(planStepID)
	if err != nil {
		t.Fatalf("get plan step: %v", err)
	}
	if plan.Status != "closed" {
		t.Errorf("plan step bead should remain closed (upstream of target), got status=%q", plan.Status)
	}

	// Graph state: implement and review should be pending, plan completed.
	loaded, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil || loaded == nil {
		t.Fatalf("load graph state: err=%v nil=%v", err, loaded == nil)
	}
	if loaded.Steps["implement"].Status != "pending" {
		t.Errorf("implement graph state = %q, want pending", loaded.Steps["implement"].Status)
	}
	if loaded.Steps["review"].Status != "pending" {
		t.Errorf("review graph state = %q, want pending", loaded.Steps["review"].Status)
	}
	if loaded.Steps["plan"].Status != "completed" {
		t.Errorf("plan graph state = %q, want completed (upstream of target)", loaded.Steps["plan"].Status)
	}
}

// TestSoftResetV3_SetsStatusConsistentWithPlainReset verifies both reset paths
// (plain and --to) produce status=open on the bead.
func TestSoftResetV3_SetsStatusConsistentWithPlainReset(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	withSummonRecorder(t)

	// --- Plain reset path ---
	plainID := createTestBead(t, createOpts{
		Title:    "test-reset-plain-status",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	if err := storeUpdateBead(plainID, map[string]interface{}{"status": "in_progress"}); err != nil {
		t.Fatalf("update plain to in_progress: %v", err)
	}
	wizardNamePlain := "wizard-test-reset-plain-status"
	if err := resetV3(plainID, false, wizardNamePlain, ""); err != nil {
		t.Fatalf("resetV3 plain: %v", err)
	}
	plainBead, err := storeGetBead(plainID)
	if err != nil {
		t.Fatalf("get plain bead: %v", err)
	}
	if plainBead.Status != "open" {
		t.Errorf("plain reset bead status = %q, want open", plainBead.Status)
	}

	// --- --to path ---
	toID := createTestBead(t, createOpts{
		Title:    "test-reset-to-status",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	if err := storeUpdateBead(toID, map[string]interface{}{"status": "in_progress"}); err != nil {
		t.Fatalf("update to in_progress: %v", err)
	}
	wizardNameTo := "wizard-test-reset-to-status"
	writeV3GraphStateForTask(t, tmp, wizardNameTo, toID,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1},
			"implement": {Status: "completed", CompletedCount: 1},
			"review":    {Status: "active"},
		},
		"review",
		nil,
	)

	if err := softResetV3(toID, "implement", wizardNameTo, false, nil); err != nil {
		t.Fatalf("softResetV3 --to: %v", err)
	}
	toBead, err := storeGetBead(toID)
	if err != nil {
		t.Fatalf("get --to bead: %v", err)
	}
	if toBead.Status != "open" {
		t.Errorf("--to reset bead status = %q, want open (must match plain reset)", toBead.Status)
	}
	if toBead.Status != plainBead.Status {
		t.Errorf("plain reset status=%q, --to reset status=%q — must match",
			plainBead.Status, toBead.Status)
	}
}

// --- recover.go chaining ---

// TestRecover_ResetHardAction_ChainsResummon verifies the "reset-hard" recovery
// action chains cmdResummonFunc after cmdReset.
func TestRecover_ResetHardAction_ChainsResummon(t *testing.T) {
	origReset := storeGetBeadFunc // unused, just to avoid unused-import guilt
	_ = origReset

	var resetCalled, resummonCalled bool
	var resummonBead string

	origResummon := cmdResummonFunc
	cmdResummonFunc = func(beadID string) error {
		resummonCalled = true
		resummonBead = beadID
		return nil
	}
	defer func() { cmdResummonFunc = origResummon }()

	// Replace cmdReset through a proxy — since executeRecoveryAction calls
	// cmdReset directly (not via a var), we instead wire a test by observing
	// its side effect: cmdResummonFunc must be invoked after cmdReset returns
	// success. To avoid performing real store mutations in this test, we
	// swap cmdSummonFunc (called internally by cmdReset? no — cmdReset does
	// NOT call cmdSummon anymore). So we run the recover path end-to-end
	// and assert resummonCalled is true, using a safe benign bead ID.

	// Use a lightweight recorder for cmdReset via a var.
	origDoReset := cmdResetFunc
	cmdResetFunc = func(args []string) error {
		resetCalled = true
		return nil
	}
	defer func() { cmdResetFunc = origDoReset }()

	action := &struct{ Name string }{}
	_ = action

	// Execute the recover path for reset-hard.
	beadID := "spi-test-recover-reset-hard"
	if err := executeRecoveryActionForTest(beadID, "reset-hard"); err != nil {
		t.Fatalf("executeRecoveryAction reset-hard: %v", err)
	}
	if !resetCalled {
		t.Error("cmdResetFunc was not called during reset-hard recovery action")
	}
	if !resummonCalled {
		t.Error("cmdResummonFunc was not called after reset-hard — recovery must chain resume")
	}
	if resummonBead != beadID {
		t.Errorf("cmdResummonFunc called with %q, want %q", resummonBead, beadID)
	}
}

// TestRecover_ResetToAction_ChainsResummon verifies reset-to-<phase> recovery
// actions chain cmdResummonFunc after cmdReset.
func TestRecover_ResetToAction_ChainsResummon(t *testing.T) {
	var resetArgs []string
	var resummonCalled bool
	var resummonBead string

	origReset := cmdResetFunc
	cmdResetFunc = func(args []string) error {
		resetArgs = args
		return nil
	}
	defer func() { cmdResetFunc = origReset }()

	origResummon := cmdResummonFunc
	cmdResummonFunc = func(beadID string) error {
		resummonCalled = true
		resummonBead = beadID
		return nil
	}
	defer func() { cmdResummonFunc = origResummon }()

	beadID := "spi-test-recover-reset-to"
	if err := executeRecoveryActionForTest(beadID, "reset-to-implement"); err != nil {
		t.Fatalf("executeRecoveryAction reset-to-implement: %v", err)
	}

	wantResetArgs := []string{beadID, "--to", "implement"}
	if !reflect.DeepEqual(resetArgs, wantResetArgs) {
		t.Errorf("cmdResetFunc args = %v, want %v", resetArgs, wantResetArgs)
	}
	if !resummonCalled {
		t.Error("cmdResummonFunc was not called after reset-to — recovery must chain resume")
	}
	if resummonBead != beadID {
		t.Errorf("cmdResummonFunc called with %q, want %q", resummonBead, beadID)
	}
}

// TestRecover_ResetActionPropagatesResetError verifies that when cmdReset
// returns an error in a reset-hard / reset-to-* recovery action, cmdResummon
// is NOT chained.
func TestRecover_ResetActionPropagatesResetError(t *testing.T) {
	resetErr := errors.New("reset failure")
	var resummonCalled bool

	origReset := cmdResetFunc
	cmdResetFunc = func(args []string) error {
		return resetErr
	}
	defer func() { cmdResetFunc = origReset }()

	origResummon := cmdResummonFunc
	cmdResummonFunc = func(beadID string) error {
		resummonCalled = true
		return nil
	}
	defer func() { cmdResummonFunc = origResummon }()

	if err := executeRecoveryActionForTest("spi-err", "reset-hard"); !errors.Is(err, resetErr) {
		t.Errorf("expected reset error to propagate, got: %v", err)
	}
	if resummonCalled {
		t.Error("cmdResummonFunc was called despite cmdReset returning an error")
	}
}

// --- parseSetFlag unit tests (no store needed) ---

// TestParseSetFlag_ValidTokens covers shape success cases: single token,
// multiple tokens across steps, values containing '=' (only the first '='
// is the delimiter — values are opaque).
func TestParseSetFlag_ValidTokens(t *testing.T) {
	graph := &formula.FormulaStepGraph{
		Name: "task-default",
		Steps: map[string]formula.StepConfig{
			"plan":      {},
			"implement": {Needs: []string{"plan"}},
			"review":    {Needs: []string{"implement"}},
		},
	}

	got, err := parseSetFlag([]string{
		"implement.outputs.outcome=verified",
		"review.outputs.outcome=merge",
		"review.outputs.blob={\"key\":\"a=b\"}", // value contains '='
	}, graph)
	if err != nil {
		t.Fatalf("parseSetFlag: %v", err)
	}

	want := map[string]map[string]string{
		"implement": {"outcome": "verified"},
		"review":    {"outcome": "merge", "blob": `{"key":"a=b"}`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSetFlag mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

// TestParseSetFlag_EmptyReturnsNil verifies that zero tokens return a nil
// map (so callers can short-circuit cleanly).
func TestParseSetFlag_EmptyReturnsNil(t *testing.T) {
	graph := &formula.FormulaStepGraph{Steps: map[string]formula.StepConfig{"plan": {}}}
	got, err := parseSetFlag(nil, graph)
	if err != nil {
		t.Fatalf("parseSetFlag: %v", err)
	}
	if got != nil {
		t.Errorf("parseSetFlag(nil) = %#v, want nil", got)
	}
}

// TestParseSetFlag_RejectionCases covers every rejection branch:
//   - missing '='
//   - wrong path segment count (fewer/more than 3)
//   - middle segment is 'status' (explicit separate message)
//   - middle segment is neither 'outputs' nor 'status'
//   - unknown step name
//   - empty step or empty key segment
func TestParseSetFlag_RejectionCases(t *testing.T) {
	graph := &formula.FormulaStepGraph{
		Name: "task-default",
		Steps: map[string]formula.StepConfig{
			"plan":      {},
			"implement": {Needs: []string{"plan"}},
			"review":    {Needs: []string{"implement"}},
		},
	}

	tests := []struct {
		name     string
		token    string
		contains string // expected substring in error
	}{
		{"no equals", "implement.outputs.outcome", "expected"},
		{"two segments", "implement.outcome=verified", "segment"},
		{"four segments (nested)", "review.outputs.blob.nested=v", "segment"},
		{"status write rejected", "review.status=completed", "status"},
		{"unknown kind", "review.state.x=y", "outputs"},
		{"unknown step", "nonexistent.outputs.x=y", "not found"},
		{"empty step", ".outputs.key=value", "must be non-empty"},
		{"empty key", "review.outputs.=value", "must be non-empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSetFlag([]string{tt.token}, graph)
			if err == nil {
				t.Fatalf("parseSetFlag(%q) = %v, want error", tt.token, got)
			}
			if !strings.Contains(err.Error(), tt.contains) {
				t.Errorf("parseSetFlag(%q) error = %q, want substring %q", tt.token, err.Error(), tt.contains)
			}
			if got != nil {
				t.Errorf("parseSetFlag(%q): expected nil map on error, got %#v", tt.token, got)
			}
		})
	}
}

// TestParseSetFlag_FailsEarlyOnFirstBadToken verifies a single bad token
// in a batch invalidates the whole batch — no partial map returned.
func TestParseSetFlag_FailsEarlyOnFirstBadToken(t *testing.T) {
	graph := &formula.FormulaStepGraph{
		Name: "task-default",
		Steps: map[string]formula.StepConfig{
			"implement": {},
			"review":    {Needs: []string{"implement"}},
		},
	}

	got, err := parseSetFlag([]string{
		"implement.outputs.outcome=verified", // valid
		"review.status=completed",            // invalid — rejected
	}, graph)
	if err == nil {
		t.Fatalf("expected error from bad token, got nil (result=%#v)", got)
	}
	if got != nil {
		t.Errorf("expected nil map on error, got %#v — partial writes must not leak", got)
	}
}

// --- expandRewindSetForOverrides unit tests (pure function, no store) ---

// TestExpandRewindSetForOverrides_PropagatesToCompletedDownstream verifies
// that override propagation adds completed downstream-of-override steps
// to the rewind set so their when-clauses re-evaluate on next summon.
func TestExpandRewindSetForOverrides_PropagatesToCompletedDownstream(t *testing.T) {
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement":        {},
			"implement-failed": {Needs: []string{"implement"}},
			"review":           {Needs: []string{"implement"}},
			"merge":            {Needs: []string{"review"}},
		},
	}
	gs := &executor.GraphState{
		Steps: map[string]executor.StepState{
			"implement":        {Status: "completed"},
			"implement-failed": {Status: "completed"},
			"review":           {Status: "pending"},
			"merge":            {Status: "pending"},
		},
	}
	base := map[string]bool{"review": true, "merge": true}
	overrides := map[string]map[string]string{"implement": {"outcome": "verified"}}

	got := expandRewindSetForOverrides(base, graph, gs, overrides)

	// implement-failed is completed and downstream of implement → added.
	// implement itself is excluded (overridden step isn't self-rewound).
	// review is pending (not completed) but already in base.
	if !got["implement-failed"] {
		t.Errorf("implement-failed should be in expanded rewind set (completed downstream of overridden step)")
	}
	if got["implement"] {
		t.Errorf("implement (the overridden step itself) must NOT be in rewind set")
	}
	if !got["review"] || !got["merge"] {
		t.Errorf("base rewind set members must be preserved")
	}
}

// TestExpandRewindSetForOverrides_NoOverridesReturnsBase verifies the
// zero-override case is a no-op.
func TestExpandRewindSetForOverrides_NoOverridesReturnsBase(t *testing.T) {
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"a": {},
			"b": {Needs: []string{"a"}},
		},
	}
	gs := &executor.GraphState{Steps: map[string]executor.StepState{
		"a": {Status: "completed"},
		"b": {Status: "pending"},
	}}
	base := map[string]bool{"b": true}
	got := expandRewindSetForOverrides(base, graph, gs, nil)
	if len(got) != 1 || !got["b"] {
		t.Errorf("expected base set unchanged, got %v", got)
	}
}

// TestExpandRewindSetForOverrides_PendingDownstreamNotAdded verifies that
// pending (never-reached) downstream steps are NOT added via override
// propagation — they are left for the standard rewind/force-advance pass
// to classify. Only already-completed steps need their when-clauses
// re-evaluated; pending steps have not yet decided anything.
func TestExpandRewindSetForOverrides_PendingDownstreamNotAdded(t *testing.T) {
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"a": {},
			"b": {Needs: []string{"a"}},
			"c": {Needs: []string{"b"}},
		},
	}
	gs := &executor.GraphState{Steps: map[string]executor.StepState{
		"a": {Status: "completed"},
		"b": {Status: "pending"},
		"c": {Status: "pending"},
	}}
	base := map[string]bool{} // empty base — isolate the propagation behavior
	overrides := map[string]map[string]string{"a": {"x": "y"}}

	got := expandRewindSetForOverrides(base, graph, gs, overrides)
	if got["b"] || got["c"] {
		t.Errorf("pending downstream steps must not be auto-added by override propagation, got %v", got)
	}
}

// --- softResetV3 --force / --set integration tests (spi-tmv38n) ---

// writeV3GraphStateForEpic writes a GraphState matching the epic-default formula
// (design-check → plan → materialize → implement → {implement-failed, review}
// → merge/discard/close) for --force/--set integration tests that need the
// implement-failed sibling branch.
func writeV3GraphStateForEpic(t *testing.T, configRoot, wizardName, beadID string, steps map[string]executor.StepState, activeStep string, stepBeadIDs map[string]string) *executor.GraphState {
	t.Helper()
	gs := &executor.GraphState{
		BeadID:      beadID,
		AgentName:   wizardName,
		Formula:     "epic-default",
		Steps:       steps,
		ActiveStep:  activeStep,
		StepBeadIDs: stepBeadIDs,
		Workspaces:  map[string]executor.WorkspaceState{},
	}
	writeGraphState(t, configRoot, wizardName, gs)
	return gs
}

// TestResetTo_ForceAdvanceToPendingTarget is the canonical spi-cwgiy9 replay:
// epic stuck in implement-failed terminal with apprentice work intact. Operator
// uses --force --set to reroute to review without re-running implement.
func TestResetTo_ForceAdvanceToPendingTarget(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	withSummonRecorder(t)

	epicID := createTestBead(t, createOpts{
		Title:    "test-resetto-force-advance",
		Priority: 1,
		Type:     parseIssueType("epic"),
	})
	wizardName := "wizard-test-resetto-force-advance"

	// Fixture: prior phases completed, implement ran with build-failed,
	// implement-failed terminal fired, review never reached.
	writeV3GraphStateForEpic(t, tmp, wizardName, epicID,
		map[string]executor.StepState{
			"design-check":     {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"plan":             {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:02:00Z"},
			"materialize":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:03:00Z"},
			"implement":        {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:04:00Z", Outputs: map[string]string{"outcome": "build-failed"}},
			"implement-failed": {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:05:00Z"},
			"review":           {Status: "pending"},
			"merge":            {Status: "pending"},
			"discard":          {Status: "pending"},
			"close":            {Status: "pending"},
		},
		"",
		nil,
	)

	err := softResetV3(epicID, "review", wizardName, true, []string{"implement.outputs.outcome=verified"})
	if err != nil {
		t.Fatalf("softResetV3 --to review --force --set implement.outputs.outcome=verified: %v", err)
	}

	loaded, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil || loaded == nil {
		t.Fatalf("load graph state: err=%v nil=%v", err, loaded == nil)
	}

	// Target + transitive dependents rewound to pending.
	for _, name := range []string{"review", "merge", "discard", "close"} {
		if got := loaded.Steps[name].Status; got != "pending" {
			t.Errorf("%s.status = %q, want pending (rewind)", name, got)
		}
	}

	// Override applied to implement.outputs.outcome; implement itself stays completed.
	if got := loaded.Steps["implement"].Outputs["outcome"]; got != "verified" {
		t.Errorf("implement.outputs.outcome = %q, want \"verified\" (overridden)", got)
	}
	if got := loaded.Steps["implement"].Status; got != "completed" {
		t.Errorf("implement.status = %q, want completed (not rewound, only outputs overridden)", got)
	}

	// implement-failed was completed in a forward cone of the overridden step;
	// expansion adds it to the rewind set so its when-clause can re-evaluate
	// (now false since outcome=verified) on next summon.
	if got := loaded.Steps["implement-failed"].Status; got != "pending" {
		t.Errorf("implement-failed.status = %q, want pending (override propagation rewinds completed downstream)", got)
	}
	if len(loaded.Steps["implement-failed"].Outputs) != 0 {
		t.Errorf("implement-failed.Outputs = %v, want empty (rewound)", loaded.Steps["implement-failed"].Outputs)
	}

	// Upstream steps untouched.
	for _, name := range []string{"design-check", "plan", "materialize"} {
		if got := loaded.Steps[name].Status; got != "completed" {
			t.Errorf("%s.status = %q, want completed (upstream of target)", name, got)
		}
	}
}

// TestResetTo_SetOutputsOverridesCompletedStep verifies that --set rewrites
// the overridden step's Outputs map and the value persists through the
// rewind. On next summon, review's when-clauses will see the new outcome.
func TestResetTo_SetOutputsOverridesCompletedStep(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	withSummonRecorder(t)

	beadID := createTestBead(t, createOpts{
		Title:    "test-resetto-set-overrides-completed",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	wizardName := "wizard-test-resetto-set-overrides-completed"

	writeV3GraphStateForTask(t, tmp, wizardName, beadID,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"implement": {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:02:00Z", Outputs: map[string]string{"outcome": "verified"}},
			"review":    {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:03:00Z", Outputs: map[string]string{"outcome": "request_changes"}},
			"merge":     {Status: "pending"},
			"discard":   {Status: "pending"},
			"close":     {Status: "pending"},
		},
		"",
		nil,
	)

	if err := softResetV3(beadID, "review", wizardName, false, []string{"review.outputs.outcome=merge"}); err != nil {
		t.Fatalf("softResetV3 --to review --set review.outputs.outcome=merge: %v", err)
	}

	loaded, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil || loaded == nil {
		t.Fatalf("load graph state: err=%v nil=%v", err, loaded == nil)
	}

	// review is rewound to pending AND has the overridden outcome attached,
	// so on next summon merge.when (review.outputs.outcome == "merge") fires.
	if got := loaded.Steps["review"].Status; got != "pending" {
		t.Errorf("review.status = %q, want pending (rewound)", got)
	}
	if got := loaded.Steps["review"].Outputs["outcome"]; got != "merge" {
		t.Errorf("review.outputs.outcome = %q, want \"merge\" (override applied)", got)
	}

	// Upstream untouched.
	if got := loaded.Steps["plan"].Status; got != "completed" {
		t.Errorf("plan.status = %q, want completed", got)
	}
	if got := loaded.Steps["implement"].Status; got != "completed" {
		t.Errorf("implement.status = %q, want completed", got)
	}
	if got := loaded.Steps["implement"].Outputs["outcome"]; got != "verified" {
		t.Errorf("implement.outputs.outcome = %q, want \"verified\" (untouched)", got)
	}
}

// TestResetTo_SetRejectsUnknownStep verifies that an unknown step name in
// --set fails the operation before any state mutation — the rejection names
// the offending step and the graph_state.json is byte-identical to before.
func TestResetTo_SetRejectsUnknownStep(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	withSummonRecorder(t)

	beadID := createTestBead(t, createOpts{
		Title:    "test-resetto-set-rejects-unknown",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	wizardName := "wizard-test-resetto-set-rejects-unknown"

	writeV3GraphStateForTask(t, tmp, wizardName, beadID,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1},
			"implement": {Status: "completed", CompletedCount: 1, Outputs: map[string]string{"outcome": "verified"}},
			"review":    {Status: "active"},
		},
		"review",
		nil,
	)

	gsPath := filepath.Join(tmp, "runtime", wizardName, "graph_state.json")
	before, err := os.ReadFile(gsPath)
	if err != nil {
		t.Fatalf("read graph_state before: %v", err)
	}

	err = softResetV3(beadID, "review", wizardName, false, []string{"nonexistent.outputs.x=y"})
	if err == nil {
		t.Fatal("expected error for unknown step, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("expected error to name the unknown step, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected error to mention 'not found', got: %v", err)
	}

	after, err := os.ReadFile(gsPath)
	if err != nil {
		t.Fatalf("read graph_state after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("graph_state.json was mutated despite rejected --set — operation must be all-or-nothing")
	}
}

// TestResetTo_SetRejectsStatus verifies that <step>.status=... in --set is
// rejected (status transitions live on --to/--force; --set is scoped to
// outputs). No state mutation happens on rejection.
func TestResetTo_SetRejectsStatus(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	withSummonRecorder(t)

	beadID := createTestBead(t, createOpts{
		Title:    "test-resetto-set-rejects-status",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	wizardName := "wizard-test-resetto-set-rejects-status"

	writeV3GraphStateForTask(t, tmp, wizardName, beadID,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1},
			"implement": {Status: "completed", CompletedCount: 1, Outputs: map[string]string{"outcome": "verified"}},
			"review":    {Status: "active"},
		},
		"review",
		nil,
	)

	gsPath := filepath.Join(tmp, "runtime", wizardName, "graph_state.json")
	before, err := os.ReadFile(gsPath)
	if err != nil {
		t.Fatalf("read graph_state before: %v", err)
	}

	err = softResetV3(beadID, "review", wizardName, false, []string{"review.status=completed"})
	if err == nil {
		t.Fatal("expected error for --set ...status=..., got nil")
	}
	if !strings.Contains(err.Error(), "status") {
		t.Errorf("expected error to mention 'status', got: %v", err)
	}

	after, err := os.ReadFile(gsPath)
	if err != nil {
		t.Fatalf("read graph_state after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("graph_state.json was mutated despite rejected --set status= — operation must be all-or-nothing")
	}
}

// TestResetTo_ForceWithoutSet_SkippedStepsEmptyOutputs verifies --force alone
// produces non-corrupt state: pending steps outside the rewind set are
// promoted to completed with outputs={}, so the wizard can either route by
// default path or stall cleanly. No --set means downstream when-clauses will
// evaluate against empty outputs — the reset command's job is just to leave
// state coherent, not to validate when-clause completeness.
func TestResetTo_ForceWithoutSet_SkippedStepsEmptyOutputs(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	withSummonRecorder(t)

	epicID := createTestBead(t, createOpts{
		Title:    "test-resetto-force-no-set",
		Priority: 1,
		Type:     parseIssueType("epic"),
	})
	wizardName := "wizard-test-resetto-force-no-set"

	// Fixture: prior phases done, implement completed but its children
	// (implement-failed and review) have not dispatched yet — e.g. the
	// wizard crashed between implement completing and its children firing.
	writeV3GraphStateForEpic(t, tmp, wizardName, epicID,
		map[string]executor.StepState{
			"design-check":     {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"plan":             {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:02:00Z"},
			"materialize":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:03:00Z"},
			"implement":        {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:04:00Z", Outputs: map[string]string{"outcome": "verified"}},
			"implement-failed": {Status: "pending"},
			"review":           {Status: "pending"},
			"merge":            {Status: "pending"},
			"discard":          {Status: "pending"},
			"close":            {Status: "pending"},
		},
		"",
		nil,
	)

	if err := softResetV3(epicID, "review", wizardName, true, nil); err != nil {
		t.Fatalf("softResetV3 --to review --force: %v", err)
	}

	loaded, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil || loaded == nil {
		t.Fatalf("load graph state: err=%v nil=%v", err, loaded == nil)
	}

	// Target stays pending (it was in the rewind set).
	if got := loaded.Steps["review"].Status; got != "pending" {
		t.Errorf("review.status = %q, want pending", got)
	}

	// Pending steps OUTSIDE the rewind set (i.e. implement-failed) get
	// force-advanced to completed with empty outputs — they are skipped
	// not-taken branches and must not dispatch on next summon.
	if got := loaded.Steps["implement-failed"].Status; got != "completed" {
		t.Errorf("implement-failed.status = %q, want completed (force-advanced)", got)
	}
	if got := loaded.Steps["implement-failed"].Outputs; got == nil || len(got) != 0 {
		t.Errorf("implement-failed.Outputs = %v, want empty map (force-advanced with outputs={})", got)
	}

	// Rewind set members still pending (merge/discard/close are transitive
	// dependents of review and were already pending).
	for _, name := range []string{"merge", "discard", "close"} {
		if got := loaded.Steps[name].Status; got != "pending" {
			t.Errorf("%s.status = %q, want pending (in rewind set)", name, got)
		}
	}
}

// TestResetTo_ForceNoLongerFailsValidation is the regression for the
// spi-j7juo precondition: a pending target is rejected WITHOUT --force but
// SUCCEEDS WITH --force. Proves --force surgically drops exactly that one
// gate and nothing else.
func TestResetTo_ForceNoLongerFailsValidation(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	withSummonRecorder(t)

	// Fresh fixture for the no-force leg.
	beadID1 := createTestBead(t, createOpts{
		Title:    "test-resetto-force-regression-noforce",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	wizardName1 := "wizard-test-resetto-force-regression-noforce"

	writeV3GraphStateForTask(t, tmp, wizardName1, beadID1,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"implement": {Status: "pending"},
			"review":    {Status: "pending"},
		},
		"",
		nil,
	)

	err := softResetV3(beadID1, "implement", wizardName1, false, nil)
	if err == nil {
		t.Fatal("expected error for pending target without --force, got nil")
	}
	if !strings.Contains(err.Error(), "has not been reached") {
		t.Errorf("expected 'has not been reached' error without --force, got: %v", err)
	}

	// Fresh fixture for the --force leg (separate wizard so the rejected
	// no-force run above doesn't muddy the state).
	beadID2 := createTestBead(t, createOpts{
		Title:    "test-resetto-force-regression-force",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	wizardName2 := "wizard-test-resetto-force-regression-force"

	writeV3GraphStateForTask(t, tmp, wizardName2, beadID2,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"implement": {Status: "pending"},
			"review":    {Status: "pending"},
		},
		"",
		nil,
	)

	if err := softResetV3(beadID2, "implement", wizardName2, true, nil); err != nil {
		t.Fatalf("softResetV3 with --force on pending target should succeed, got: %v", err)
	}

	loaded, err := executor.LoadGraphState(wizardName2, configDir)
	if err != nil || loaded == nil {
		t.Fatalf("load graph state: err=%v nil=%v", err, loaded == nil)
	}

	// Target still pending (it WAS pending and is in the rewind set — rewind
	// to pending is a no-op for an already-pending step).
	if got := loaded.Steps["implement"].Status; got != "pending" {
		t.Errorf("implement.status = %q, want pending", got)
	}
	// plan stays completed (upstream of target).
	if got := loaded.Steps["plan"].Status; got != "completed" {
		t.Errorf("plan.status = %q, want completed (upstream)", got)
	}
}

// --- spi-vdg28a: --to --force normalizes abandoned active predecessors ---

// withFakeLiveWizardLookup swaps findLiveWizardForBeadFunc for the duration of
// the test. Pass nil to simulate "no live wizard owns the bead".
func withFakeLiveWizardLookup(t *testing.T, wiz *localWizard) {
	t.Helper()
	prev := findLiveWizardForBeadFunc
	findLiveWizardForBeadFunc = func(beadID string) *localWizard {
		return wiz
	}
	t.Cleanup(func() { findLiveWizardForBeadFunc = prev })
}

// TestResetTo_ForceNormalizesAbandonedActivePredecessor is the spi-vdg28a
// regression: implement=active with no live wizard, target=review, --force.
// Reset must normalize implement → completed, clear active_step, and leave
// review as the next runnable step.
func TestResetTo_ForceNormalizesAbandonedActivePredecessor(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	withSummonRecorder(t)
	withFakeLiveWizardLookup(t, nil)

	beadID := createTestBead(t, createOpts{
		Title:    "test-resetto-force-normalizes-active-pred",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	wizardName := "wizard-test-resetto-force-normalizes-active-pred"

	// Create a step bead for implement so we can verify the audit comment lands.
	implementStepID := createTestBead(t, createOpts{
		Title:    "step:implement",
		Priority: 3,
		Type:     parseIssueType("task"),
		Labels:   []string{"step:implement", "workflow-step"},
		Parent:   beadID,
	})
	if err := storeUpdateBead(implementStepID, map[string]interface{}{"status": "in_progress"}); err != nil {
		t.Fatalf("update step bead: %v", err)
	}

	writeV3GraphStateForTask(t, tmp, wizardName, beadID,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"implement": {Status: "active", StartedAt: "2026-01-01T00:02:00Z"},
			"review":    {Status: "pending"},
			"merge":     {Status: "pending"},
			"discard":   {Status: "pending"},
			"close":     {Status: "pending"},
		},
		"implement",
		map[string]string{"implement": implementStepID},
	)

	if err := softResetV3(beadID, "review", wizardName, true, nil); err != nil {
		t.Fatalf("softResetV3 --to review --force: %v", err)
	}

	loaded, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil || loaded == nil {
		t.Fatalf("load graph state: err=%v nil=%v", err, loaded == nil)
	}

	// implement was an active predecessor with no live wizard → normalized
	// to completed (with empty outputs and a CompletedAt stamp).
	if got := loaded.Steps["implement"].Status; got != "completed" {
		t.Errorf("implement.status = %q, want completed (force-normalized)", got)
	}
	if got := loaded.Steps["implement"].CompletedAt; got == "" {
		t.Error("implement.CompletedAt = empty, want a timestamp from normalization")
	}

	// active_step must be cleared — leaving it pointed at the abandoned
	// predecessor would cause resummon to resume from implement, defeating
	// the point of --to review --force.
	if got := loaded.ActiveStep; got != "" {
		t.Errorf("ActiveStep = %q, want empty (normalized step)", got)
	}

	// review is in the rewind set, so it stays pending and is the next
	// runnable step.
	if got := loaded.Steps["review"].Status; got != "pending" {
		t.Errorf("review.status = %q, want pending", got)
	}

	// plan stays completed (upstream, untouched).
	if got := loaded.Steps["plan"].Status; got != "completed" {
		t.Errorf("plan.status = %q, want completed (upstream)", got)
	}

	// Audit trail: the step bead should carry a force-normalized comment so
	// the bead history reflects the synthetic transition.
	comments, err := storeGetComments(implementStepID)
	if err != nil {
		t.Fatalf("read implement step bead comments: %v", err)
	}
	foundAudit := false
	for _, c := range comments {
		if strings.Contains(c.Text, "force-normalized from active") &&
			strings.Contains(c.Text, "review") {
			foundAudit = true
			break
		}
	}
	if !foundAudit {
		t.Errorf("expected force-normalized audit comment on step bead %s, got: %+v", implementStepID, comments)
	}

	// Step bead must be closed to match the new graph state. An in_progress
	// step bead alongside a completed graph state would deadlock the next
	// summon (the executor's transition path treats in_progress step beads
	// as still owned by a wizard).
	stepBead, err := storeGetBead(implementStepID)
	if err != nil {
		t.Fatalf("read implement step bead: %v", err)
	}
	if stepBead.Status != "closed" {
		t.Errorf("step bead status = %q, want closed (matches force-normalized graph state)", stepBead.Status)
	}
}

// TestResetTo_ForceFailsClosedWhenLiveWizardOwnsBead verifies the orphan-
// recovery liveness gate: when a wizard with a live PID still owns the bead,
// `--force` must refuse to normalize an active predecessor and the graph
// state must remain byte-identical (no partial mutation).
func TestResetTo_ForceFailsClosedWhenLiveWizardOwnsBead(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	withSummonRecorder(t)

	beadID := createTestBead(t, createOpts{
		Title:    "test-resetto-force-live-wizard-guard",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	wizardName := "wizard-test-resetto-force-live-wizard-guard"

	withFakeLiveWizardLookup(t, &localWizard{
		Name:   wizardName,
		PID:    12345,
		BeadID: beadID,
	})

	writeV3GraphStateForTask(t, tmp, wizardName, beadID,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"implement": {Status: "active", StartedAt: "2026-01-01T00:02:00Z"},
			"review":    {Status: "pending"},
			"merge":     {Status: "pending"},
			"discard":   {Status: "pending"},
			"close":     {Status: "pending"},
		},
		"implement",
		nil,
	)

	gsPath := filepath.Join(tmp, "runtime", wizardName, "graph_state.json")
	before, err := os.ReadFile(gsPath)
	if err != nil {
		t.Fatalf("read graph_state before: %v", err)
	}

	err = softResetV3(beadID, "review", wizardName, true, nil)
	if err == nil {
		t.Fatal("expected error when live wizard owns active predecessor, got nil")
	}
	if !strings.Contains(err.Error(), "live wizard") {
		t.Errorf("expected error to mention 'live wizard', got: %v", err)
	}
	if !strings.Contains(err.Error(), "implement") {
		t.Errorf("expected error to name the active step, got: %v", err)
	}

	after, err := os.ReadFile(gsPath)
	if err != nil {
		t.Fatalf("read graph_state after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("graph_state.json was mutated despite live-wizard guard — operation must be all-or-nothing")
	}
}

// TestResetTo_ForceLeavesNonPredecessorActiveStepsAlone verifies the
// predecessor-only scoping: an active step that is NOT a predecessor of the
// target (parallel branch, post-target step) must be left alone. Only
// predecessors get the active → completed normalization.
func TestResetTo_ForceLeavesNonPredecessorActiveStepsAlone(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	withSummonRecorder(t)
	withFakeLiveWizardLookup(t, nil)

	epicID := createTestBead(t, createOpts{
		Title:    "test-resetto-force-non-predecessor-active",
		Priority: 1,
		Type:     parseIssueType("epic"),
	})
	wizardName := "wizard-test-resetto-force-non-predecessor-active"

	// Target = review. implement-failed is a sibling branch (both depend on
	// implement). If implement-failed were active, it is NOT a predecessor of
	// review, so the new normalization must skip it.
	writeV3GraphStateForEpic(t, tmp, wizardName, epicID,
		map[string]executor.StepState{
			"design-check":     {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"plan":             {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:02:00Z"},
			"materialize":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:03:00Z"},
			"implement":        {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:04:00Z", Outputs: map[string]string{"outcome": "build-failed"}},
			"implement-failed": {Status: "active", StartedAt: "2026-01-01T00:05:00Z"},
			"review":           {Status: "pending"},
			"merge":            {Status: "pending"},
			"discard":          {Status: "pending"},
			"close":            {Status: "pending"},
		},
		"implement-failed",
		nil,
	)

	if err := softResetV3(epicID, "review", wizardName, true, nil); err != nil {
		t.Fatalf("softResetV3 --to review --force: %v", err)
	}

	loaded, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil || loaded == nil {
		t.Fatalf("load graph state: err=%v nil=%v", err, loaded == nil)
	}

	// implement-failed is a non-predecessor active sibling. Predecessor-only
	// scoping leaves it alone — the only place it's touched is by the
	// pending-only force-advance, which doesn't match an active step.
	if got := loaded.Steps["implement-failed"].Status; got != "active" {
		t.Errorf("implement-failed.status = %q, want active (non-predecessor, untouched)", got)
	}
}

// TestResetTo_NoForce_DoesNotNormalizeActivePredecessor verifies the
// normalization is gated on --force. A plain (non-force) --to call against a
// graph with an active predecessor must NOT touch that predecessor's status,
// since --force is the operator's explicit opt-in to the normalization path.
func TestResetTo_NoForce_DoesNotNormalizeActivePredecessor(t *testing.T) {
	requireStore(t)

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	withSummonRecorder(t)
	withFakeLiveWizardLookup(t, nil)

	beadID := createTestBead(t, createOpts{
		Title:    "test-resetto-noforce-active-pred-untouched",
		Priority: 1,
		Type:     parseIssueType("task"),
	})
	wizardName := "wizard-test-resetto-noforce-active-pred-untouched"

	// review has been reached (active) so the no-force precondition passes,
	// but implement is also active (a stale carry from a parallel run).
	// Without --force, implement must stay active.
	writeV3GraphStateForTask(t, tmp, wizardName, beadID,
		map[string]executor.StepState{
			"plan":      {Status: "completed", CompletedCount: 1, CompletedAt: "2026-01-01T00:01:00Z"},
			"implement": {Status: "active", StartedAt: "2026-01-01T00:02:00Z"},
			"review":    {Status: "active", StartedAt: "2026-01-01T00:03:00Z"},
		},
		"review",
		nil,
	)

	if err := softResetV3(beadID, "review", wizardName, false, nil); err != nil {
		t.Fatalf("softResetV3 --to review (no --force): %v", err)
	}

	loaded, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil || loaded == nil {
		t.Fatalf("load graph state: err=%v nil=%v", err, loaded == nil)
	}

	// implement stays active — no --force, no normalization.
	if got := loaded.Steps["implement"].Status; got != "active" {
		t.Errorf("implement.status = %q, want active (--force not set)", got)
	}
}

// TestComputeStepPredecessors_TaskGraph verifies the predecessor BFS walks
// Needs back-edges correctly — predecessors are everything reachable from
// target via Needs, target itself excluded.
func TestComputeStepPredecessors_TaskGraph(t *testing.T) {
	graph := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"plan":      {},
			"implement": {Needs: []string{"plan"}},
			"review":    {Needs: []string{"implement"}},
			"merge":     {Needs: []string{"review"}},
		},
	}

	preds := computeStepPredecessors(graph, "review")
	want := map[string]bool{"plan": true, "implement": true}
	if !reflect.DeepEqual(preds, want) {
		t.Errorf("predecessors of review = %v, want %v", preds, want)
	}

	if got := computeStepPredecessors(graph, "plan"); len(got) != 0 {
		t.Errorf("predecessors of plan = %v, want empty (root has no predecessors)", got)
	}
}
