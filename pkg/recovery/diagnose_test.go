package recovery

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseStepContext(t *testing.T) {
	tests := []struct {
		name       string
		result     string
		wantNil    bool
		wantStep   string
		wantAction string
		wantFlow   string
		wantWS     string
	}{
		{
			name:       "v3 node-scoped result",
			result:     "failure: step implement action=wizard.run flow=implement workspace=feature: subprocess exited",
			wantStep:   "implement",
			wantAction: "wizard.run",
			wantFlow:   "implement",
			wantWS:     "feature",
		},
		{
			name:       "v3 action only",
			result:     "failure: step plan action=wizard.run: plan failed",
			wantStep:   "plan",
			wantAction: "wizard.run",
		},
		{
			name:    "v2 phase result",
			result:  "failure: phase implement: error",
			wantNil: true,
		},
		{
			name:    "empty result",
			result:  "",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := parseStepContext(tt.result)
			if tt.wantNil {
				if sc != nil {
					t.Errorf("expected nil, got %+v", sc)
				}
				return
			}
			if sc == nil {
				t.Fatal("expected non-nil StepContext")
			}
			if sc.StepName != tt.wantStep {
				t.Errorf("StepName = %q, want %q", sc.StepName, tt.wantStep)
			}
			if sc.Action != tt.wantAction {
				t.Errorf("Action = %q, want %q", sc.Action, tt.wantAction)
			}
			if sc.Flow != tt.wantFlow {
				t.Errorf("Flow = %q, want %q", sc.Flow, tt.wantFlow)
			}
			if sc.Workspace != tt.wantWS {
				t.Errorf("Workspace = %q, want %q", sc.Workspace, tt.wantWS)
			}
		})
	}
}

func TestDiagnose_StepContext(t *testing.T) {
	deps := mockDeps()
	deps.GetChildren = func(parentID string) ([]DepBead, error) {
		return []DepBead{
			{
				ID:     parentID + ".a1",
				Status: "closed",
				Labels: []string{
					"attempt",
					"agent:wizard-" + parentID,
					"result:failure: step implement action=wizard.run flow=implement workspace=feature: subprocess exited",
				},
			},
		}, nil
	}

	diag, err := Diagnose("spi-v3test", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.StepContext == nil {
		t.Fatal("expected non-nil StepContext")
	}
	if diag.StepContext.StepName != "implement" {
		t.Errorf("StepName = %q, want %q", diag.StepContext.StepName, "implement")
	}
	if diag.StepContext.Action != "wizard.run" {
		t.Errorf("Action = %q, want %q", diag.StepContext.Action, "wizard.run")
	}
	if diag.StepContext.Flow != "implement" {
		t.Errorf("Flow = %q, want %q", diag.StepContext.Flow, "implement")
	}
	if diag.StepContext.Workspace != "feature" {
		t.Errorf("Workspace = %q, want %q", diag.StepContext.Workspace, "feature")
	}
}

// mockDeps returns a Deps with sensible defaults for testing.
func mockDeps() *Deps {
	return &Deps{
		GetBead: func(id string) (DepBead, error) {
			return DepBead{
				ID:     id,
				Title:  "Test bead",
				Status: "in_progress",
				Labels: []string{"interrupted:merge-failure", "needs-human", "phase:merge"},
			}, nil
		},
		GetChildren: func(parentID string) ([]DepBead, error) {
			return []DepBead{
				{ID: parentID + ".a1", Status: "closed", Labels: []string{"attempt", "agent:wizard-" + parentID, "result:failure"}},
			}, nil
		},
		GetDependentsWithMeta: func(id string) ([]DepDependent, error) {
			return nil, nil
		},
		LoadExecutorState: func(agentName string) (*RuntimeState, error) {
			return nil, fmt.Errorf("no state")
		},
		LookupRegistry: func(beadID string) (string, int, bool, error) {
			return "wizard-" + beadID, 0, false, nil
		},
		ResolveRepo: func(beadID string) (string, string, error) {
			return "/tmp/repo", "main", nil
		},
		CheckBranchExists: func(repoPath, branch string) bool {
			return true
		},
		CheckWorktreeExists: func(dir string) bool { return false },
		CheckWorktreeDirty:  func(dir string) bool { return false },
	}
}

func TestDiagnose_MergeFailure(t *testing.T) {
	deps := mockDeps()
	diag, err := Diagnose("spi-test1", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.FailureMode != FailMerge {
		t.Errorf("expected FailMerge, got %s", diag.FailureMode)
	}
	if diag.Phase != "merge" {
		t.Errorf("expected phase merge, got %s", diag.Phase)
	}
	if diag.AttemptCount != 1 {
		t.Errorf("expected 1 attempt, got %d", diag.AttemptCount)
	}
	if diag.LastAttemptResult != "failure" {
		t.Errorf("expected result failure, got %s", diag.LastAttemptResult)
	}
	if len(diag.Actions) == 0 {
		t.Fatal("expected actions, got none")
	}
	// First action should be resummon (non-destructive, branch exists).
	if diag.Actions[0].Name != "resummon" {
		t.Errorf("expected first action resummon, got %s", diag.Actions[0].Name)
	}
	// Should have reset-hard as a destructive option.
	hasHard := false
	for _, a := range diag.Actions {
		if a.Name == "reset-hard" {
			hasHard = true
			if !a.Destructive {
				t.Error("reset-hard should be destructive")
			}
		}
	}
	if !hasHard {
		t.Error("expected reset-hard action")
	}
}

func TestDiagnose_EmptyImplement(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Test bead",
			Status: "in_progress",
			Labels: []string{"interrupted:empty-implement", "needs-human", "phase:implement"},
		}, nil
	}

	diag, err := Diagnose("spi-test2", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.FailureMode != FailEmptyImplement {
		t.Errorf("expected FailEmptyImplement, got %s", diag.FailureMode)
	}
	// Should have resummon, reset-to-design, close.
	names := make(map[string]bool)
	for _, a := range diag.Actions {
		names[a.Name] = true
	}
	if !names["resummon"] {
		t.Error("expected resummon action")
	}
	if !names["reset-to-design"] {
		t.Error("expected reset-to-design action")
	}
	if !names["close"] {
		t.Error("expected close action")
	}
}

func TestDiagnose_BuildFailure(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Test bead",
			Status: "in_progress",
			Labels: []string{"interrupted:build-failure", "needs-human", "phase:implement"},
		}, nil
	}

	diag, err := Diagnose("spi-test3", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.FailureMode != FailBuild {
		t.Errorf("expected FailBuild, got %s", diag.FailureMode)
	}
	names := make(map[string]bool)
	for _, a := range diag.Actions {
		names[a.Name] = true
	}
	if !names["resummon"] {
		t.Error("expected resummon action")
	}
	if !names["reset-to-implement"] {
		t.Error("expected reset-to-implement action")
	}
}

