package executor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/steveyegge/beads"
)

// initSeamTestRepo creates a temporary git repo with an initial commit on main.
// Returns the repo directory path.
func initSeamTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644)
	runGit("add", "-A")
	runGit("commit", "-m", "initial commit")
	runGit("branch", "-M", "main")
	return dir
}

// =============================================================================
// Seam 1: wizardPlan() plan-bypass when only workflow-step children exist
//
// Bug: wizardPlan() calls GetChildren() and if len(children) > 0, it skips
// plan generation entirely (line 50). After ensureStepBeads() runs, the bead
// already has workflow-step children. So wizardPlan sees children > 0 and
// bypasses plan generation, falling through to enrichment of internal DAG beads.
//
// The fix should filter out step beads before the len(children) > 0 check,
// so that only real task children cause the plan to be skipped.
// =============================================================================

// TestWizardPlan_OnlyStepBeadChildren documents the current (buggy) behavior:
// wizardPlan() sees workflow-step children and skips plan generation.
//
// TODO(spi-b8kf3): After the fix lands, this test should verify that
// ClaudeRunner IS called (plan generation happens) when only step beads exist.
func TestWizardPlan_OnlyStepBeadChildren(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	claudeRunnerCalled := false
	enrichCommentCount := 0

	stepChildren := []Bead{
		{ID: "spi-plan.1", Labels: []string{"workflow-step", "step:implement"}, Status: "in_progress"},
		{ID: "spi-plan.2", Labels: []string{"workflow-step", "step:review"}, Status: "open"},
		{ID: "spi-plan.3", Labels: []string{"workflow-step", "step:merge"}, Status: "open"},
	}

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Test Epic", Description: "Test epic desc", Priority: 1}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return stepChildren, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			enrichCommentCount++
			return nil
		},
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			claudeRunnerCalled = true
			return []byte(`{"title": "Task 1", "description": "Do something", "deps": [], "shared_files": [], "do_not_touch": []}`), nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			return "spi-plan.new-1", nil
		},
		AddDep:     func(issueID, dependsOnID string) error { return nil },
		IsAttemptBead: func(b Bead) bool {
			for _, l := range b.Labels {
				if l == "attempt" {
					return true
				}
			}
			return false
		},
		IsStepBead: func(b Bead) bool {
			for _, l := range b.Labels {
				if l == "workflow-step" {
					return true
				}
			}
			return false
		},
		IsReviewRoundBead: func(b Bead) bool { return false },
		ParseIssueType:    func(s string) beads.IssueType { return beads.TypeTask },
	}

	state := &State{
		BeadID:    "spi-plan",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-plan", "wizard-test", state, deps)
	bead, _ := deps.GetBead("spi-plan")

	err := e.wizardPlanEpic(bead, "claude-sonnet-4-6", 0)

	// CURRENT BEHAVIOR (buggy): wizardPlan sees 3 step-bead children,
	// enters the enrichment path, and enrichSubtasksWithChangeSpecs skips
	// all of them (they're step beads). No ClaudeRunner call happens.
	// The function returns nil — no error, no plan generated.
	if claudeRunnerCalled {
		// If this fires, the bug has been fixed — update this test.
		t.Log("NOTE: ClaudeRunner was called — the plan-bypass bug may have been fixed")
	} else {
		t.Log("KNOWN BUG: ClaudeRunner NOT called — step beads caused plan bypass")
	}

	// The function should succeed (it enters enrichment, which skips all step beads).
	if err != nil {
		t.Fatalf("wizardPlan returned error: %v", err)
	}
}

// TestWizardPlan_MixedChildren verifies that when both step beads AND real
// task children exist, plan generation is correctly skipped (the plan already ran)
// and enrichment proceeds on the real children only.
func TestWizardPlan_MixedChildren(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	claudeRunnerCalled := 0
	var enrichedIDs []string

	mixedChildren := []Bead{
		{ID: "spi-mix.1", Labels: []string{"workflow-step", "step:implement"}, Status: "in_progress"},
		{ID: "spi-mix.2", Labels: []string{"workflow-step", "step:review"}, Status: "open"},
		{ID: "spi-mix.3", Labels: []string{"workflow-step", "step:merge"}, Status: "open"},
		{ID: "spi-mix.4", Title: "Real task 1", Status: "open"},
		{ID: "spi-mix.5", Title: "Real task 2", Status: "open"},
	}

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Test Epic", Description: "desc", Priority: 1, Type: "epic"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return mixedChildren, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			return nil
		},
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			claudeRunnerCalled++
			// Track which bead is being enriched (the prompt contains the subtask ID)
			for _, arg := range args {
				for _, child := range mixedChildren {
					if strings.Contains(arg, child.ID) && !strings.Contains(arg, "workflow-step") {
						enrichedIDs = append(enrichedIDs, child.ID)
					}
				}
			}
			return []byte("**Change spec: fake**\n\n**Files to modify:**\n- foo.go"), nil
		},
		IsAttemptBead: func(b Bead) bool { return false },
		IsStepBead: func(b Bead) bool {
			for _, l := range b.Labels {
				if l == "workflow-step" {
					return true
				}
			}
			return false
		},
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{
		BeadID:    "spi-mix",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-mix", "wizard-test", state, deps)
	bead, _ := deps.GetBead("spi-mix")

	err := e.wizardPlanEpic(bead, "claude-sonnet-4-6", 0)

	if err != nil {
		t.Fatalf("wizardPlan returned error: %v", err)
	}

	// With mixed children, the enrichment path runs. ClaudeRunner should be
	// called exactly 2 times (for the 2 real children, not the 3 step beads).
	if claudeRunnerCalled != 2 {
		t.Errorf("ClaudeRunner called %d times, want 2 (only real children)", claudeRunnerCalled)
	}
}

