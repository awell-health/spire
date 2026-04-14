package executor

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// ---------------------------------------------------------------------------
// parseFailedStepFromLabels
// ---------------------------------------------------------------------------

func TestParseFailedStepFromLabels_Found(t *testing.T) {
	labels := []string{"step:implement", "interrupted:verify-build", "workflow-step"}
	got := parseFailedStepFromLabels(labels)
	if got != "verify-build" {
		t.Errorf("parseFailedStepFromLabels = %q, want 'verify-build'", got)
	}
}

func TestParseFailedStepFromLabels_NotFound(t *testing.T) {
	labels := []string{"step:implement", "workflow-step"}
	got := parseFailedStepFromLabels(labels)
	if got != "" {
		t.Errorf("parseFailedStepFromLabels = %q, want empty", got)
	}
}

func TestParseFailedStepFromLabels_Empty(t *testing.T) {
	got := parseFailedStepFromLabels(nil)
	if got != "" {
		t.Errorf("parseFailedStepFromLabels(nil) = %q, want empty", got)
	}
}

func TestParseFailedStepFromLabels_MultipleInterrupted(t *testing.T) {
	// First match wins.
	labels := []string{"interrupted:build-gate", "interrupted:test"}
	got := parseFailedStepFromLabels(labels)
	if got != "build-gate" {
		t.Errorf("parseFailedStepFromLabels = %q, want 'build-gate'", got)
	}
}

// ---------------------------------------------------------------------------
// resolveBranchFromBead
// ---------------------------------------------------------------------------

func TestResolveBranchFromBead_WithLabel(t *testing.T) {
	b := store.Bead{
		ID:     "spi-abc12",
		Labels: []string{"feat-branch:epic/spi-abc12"},
	}
	got := resolveBranchFromBead(b)
	if got != "epic/spi-abc12" {
		t.Errorf("resolveBranchFromBead = %q, want 'epic/spi-abc12'", got)
	}
}

func TestResolveBranchFromBead_Fallback(t *testing.T) {
	b := store.Bead{
		ID:     "spi-xyz99",
		Labels: []string{"workflow-step"},
	}
	got := resolveBranchFromBead(b)
	if got != "feat/spi-xyz99" {
		t.Errorf("resolveBranchFromBead = %q, want 'feat/spi-xyz99'", got)
	}
}

func TestResolveBranchFromBead_NoLabels(t *testing.T) {
	b := store.Bead{ID: "spi-nolbl"}
	got := resolveBranchFromBead(b)
	if got != "feat/spi-nolbl" {
		t.Errorf("resolveBranchFromBead = %q, want 'feat/spi-nolbl'", got)
	}
}

// ---------------------------------------------------------------------------
// extractHumanComments
// ---------------------------------------------------------------------------

func TestExtractHumanComments_FiltersAgents(t *testing.T) {
	comments := []*beads.Comment{
		{Author: "JB", Text: "try rebase onto main"},
		{Author: "wizard-spi-abc", Text: "starting phase"},
		{Author: "apprentice-spi-xyz", Text: "implemented fix"},
		{Author: "alice", Text: "looks good to me"},
		{Author: "sage-review", Text: "review complete"},
	}
	got := extractHumanComments(comments)
	if len(got) != 2 {
		t.Fatalf("extractHumanComments returned %d comments, want 2", len(got))
	}
	if got[0] != "try rebase onto main" {
		t.Errorf("got[0] = %q", got[0])
	}
	if got[1] != "looks good to me" {
		t.Errorf("got[1] = %q", got[1])
	}
}

func TestExtractHumanComments_SkipsNilAndEmpty(t *testing.T) {
	comments := []*beads.Comment{
		nil,
		{Author: "JB", Text: ""},
		{Author: "JB", Text: "actual guidance"},
	}
	got := extractHumanComments(comments)
	if len(got) != 1 {
		t.Fatalf("extractHumanComments returned %d comments, want 1", len(got))
	}
	if got[0] != "actual guidance" {
		t.Errorf("got[0] = %q", got[0])
	}
}