func TestDiagnose_RepoResolution(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Test bead",
			Status: "in_progress",
			Labels: []string{"interrupted:repo-resolution", "needs-human"},
		}, nil
	}

	diag, err := Diagnose("spi-test4", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.FailureMode != FailRepoResolution {
		t.Errorf("expected FailRepoResolution, got %s", diag.FailureMode)
	}
	// First action should be manual-fix (non-destructive guidance).
	if diag.Actions[0].Name != "manual-fix" {
		t.Errorf("expected manual-fix first, got %s", diag.Actions[0].Name)
	}
}

func TestDiagnose_ArbiterFailure(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Test bead",
			Status: "in_progress",
			Labels: []string{"interrupted:arbiter-failure", "needs-human", "phase:review"},
		}, nil
	}

	diag, err := Diagnose("spi-test5", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.FailureMode != FailArbiter {
		t.Errorf("expected FailArbiter, got %s", diag.FailureMode)
	}
	names := make(map[string]bool)
	for _, a := range diag.Actions {
		names[a.Name] = true
	}
	if !names["manual-review"] {
		t.Error("expected manual-review action")
	}
	if !names["close"] {
		t.Error("expected close action")
	}
}

func TestDiagnose_UnknownFailure(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Test bead",
			Status: "in_progress",
			Labels: []string{"interrupted:some-new-reason", "needs-human"},
		}, nil
	}

	diag, err := Diagnose("spi-test6", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.FailureMode != FailUnknown {
		t.Errorf("expected FailUnknown, got %s", diag.FailureMode)
	}
}