// TestWizardPlan_OnlyRealChildren verifies that when only real task children
// exist (no step beads), plan generation is skipped and enrichment proceeds.
func TestWizardPlan_OnlyRealChildren(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	claudeRunnerCalled := 0

	realChildren := []Bead{
		{ID: "spi-real.1", Title: "Task 1", Status: "open"},
		{ID: "spi-real.2", Title: "Task 2", Status: "open"},
	}

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Test Epic", Description: "desc", Priority: 1, Type: "epic"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return realChildren, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			return nil
		},
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			claudeRunnerCalled++
			return []byte("**Change spec: fake**\n\n**Files to modify:**\n- foo.go"), nil
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{
		BeadID:    "spi-real",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-real", "wizard-test", state, deps)
	bead, _ := deps.GetBead("spi-real")

	err := e.wizardPlanEpic(bead, "claude-sonnet-4-6", 0)

	if err != nil {
		t.Fatalf("wizardPlan returned error: %v", err)
	}

	// With only real children, enrichment runs for each.
	if claudeRunnerCalled != 2 {
		t.Errorf("ClaudeRunner called %d times, want 2", claudeRunnerCalled)
	}
}

// mockHandle is a simple mock for agent.Handle.
type mockHandle struct {
	waitErr error
}

func (h *mockHandle) Wait() error              { return h.waitErr }
func (h *mockHandle) Signal(os.Signal) error    { return nil }
func (h *mockHandle) Alive() bool               { return false }
func (h *mockHandle) Name() string              { return "mock" }
func (h *mockHandle) Identifier() string        { return "mock-id" }

// mockBackend is a simple mock for agent.Backend.
type mockBackend struct {
	spawnFn func(cfg agent.SpawnConfig) (agent.Handle, error)
}

func (b *mockBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) {
	if b.spawnFn != nil {
		return b.spawnFn(cfg)
	}
	return &mockHandle{}, nil
}

func (b *mockBackend) List() ([]agent.Info, error) {
	return nil, nil
}

func (b *mockBackend) Logs(name string) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}

func (b *mockBackend) Kill(name string) error {
	return nil
}




// =============================================================================
// Seam: ensureStagingWorktree resume and stale paths
//
// These tests verify the worktree state management without real git operations.
// Since ensureStagingWorktree() calls real git commands, we test the state
// conditions that control which path it takes.
// =============================================================================

// TestEnsureStagingWorktree_Resume verifies that when WorktreeDir is set
// and the directory exists, the worktree is resumed (not recreated).
func TestEnsureStagingWorktree_Resume(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	// Create the "worktree" directory so the resume path is taken.
	wtDir := dir + "/worktrees/spi-resume-wt"
	os.MkdirAll(wtDir, 0755)

	deps := &Deps{
		ConfigDir: configDirFn,
		AddLabel:  func(id, label string) error { return nil },
	}

	state := &State{
		BeadID:        "spi-resume-wt",
		AgentName:     "wizard-test",

		RepoPath:      dir,
		BaseBranch:    "main",
		StagingBranch: "staging/spi-resume-wt",
		WorktreeDir:   wtDir,
	}

	e := NewForTest("spi-resume-wt", "wizard-test", state, deps)

	wt, err := e.ensureStagingWorktree()
	if err != nil {
		t.Fatalf("ensureStagingWorktree error: %v", err)
	}

	if wt == nil {
		t.Fatal("expected non-nil staging worktree")
	}

	// Verify the returned worktree wraps the existing dir.
	if wt.Dir != wtDir {
		t.Errorf("worktree Dir = %q, want %q", wt.Dir, wtDir)
	}
	if wt.Branch != "staging/spi-resume-wt" {
		t.Errorf("worktree Branch = %q, want staging/spi-resume-wt", wt.Branch)
	}
}

