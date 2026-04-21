package recovery

import (
	"context"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/store"
)

// ---------------------------------------------------------------------------
// buildDecidePrompt tests
// ---------------------------------------------------------------------------

func TestBuildDecidePrompt_IncludesTriageGuidance(t *testing.T) {
	cc := &promptInputs{
		Diagnosis: &Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
			Git: &GitState{
				WorktreeExists: true,
				BranchExists:   true,
			},
		},
		WizardLogTail: "FAIL: TestFoo\n    expected 1, got 2",
	}

	prompt := buildDecidePrompt(cc, 0, nil)

	if !strings.Contains(prompt, `"triage"`) {
		t.Error("prompt missing triage in chosen_action enum")
	}
	if !strings.Contains(prompt, "Triage Action") {
		t.Error("prompt missing Triage Action guidance section")
	}
	if !strings.Contains(prompt, "0 of 2 attempts used") {
		t.Error("prompt missing correct triage budget for count=0")
	}
	if !strings.Contains(prompt, "2 remaining") {
		t.Error("prompt missing remaining count")
	}
	if !strings.Contains(prompt, "Worktree exists:** yes") {
		t.Error("prompt missing worktree existence confirmation")
	}
}

func TestBuildDecidePrompt_TriageBudgetExhausted(t *testing.T) {
	cc := &promptInputs{
		Diagnosis: &Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
		},
	}

	prompt := buildDecidePrompt(cc, 2, nil)

	if !strings.Contains(prompt, "2 of 2 attempts used") {
		t.Error("prompt missing correct budget for count=2")
	}
	if !strings.Contains(prompt, "0 remaining") {
		t.Error("prompt missing zero remaining")
	}
	if !strings.Contains(prompt, "do NOT choose `triage`") {
		t.Error("prompt missing exhaustion warning")
	}
}

func TestBuildDecidePrompt_WorktreeNotExists(t *testing.T) {
	cc := &promptInputs{
		Diagnosis: &Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
			Git: &GitState{
				WorktreeExists: false,
			},
		},
	}

	prompt := buildDecidePrompt(cc, 0, nil)

	if !strings.Contains(prompt, "Worktree exists:** no") {
		t.Error("prompt missing worktree non-existence indicator")
	}
	if !strings.Contains(prompt, "triage is NOT possible") {
		t.Error("prompt missing triage-not-possible note")
	}
}

func TestBuildDecidePrompt_PartialCount(t *testing.T) {
	cc := &promptInputs{}

	prompt := buildDecidePrompt(cc, 1, nil)

	if !strings.Contains(prompt, "1 of 2 attempts used") {
		t.Error("prompt missing correct budget for count=1")
	}
	if !strings.Contains(prompt, "1 remaining") {
		t.Error("prompt missing 1 remaining")
	}
}

// ---------------------------------------------------------------------------
// buildDecidePrompt with LearningStats tests
// ---------------------------------------------------------------------------

func TestBuildDecidePrompt_WithStats(t *testing.T) {
	cc := &promptInputs{
		Diagnosis: &Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
		},
	}
	stats := &store.LearningStats{
		FailureClass:    "step-failure",
		TotalRecoveries: 10,
		ActionStats: []store.ActionOutcomeStat{
			{ResolutionKind: "resummon", Total: 6, CleanCount: 5, DirtyCount: 1, RelapsedCount: 0, SuccessRate: 0.833},
			{ResolutionKind: "reset", Total: 4, CleanCount: 2, DirtyCount: 1, RelapsedCount: 1, SuccessRate: 0.5},
		},
		PredictionAccuracy: 0.75,
	}

	prompt := buildDecidePrompt(cc, 0, stats)

	if !strings.Contains(prompt, "## Historical Outcome Statistics") {
		t.Error("prompt missing Historical Outcome Statistics header")
	}
	if !strings.Contains(prompt, "Based on 10 prior recoveries for failure class `step-failure`") {
		t.Error("prompt missing recovery count and failure class")
	}
	if !strings.Contains(prompt, "| resummon | 6 | 83%") {
		t.Error("prompt missing resummon stats row")
	}
	if !strings.Contains(prompt, "| reset | 4 | 50%") {
		t.Error("prompt missing reset stats row")
	}
	if !strings.Contains(prompt, "prediction accuracy: 75%") {
		t.Error("prompt missing prediction accuracy")
	}
	if !strings.Contains(prompt, "Weight your action choice by historical success rates") {
		t.Error("prompt missing action weighting guidance")
	}
}