func TestDiagnose_AttemptWarning(t *testing.T) {
	deps := mockDeps()
	deps.GetChildren = func(parentID string) ([]DepBead, error) {
		return []DepBead{
			{ID: "a1", Status: "closed", Labels: []string{"attempt", "result:failure"}},
			{ID: "a2", Status: "closed", Labels: []string{"attempt", "result:failure"}},
			{ID: "a3", Status: "closed", Labels: []string{"attempt", "result:failure"}},
		}, nil
	}

	diag, err := Diagnose("spi-test7", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.AttemptCount != 3 {
		t.Errorf("expected 3 attempts, got %d", diag.AttemptCount)
	}
	// Resummon action should have a warning.
	for _, a := range diag.Actions {
		if a.Name == "resummon" {
			if a.Warning == "" {
				t.Error("expected warning on resummon action for >2 attempts")
			}
			return
		}
	}
	t.Error("no resummon action found")
}

func TestDiagnose_NoBranch_RemovesResummon(t *testing.T) {
	deps := mockDeps()
	deps.CheckBranchExists = func(repoPath, branch string) bool {
		return false
	}

	diag, err := Diagnose("spi-test8", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	for _, a := range diag.Actions {
		if a.Name == "resummon" {
			t.Error("resummon should not be offered when branch doesn't exist")
		}
	}
	// Should still have reset-hard.
	hasHard := false
	for _, a := range diag.Actions {
		if a.Name == "reset-hard" {
			hasHard = true
		}
	}
	if !hasHard {
		t.Error("expected reset-hard even without branch")
	}
}

func TestDiagnose_NotInterrupted_Error(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Normal bead",
			Status: "in_progress",
			Labels: []string{"phase:implement"},
		}, nil
	}

	_, err := Diagnose("spi-test9", deps)
	if err == nil {
		t.Fatal("expected error for non-interrupted bead")
	}
}

func TestDiagnose_ClosedBead_Error(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Closed bead",
			Status: "closed",
			Labels: []string{"interrupted:merge-failure"},
		}, nil
	}

	_, err := Diagnose("spi-test10", deps)
	if err == nil {
		t.Fatal("expected error for closed bead")
	}
}

func TestDiagnose_AlertBeads(t *testing.T) {
	deps := mockDeps()
	deps.GetDependentsWithMeta = func(id string) ([]DepDependent, error) {
		return []DepDependent{
			{ID: "alert-1", Status: "open", Labels: []string{"alert:merge-failure"}, DependencyType: "caused-by"},
			{ID: "other-1", Status: "open", Labels: []string{"some-label"}, DependencyType: "caused-by"},
			{ID: "alert-2", Status: "closed", Labels: []string{"alert:old"}, DependencyType: "caused-by"},
		}, nil
	}

	diag, err := Diagnose("spi-test11", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if len(diag.AlertBeads) != 1 {
		t.Errorf("expected 1 alert bead, got %d", len(diag.AlertBeads))
	}
	if diag.AlertBeads[0].ID != "alert-1" {
		t.Errorf("expected alert-1, got %s", diag.AlertBeads[0].ID)
	}
}

func TestDiagnose_WizardRunning(t *testing.T) {
	deps := mockDeps()
	deps.LookupRegistry = func(beadID string) (string, int, bool, error) {
		return "wizard-" + beadID, 12345, true, nil
	}

	diag, err := Diagnose("spi-test12", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if !diag.WizardRunning {
		t.Error("expected WizardRunning=true")
	}
}

func TestVerify_Clean(t *testing.T) {
	deps := &Deps{
		GetBead: func(id string) (DepBead, error) {
			return DepBead{
				ID:     id,
				Status: "in_progress",
				Labels: []string{"phase:implement"},
			}, nil
		},
		GetDependentsWithMeta: func(id string) ([]DepDependent, error) {
			return nil, nil
		},
	}

	result, err := Verify("spi-v1", deps)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if !result.Clean {
		t.Error("expected clean result")
	}
}

func TestVerify_Dirty(t *testing.T) {
	deps := &Deps{
		GetBead: func(id string) (DepBead, error) {
			return DepBead{
				ID:     id,
				Status: "in_progress",
				Labels: []string{"interrupted:merge-failure", "needs-human"},
			}, nil
		},
		GetDependentsWithMeta: func(id string) ([]DepDependent, error) {
			return []DepDependent{
				{ID: "alert-1", Status: "open", Labels: []string{"alert:merge-failure"}, DependencyType: "caused-by"},
			}, nil
		},
	}

	result, err := Verify("spi-v2", deps)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if result.Clean {
		t.Error("expected dirty result")
	}
	if len(result.InterruptLabels) != 1 {
		t.Errorf("expected 1 interrupt label, got %d", len(result.InterruptLabels))
	}
	if !result.NeedsHuman {
		t.Error("expected NeedsHuman=true")
	}
	if result.AlertsOpen != 1 {
		t.Errorf("expected 1 open alert, got %d", result.AlertsOpen)
	}
}

func TestClassify_AllModes(t *testing.T) {
	tests := []struct {
		label    string
		expected FailureClass
	}{
		{"interrupted:empty-implement", FailEmptyImplement},
		{"interrupted:merge-failure", FailMerge},
		{"interrupted:build-failure", FailBuild},
		{"interrupted:review-fix", FailReviewFix},
		{"interrupted:review-fix-merge-conflict", FailReviewFix},
		{"interrupted:repo-resolution", FailRepoResolution},
		{"interrupted:arbiter-failure", FailArbiter},
		{"interrupted:arbiter", FailArbiter},
		{"interrupted:step-failure", FailStepFailure},
		{"interrupted:something-else", FailUnknown},
		{"not-an-interrupt", FailUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got := classifyInterruptLabel(tt.label)
			if got != tt.expected {
				t.Errorf("classifyInterruptLabel(%q) = %s, want %s", tt.label, got, tt.expected)
			}
		})
	}
}

func TestDiagnose_ReviewFixMergeConflict(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Test bead",
			Status: "in_progress",
			Labels: []string{"interrupted:review-fix-merge-conflict", "needs-human", "phase:review"},
		}, nil
	}

	diag, err := Diagnose("spi-rfmc", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.FailureMode != FailReviewFix {
		t.Errorf("expected FailReviewFix, got %s", diag.FailureMode)
	}
}