// TestEnsureStagingWorktree_StalePath verifies that when WorktreeDir is set
// but the directory does NOT exist, the stale state is cleared and recreation
// is attempted.
func TestEnsureStagingWorktree_StalePath(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{
		ConfigDir: configDirFn,
		AddLabel:  func(id, label string) error { return nil },
		ActiveTowerConfig: func() (*config.TowerConfig, error) {
			return nil, os.ErrNotExist
		},
	}

	state := &State{
		BeadID:        "spi-stale",
		AgentName:     "wizard-test",

		RepoPath:      dir,
		BaseBranch:    "main",
		StagingBranch: "staging/spi-stale",
		WorktreeDir:   dir + "/nonexistent-worktree", // doesn't exist
	}

	e := NewForTest("spi-stale", "wizard-test", state, deps)

	// This will fail because it tries to do real git operations, but we can
	// verify the state was cleared.
	_, _ = e.ensureStagingWorktree()

	// The stale WorktreeDir should have been cleared.
	if e.state.WorktreeDir == dir+"/nonexistent-worktree" {
		t.Error("stale WorktreeDir should have been cleared")
	}
}

// TestEnsureStagingWorktree_NoStagingBranch verifies error when no staging
// branch is configured.
func TestEnsureStagingWorktree_NoStagingBranch(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
	}

	state := &State{
		BeadID:        "spi-no-staging",
		AgentName:     "wizard-test",

		RepoPath:      dir,
		StagingBranch: "", // empty
	}

	e := NewForTest("spi-no-staging", "wizard-test", state, deps)

	_, err := e.ensureStagingWorktree()
	if err == nil {
		t.Fatal("expected error when no staging branch configured")
	}
	if !strings.Contains(err.Error(), "no staging branch") {
		t.Errorf("error should mention no staging branch, got: %s", err)
	}
}

// TestCloseStagingWorktree verifies cleanup clears state.
func TestCloseStagingWorktree(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
	}

	state := &State{
		BeadID:      "spi-close-wt",
		AgentName:   "wizard-test",
		WorktreeDir: "/some/path",
	}

	e := NewForTest("spi-close-wt", "wizard-test", state, deps)

	// Set a mock staging worktree (without real git ops)
	// Just test that closeStagingWorktree clears the state field.
	e.closeStagingWorktree()

	if e.state.WorktreeDir != "" {
		t.Errorf("WorktreeDir should be cleared after closeStagingWorktree, got %q", e.state.WorktreeDir)
	}
}

// =============================================================================
// Additional: ArchmageIdentity
// =============================================================================

// TestArchmageIdentity_Default verifies fallback identity when no tower exists.
func TestArchmageIdentity_Default(t *testing.T) {
	deps := &Deps{
		ActiveTowerConfig: func() (*config.TowerConfig, error) {
			return nil, os.ErrNotExist
		},
	}

	name, email := ArchmageIdentity(deps)
	if name != "spire" {
		t.Errorf("name = %q, want spire", name)
	}
	if email != "spire@spire.local" {
		t.Errorf("email = %q, want spire@spire.local", email)
	}
}

// TestArchmageIdentity_FromTower verifies identity is read from tower config.
func TestArchmageIdentity_FromTower(t *testing.T) {
	deps := &Deps{
		ActiveTowerConfig: func() (*config.TowerConfig, error) {
			return &config.TowerConfig{
				Archmage: config.ArchmageConfig{
					Name:  "Test User",
					Email: "test@example.com",
				},
			}, nil
		},
	}

	name, email := ArchmageIdentity(deps)
	if name != "Test User" {
		t.Errorf("name = %q, want Test User", name)
	}
	if email != "test@example.com" {
		t.Errorf("email = %q, want test@example.com", email)
	}
}

// TestEscalateHumanFailure_AlertBead verifies that EscalateHumanFailure
// creates an alert bead and recovery bead (no label-based routing).
func TestEscalateHumanFailure_InterruptedLabel(t *testing.T) {
	var beadsCreated []CreateOpts
	var depsAdded []string

	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
		CreateBead: func(opts CreateOpts) (string, error) {
			beadsCreated = append(beadsCreated, opts)
			return "spi-alert-hf", nil
		},
		AddDepTyped: func(issueID, dependsOnID, depType string) error {
			depsAdded = append(depsAdded, issueID+"→"+dependsOnID+":"+depType)
			return nil
		},
		GetDependentsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
	}

	EscalateHumanFailure("spi-test", "wizard-test", "merge-failure", "merge conflict", deps)

	// Verify alert bead was created with correct label.
	foundAlert := false
	for _, opts := range beadsCreated {
		for _, lbl := range opts.Labels {
			if lbl == "alert:merge-failure" {
				foundAlert = true
			}
		}
	}
	if !foundAlert {
		t.Errorf("expected alert bead with alert:merge-failure label, got: %v", beadsCreated)
	}

	// Verify caused-by dep was added.
	foundDep := false
	for _, d := range depsAdded {
		if d == "spi-alert-hf→spi-test:caused-by" {
			foundDep = true
		}
	}
	if !foundDep {
		t.Errorf("expected caused-by dep, got: %v", depsAdded)
	}
}

