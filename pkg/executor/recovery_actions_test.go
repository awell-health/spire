package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
)

// ---------------------------------------------------------------------------
// generateRecoveryAttemptID
// ---------------------------------------------------------------------------

func TestGenerateRecoveryAttemptID_Format(t *testing.T) {
	id := generateRecoveryAttemptID()
	if !strings.HasPrefix(id, "ra-") {
		t.Errorf("generateRecoveryAttemptID() = %q, want prefix 'ra-'", id)
	}
	// "ra-" + 8 hex chars = 11 chars total
	if len(id) != 11 {
		t.Errorf("generateRecoveryAttemptID() length = %d, want 11", len(id))
	}
	// Verify hex portion
	hexPart := id[3:]
	matched, _ := regexp.MatchString(`^[0-9a-f]{8}$`, hexPart)
	if !matched {
		t.Errorf("generateRecoveryAttemptID() hex part %q is not valid hex", hexPart)
	}
}

func TestGenerateRecoveryAttemptID_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateRecoveryAttemptID()
		if seen[id] {
			t.Fatalf("duplicate ID generated: %s", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// validCommitSHA regex
// ---------------------------------------------------------------------------

func TestValidCommitSHA(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"abc1234", true},                                      // 7 chars, valid
		{"abc12345", true},                                     // 8 chars
		{"abc1234567890abcdef1234567890abcdef12345678", false}, // 42 chars, too long
		{"abc1234567890abcdef1234567890abcdef12345678", false}, // 42 chars
		{"abcdef1234567890abcdef1234567890abcdef12", true},     // 40 chars, full SHA
		{"abc123", false},                                      // 6 chars, too short
		{"", false},                                            // empty
		{"abc123; rm -rf /", false},                            // injection attempt
		{"abc1234\nmalicious", false},                          // newline injection
		{"ABCDEF1234567", true},                                // uppercase hex
		{"ghijkl1234567", false},                               // non-hex chars
		{"abc 1234567", false},                                 // space
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := validCommitSHA.MatchString(tt.input)
			if got != tt.want {
				t.Errorf("validCommitSHA.MatchString(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// mechanicalActions lookup table
// ---------------------------------------------------------------------------

func TestMechanicalActions_CoversCanonicalMechanicals(t *testing.T) {
	expected := []string{"rebase-onto-base", "cherry-pick", "rebuild", "reset-to-step"}
	for _, name := range expected {
		if _, ok := mechanicalActions[name]; !ok {
			t.Errorf("mechanicalActions missing canonical entry %q", name)
		}
	}
}

// ---------------------------------------------------------------------------
// mechanicalResetToStep — record-only mechanical that logs the step target
// and returns a captured recipe.
// ---------------------------------------------------------------------------

func TestMechanicalResetToStep_MissingStep(t *testing.T) {
	fn := mechanicalActions["reset-to-step"]
	ctx := &RecoveryActionCtx{Log: func(msg string) {}}
	plan := recovery.RepairPlan{Mode: recovery.RepairModeMechanical, Action: "reset-to-step"}
	recipe, err := fn(ctx, plan, WorkspaceHandle{})
	if err == nil {
		t.Fatal("expected error for missing 'step' parameter")
	}
	if recipe != nil {
		t.Errorf("recipe should be nil on failure, got %+v", recipe)
	}
}

func TestMechanicalResetToStep_WithStep(t *testing.T) {
	fn := mechanicalActions["reset-to-step"]
	var logged string
	ctx := &RecoveryActionCtx{Log: func(msg string) { logged = msg }}
	plan := recovery.RepairPlan{
		Mode:   recovery.RepairModeMechanical,
		Action: "reset-to-step",
		Params: map[string]string{"step": "verify-build"},
	}
	recipe, err := fn(ctx, plan, WorkspaceHandle{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(logged, "verify-build") {
		t.Errorf("log = %q, want to contain step name", logged)
	}
	if recipe == nil || recipe.Action != "reset-to-step" {
		t.Errorf("captured recipe = %+v, want builtin reset-to-step", recipe)
	}
}

// ---------------------------------------------------------------------------
// mechanicalCherryPick — SHA validation guards against shell injection.
// ---------------------------------------------------------------------------

func TestMechanicalCherryPick_MissingCommit(t *testing.T) {
	fn := mechanicalActions["cherry-pick"]
	ctx := &RecoveryActionCtx{Log: func(msg string) {}}
	plan := recovery.RepairPlan{Mode: recovery.RepairModeMechanical, Action: "cherry-pick"}
	_, err := fn(ctx, plan, WorkspaceHandle{})
	if err == nil {
		t.Fatal("expected error for missing 'commit' parameter")
	}
}

func TestMechanicalCherryPick_InvalidSHA(t *testing.T) {
	fn := mechanicalActions["cherry-pick"]
	ctx := &RecoveryActionCtx{Log: func(msg string) {}}
	plan := recovery.RepairPlan{
		Mode:   recovery.RepairModeMechanical,
		Action: "cherry-pick",
		Params: map[string]string{"commit": "abc; rm -rf /"},
	}
	_, err := fn(ctx, plan, WorkspaceHandle{})
	if err == nil {
		t.Fatal("expected error for invalid commit hash")
	}
	if !strings.Contains(err.Error(), "invalid commit hash") {
		t.Errorf("error = %q, want to contain 'invalid commit hash'", err)
	}
}

// ---------------------------------------------------------------------------
// actionTargetedFix — tombstone raises a helpful error and never calls out
// to a runtime primitive. Historical recovery beads may still reference the
// action name via resume paths; this test pins the error message that tells
// the caller to dispatch via RepairModeWorker instead.
// ---------------------------------------------------------------------------

func TestActionTargetedFix_Retired(t *testing.T) {
	_, err := actionTargetedFix(&RecoveryActionCtx{}, recovery.RepairPlan{Action: "targeted-fix"}, WorkspaceHandle{})
	if err == nil {
		t.Fatal("expected retirement error")
	}
	if !strings.Contains(err.Error(), "targeted-fix is retired") {
		t.Errorf("error = %q, want to contain 'targeted-fix is retired'", err)
	}
	if !strings.Contains(err.Error(), "RepairModeWorker") {
		t.Errorf("error = %q, want to mention RepairModeWorker", err)
	}
}

// ---------------------------------------------------------------------------
// executeRecipe — stub until chunk 7 wires Recipe.ToRepairPlan().
// ---------------------------------------------------------------------------

func TestExecuteRecipe_Stubbed(t *testing.T) {
	_, err := executeRecipe(&RecoveryActionCtx{}, recovery.RepairPlan{Mode: recovery.RepairModeRecipe}, WorkspaceHandle{})
	if err == nil {
		t.Fatal("expected stub error")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("error = %q, want to mention 'not yet implemented'", err)
	}
}

// ---------------------------------------------------------------------------
// ProvisionRecoveryWorktree — cleanup deletes the branch
// ---------------------------------------------------------------------------

// initTestRepo creates a temporary git repo with an initial commit on main.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "git", "init")
	runGit(t, dir, "git", "config", "user.name", "Test")
	runGit(t, dir, "git", "config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644)
	runGit(t, dir, "git", "add", "-A")
	runGit(t, dir, "git", "commit", "-m", "initial commit")
	runGit(t, dir, "git", "branch", "-M", "main")
	return dir
}

func runGit(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
}

func TestProvisionRecoveryWorktree_CleanupDeletesBranch(t *testing.T) {
	repoDir := initTestRepo(t)

	// Create the feat/<beadID> branch that ProvisionRecoveryWorktree uses as startPoint.
	beadID := "test-cleanup-1"
	runGit(t, repoDir, "git", "branch", "feat/"+beadID, "main")

	// First provision — should succeed.
	wc, cleanup, err := ProvisionRecoveryWorktree(repoDir, beadID, "main")
	if err != nil {
		t.Fatalf("first ProvisionRecoveryWorktree: %v", err)
	}
	if wc == nil {
		t.Fatal("first provision returned nil WorktreeContext")
	}

	// Verify the recovery branch exists.
	rc := &spgit.RepoContext{Dir: repoDir, BaseBranch: "main"}
	branch := "recovery/" + beadID
	if !rc.BranchExists(branch) {
		t.Fatalf("branch %s should exist after provision", branch)
	}

	// Call cleanup — should remove worktree AND delete the branch.
	cleanup()

	// Verify the branch is gone.
	if rc.BranchExists(branch) {
		t.Fatalf("branch %s should be deleted after cleanup", branch)
	}

	// Second provision — should succeed now that the branch is cleaned up.
	wc2, cleanup2, err := ProvisionRecoveryWorktree(repoDir, beadID, "main")
	if err != nil {
		t.Fatalf("second ProvisionRecoveryWorktree: %v", err)
	}
	defer cleanup2()
	if wc2 == nil {
		t.Fatal("second provision returned nil WorktreeContext")
	}
}