// --- TestFindLinkedBeads ---

func TestFindLinkedBeads(t *testing.T) {
	t.Run("alert-only dependents", func(t *testing.T) {
		deps := &Deps{
			GetDependentsWithMeta: func(id string) ([]DepDependent, error) {
				return []DepDependent{
					{ID: "alert-1", Status: "open", Labels: []string{"alert:merge-failure"}, DependencyType: "caused-by"},
					{ID: "alert-2", Status: "open", Labels: []string{"alert:build-failure"}, DependencyType: "caused-by"},
				}, nil
			},
		}
		alerts, recovery := findLinkedBeads("spi-parent", deps)
		if len(alerts) != 2 {
			t.Errorf("expected 2 alerts, got %d", len(alerts))
		}
		if recovery != nil {
			t.Errorf("expected nil recovery, got %+v", recovery)
		}
	})

	t.Run("recovery-only dependents", func(t *testing.T) {
		deps := &Deps{
			GetDependentsWithMeta: func(id string) ([]DepDependent, error) {
				return []DepDependent{
					{ID: "rec-1", Status: "open", Title: "recovery: merge-failure", DependencyType: "recovery-for"},
				}, nil
			},
		}
		alerts, recovery := findLinkedBeads("spi-parent", deps)
		if len(alerts) != 0 {
			t.Errorf("expected 0 alerts, got %d", len(alerts))
		}
		if recovery == nil {
			t.Fatal("expected non-nil recovery")
		}
		if recovery.ID != "rec-1" {
			t.Errorf("expected rec-1, got %s", recovery.ID)
		}
	})

	t.Run("mixed alert and recovery dependents", func(t *testing.T) {
		deps := &Deps{
			GetDependentsWithMeta: func(id string) ([]DepDependent, error) {
				return []DepDependent{
					{ID: "alert-1", Status: "open", Labels: []string{"alert:merge-failure"}, DependencyType: "caused-by"},
					{ID: "rec-1", Status: "open", Title: "recovery: merge-failure", DependencyType: "recovery-for"},
					{ID: "other-1", Status: "open", Labels: []string{"unrelated"}, DependencyType: "related"},
				}, nil
			},
		}
		alerts, recovery := findLinkedBeads("spi-parent", deps)
		if len(alerts) != 1 {
			t.Errorf("expected 1 alert, got %d", len(alerts))
		}
		if alerts[0].ID != "alert-1" {
			t.Errorf("expected alert-1, got %s", alerts[0].ID)
		}
		if recovery == nil {
			t.Fatal("expected non-nil recovery")
		}
		if recovery.ID != "rec-1" {
			t.Errorf("expected rec-1, got %s", recovery.ID)
		}
	})

	t.Run("closed recovery beads are skipped", func(t *testing.T) {
		deps := &Deps{
			GetDependentsWithMeta: func(id string) ([]DepDependent, error) {
				return []DepDependent{
					{ID: "rec-closed", Status: "closed", Title: "old recovery", DependencyType: "recovery-for"},
				}, nil
			},
		}
		alerts, recovery := findLinkedBeads("spi-parent", deps)
		if len(alerts) != 0 {
			t.Errorf("expected 0 alerts, got %d", len(alerts))
		}
		if recovery != nil {
			t.Errorf("expected nil recovery for closed bead, got %+v", recovery)
		}
	})

	t.Run("first open recovery bead wins", func(t *testing.T) {
		deps := &Deps{
			GetDependentsWithMeta: func(id string) ([]DepDependent, error) {
				return []DepDependent{
					{ID: "rec-closed", Status: "closed", Title: "old recovery", DependencyType: "recovery-for"},
					{ID: "rec-first", Status: "open", Title: "first open", DependencyType: "recovery-for"},
					{ID: "rec-second", Status: "open", Title: "second open", DependencyType: "recovery-for"},
				}, nil
			},
		}
		_, recovery := findLinkedBeads("spi-parent", deps)
		if recovery == nil {
			t.Fatal("expected non-nil recovery")
		}
		if recovery.ID != "rec-first" {
			t.Errorf("expected rec-first (first open), got %s", recovery.ID)
		}
	})

	t.Run("nil GetDependentsWithMeta returns nil", func(t *testing.T) {
		deps := &Deps{}
		alerts, recovery := findLinkedBeads("spi-parent", deps)
		if alerts != nil {
			t.Errorf("expected nil alerts, got %v", alerts)
		}
		if recovery != nil {
			t.Errorf("expected nil recovery, got %+v", recovery)
		}
	})

	t.Run("error returns nil", func(t *testing.T) {
		deps := &Deps{
			GetDependentsWithMeta: func(id string) ([]DepDependent, error) {
				return nil, fmt.Errorf("store error")
			},
		}
		alerts, recovery := findLinkedBeads("spi-parent", deps)
		if alerts != nil {
			t.Errorf("expected nil alerts on error, got %v", alerts)
		}
		if recovery != nil {
			t.Errorf("expected nil recovery on error, got %+v", recovery)
		}
	})
}