// TestEscalateHumanFailure_RecoveryBead verifies that EscalateHumanFailure
// creates a type=recovery bead with recovery-bead + failure_class labels and caused-by dep.
func TestEscalateHumanFailure_RecoveryBead(t *testing.T) {
	var beadsCreated []CreateOpts
	var depsAdded []string
	var commentsAdded []string

	deps := &Deps{
		AddLabel:   func(id, label string) error { return nil },
		AddComment: func(id, text string) error {
			commentsAdded = append(commentsAdded, id+":"+text)
			return nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			beadsCreated = append(beadsCreated, opts)
			if len(opts.Labels) > 0 && opts.Labels[0] == "recovery-bead" {
				return "spi-recovery-1", nil
			}
			return "spi-alert-1", nil
		},
		AddDepTyped: func(issueID, dependsOnID, depType string) error {
			depsAdded = append(depsAdded, issueID+"→"+dependsOnID+":"+depType)
			return nil
		},
		GetDependentsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil // no existing recovery bead
		},
	}

	EscalateHumanFailure("spi-test", "wizard-test", "merge-failure", "merge conflict", deps)

	// Verify a recovery bead was created with recovery-bead label and type=recovery.
	foundRecoveryBead := false
	for _, opts := range beadsCreated {
		for _, lbl := range opts.Labels {
			if lbl == "recovery-bead" {
				foundRecoveryBead = true
				if opts.Priority != 1 {
					t.Errorf("expected recovery bead priority 1, got %d", opts.Priority)
				}
				if string(opts.Type) != "recovery" {
					t.Errorf("expected recovery bead type=recovery, got %s", opts.Type)
				}
				// Verify failure_class label.
				hasFC := false
				for _, l := range opts.Labels {
					if l == "failure_class:merge-failure" {
						hasFC = true
					}
				}
				if !hasFC {
					t.Errorf("expected failure_class:merge-failure label, got: %v", opts.Labels)
				}
			}
		}
	}
	if !foundRecoveryBead {
		t.Errorf("expected recovery bead with recovery-bead label, got: %v", beadsCreated)
	}

	// Verify caused-by dep was added (not recovery-for).
	foundRecoveryDep := false
	for _, d := range depsAdded {
		if d == "spi-recovery-1→spi-test:caused-by" {
			foundRecoveryDep = true
		}
	}
	if !foundRecoveryDep {
		t.Errorf("expected caused-by dep, got: %v", depsAdded)
	}

	// Verify a context comment was added to the recovery bead.
	foundRecoveryComment := false
	for _, c := range commentsAdded {
		if strings.HasPrefix(c, "spi-recovery-1:") && strings.Contains(c, "Recovery work surface") {
			foundRecoveryComment = true
		}
	}
	if !foundRecoveryComment {
		t.Errorf("expected recovery comment on recovery bead, got: %v", commentsAdded)
	}
}

// TestEscalateHumanFailure_RecoveryBead_Dedup verifies that a second escalation
// on the same parent with the same failure class reuses the existing open
// recovery bead instead of creating a duplicate.
func TestEscalateHumanFailure_RecoveryBead_Dedup(t *testing.T) {
	var beadsCreated []CreateOpts
	var commentsAdded []string

	deps := &Deps{
		AddLabel:   func(id, label string) error { return nil },
		AddComment: func(id, text string) error {
			commentsAdded = append(commentsAdded, id+":"+text)
			return nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			beadsCreated = append(beadsCreated, opts)
			return "spi-alert-2", nil
		},
		AddDepTyped: func(issueID, dependsOnID, depType string) error { return nil },
		GetDependentsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			// Return an existing open recovery bead with matching failure_class label.
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue: beads.Issue{
						ID:     "spi-existing-recovery",
						Status: "open",
						Labels: []string{"recovery-bead", "failure_class:build-failure"},
					},
					DependencyType: "recovery-for",
				},
			}, nil
		},
	}

	EscalateHumanFailure("spi-test", "wizard-test", "build-failure", "build broke", deps)

	// Verify NO new recovery bead was created (only the alert bead).
	for _, opts := range beadsCreated {
		for _, lbl := range opts.Labels {
			if lbl == "recovery-bead" {
				t.Errorf("should not create a new recovery bead when one exists, got: %v", opts)
			}
		}
	}

	// Verify a comment was added to the existing recovery bead.
	foundUpdate := false
	for _, c := range commentsAdded {
		if strings.HasPrefix(c, "spi-existing-recovery:") && strings.Contains(c, "Recovery work surface") {
			foundUpdate = true
		}
	}
	if !foundUpdate {
		t.Errorf("expected recovery comment on existing recovery bead, got: %v", commentsAdded)
	}
}

// =============================================================================
// Seam: wizardPlanTask() — standalone task planning path
//
// wizardPlanTask() is invoked for non-epic beads. It collects context,
// invokes Claude for a plan, and posts the result as an "Implementation plan:"
// comment. These tests cover the three key paths: happy, resume, empty plan.
// =============================================================================

