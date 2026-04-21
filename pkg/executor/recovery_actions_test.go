package executor

import (
	"errors"
	"os/exec"
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
// executeRecipe — dispatches promoted recipes through the SAME runtime
// paths as un-promoted plans (design spi-h32xj §6 chunk 7). Unit tests
// here cover the reconstruction / dispatch-selection surface; end-to-end
// regression with a real worktree lives in recovery_phase_test.go.
// ---------------------------------------------------------------------------

// TestExecuteRecipe_MissingActionFailsFast guards against a caller passing
// a RepairPlan with an empty Action — the plan is not replayable and we
// should never silently resolve to a do-nothing result.
func TestExecuteRecipe_MissingActionFailsFast(t *testing.T) {
	_, err := executeRecipe(&RecoveryActionCtx{}, recovery.RepairPlan{Mode: recovery.RepairModeRecipe}, WorkspaceHandle{})
	if err == nil {
		t.Fatal("expected error for plan with no Action")
	}
	if !strings.Contains(err.Error(), "missing action") {
		t.Errorf("error = %q, want to mention 'missing action'", err)
	}
}

// TestExecuteRecipe_MechanicalDispatchesThroughSameMap asserts that a
// promoted mechanical recipe routes into the identical mechanicalActions
// entry the un-promoted plan would hit — the "no second dispatch map"
// guarantee from design §6 chunk 7. We swap the rebase-onto-base entry
// for a recording stub so the test can directly observe the dispatch
// target without needing a real worktree.
func TestExecuteRecipe_MechanicalDispatchesThroughSameMap(t *testing.T) {
	var invokedAction string
	var invokedParams map[string]string
	stub := func(_ *RecoveryActionCtx, p recovery.RepairPlan, _ WorkspaceHandle) (*recovery.MechanicalRecipe, error) {
		invokedAction = p.Action
		invokedParams = p.Params
		return recovery.NewBuiltinRecipe(p.Action, p.Params), nil
	}
	orig := mechanicalActions["rebase-onto-base"]
	mechanicalActions["rebase-onto-base"] = stub
	defer func() { mechanicalActions["rebase-onto-base"] = orig }()

	plan := recovery.RepairPlan{
		Mode:   recovery.RepairModeRecipe,
		Action: "rebase-onto-base",
		Params: map[string]string{"foo": "bar"},
	}
	result, err := executeRecipe(&RecoveryActionCtx{Log: func(string) {}}, plan, WorkspaceHandle{})
	if err != nil {
		t.Fatalf("executeRecipe err = %v, want nil", err)
	}
	if invokedAction != "rebase-onto-base" {
		t.Errorf("dispatched action = %q, want rebase-onto-base", invokedAction)
	}
	if invokedParams["foo"] != "bar" {
		t.Errorf("dispatched params[foo] = %q, want bar", invokedParams["foo"])
	}
	if result.Recipe == nil || result.Recipe.Action != "rebase-onto-base" {
		t.Errorf("result.Recipe = %+v, want rebase-onto-base recipe", result.Recipe)
	}
	if !strings.Contains(result.Output, "mechanical") {
		t.Errorf("result.Output = %q, want to mention 'mechanical'", result.Output)
	}
}

// TestExecuteRecipe_MechanicalFailurePropagates confirms that a mechanical
// dispatch error surfaces verbatim — the caller (handlePlanExecute) uses
// that error to demote the promotion counter, so silently swallowing
// would leave a broken recipe promoted.
func TestExecuteRecipe_MechanicalFailurePropagates(t *testing.T) {
	stub := func(_ *RecoveryActionCtx, _ recovery.RepairPlan, _ WorkspaceHandle) (*recovery.MechanicalRecipe, error) {
		return nil, errors.New("rebase conflict")
	}
	orig := mechanicalActions["rebase-onto-base"]
	mechanicalActions["rebase-onto-base"] = stub
	defer func() { mechanicalActions["rebase-onto-base"] = orig }()

	plan := recovery.RepairPlan{
		Mode:   recovery.RepairModeRecipe,
		Action: "rebase-onto-base",
	}
	_, err := executeRecipe(&RecoveryActionCtx{Log: func(string) {}}, plan, WorkspaceHandle{})
	if err == nil {
		t.Fatal("expected mechanical error to propagate")
	}
	if !strings.Contains(err.Error(), "rebase conflict") {
		t.Errorf("error = %q, want to mention 'rebase conflict'", err)
	}
}

// TestExecuteRecipe_WorkerActionDispatchesThroughSpawnRepairWorker asserts
// that a promoted worker recipe (e.g. resolve-conflicts) routes into the
// canonical SpawnRepairWorker path — the same entry the un-promoted
// worker plan uses. The test proxies the spawner with a DispatchFn so no
// real apprentice is launched; success depends on workspace shape, which
// we short-circuit by pointing at an empty worktree (no conflict files
// means SpawnRepairWorker returns nil without dispatching).
func TestExecuteRecipe_WorkerActionDispatchesThroughSpawnRepairWorker(t *testing.T) {
	dir := t.TempDir()
	// Minimal git init so WorktreeContext helpers don't blow up.
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}
	ctx := &RecoveryActionCtx{
		Worktree: wc,
		Spawner:  fakeSpawner{},
		Log:      func(string) {},
	}
	plan := recovery.RepairPlan{
		Mode:   recovery.RepairModeRecipe,
		Action: "resolve-conflicts",
	}
	result, err := executeRecipe(ctx, plan, WorkspaceHandle{Path: dir})
	if err != nil {
		t.Fatalf("executeRecipe err = %v", err)
	}
	if result.Recipe == nil || result.Recipe.Action != "resolve-conflicts" {
		t.Errorf("result.Recipe = %+v, want resolve-conflicts recipe", result.Recipe)
	}
	if !strings.Contains(result.Output, "worker") {
		t.Errorf("result.Output = %q, want to mention 'worker'", result.Output)
	}
}