func TestExtractHumanComments_NoComments(t *testing.T) {
	got := extractHumanComments(nil)
	if len(got) != 0 {
		t.Errorf("extractHumanComments(nil) returned %d comments, want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// isAgentAuthor
// ---------------------------------------------------------------------------

func TestIsAgentAuthor(t *testing.T) {
	tests := []struct {
		author string
		want   bool
	}{
		{"wizard-spi-abc", true},
		{"apprentice-spi-xyz", true},
		{"sage-review", true},
		{"steward-global", true},
		{"recovery-spi-123", true},
		{"JB", false},
		{"alice", false},
		{"", false},
		{"wizardof-oz", false}, // doesn't start with "wizard-"
	}
	for _, tt := range tests {
		t.Run(tt.author, func(t *testing.T) {
			got := isAgentAuthor(tt.author)
			if got != tt.want {
				t.Errorf("isAgentAuthor(%q) = %v, want %v", tt.author, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// minInt
// ---------------------------------------------------------------------------

func TestMinInt(t *testing.T) {
	tests := []struct {
		a, b, want int
	}{
		{0, 0, 0},
		{1, 2, 1},
		{2, 1, 1},
		{-1, 0, -1},
		{5, 5, 5},
		{8, 3, 3},
	}
	for _, tt := range tests {
		got := minInt(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("minInt(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// SummarizeContext
// ---------------------------------------------------------------------------

func TestSummarizeContext_MinimalContext(t *testing.T) {
	ctx := &FullRecoveryContext{
		RecoveryBead: store.Bead{ID: "spi-recovery-1"},
		TargetBead:   store.Bead{ID: "spi-target-1", Title: "Fix build", Status: "in_progress"},
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "spi-target-1") {
		t.Error("missing target bead ID")
	}
	if !strings.Contains(got, "spi-recovery-1") {
		t.Error("missing recovery bead ID")
	}
	if !strings.Contains(got, "Fix build") {
		t.Error("missing target bead title")
	}
	if !strings.Contains(got, "Branch diagnostics unavailable") {
		t.Error("should show branch diagnostics unavailable")
	}
	if !strings.Contains(got, "No worktree found") {
		t.Error("should show no worktree found")
	}
}

func TestSummarizeContext_WithGitState(t *testing.T) {
	ctx := &FullRecoveryContext{
		RecoveryBead: store.Bead{ID: "spi-recovery-2"},
		TargetBead:   store.Bead{ID: "spi-target-2", Title: "Auth fix", Status: "in_progress"},
		GitState: &git.BranchDiagnostics{
			AheadOfMain:    3,
			BehindMain:     5,
			MainRef:        "main",
			BranchRef:      "feat/spi-target-2",
			LastCommitHash: "abc12345deadbeef",
			LastCommitMsg:  "feat: add oauth",
			Diverged:       true,
		},
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "feat/spi-target-2") {
		t.Error("missing branch ref")
	}
	if !strings.Contains(got, "Ahead of main: 3") {
		t.Error("missing ahead count")
	}
	if !strings.Contains(got, "Behind main: 5") {
		t.Error("missing behind count")
	}
	if !strings.Contains(got, "DIVERGED") {
		t.Error("missing diverged marker")
	}
	if !strings.Contains(got, "abc12345") {
		t.Error("missing truncated commit hash")
	}
	if !strings.Contains(got, "feat: add oauth") {
		t.Error("missing commit message")
	}
}

func TestSummarizeContext_WithWorktreeState(t *testing.T) {
	ctx := &FullRecoveryContext{
		RecoveryBead: store.Bead{ID: "spi-recovery-3"},
		TargetBead:   store.Bead{ID: "spi-target-3", Title: "Test", Status: "in_progress"},
		WorktreeState: &git.WorktreeDiagnostics{
			Exists:         true,
			Path:           "/tmp/worktree",
			IsDirty:        true,
			UntrackedFiles: []string{"file1.go", "file2.go"},
			Branch:         "feat/spi-target-3",
		},
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "/tmp/worktree") {
		t.Error("missing worktree path")
	}
	if !strings.Contains(got, "Dirty") {
		t.Error("missing dirty marker")
	}
	if !strings.Contains(got, "Untracked files: 2") {
		t.Error("missing untracked count")
	}
}

func TestSummarizeContext_WithWorktreeClean(t *testing.T) {
	ctx := &FullRecoveryContext{
		RecoveryBead: store.Bead{ID: "spi-recovery-4"},
		TargetBead:   store.Bead{ID: "spi-target-4", Title: "Test", Status: "open"},
		WorktreeState: &git.WorktreeDiagnostics{
			Exists:  true,
			Path:    "/tmp/clean-wt",
			IsDirty: false,
			Branch:  "feat/spi-target-4",
		},
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "Clean") {
		t.Error("missing clean marker")
	}
}

func TestSummarizeContext_WithStepOutput(t *testing.T) {
	ctx := &FullRecoveryContext{
		RecoveryBead: store.Bead{ID: "spi-r"},
		TargetBead:   store.Bead{ID: "spi-t", Title: "T", Status: "in_progress"},
		StepOutput:   "exit status 1\nerror in main.go:42",
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "Failed Step Output") {
		t.Error("missing step output header")
	}
	if !strings.Contains(got, "exit status 1") {
		t.Error("missing step output content")
	}
}

func TestSummarizeContext_WithAttemptHistory(t *testing.T) {
	ctx := &FullRecoveryContext{
		RecoveryBead: store.Bead{ID: "spi-r"},
		TargetBead:   store.Bead{ID: "spi-t", Title: "T", Status: "in_progress"},
		TotalAttempts: 2,
		AttemptHistory: []store.RecoveryAttempt{
			{AttemptNumber: 1, Action: "rebase-onto-main", Outcome: "failure", Error: "conflicts"},
			{AttemptNumber: 2, Action: "rebuild", Outcome: "success"},
		},
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "Total attempts:** 2") {
		t.Error("missing total attempts")
	}
	if !strings.Contains(got, "action=rebase-onto-main outcome=failure") {
		t.Error("missing first attempt")
	}
	if !strings.Contains(got, "action=rebuild outcome=success") {
		t.Error("missing second attempt")
	}
}

func TestSummarizeContext_WithRepeatedFailures(t *testing.T) {
	ctx := &FullRecoveryContext{
		RecoveryBead:     store.Bead{ID: "spi-r"},
		TargetBead:       store.Bead{ID: "spi-t", Title: "T", Status: "in_progress"},
		RepeatedFailures: map[string]int{"rebase-onto-main": 3, "rebuild": 1},
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "Repeated Failures") {
		t.Error("missing repeated failures header")
	}
	if !strings.Contains(got, "rebase-onto-main: 3 failures") {
		t.Error("missing rebase failure count")
	}
}

func TestSummarizeContext_WithHumanComments(t *testing.T) {
	ctx := &FullRecoveryContext{
		RecoveryBead:  store.Bead{ID: "spi-r"},
		TargetBead:    store.Bead{ID: "spi-t", Title: "T", Status: "in_progress"},
		HumanComments: []string{"try rebase", "also check the test output"},
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "Human Guidance") {
		t.Error("missing human guidance header")
	}
	if !strings.Contains(got, "try rebase") {
		t.Error("missing first comment")
	}
	if !strings.Contains(got, "also check the test output") {
		t.Error("missing second comment")
	}
}

func TestSummarizeContext_WithFailedStepAndReason(t *testing.T) {
	ctx := &FullRecoveryContext{
		RecoveryBead:  store.Bead{ID: "spi-r"},
		TargetBead:    store.Bead{ID: "spi-t", Title: "T", Status: "in_progress"},
		FailedStep:    "verify-build",
		FailureReason: "build-failure",
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "Failed step:** verify-build") {
		t.Error("missing failed step")
	}
	if !strings.Contains(got, "Failure reason:** build-failure") {
		t.Error("missing failure reason")
	}
}

func TestSummarizeContext_ShortCommitHash(t *testing.T) {
	// Test that a short commit hash (<8 chars) doesn't panic in minInt.
	ctx := &FullRecoveryContext{
		RecoveryBead: store.Bead{ID: "spi-r"},
		TargetBead:   store.Bead{ID: "spi-t", Title: "T", Status: "open"},
		GitState: &git.BranchDiagnostics{
			LastCommitHash: "abc",
			LastCommitMsg:  "short hash",
			BranchRef:      "feat/x",
			MainRef:        "main",
		},
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "abc") {
		t.Error("missing short hash")
	}
}

func TestSummarizeContext_WorktreeNotExists(t *testing.T) {
	ctx := &FullRecoveryContext{
		RecoveryBead: store.Bead{ID: "spi-r"},
		TargetBead:   store.Bead{ID: "spi-t", Title: "T", Status: "open"},
		WorktreeState: &git.WorktreeDiagnostics{
			Exists: false,
		},
	}
	got := SummarizeContext(ctx)
	if !strings.Contains(got, "No worktree found") {
		t.Error("should show no worktree when Exists=false")
	}
}