// TestWizardPlanTask_HappyPath verifies that a non-epic bead invokes Claude
// and posts an "Implementation plan:" comment on the bead.
func TestWizardPlanTask_HappyPath(t *testing.T) {
	dir := t.TempDir()

	claudeRunnerCalled := false
	var postedComment string
	var postedCommentBeadID string

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Fix auth bug", Description: "Token refresh fails", Type: "task", Priority: 2}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil // no existing comments
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil // no design deps
		},
		AddComment: func(id, text string) error {
			postedCommentBeadID = id
			postedComment = text
			return nil
		},
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			claudeRunnerCalled = true
			return []byte("**Approach:** Fix the token refresh logic\n\n**Key files:**\n- auth.go"), nil
		},
	}

	state := &State{
		BeadID:    "spi-task1",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-task1", "wizard-test", state, deps)
	bead, _ := deps.GetBead("spi-task1")

	err := e.wizardPlanTask(bead, "claude-opus-4-6", 0)
	if err != nil {
		t.Fatalf("wizardPlanTask returned error: %v", err)
	}

	if !claudeRunnerCalled {
		t.Error("ClaudeRunner was not called — task plan generation should invoke Claude")
	}

	if postedCommentBeadID != "spi-task1" {
		t.Errorf("comment posted to %q, want %q", postedCommentBeadID, "spi-task1")
	}

	if !strings.HasPrefix(postedComment, "Implementation plan:") {
		t.Errorf("comment does not start with 'Implementation plan:', got: %q", postedComment)
	}

	if !strings.Contains(postedComment, "Fix the token refresh logic") {
		t.Errorf("comment does not contain Claude's plan output, got: %q", postedComment)
	}
}

// TestWizardPlanTask_Resume verifies that when an "Implementation plan:" comment
// already exists, wizardPlanTask skips Claude invocation and returns nil.
func TestWizardPlanTask_Resume(t *testing.T) {
	dir := t.TempDir()

	claudeRunnerCalled := false

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Fix auth bug", Type: "task", Priority: 2}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return []*beads.Comment{
				{ID: "c1", IssueID: id, Author: "wizard", Text: "Implementation plan:\n\n**Approach:** Fix the thing"},
			}, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			t.Error("AddComment should not be called on resume")
			return nil
		},
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			claudeRunnerCalled = true
			return nil, nil
		},
	}

	state := &State{
		BeadID:    "spi-task2",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-task2", "wizard-test", state, deps)
	bead, _ := deps.GetBead("spi-task2")

	err := e.wizardPlanTask(bead, "claude-opus-4-6", 0)
	if err != nil {
		t.Fatalf("wizardPlanTask returned error: %v", err)
	}

	if claudeRunnerCalled {
		t.Error("ClaudeRunner should not be called when plan comment already exists")
	}
}

// TestWizardPlanTask_EmptyPlan verifies that when Claude returns an empty plan,
// wizardPlanTask returns an error.
func TestWizardPlanTask_EmptyPlan(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Fix auth bug", Type: "task", Priority: 2}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			t.Error("AddComment should not be called when plan is empty")
			return nil
		},
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			return []byte("   \n  \n  "), nil // whitespace-only output
		},
	}

	state := &State{
		BeadID:    "spi-task3",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-task3", "wizard-test", state, deps)
	bead, _ := deps.GetBead("spi-task3")

	err := e.wizardPlanTask(bead, "claude-opus-4-6", 0)
	if err == nil {
		t.Fatal("wizardPlanTask should return error when Claude produces empty plan")
	}

	if !strings.Contains(err.Error(), "empty task plan") {
		t.Errorf("error = %q, want to contain 'empty task plan'", err.Error())
	}
}

// =============================================================================
// Seam: collectDesignContext() — shared helper for design context assembly
//
// collectDesignContext() is used by both wizardPlanTask and wizardPlanEpic.
// It filters deps to discovered-from + design type, then assembles context
// from titles, descriptions, and comments.
// =============================================================================

// TestCollectDesignContext verifies that design beads linked via discovered-from
// deps are assembled into context text.
func TestCollectDesignContext(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue:          beads.Issue{ID: "spi-des1", Title: "Auth redesign", Description: "Use OAuth2 flow", IssueType: "design"},
					DependencyType: beads.DepDiscoveredFrom,
				},
				{
					Issue:          beads.Issue{ID: "spi-des2", Title: "Not a design", Description: "Some task", IssueType: "task"},
					DependencyType: beads.DepDiscoveredFrom,
				},
				{
					Issue:          beads.Issue{ID: "spi-des3", Title: "Related design", Description: "Unrelated", IssueType: "design"},
					DependencyType: beads.DepBlocks, // not discovered-from
				},
			}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			if id == "spi-des1" {
				return []*beads.Comment{
					{ID: "c1", IssueID: id, Author: "archmage", Text: "Use PKCE flow"},
				}, nil
			}
			return nil, nil
		},
	}

	state := &State{
		BeadID:    "spi-ctx",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-ctx", "wizard-test", state, deps)
	result := e.collectDesignContext()

	// Should include the discovered-from design bead.
	if !strings.Contains(result, "spi-des1") {
		t.Errorf("result should contain design bead spi-des1, got: %q", result)
	}
	if !strings.Contains(result, "Auth redesign") {
		t.Errorf("result should contain design bead title, got: %q", result)
	}
	if !strings.Contains(result, "Use OAuth2 flow") {
		t.Errorf("result should contain design bead description, got: %q", result)
	}
	if !strings.Contains(result, "Use PKCE flow") {
		t.Errorf("result should contain design bead comment, got: %q", result)
	}

	// Should NOT include non-design dep (spi-des2 is a task, not design).
	if strings.Contains(result, "spi-des2") {
		t.Errorf("result should not contain non-design bead spi-des2, got: %q", result)
	}

	// Should NOT include non-discovered-from dep (spi-des3 is blocks, not discovered-from).
	if strings.Contains(result, "spi-des3") {
		t.Errorf("result should not contain non-discovered-from bead spi-des3, got: %q", result)
	}
}