func TestBuildDecidePrompt_NilStats(t *testing.T) {
	cc := &promptInputs{
		Diagnosis: &Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
		},
	}

	prompt := buildDecidePrompt(cc, 0, nil)

	if strings.Contains(prompt, "Historical Outcome Statistics") {
		t.Error("prompt should NOT contain statistics section when stats is nil")
	}
}

func TestBuildDecidePrompt_ZeroRecoveries(t *testing.T) {
	cc := &promptInputs{
		Diagnosis: &Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
		},
	}
	stats := &store.LearningStats{
		FailureClass:    "step-failure",
		TotalRecoveries: 0,
	}

	prompt := buildDecidePrompt(cc, 0, stats)

	if strings.Contains(prompt, "Historical Outcome Statistics") {
		t.Error("prompt should NOT contain statistics section when TotalRecoveries is 0")
	}
}

func TestBuildDecidePrompt_StatsWithoutPredictionAccuracy(t *testing.T) {
	cc := &promptInputs{
		Diagnosis: &Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
		},
	}
	stats := &store.LearningStats{
		FailureClass:       "step-failure",
		TotalRecoveries:    5,
		ActionStats:        []store.ActionOutcomeStat{{ResolutionKind: "resummon", Total: 5, CleanCount: 4, DirtyCount: 1, SuccessRate: 0.8}},
		PredictionAccuracy: 0,
	}

	prompt := buildDecidePrompt(cc, 0, stats)

	if !strings.Contains(prompt, "## Historical Outcome Statistics") {
		t.Error("prompt missing statistics section")
	}
	if strings.Contains(prompt, "prediction accuracy") {
		t.Error("prompt should NOT contain prediction accuracy line when accuracy is 0")
	}
}

// ---------------------------------------------------------------------------
// parseHumanGuidance
// ---------------------------------------------------------------------------

func TestParseHumanGuidance_KeywordMatching(t *testing.T) {
	tests := []struct {
		name     string
		comments []string
		want     string
	}{
		{"rebase keyword", []string{"try rebase onto main"}, "rebase-onto-base"},
		{"rebase simple", []string{"rebase"}, "rebase-onto-base"},
		{"cherry-pick", []string{"cherry-pick abc123"}, "cherry-pick"},
		{"cherry pick no hyphen", []string{"cherry pick that commit"}, "cherry-pick"},
		{"resolve conflicts", []string{"resolve conflicts please"}, "resolve-conflicts"},
		{"resolve conflict singular", []string{"resolve conflict"}, "resolve-conflicts"},
		{"rebuild", []string{"rebuild the project"}, "rebuild"},
		{"try rebuild", []string{"try rebuild"}, "rebuild"},
		{"resummon", []string{"resummon an apprentice"}, "resummon"},
		{"re-summon", []string{"re-summon"}, "resummon"},
		{"try again", []string{"try again"}, "resummon"},
		{"reset", []string{"reset the step"}, "reset-to-step"},
		{"reset to step", []string{"reset to step verify"}, "reset-to-step"},
		{"escalate", []string{"escalate to human"}, "escalate"},
		{"fix", []string{"fix the build issue"}, "targeted-fix"},
		{"targeted fix", []string{"targeted fix needed"}, "targeted-fix"},
		{"no match", []string{"hello world"}, ""},
		{"empty comments", []string{}, ""},
		{"nil comments", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHumanGuidance(tt.comments, nil)
			if got != tt.want {
				t.Errorf("parseHumanGuidance(%v, nil) = %q, want %q", tt.comments, got, tt.want)
			}
		})
	}
}

func TestParseHumanGuidance_MostRecentCommentWins(t *testing.T) {
	comments := []string{
		"try rebase",
		"rebuild please",
	}
	got := parseHumanGuidance(comments, nil)
	if got != "rebuild" {
		t.Errorf("parseHumanGuidance = %q, want 'rebuild' (most recent comment)", got)
	}
}

func TestParseHumanGuidance_CaseInsensitive(t *testing.T) {
	comments := []string{"REBASE onto main"}
	got := parseHumanGuidance(comments, nil)
	if got != "rebase-onto-base" {
		t.Errorf("parseHumanGuidance = %q, want 'rebase-onto-base'", got)
	}
}

func TestParseHumanGuidance_SkipsRepeatedFailures(t *testing.T) {
	comments := []string{"try rebase"}
	repeated := map[string]int{"rebase-onto-base": 2}
	got := parseHumanGuidance(comments, repeated)
	if got != "" {
		t.Errorf("parseHumanGuidance = %q, want empty (rebase has 2 failures)", got)
	}
}