// --- TestDiagnose_RecoveryBead ---

func TestDiagnose_RecoveryBead(t *testing.T) {
	deps := mockDeps()
	deps.GetDependentsWithMeta = func(id string) ([]DepDependent, error) {
		return []DepDependent{
			{ID: "alert-1", Status: "open", Labels: []string{"alert:merge-failure"}, DependencyType: "caused-by"},
			{ID: "rec-1", Status: "open", Title: "recovery: merge-failure", DependencyType: "recovery-for"},
		}, nil
	}

	diag, err := Diagnose("spi-rec-test", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if len(diag.AlertBeads) != 1 {
		t.Errorf("expected 1 alert, got %d", len(diag.AlertBeads))
	}
	if diag.RecoveryBead == nil {
		t.Fatal("expected non-nil RecoveryBead")
	}
	if diag.RecoveryBead.ID != "rec-1" {
		t.Errorf("expected rec-1, got %s", diag.RecoveryBead.ID)
	}
}

func TestDiagnose_NoRecoveryBead(t *testing.T) {
	deps := mockDeps()
	// Default mockDeps returns no dependents — no recovery bead.
	diag, err := Diagnose("spi-no-rec", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.RecoveryBead != nil {
		t.Errorf("expected nil RecoveryBead, got %+v", diag.RecoveryBead)
	}
}

// TestDiagnose_HookedBeadWithoutFailureEvidence_IsNotRecoverable verifies that
// a hooked bead with no recovery bead, no alert beads, and no interrupted:* label
// is rejected — it's an approval gate or design wait, not a failure.
func TestDiagnose_HookedBeadWithoutFailureEvidence_IsNotRecoverable(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Hooked approval gate",
			Status: "hooked",
			Labels: []string{"phase:implement"},
		}, nil
	}

	_, err := Diagnose("spi-hooked", deps)
	if err == nil {
		t.Fatal("expected error for hooked bead with no failure evidence")
	}
	if !strings.Contains(err.Error(), "no failure evidence") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestDiagnose_HookedBeadWithRecoveryBead_UsesFailureClass verifies that a hooked
// bead with a linked recovery bead and alert bead classifies using the alert label.
func TestDiagnose_HookedBeadWithRecoveryBead_UsesFailureClass(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Hooked bead with recovery",
			Status: "hooked",
			Labels: []string{"phase:implement"},
		}, nil
	}
	deps.GetDependentsWithMeta = func(id string) ([]DepDependent, error) {
		return []DepDependent{
			{ID: "spi-rec1", Title: "recovery", Status: "open", DependencyType: "recovery-for"},
			{ID: "spi-alert1", Title: "alert", Status: "open", DependencyType: "caused-by",
				Labels: []string{"alert:step-failure"}},
		}, nil
	}

	diag, err := Diagnose("spi-hooked", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.InterruptLabel != "interrupted:step-failure" {
		t.Errorf("expected InterruptLabel=%q, got %q", "interrupted:step-failure", diag.InterruptLabel)
	}
	if diag.RecoveryBead == nil {
		t.Error("expected RecoveryBead to be set")
	}
}