// =============================================================================
// Seam: wizardValidateDesign — auto-create, poll-until-closed, poll-until-enriched
//
// wizardValidateDesign now polls instead of exiting when a design bead is
// missing, open, or empty. It auto-creates a design bead if none exists.
// =============================================================================

// TestWizardValidateDesign_CreatesDesignBead verifies that when no design bead
// exists, wizardValidateDesign auto-creates one, links it via discovered-from,
// adds comments, labels needs-human, and messages the archmage. On the second
// poll iteration, the newly created bead is found closed with content → returns nil.
func TestWizardValidateDesign_CreatesDesignBead(t *testing.T) {
	dir := t.TempDir()

	var (
		designBeadOpts   *CreateOpts
		addDepTypedCalls []struct{ issue, dep, depType string }
		addedLabels      []struct{ id, label string }
		addedComments    []struct{ id, text string }
		messageCount     int
	)

	pollCount := 0

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			pollCount++
			if pollCount <= 1 {
				// First poll: no design beads.
				return nil, nil
			}
			// Second poll: design bead exists, closed, with content.
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue: beads.Issue{
						ID:          "spi-design-new",
						Title:       "Design: spi-epic1",
						Description: "Design decisions here",
						IssueType:   "design",
						Status:      "closed",
					},
					DependencyType: beads.DepDiscoveredFrom,
				},
			}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			if id == "spi-design-new" {
				return []*beads.Comment{{ID: "c1", IssueID: id, Text: "PKCE flow"}}, nil
			}
			return nil, nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			// Check if it's a message (has msg label) vs a design bead.
			for _, l := range opts.Labels {
				if l == "msg" {
					messageCount++
					return "spi-msg-1", nil
				}
			}
			// Design bead creation.
			copied := opts
			designBeadOpts = &copied
			return "spi-design-new", nil
		},
		AddDepTyped: func(issueID, dependsOnID, depType string) error {
			addDepTypedCalls = append(addDepTypedCalls, struct{ issue, dep, depType string }{issueID, dependsOnID, depType})
			return nil
		},
		AddDep: func(issueID, dependsOnID string) error { return nil },
		AddComment: func(id, text string) error {
			addedComments = append(addedComments, struct{ id, text string }{id, text})
			return nil
		},
		AddLabel: func(id, label string) error {
			addedLabels = append(addedLabels, struct{ id, label string }{id, label})
			return nil
		},
		RemoveLabel:    func(id, label string) error { return nil },
		ParseIssueType: func(s string) beads.IssueType { return beads.IssueType(s) },
	}

	state := &State{
		BeadID:    "spi-epic1",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-epic1", "wizard-test", state, deps)
	e.designPollInterval = time.Millisecond

	err := e.wizardValidateDesign()
	if err != nil {
		t.Fatalf("wizardValidateDesign returned error: %v", err)
	}

	// Verify CreateBead was called with type=design.
	if designBeadOpts == nil {
		t.Fatal("CreateBead was never called for design bead")
	}
	if string(designBeadOpts.Type) != "design" {
		t.Errorf("CreateBead type = %q, want %q", designBeadOpts.Type, "design")
	}

	// Verify discovered-from dep was added.
	if len(addDepTypedCalls) == 0 {
		t.Fatal("AddDepTyped was not called")
	}
	dep := addDepTypedCalls[0]
	if dep.issue != "spi-epic1" || dep.dep != "spi-design-new" || dep.depType != "discovered-from" {
		t.Errorf("AddDepTyped(%q, %q, %q), want (spi-epic1, spi-design-new, discovered-from)",
			dep.issue, dep.dep, dep.depType)
	}

	// Verify needs-human label was added to the epic.
	foundNeedsHuman := false
	for _, l := range addedLabels {
		if l.id == "spi-epic1" && l.label == "needs-human" {
			foundNeedsHuman = true
			break
		}
	}
	if !foundNeedsHuman {
		t.Error("needs-human label was not added to epic")
	}

	// Verify comments were added to both the epic and the design bead.
	epicCommented, designCommented := false, false
	for _, c := range addedComments {
		if c.id == "spi-epic1" && strings.Contains(c.text, "auto-created") {
			epicCommented = true
		}
		if c.id == "spi-design-new" && strings.Contains(c.text, "auto-created") {
			designCommented = true
		}
	}
	if !epicCommented {
		t.Error("epic was not commented about auto-created design bead")
	}
	if !designCommented {
		t.Error("design bead was not commented")
	}

	// Verify archmage was messaged.
	if messageCount == 0 {
		t.Error("archmage was not messaged")
	}
}