func TestParseHumanGuidance_SkipsRepeatedButFindsAlternative(t *testing.T) {
	comments := []string{"try rebase or rebuild"}
	repeated := map[string]int{"rebase-onto-base": 3}
	got := parseHumanGuidance(comments, repeated)
	if got != "rebuild" {
		t.Errorf("parseHumanGuidance = %q, want 'rebuild' (rebase filtered out)", got)
	}
}

func TestParseHumanGuidance_RepeatedBelowThreshold(t *testing.T) {
	comments := []string{"try rebase"}
	repeated := map[string]int{"rebase-onto-base": 1}
	got := parseHumanGuidance(comments, repeated)
	if got != "rebase-onto-base" {
		t.Errorf("parseHumanGuidance = %q, want 'rebase-onto-base' (only 1 failure)", got)
	}
}

// TestParseHumanGuidance_RejectsSystemFailureReport regression-guards spi-uh5oo
// bug 3: a "spire"-authored failure-report comment must not parse as guidance,
// even though it contains the action keyword "rebase-onto-base".
func TestParseHumanGuidance_RejectsSystemFailureReport(t *testing.T) {
	comments := []string{`recovery action "rebase-onto-base" failed`}
	got := parseHumanGuidance(comments, nil)
	if got != "" {
		t.Errorf("parseHumanGuidance = %q, want empty (system failure report)", got)
	}
}

// TestParseHumanGuidance_RejectsRetrySchedulingComment regression-guards the
// self-amplification case from spi-uh5oo bug 3: the retry-scheduling comment
// posted by handleRecordExecuteError contains the word "rebase" but opens
// with "Cleric", not an imperative — it must not parse as guidance.
func TestParseHumanGuidance_RejectsRetrySchedulingComment(t *testing.T) {
	comments := []string{
		"Cleric execute errored — scheduling retry:\n\n```\nrebase conflict in files: pkg/gateway/gateway_test.go\n```",
	}
	got := parseHumanGuidance(comments, nil)
	if got != "" {
		t.Errorf("parseHumanGuidance = %q, want empty (retry-scheduling comment)", got)
	}
}

// TestParseHumanGuidance_AcceptsImperativeWithConflictWord verifies that a
// legitimate human imperative containing the word "conflict" still parses.
func TestParseHumanGuidance_AcceptsImperativeWithConflictWord(t *testing.T) {
	comments := []string{"resolve the conflict, rebase onto base"}
	got := parseHumanGuidance(comments, nil)
	if got == "" {
		t.Errorf("parseHumanGuidance returned empty, want a match for 'resolve the conflict, rebase onto base'")
	}
}

// TestParseHumanGuidance_RejectsNonImperativeOpener verifies that comments
// whose first token is not in the imperative set are rejected, even when
// they contain an action keyword.
func TestParseHumanGuidance_RejectsNonImperativeOpener(t *testing.T) {
	cases := []string{
		"Please rebase onto main",
		"Let's rebuild the project",
		"Can you escalate this?",
		"the rebase failed again",
	}
	for _, c := range cases {
		if got := parseHumanGuidance([]string{c}, nil); got != "" {
			t.Errorf("parseHumanGuidance(%q) = %q, want empty (non-imperative opener)", c, got)
		}
	}
}