// TestDiagnose_HookedApprovalGate_DoesNotOfferRecoveryActions verifies that
// a normal parked approval wait (hooked, no failure artifacts) is not recoverable.
func TestDiagnose_HookedApprovalGate_DoesNotOfferRecoveryActions(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Waiting for approval",
			Status: "hooked",
		}, nil
	}

	_, err := Diagnose("spi-approval", deps)
	if err == nil {
		t.Fatal("expected error — approval gates should not be recoverable")
	}
}

// TestDiagnose_HookedBeadWithCausedByRecoveryBead_IsRecoverable verifies that
// a hooked bead with a caused-by recovery-bead dependent (current model) is
// accepted as recoverable even without an interrupted:* label or alert beads.
func TestDiagnose_HookedBeadWithCausedByRecoveryBead_IsRecoverable(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Hooked with caused-by recovery",
			Status: "hooked",
			Labels: []string{"phase:implement"},
		}, nil
	}
	deps.GetDependentsWithMeta = func(id string) ([]DepDependent, error) {
		return []DepDependent{
			{ID: "spi-rec1", Title: "[recovery] step-failure", Status: "open",
				DependencyType: "caused-by", Labels: []string{"recovery-bead"}},
		}, nil
	}

	diag, err := Diagnose("spi-hooked", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.RecoveryBead == nil {
		t.Fatal("expected RecoveryBead to be populated from caused-by recovery-bead")
	}
	if diag.RecoveryBead.ID != "spi-rec1" {
		t.Errorf("expected RecoveryBead.ID=%q, got %q", "spi-rec1", diag.RecoveryBead.ID)
	}
}

// TestDiagnose_HookedBeadWithCausedByRecoveryBeadAndAlert_PopulatesRecoveryBead
// verifies that when both a caused-by recovery bead and an alert bead exist,
// the failure class comes from the alert and RecoveryBead is still populated.
func TestDiagnose_HookedBeadWithCausedByRecoveryBeadAndAlert_PopulatesRecoveryBead(t *testing.T) {
	deps := mockDeps()
	deps.GetBead = func(id string) (DepBead, error) {
		return DepBead{
			ID:     id,
			Title:  "Hooked with recovery + alert",
			Status: "hooked",
			Labels: []string{"phase:implement"},
		}, nil
	}
	deps.GetDependentsWithMeta = func(id string) ([]DepDependent, error) {
		return []DepDependent{
			{ID: "spi-rec1", Title: "[recovery] step-failure", Status: "open",
				DependencyType: "caused-by", Labels: []string{"recovery-bead"}},
			{ID: "spi-alert1", Title: "alert", Status: "open",
				DependencyType: "caused-by", Labels: []string{"alert:step-failure"}},
		}, nil
	}

	diag, err := Diagnose("spi-hooked", deps)
	if err != nil {
		t.Fatalf("Diagnose returned error: %v", err)
	}
	if diag.InterruptLabel != "interrupted:step-failure" {
		t.Errorf("expected InterruptLabel=%q, got %q", "interrupted:step-failure", diag.InterruptLabel)
	}
	if diag.RecoveryBead == nil {
		t.Fatal("expected RecoveryBead to be populated")
	}
	if diag.RecoveryBead.ID != "spi-rec1" {
		t.Errorf("expected RecoveryBead.ID=%q, got %q", "spi-rec1", diag.RecoveryBead.ID)
	}
}