// TestWizardValidateDesign_WaitsForOpenDesign verifies that when a design bead
// exists but is open, wizardValidateDesign polls until it's closed with content.
func TestWizardValidateDesign_WaitsForOpenDesign(t *testing.T) {
	dir := t.TempDir()

	pollCount := 0
	var addedLabels []string

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			pollCount++
			status := beads.Status("open")
			if pollCount >= 2 {
				status = "closed"
			}
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue: beads.Issue{
						ID:          "spi-des1",
						Title:       "Auth design",
						Description: "Use OAuth2",
						IssueType:   "design",
						Status:      status,
					},
					DependencyType: beads.DepDiscoveredFrom,
				},
			}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return []*beads.Comment{{ID: "c1", IssueID: id, Text: "Some content"}}, nil
		},
		AddComment: func(id, text string) error { return nil },
		AddLabel: func(id, label string) error {
			addedLabels = append(addedLabels, label)
			return nil
		},
		RemoveLabel: func(id, label string) error { return nil },
		CreateBead: func(opts CreateOpts) (string, error) {
			return "spi-msg-1", nil // for archmage message
		},
		AddDep:         func(issueID, dependsOnID string) error { return nil },
		ParseIssueType: func(s string) beads.IssueType { return beads.IssueType(s) },
	}

	state := &State{
		BeadID:    "spi-epic2",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-epic2", "wizard-test", state, deps)
	e.designPollInterval = time.Millisecond

	err := e.wizardValidateDesign()
	if err != nil {
		t.Fatalf("wizardValidateDesign returned error: %v", err)
	}

	// Should have polled at least twice.
	if pollCount < 2 {
		t.Errorf("pollCount = %d, want >= 2", pollCount)
	}

	// Verify needs-human label was added while waiting.
	found := false
	for _, l := range addedLabels {
		if l == "needs-human" {
			found = true
			break
		}
	}
	if !found {
		t.Error("needs-human label was not added while waiting for open design bead")
	}
}

// TestWizardValidateDesign_WaitsForEmptyDesign verifies that when a design bead
// is closed but has no content, wizardValidateDesign polls until it has content.
func TestWizardValidateDesign_WaitsForEmptyDesign(t *testing.T) {
	dir := t.TempDir()

	pollCount := 0

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			pollCount++
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue: beads.Issue{
						ID:        "spi-des1",
						Title:     "Auth design",
						IssueType: "design",
						Status:    "closed",
						// Description intentionally empty.
					},
					DependencyType: beads.DepDiscoveredFrom,
				},
			}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			if pollCount >= 2 {
				// Second poll: content added.
				return []*beads.Comment{{ID: "c1", IssueID: id, Text: "Design decisions"}}, nil
			}
			// First poll: no comments.
			return nil, nil
		},
		AddComment:  func(id, text string) error { return nil },
		AddLabel:    func(id, label string) error { return nil },
		RemoveLabel: func(id, label string) error { return nil },
		CreateBead: func(opts CreateOpts) (string, error) {
			return "spi-msg-1", nil
		},
		AddDep:         func(issueID, dependsOnID string) error { return nil },
		ParseIssueType: func(s string) beads.IssueType { return beads.IssueType(s) },
	}

	state := &State{
		BeadID:    "spi-epic3",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-epic3", "wizard-test", state, deps)
	e.designPollInterval = time.Millisecond

	err := e.wizardValidateDesign()
	if err != nil {
		t.Fatalf("wizardValidateDesign returned error: %v", err)
	}

	// Should have polled at least twice (first: empty, second: has content).
	if pollCount < 2 {
		t.Errorf("pollCount = %d, want >= 2", pollCount)
	}
}