// TestParseHumanGuidance_NormalizesLeadingPunctuation verifies the imperative
// check strips leading markdown/quote punctuation so a comment like
// "- try rebase..." or "> rebase..." still matches.
func TestParseHumanGuidance_NormalizesLeadingPunctuation(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"- try rebase onto main", "rebase-onto-base"},
		{"> rebase please", "rebase-onto-base"},
		{"  \"rebase onto base\"", "rebase-onto-base"},
		{"* rebuild the project", "rebuild"},
	}
	for _, c := range cases {
		got := parseHumanGuidance([]string{c.in}, nil)
		if got != c.want {
			t.Errorf("parseHumanGuidance(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// decideFromGitState
// ---------------------------------------------------------------------------

func TestDecideFromGitState_Diverged(t *testing.T) {
	branch := &git.BranchDiagnostics{
		Diverged:    true,
		BehindMain:  3,
		AheadOfMain: 2,
	}
	got := decideFromGitState(branch, nil, nil)
	if got != "rebase-onto-base" {
		t.Errorf("decideFromGitState(diverged) = %q, want 'rebase-onto-base'", got)
	}
}

func TestDecideFromGitState_BehindMain(t *testing.T) {
	branch := &git.BranchDiagnostics{
		BehindMain:  5,
		AheadOfMain: 1,
		Diverged:    false,
	}
	got := decideFromGitState(branch, nil, nil)
	if got != "rebase-onto-base" {
		t.Errorf("decideFromGitState(behind) = %q, want 'rebase-onto-base'", got)
	}
}

func TestDecideFromGitState_DirtyWorktree(t *testing.T) {
	branch := &git.BranchDiagnostics{BehindMain: 0}
	worktree := &git.WorktreeDiagnostics{
		Exists:  true,
		IsDirty: true,
	}
	got := decideFromGitState(branch, worktree, nil)
	if got != "rebuild" {
		t.Errorf("decideFromGitState(dirty worktree) = %q, want 'rebuild'", got)
	}
}

func TestDecideFromGitState_CleanState(t *testing.T) {
	branch := &git.BranchDiagnostics{BehindMain: 0}
	worktree := &git.WorktreeDiagnostics{
		Exists:  true,
		IsDirty: false,
	}
	got := decideFromGitState(branch, worktree, nil)
	if got != "" {
		t.Errorf("decideFromGitState(clean) = %q, want empty", got)
	}
}

func TestDecideFromGitState_NilGitState(t *testing.T) {
	got := decideFromGitState(nil, nil, nil)
	if got != "" {
		t.Errorf("decideFromGitState(nil git) = %q, want empty", got)
	}
}

func TestDecideFromGitState_WorktreeNotExists(t *testing.T) {
	branch := &git.BranchDiagnostics{BehindMain: 0}
	worktree := &git.WorktreeDiagnostics{Exists: false}
	got := decideFromGitState(branch, worktree, nil)
	if got != "" {
		t.Errorf("decideFromGitState(worktree not exists) = %q, want empty", got)
	}
}

func TestDecideFromGitState_PriorityOrder(t *testing.T) {
	branch := &git.BranchDiagnostics{
		Diverged:   true,
		BehindMain: 3,
	}
	worktree := &git.WorktreeDiagnostics{
		Exists:  true,
		IsDirty: true,
	}
	got := decideFromGitState(branch, worktree, nil)
	if got != "rebase-onto-base" {
		t.Errorf("decideFromGitState(diverged+dirty) = %q, want 'rebase-onto-base' (diverged takes priority)", got)
	}
}

// TestDecideFromGitState_ConflictsRouteToResolveConflicts verifies the
// routing added for spi-nghqn: when conflictedFiles is non-empty, decide
// must route to resolve-conflicts (the agentic resolver), NOT rebase-onto-base.
func TestDecideFromGitState_ConflictsRouteToResolveConflicts(t *testing.T) {
	got := decideFromGitState(nil, nil, []string{"pkg/gateway/gateway_test.go"})
	if got != "resolve-conflicts" {
		t.Errorf("decideFromGitState(conflicts present) = %q, want 'resolve-conflicts'", got)
	}
}

// TestDecideFromGitState_ConflictsTakePriorityOverBehind verifies that even
// when the branch also reports Diverged/Behind, conflicted files route to
// resolve-conflicts — conflicts mean a paused git op, not a stale branch.
func TestDecideFromGitState_ConflictsTakePriorityOverBehind(t *testing.T) {
	branch := &git.BranchDiagnostics{
		Diverged:   true,
		BehindMain: 5,
	}
	worktree := &git.WorktreeDiagnostics{
		Exists:  true,
		IsDirty: true,
	}
	got := decideFromGitState(branch, worktree, []string{"a.go", "b.go"})
	if got != "resolve-conflicts" {
		t.Errorf("decideFromGitState(conflicts + diverged + dirty) = %q, want 'resolve-conflicts' (conflicts take top priority)", got)
	}
}

// TestDecideFromGitState_EmptyConflictedFilesFallsThrough verifies that an
// empty (but non-nil) slice is treated like nil — decide falls through to
// behind/dirty logic.
func TestDecideFromGitState_EmptyConflictedFilesFallsThrough(t *testing.T) {
	branch := &git.BranchDiagnostics{BehindMain: 2}
	got := decideFromGitState(branch, nil, []string{})
	if got != "rebase-onto-base" {
		t.Errorf("decideFromGitState(empty-slice + behind) = %q, want 'rebase-onto-base'", got)
	}
}

// TestGitStateReasoning_ResolveConflictsUsesCount verifies gitStateReasoning
// reports the file count when the action is resolve-conflicts and
// conflictedFiles is populated.
func TestGitStateReasoning_ResolveConflictsUsesCount(t *testing.T) {
	got := gitStateReasoning(nil, []string{"a.go", "b.go", "c.go"}, "resolve-conflicts")
	if !strings.Contains(got, "3 file") {
		t.Errorf("gitStateReasoning(resolve-conflicts, 3 files) = %q, want to mention '3 file'", got)
	}
}

// ---------------------------------------------------------------------------
// gitStateReasoning
// ---------------------------------------------------------------------------

func TestGitStateReasoning(t *testing.T) {
	branch := &git.BranchDiagnostics{
		BehindMain: 7,
		MainRef:    "main",
	}

	tests := []struct {
		action   string
		contains string
	}{
		{"resolve-conflicts", "merge conflicts"},
		{"rebase-onto-base", "7 commits behind main"},
		{"rebuild", "uncommitted changes"},
		{"unknown-action", "unknown-action"},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got := gitStateReasoning(branch, nil, tt.action)
			if !strings.Contains(got, tt.contains) {
				t.Errorf("gitStateReasoning(%q) = %q, want to contain %q", tt.action, got, tt.contains)
			}
		})
	}
}

func TestGitStateReasoning_NilGitState(t *testing.T) {
	got := gitStateReasoning(nil, nil, "rebase-onto-base")
	if got != "branch is behind base" {
		t.Errorf("gitStateReasoning(nil git, rebase) = %q, want 'branch is behind base'", got)
	}
}

// ---------------------------------------------------------------------------
// Decide: FailureClass → RepairMode matrix (design spi-h32xj §7)
// ---------------------------------------------------------------------------

// TestDecide_MergeFailure_MapsToMechanical pins the matrix row
// `FailureClass=merge-failure → RepairMode=mechanical (rebase)`. A merge
// failure typically surfaces as a branch that is behind/diverged from base
// without unresolved conflicts on disk — decide routes to the
// `rebase-onto-base` mechanical.
func TestDecide_MergeFailure_MapsToMechanical(t *testing.T) {
	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailMerge,
	}
	deps := Deps{
		BranchDiagnostics: &git.BranchDiagnostics{BehindMain: 3, MainRef: "origin/main"},
	}
	plan, err := Decide(context.Background(), diagnosis, nil, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if plan.Mode != RepairModeMechanical {
		t.Errorf("Mode = %q, want %q", plan.Mode, RepairModeMechanical)
	}
	if plan.Action != "rebase-onto-base" {
		t.Errorf("Action = %q, want rebase-onto-base", plan.Action)
	}
}

// TestDecide_BuildFailure_PromotedRecipeMapsToRecipeMode pins the matrix row
// `FailureClass=build-failure → RepairMode=recipe when covered`. When a
// promoted mechanical recipe exists for the bead's failure signature,
// decide short-circuits to recipe replay with Mode=RepairModeRecipe.
func TestDecide_BuildFailure_PromotedRecipeMapsToRecipeMode(t *testing.T) {
	restore := withPromotionStubs(t,
		func(sig string) (*store.PromotionSnapshot, error) {
			return &store.PromotionSnapshot{
				FailureSig:   sig,
				CleanCount:   3,
				LatestRecipe: `{"kind":"builtin","action":"rebuild"}`,
			}, nil
		},
		nil,
	)
	defer restore()

	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailBuild,
	}
	deps := Deps{
		FailureSignature:   "build-failure:some-sig",
		PromotionThreshold: func(string) int { return 3 },
	}
	plan, err := Decide(context.Background(), diagnosis, nil, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if plan.Mode != RepairModeRecipe {
		t.Errorf("Mode = %q, want %q", plan.Mode, RepairModeRecipe)
	}
	if plan.Action != "rebuild" {
		t.Errorf("Action = %q, want rebuild", plan.Action)
	}
}

// TestDecide_ReviewFix_FallbackMapsToWorker pins the matrix row
// `FailureClass=review-fix → RepairMode=worker`. With no git-state signal
// and no ClaudeRunner, decide falls back to `resummon` which maps to
// RepairModeWorker — the normal path for handing off a review-fix to a
// repair worker on a borrowed workspace.
func TestDecide_ReviewFix_FallbackMapsToWorker(t *testing.T) {
	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailReviewFix,
	}
	deps := Deps{
		// No BranchDiagnostics / WorktreeDiagnostics / ConflictedFiles so
		// the git-state heuristic falls through.
		// No ClaudeRunner so decide emits the fallback plan.
	}
	plan, err := Decide(context.Background(), diagnosis, nil, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if plan.Mode != RepairModeWorker {
		t.Errorf("Mode = %q, want %q", plan.Mode, RepairModeWorker)
	}
	if plan.Action != "resummon" {
		t.Errorf("Action = %q, want resummon", plan.Action)
	}
}
