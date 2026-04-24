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
// Decide: agent-first routing (design spi-uhxdn)
// ---------------------------------------------------------------------------
//
// The priority ladder is (0)/(a)/(a2)/(c)/(d). Git-state signals (conflicts,
// behind-base divergence, dirty worktree) no longer short-circuit the
// decision — they reach Claude via the diagnosis context so the agent can
// reason about them directly. When no ClaudeRunner is wired, decide falls
// back to `resummon` (step d).

// TestDecide_GitStateSignalsFallThroughWhenClaudeAbsent verifies that
// git-state signals that previously preempted to `rebase-onto-base` /
// `resolve-conflicts` / `rebuild` no longer short-circuit: with no
// ClaudeRunner, decide routes to the step-(d) fallback instead.
func TestDecide_GitStateSignalsFallThroughWhenClaudeAbsent(t *testing.T) {
	cases := []struct {
		name string
		deps Deps
	}{
		{
			name: "behind base (would-have-been rebase-onto-base)",
			deps: Deps{
				BranchDiagnostics: &git.BranchDiagnostics{BehindMain: 3, MainRef: "origin/main"},
			},
		},
		{
			name: "diverged (would-have-been rebase-onto-base)",
			deps: Deps{
				BranchDiagnostics: &git.BranchDiagnostics{Diverged: true, BehindMain: 3, AheadOfMain: 2, MainRef: "origin/main"},
			},
		},
		{
			name: "unresolved conflicts (would-have-been resolve-conflicts)",
			deps: Deps{
				ConflictedFiles: []string{"a.go", "b.go"},
			},
		},
		{
			name: "dirty worktree (would-have-been rebuild)",
			deps: Deps{
				WorktreeDiagnostics: &git.WorktreeDiagnostics{Exists: true, IsDirty: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diagnosis := Diagnosis{BeadID: "spi-src1"}
			plan, err := Decide(context.Background(), diagnosis, nil, tc.deps)
			if err != nil {
				t.Fatalf("Decide err = %v", err)
			}
			if plan.Action != "resummon" {
				t.Errorf("Action = %q, want resummon (fallback when Claude unavailable)", plan.Action)
			}
			if plan.Mode != RepairModeWorker {
				t.Errorf("Mode = %q, want %q", plan.Mode, RepairModeWorker)
			}
		})
	}
}

// TestDecide_GitStateSignalsRouteToClaudeWhenAvailable verifies that with
// git-state signals present and a ClaudeRunner wired, decide routes through
// Claude — the heuristics no longer preempt step (c).
func TestDecide_GitStateSignalsRouteToClaudeWhenAvailable(t *testing.T) {
	var claudeCalled bool
	claudeStub := func(args []string, label string) ([]byte, error) {
		claudeCalled = true
		return []byte(`{"chosen_action":"resummon","confidence":0.8,"reasoning":"stubbed","needs_human":false,"expected_outcome":"ok"}`), nil
	}

	diagnosis := Diagnosis{BeadID: "spi-src1"}
	deps := Deps{
		BranchDiagnostics:   &git.BranchDiagnostics{BehindMain: 3, MainRef: "origin/main"},
		WorktreeDiagnostics: &git.WorktreeDiagnostics{Exists: true, IsDirty: true},
		ConflictedFiles:     []string{"a.go"},
		ClaudeRunner:        claudeStub,
	}
	_, err := Decide(context.Background(), diagnosis, nil, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if !claudeCalled {
		t.Fatal("Claude runner not invoked — git-state signals should not preempt Claude")
	}
}

// TestDecide_ClaudeReceivesContextSummary verifies that the diagnosis
// context summary (which carries conflict list, divergence counts, dirty
// tree signals) reaches the Claude prompt — the heuristics-branch removal
// requires these signals to flow through ContextSummary rather than
// short-circuit the decision.
func TestDecide_ClaudeReceivesContextSummary(t *testing.T) {
	var capturedPrompt string
	claudeStub := func(args []string, label string) ([]byte, error) {
		for i, a := range args {
			if a == "-p" && i+1 < len(args) {
				capturedPrompt = args[i+1]
				break
			}
		}
		return []byte(`{"chosen_action":"resummon","confidence":0.8,"reasoning":"ok","needs_human":false,"expected_outcome":"ok"}`), nil
	}

	diagnosis := Diagnosis{BeadID: "spi-src1"}
	deps := Deps{
		ClaudeRunner:   claudeStub,
		ContextSummary: "## Unresolved Merge Conflicts (2 file(s))\n- a.go\n- b.go\n",
	}
	if _, err := Decide(context.Background(), diagnosis, nil, deps); err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if !strings.Contains(capturedPrompt, "Unresolved Merge Conflicts") {
		t.Error("ContextSummary did not flow into the Claude prompt")
	}
	if !strings.Contains(capturedPrompt, "a.go") {
		t.Error("conflict file list missing from Claude prompt")
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

// TestDecide_MergeRaceSubClass_ShortCircuitsToRetryMerge verifies the
// deterministic merge-race path: Decide must emit a Mechanical RepairPlan with
// action=retry-merge WITHOUT invoking Claude. This is the core guarantee that
// keeps merge-race recovery in-process.
func TestDecide_MergeRaceSubClass_ShortCircuitsToRetryMerge(t *testing.T) {
	claudeCalled := false
	claudeStub := func(args []string, label string) ([]byte, error) {
		claudeCalled = true
		return []byte(`{"chosen_action":"escalate"}`), nil
	}

	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailMerge,
		SubClass:    SubClassMergeRace,
	}
	deps := Deps{ClaudeRunner: claudeStub}

	plan, err := Decide(context.Background(), diagnosis, nil, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if claudeCalled {
		t.Error("Claude runner was invoked — merge-race should short-circuit before (c)")
	}
	if plan.Mode != RepairModeMechanical {
		t.Errorf("Mode = %q, want %q", plan.Mode, RepairModeMechanical)
	}
	if plan.Action != "retry-merge" {
		t.Errorf("Action = %q, want retry-merge", plan.Action)
	}
}

// TestDecide_StaleWorktreeSubClass_ShortCircuitsToCleanup verifies the
// deterministic stale-worktree path.
func TestDecide_StaleWorktreeSubClass_ShortCircuitsToCleanup(t *testing.T) {
	claudeCalled := false
	claudeStub := func(args []string, label string) ([]byte, error) {
		claudeCalled = true
		return []byte(`{"chosen_action":"escalate"}`), nil
	}

	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailMerge,
		SubClass:    SubClassStaleWorktree,
	}
	deps := Deps{ClaudeRunner: claudeStub}

	plan, err := Decide(context.Background(), diagnosis, nil, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if claudeCalled {
		t.Error("Claude runner was invoked — stale-worktree should short-circuit before (c)")
	}
	if plan.Mode != RepairModeMechanical {
		t.Errorf("Mode = %q, want %q", plan.Mode, RepairModeMechanical)
	}
	if plan.Action != "cleanup-stale-worktrees" {
		t.Errorf("Action = %q, want cleanup-stale-worktrees", plan.Action)
	}
}

// TestDecide_MergeSubClass_FallsThroughWhenRepeated verifies the repeated-
// failure guard: after a mechanical retry has failed twice already, Decide
// must NOT keep looping on the same mechanical — it falls through to the
// normal Claude path so the agent can choose to escalate instead.
func TestDecide_MergeSubClass_FallsThroughWhenRepeated(t *testing.T) {
	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailMerge,
		SubClass:    SubClassMergeRace,
	}

	history := []Attempt{
		{Action: "retry-merge", Outcome: "failure"},
		{Action: "retry-merge", Outcome: "failure"},
	}

	// No ClaudeRunner → Decide's fallback path fires. Whatever it picks must
	// NOT be "retry-merge" (the deterministic path is suppressed by the guard).
	deps := Deps{}

	plan, err := Decide(context.Background(), diagnosis, history, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if plan.Action == "retry-merge" {
		t.Errorf("Action = retry-merge after 2 prior failures — repeated-failure guard did not fire")
	}
}

// TestDecide_PostRebaseFFSubClass_ShortCircuitsToRetryMerge verifies the
// deterministic post-rebase-ff-only path: Decide must emit a Mechanical
// RepairPlan with action=retry-merge WITHOUT invoking Claude. This is the
// follow-up path when main advances between a successful rebase and the
// ff-only merge.
func TestDecide_PostRebaseFFSubClass_ShortCircuitsToRetryMerge(t *testing.T) {
	claudeCalled := false
	claudeStub := func(args []string, label string) ([]byte, error) {
		claudeCalled = true
		return []byte(`{"chosen_action":"escalate"}`), nil
	}

	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailMerge,
		SubClass:    SubClassPostRebaseFF,
	}
	deps := Deps{ClaudeRunner: claudeStub}

	plan, err := Decide(context.Background(), diagnosis, nil, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if claudeCalled {
		t.Error("Claude runner was invoked — post-rebase-ff-only should short-circuit before (c)")
	}
	if plan.Mode != RepairModeMechanical {
		t.Errorf("Mode = %q, want %q", plan.Mode, RepairModeMechanical)
	}
	if plan.Action != "retry-merge" {
		t.Errorf("Action = %q, want retry-merge", plan.Action)
	}
}

// TestDecide_MergeConflictSubClass_RoutesToWorker verifies that a content-
// collision rebase conflict routes to a Worker (resolve-conflicts) without
// invoking Claude for the decision. The conflict resolver IS the Worker.
func TestDecide_MergeConflictSubClass_RoutesToWorker(t *testing.T) {
	claudeCalled := false
	claudeStub := func(args []string, label string) ([]byte, error) {
		claudeCalled = true
		return []byte(`{"chosen_action":"escalate"}`), nil
	}

	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailMerge,
		SubClass:    SubClassMergeConflict,
	}
	deps := Deps{ClaudeRunner: claudeStub}

	plan, err := Decide(context.Background(), diagnosis, nil, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if claudeCalled {
		t.Error("Claude runner was invoked — merge-conflict should short-circuit before (c)")
	}
	if plan.Mode != RepairModeWorker {
		t.Errorf("Mode = %q, want %q", plan.Mode, RepairModeWorker)
	}
	if plan.Action != "resolve-conflicts" {
		t.Errorf("Action = %q, want resolve-conflicts", plan.Action)
	}
}

// TestDecide_MergeRace_FailsTwice_UpgradesToWorker verifies the budget
// upgrade: after 2 failed retry-merge rounds, Decide must emit a Worker plan
// with action=resolve-conflicts so persistent contention escalates from
// blind retry to a conflict-resolving agent.
func TestDecide_MergeRace_FailsTwice_UpgradesToWorker(t *testing.T) {
	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailMerge,
		SubClass:    SubClassMergeRace,
	}

	history := []Attempt{
		{Action: "retry-merge", Outcome: "failure"},
		{Action: "retry-merge", Outcome: "failure"},
	}

	// No ClaudeRunner — we should still get the Worker upgrade, not fall
	// through to the ClaudeRunner-absent resummon fallback.
	deps := Deps{}

	plan, err := Decide(context.Background(), diagnosis, history, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if plan.Action != "resolve-conflicts" {
		t.Errorf("Action = %q, want resolve-conflicts (budget upgrade)", plan.Action)
	}
	if plan.Mode != RepairModeWorker {
		t.Errorf("Mode = %q, want %q (Worker upgrade)", plan.Mode, RepairModeWorker)
	}
}

// TestDecide_MergeRace_FailsThrice_Escalates verifies that on the 3rd total
// failure, the totalAttempts >= maxAttempts guard fires and Decide emits an
// escalate plan. This is the budget-exhaustion exit.
func TestDecide_MergeRace_FailsThrice_Escalates(t *testing.T) {
	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailMerge,
		SubClass:    SubClassMergeRace,
	}

	history := []Attempt{
		{Action: "retry-merge", Outcome: "failure"},
		{Action: "retry-merge", Outcome: "failure"},
		{Action: "resolve-conflicts", Outcome: "failure"},
	}

	deps := Deps{}

	plan, err := Decide(context.Background(), diagnosis, history, deps)
	if err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if plan.Action != "escalate" {
		t.Errorf("Action = %q, want escalate (budget exhausted)", plan.Action)
	}
	if plan.Mode != RepairModeEscalate {
		t.Errorf("Mode = %q, want %q", plan.Mode, RepairModeEscalate)
	}
}

// TestDecide_UnknownMergeSubClass_FallsThrough verifies that a merge failure
// without a known sub-class takes the normal decide pipeline (Claude or
// fallback). This keeps the existing FailMerge code path intact for failures
// that ClassifyError didn't refine.
func TestDecide_UnknownMergeSubClass_FallsThrough(t *testing.T) {
	claudeCalled := false
	claudeStub := func(args []string, label string) ([]byte, error) {
		claudeCalled = true
		return []byte(`{"chosen_action":"resummon","confidence":0.8,"reasoning":"ok","needs_human":false,"expected_outcome":"ok"}`), nil
	}

	diagnosis := Diagnosis{
		BeadID:      "spi-src1",
		FailureMode: FailMerge,
		// SubClass left empty.
	}
	deps := Deps{ClaudeRunner: claudeStub}

	if _, err := Decide(context.Background(), diagnosis, nil, deps); err != nil {
		t.Fatalf("Decide err = %v", err)
	}
	if !claudeCalled {
		t.Error("Claude was not invoked — FailMerge without sub-class should flow through to Claude")
	}
}