// TestWizardValidateDesign_HappyPath verifies that when a closed design bead
// with content already exists, wizardValidateDesign returns immediately.
func TestWizardValidateDesign_HappyPath(t *testing.T) {
	dir := t.TempDir()

	pollCount := 0
	removedLabels := []string{}

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			pollCount++
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue: beads.Issue{
						ID:          "spi-des1",
						Title:       "Auth design",
						Description: "Solid design",
						IssueType:   "design",
						Status:      "closed",
					},
					DependencyType: beads.DepDiscoveredFrom,
				},
			}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return []*beads.Comment{{ID: "c1", IssueID: id, Text: "PKCE flow"}}, nil
		},
		AddComment: func(id, text string) error { return nil },
		AddLabel:   func(id, label string) error { return nil },
		RemoveLabel: func(id, label string) error {
			removedLabels = append(removedLabels, label)
			return nil
		},
		ParseIssueType: func(s string) beads.IssueType { return beads.IssueType(s) },
	}

	state := &State{
		BeadID:    "spi-epic4",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-epic4", "wizard-test", state, deps)
	e.designPollInterval = time.Millisecond

	err := e.wizardValidateDesign()
	if err != nil {
		t.Fatalf("wizardValidateDesign returned error: %v", err)
	}

	// Should return on first poll — no waiting needed.
	if pollCount != 1 {
		t.Errorf("pollCount = %d, want 1 (should return immediately)", pollCount)
	}

	// Verify both needs-human and needs-design labels are removed on advance.
	wantRemoved := map[string]bool{"needs-human": false, "needs-design": false}
	for _, l := range removedLabels {
		if _, ok := wantRemoved[l]; ok {
			wantRemoved[l] = true
		}
	}
	for label, found := range wantRemoved {
		if !found {
			t.Errorf("label %q was not removed on advance", label)
		}
	}
}

// TestCollectDesignContext_NoDeps verifies that collectDesignContext returns
// an empty string when there are no discovered-from design deps.
func TestCollectDesignContext_NoDeps(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
	}

	state := &State{
		BeadID:    "spi-nodeps",
		AgentName: "wizard-test",

		RepoPath:  dir,
	}

	e := NewForTest("spi-nodeps", "wizard-test", state, deps)
	result := e.collectDesignContext()

	if result != "" {
		t.Errorf("expected empty design context, got: %q", result)
	}
}

// =============================================================================
// Seam: Wave executor exit cleanup — orphaned-open-attempt
//
// Bug: wave executor disappears after startup, leaving attempt bead open.
// These tests verify the invariant: after executor exit (success or failure),
// CloseAttemptBead is called.
//
// Note: RegistryAdd/RegistryRemove are no longer called by RunGraph. Registry
// entries are created by BeginWork (PID=0 placeholder) and stamped by RunGraph
// via registry.Update. Cleanup is the responsibility of OrphanSweep/EndWork.
// spi-pbuhit Phase 3: RegisterSelf dep removed.
// =============================================================================


// TestGraphExecutorExitCleansUpRegistry verifies that when a v3 graph executor
// fails during startup (repo resolution error), both RegistryRemove and
// CloseAttemptBead are called — preventing the orphaned-open-attempt / empty-registry bug.
func TestGraphExecutorExitCleansUpRegistry(t *testing.T) {
	configDir := t.TempDir()
	configDirFn := func() (string, error) { return configDir, nil }

	var closedAttemptID string
	var closedAttemptResult string

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			return "spi-graph.attempt-1", nil
		},
		CloseAttemptBead: func(attemptID, result string) error {
			closedAttemptID = attemptID
			closedAttemptResult = result
			return nil
		},
		HasLabel:      func(b Bead, prefix string) string { return "" },
		AddLabel:      func(id, label string) error { return nil },
		RemoveLabel:   func(id, label string) error { return nil },
		ContainsLabel: func(b Bead, label string) bool { return false },
		// ResolveRepo fails — simulates the early-exit path.
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return "", "", "", fmt.Errorf("simulated repo resolution failure")
		},
		RegistryRemove: func(name string) error { return nil },
		UpdateBead: func(id string, updates map[string]interface{}) error { return nil },
		CreateBead: func(opts CreateOpts) (string, error) {
			return "spi-graph.alert-1", nil
		},
		CreateStepBead:   func(parentID, stepName string) (string, error) { return "", nil },
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		AddDep:           func(issueID, dependsOnID string) error { return nil },
		AddDepTyped:      func(issueID, dependsOnID, depType string) error { return nil },
		GetDependentsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		Spawner:           &mockBackend{},
		AddComment:        func(id, text string) error { return nil },
	}

	// Minimal v3 graph.
	graph := &formula.FormulaStepGraph{
		Name:    "test-graph",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"implement": {
				Action: "noop",
			},
			"done": {
				Needs:    []string{"implement"},
				Action:   "noop",
				Terminal: true,
			},
		},
		Entry: "implement",
	}

	state := NewGraphState(graph, "spi-graph", "wizard-graph")

	e := NewGraphForTest("spi-graph", "wizard-graph", graph, state, deps)

	err := e.RunGraph(graph, state)
	// Expect an error from the failed repo resolution.
	if err == nil {
		t.Fatal("expected error from repo resolution failure, got nil")
	}

	// INVARIANT: CloseAttemptBead must have been called.
	if closedAttemptID != "spi-graph.attempt-1" {
		t.Errorf("CloseAttemptBead called with %q, want %q", closedAttemptID, "spi-graph.attempt-1")
	}
	if closedAttemptResult == "" {
		t.Error("CloseAttemptBead result is empty — attempt was not closed")
	}
}

