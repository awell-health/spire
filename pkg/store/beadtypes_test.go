package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/steveyegge/beads"
)

// mockCommentStorage extends mockStorage with GetIssueComments support.
type mockCommentStorage struct {
	beads.Storage
	comments map[string][]*beads.Comment
}

func (m *mockCommentStorage) SearchIssues(_ context.Context, _ string, _ beads.IssueFilter) ([]*beads.Issue, error) {
	return nil, nil
}

func (m *mockCommentStorage) GetIssueComments(_ context.Context, id string) ([]*beads.Comment, error) {
	return m.comments[id], nil
}

func (m *mockCommentStorage) Close() error { return nil }

func TestAttemptResult(t *testing.T) {
	tests := []struct {
		name     string
		bead     Bead
		comments map[string][]*beads.Comment
		want     string
	}{
		{
			name: "result from label (fast path)",
			bead: Bead{ID: "att-1", Labels: []string{"attempt", "result:success"}},
			want: "success",
		},
		{
			name: "result from label with failure",
			bead: Bead{ID: "att-2", Labels: []string{"attempt", "result:failure"}},
			want: "failure",
		},
		{
			name: "fallback to comment when no label",
			bead: Bead{ID: "att-3", Labels: []string{"attempt"}},
			comments: map[string][]*beads.Comment{
				"att-3": {
					{Text: "starting work"},
					{Text: "success"},
				},
			},
			want: "success",
		},
		{
			name: "fallback walks comments in reverse",
			bead: Bead{ID: "att-4", Labels: []string{"attempt"}},
			comments: map[string][]*beads.Comment{
				"att-4": {
					{Text: "success"},
					{Text: "some log output"},
					{Text: "failure"},
				},
			},
			want: "failure",
		},
		{
			name: "no result found — no label, no matching comment",
			bead: Bead{ID: "att-5", Labels: []string{"attempt"}},
			comments: map[string][]*beads.Comment{
				"att-5": {
					{Text: "starting work"},
					{Text: "still working"},
				},
			},
			want: "",
		},
		{
			name:     "no result found — no label, empty comments",
			bead:     Bead{ID: "att-6", Labels: []string{"attempt"}},
			comments: map[string][]*beads.Comment{},
			want:     "",
		},
		{
			name: "no result found — no label, nil comments",
			bead: Bead{ID: "att-7", Labels: []string{"attempt"}},
			want: "",
		},
		{
			name: "comment with whitespace is trimmed",
			bead: Bead{ID: "att-8", Labels: []string{"attempt"}},
			comments: map[string][]*beads.Comment{
				"att-8": {
					{Text: "  timeout  "},
				},
			},
			want: "timeout",
		},
		{
			name: "all known result values recognized in comments",
			bead: Bead{ID: "att-9", Labels: []string{"attempt"}},
			comments: map[string][]*beads.Comment{
				"att-9": {
					{Text: "review_rejected"},
				},
			},
			want: "review_rejected",
		},
		{
			name: "label takes precedence over comments",
			bead: Bead{ID: "att-10", Labels: []string{"attempt", "result:success"}},
			comments: map[string][]*beads.Comment{
				"att-10": {
					{Text: "failure"},
				},
			},
			want: "success",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockCommentStorage{comments: tt.comments}
			setTestStore(t, mock)

			got := AttemptResult(tt.bead)
			if got != tt.want {
				t.Errorf("AttemptResult() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsAttemptBead(t *testing.T) {
	tests := []struct {
		name string
		bead Bead
		want bool
	}{
		{"by title prefix", Bead{Title: "attempt: wizard"}, true},
		{"by label", Bead{Title: "something", Labels: []string{"attempt"}}, true},
		{"not attempt", Bead{Title: "fix bug", Labels: []string{"review-round"}}, false},
		{"empty", Bead{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAttemptBead(tt.bead); got != tt.want {
				t.Errorf("IsAttemptBead() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsReviewRoundBead(t *testing.T) {
	tests := []struct {
		name string
		bead Bead
		want bool
	}{
		{"by title prefix", Bead{Title: "review-round-1"}, true},
		{"by label", Bead{Title: "round 1", Labels: []string{"review-round"}}, true},
		{"not review", Bead{Title: "fix bug"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReviewRoundBead(tt.bead); got != tt.want {
				t.Errorf("IsReviewRoundBead() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStepBeadPhaseName(t *testing.T) {
	tests := []struct {
		name string
		bead Bead
		want string
	}{
		{"has step label", Bead{Labels: []string{"workflow-step", "step:implement"}}, "implement"},
		{"no step label", Bead{Labels: []string{"workflow-step"}}, ""},
		{"empty labels", Bead{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StepBeadPhaseName(tt.bead); got != tt.want {
				t.Errorf("StepBeadPhaseName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// hookMockStorage supports GetIssue, UpdateIssue, and SearchIssues for
// HookStepBead/UnhookStepBead/GetHookedSteps tests.
type hookMockStorage struct {
	beads.Storage
	issues    map[string]*beads.Issue
	children  map[string][]*beads.Issue
	updates   []migrateUpdate
	getErr    map[string]error
	updateErr error
}

func (m *hookMockStorage) GetIssue(_ context.Context, id string) (*beads.Issue, error) {
	if err, ok := m.getErr[id]; ok {
		return nil, err
	}
	if issue, ok := m.issues[id]; ok {
		// Non-nil Dependencies so GetBead skips GetDependenciesWithMetadata.
		if issue.Dependencies == nil {
			issue.Dependencies = []*beads.Dependency{}
		}
		return issue, nil
	}
	return nil, fmt.Errorf("issue not found: %s", id)
}

func (m *hookMockStorage) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	m.updates = append(m.updates, migrateUpdate{ID: id, Updates: updates})
	// Reflect the update in the mock so subsequent GetIssue calls see it.
	if issue, ok := m.issues[id]; ok {
		if s, ok := updates["status"].(string); ok {
			issue.Status = beads.Status(s)
		}
	}
	return nil
}

func (m *hookMockStorage) SearchIssues(_ context.Context, _ string, filter beads.IssueFilter) ([]*beads.Issue, error) {
	if filter.ParentID == nil {
		return nil, nil
	}
	return m.children[*filter.ParentID], nil
}

func (m *hookMockStorage) Close() error { return nil }

func TestHookStepBead(t *testing.T) {
	tests := []struct {
		name       string
		stepID     string
		issues     map[string]*beads.Issue
		getErr     map[string]error
		wantErr    bool
		wantStatus beads.Status
	}{
		{
			name:   "hooks a step bead successfully",
			stepID: "step-1",
			issues: map[string]*beads.Issue{
				"step-1": {ID: "step-1", IssueType: "step", Status: beads.StatusInProgress},
			},
			wantStatus: StatusHooked,
		},
		{
			name:   "rejects non-step bead (task)",
			stepID: "task-1",
			issues: map[string]*beads.Issue{
				"task-1": {ID: "task-1", IssueType: beads.TypeTask, Status: beads.StatusInProgress},
			},
			wantErr: true,
		},
		{
			name:   "rejects non-step bead (attempt)",
			stepID: "att-1",
			issues: map[string]*beads.Issue{
				"att-1": {ID: "att-1", IssueType: "attempt", Status: beads.StatusInProgress},
			},
			wantErr: true,
		},
		{
			name:    "propagates GetBead error",
			stepID:  "missing-1",
			getErr:  map[string]error{"missing-1": fmt.Errorf("not found")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &hookMockStorage{issues: tt.issues, getErr: tt.getErr}
			setTestStore(t, mock)

			err := HookStepBead(tt.stepID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("HookStepBead() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if len(mock.updates) != 0 {
					t.Errorf("expected no UpdateIssue call on error, got %d", len(mock.updates))
				}
				return
			}
			if len(mock.updates) != 1 {
				t.Fatalf("expected 1 UpdateIssue call, got %d", len(mock.updates))
			}
			u := mock.updates[0]
			if u.ID != tt.stepID {
				t.Errorf("UpdateIssue id = %q, want %q", u.ID, tt.stepID)
			}
			gotStatus, _ := u.Updates["status"].(string)
			if beads.Status(gotStatus) != tt.wantStatus {
				t.Errorf("UpdateIssue status = %q, want %q", gotStatus, tt.wantStatus)
			}
		})
	}
}

func TestUnhookStepBead(t *testing.T) {
	tests := []struct {
		name       string
		stepID     string
		issues     map[string]*beads.Issue
		getErr     map[string]error
		wantErr    bool
		wantStatus string
	}{
		{
			name:   "unhooks a hooked step back to open",
			stepID: "step-1",
			issues: map[string]*beads.Issue{
				"step-1": {ID: "step-1", IssueType: "step", Status: StatusHooked},
			},
			wantStatus: "open",
		},
		{
			name:   "rejects non-step bead",
			stepID: "task-1",
			issues: map[string]*beads.Issue{
				"task-1": {ID: "task-1", IssueType: beads.TypeTask, Status: StatusHooked},
			},
			wantErr: true,
		},
		{
			name:    "propagates GetBead error",
			stepID:  "missing-1",
			getErr:  map[string]error{"missing-1": fmt.Errorf("not found")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &hookMockStorage{issues: tt.issues, getErr: tt.getErr}
			setTestStore(t, mock)

			err := UnhookStepBead(tt.stepID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("UnhookStepBead() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if len(mock.updates) != 0 {
					t.Errorf("expected no UpdateIssue call on error, got %d", len(mock.updates))
				}
				return
			}
			if len(mock.updates) != 1 {
				t.Fatalf("expected 1 UpdateIssue call, got %d", len(mock.updates))
			}
			u := mock.updates[0]
			if u.ID != tt.stepID {
				t.Errorf("UpdateIssue id = %q, want %q", u.ID, tt.stepID)
			}
			gotStatus, _ := u.Updates["status"].(string)
			if gotStatus != tt.wantStatus {
				t.Errorf("UpdateIssue status = %q, want %q", gotStatus, tt.wantStatus)
			}
		})
	}
}

func TestGetHookedSteps(t *testing.T) {
	parentID := "parent-1"
	mock := &hookMockStorage{
		children: map[string][]*beads.Issue{
			parentID: {
				{
					ID: "step-1", Title: "step:plan", IssueType: "step",
					Status: beads.StatusClosed, Labels: []string{"workflow-step", "step:plan"},
				},
				{
					ID: "step-2", Title: "step:implement", IssueType: "step",
					Status: StatusHooked, Labels: []string{"workflow-step", "step:implement"},
				},
				{
					ID: "step-3", Title: "step:review", IssueType: "step",
					Status: beads.StatusOpen, Labels: []string{"workflow-step", "step:review"},
				},
				{
					ID: "step-4", Title: "step:merge", IssueType: "step",
					Status: StatusHooked, Labels: []string{"workflow-step", "step:merge"},
				},
				// A non-step child that should be filtered out even if status=hooked.
				{
					ID: "att-1", Title: "attempt: wizard", IssueType: "attempt",
					Status: StatusHooked, Labels: []string{"attempt"},
				},
			},
		},
	}
	setTestStore(t, mock)

	hooked, err := GetHookedSteps(parentID)
	if err != nil {
		t.Fatalf("GetHookedSteps() error = %v", err)
	}
	if len(hooked) != 2 {
		t.Fatalf("expected 2 hooked steps, got %d", len(hooked))
	}
	gotIDs := map[string]bool{hooked[0].ID: true, hooked[1].ID: true}
	for _, want := range []string{"step-2", "step-4"} {
		if !gotIDs[want] {
			t.Errorf("expected hooked step %s in results, got %v", want, gotIDs)
		}
	}
}

func TestGetHookedSteps_NoneHooked(t *testing.T) {
	parentID := "parent-1"
	mock := &hookMockStorage{
		children: map[string][]*beads.Issue{
			parentID: {
				{
					ID: "step-1", IssueType: "step",
					Status: beads.StatusOpen, Labels: []string{"workflow-step", "step:plan"},
				},
			},
		},
	}
	setTestStore(t, mock)

	hooked, err := GetHookedSteps(parentID)
	if err != nil {
		t.Fatalf("GetHookedSteps() error = %v", err)
	}
	if len(hooked) != 0 {
		t.Errorf("expected 0 hooked steps, got %d", len(hooked))
	}
}

// TestMaxRoundNumberFromBeads_Monotonic covers the spi-cjotlm round counter
// behavior: scanning round:<N> labels across review-round children must return
// the numeric maximum so the next round = max + 1, regardless of bead status
// or insertion order. This is what makes round numbers monotonic across
// reset cycles.
func TestMaxRoundNumberFromBeads_Monotonic(t *testing.T) {
	tests := []struct {
		name string
		in   []Bead
		want int
	}{
		{
			name: "empty input → 0 (next round will be 1)",
			in:   nil,
			want: 0,
		},
		{
			name: "single review with round:3 → 3",
			in: []Bead{
				{ID: "r-1", Title: "review-round-3", Status: "closed", Labels: []string{"review-round", "round:3"}},
			},
			want: 3,
		},
		{
			name: "rounds 1, 2, 3 → 3 (closed reviews preserved across reset)",
			in: []Bead{
				{ID: "r-3", Title: "review-round-3", Status: "closed", Labels: []string{"review-round", "round:3"}},
				{ID: "r-1", Title: "review-round-1", Status: "closed", Labels: []string{"review-round", "round:1"}},
				{ID: "r-2", Title: "review-round-2", Status: "closed", Labels: []string{"review-round", "round:2"}},
			},
			want: 3,
		},
		{
			name: "round:10 outranks round:2 (numeric, not lexical)",
			in: []Bead{
				{ID: "r-2", Title: "review-round-2", Status: "closed", Labels: []string{"review-round", "round:2"}},
				{ID: "r-10", Title: "review-round-10", Status: "closed", Labels: []string{"review-round", "round:10"}},
			},
			want: 10,
		},
		{
			name: "non-review beads are ignored",
			in: []Bead{
				{ID: "att-1", Title: "attempt: w", Status: "closed", Labels: []string{"attempt", "round:99"}},
				{ID: "r-1", Title: "review-round-1", Status: "closed", Labels: []string{"review-round", "round:1"}},
			},
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaxRoundNumberFromBeads(tt.in); got != tt.want {
				t.Errorf("MaxRoundNumberFromBeads() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestMaxRoundNumberFromBeads_HandlesMalformed verifies the helper is robust
// against malformed round labels — they are skipped, not panicked on, and
// the max scan continues across the rest of the input.
func TestMaxRoundNumberFromBeads_HandlesMalformed(t *testing.T) {
	in := []Bead{
		{ID: "r-good", Title: "review-round-2", Status: "closed", Labels: []string{"review-round", "round:2"}},
		{ID: "r-empty", Title: "review-round-x", Status: "closed", Labels: []string{"review-round", "round:"}},
		{ID: "r-alpha", Title: "review-round-x", Status: "closed", Labels: []string{"review-round", "round:abc"}},
	}
	if got := MaxRoundNumberFromBeads(in); got != 2 {
		t.Errorf("MaxRoundNumberFromBeads() with malformed labels = %d, want 2", got)
	}
}

// TestAttemptNumber covers parsing of the attempt:<N> label introduced in
// spi-cjotlm. Missing or malformed labels return 0 (the legacy default for
// pre-feature attempts).
func TestAttemptNumber(t *testing.T) {
	tests := []struct {
		name string
		bead Bead
		want int
	}{
		{"with attempt:5", Bead{Labels: []string{"attempt", "attempt:5"}}, 5},
		{"with attempt:1", Bead{Labels: []string{"attempt", "attempt:1"}}, 1},
		{"no attempt label (legacy)", Bead{Labels: []string{"attempt", "agent:wizard"}}, 0},
		{"empty labels", Bead{}, 0},
		{"malformed attempt:abc", Bead{Labels: []string{"attempt", "attempt:abc"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AttemptNumber(tt.bead); got != tt.want {
				t.Errorf("AttemptNumber() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestResetCycleNumber_DefaultsToOne covers the migration policy: beads
// without a reset-cycle:<N> label are treated as cycle 1 (the implicit first
// cycle), and malformed labels also default to 1.
func TestResetCycleNumber_DefaultsToOne(t *testing.T) {
	tests := []struct {
		name string
		bead Bead
		want int
	}{
		{"no label → cycle 1 (pre-feature default)", Bead{Labels: []string{"attempt"}}, 1},
		{"empty labels → cycle 1", Bead{}, 1},
		{"reset-cycle:1", Bead{Labels: []string{"attempt", "reset-cycle:1"}}, 1},
		{"reset-cycle:5", Bead{Labels: []string{"attempt", "reset-cycle:5"}}, 5},
		{"reset-cycle:42", Bead{Labels: []string{"attempt", "reset-cycle:42"}}, 42},
		{"malformed reset-cycle: → 1", Bead{Labels: []string{"reset-cycle:"}}, 1},
		{"malformed reset-cycle:abc → 1", Bead{Labels: []string{"reset-cycle:abc"}}, 1},
		{"malformed reset-cycle:0 → 1 (cycles are 1-indexed)", Bead{Labels: []string{"reset-cycle:0"}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResetCycleNumber(tt.bead); got != tt.want {
				t.Errorf("ResetCycleNumber() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestMaxAttemptNumberFromBeads covers attempt-counter parity with the round
// counter. Like rounds, attempts must be picked numerically so attempt:10
// outranks attempt:2.
func TestMaxAttemptNumberFromBeads(t *testing.T) {
	tests := []struct {
		name string
		in   []Bead
		want int
	}{
		{"empty → 0", nil, 0},
		{
			name: "max across attempts (numeric, not lexical)",
			in: []Bead{
				{ID: "a-2", Title: "attempt: w", Labels: []string{"attempt", "attempt:2"}},
				{ID: "a-10", Title: "attempt: w", Labels: []string{"attempt", "attempt:10"}},
			},
			want: 10,
		},
		{
			name: "non-attempt beads ignored",
			in: []Bead{
				{ID: "r-1", Title: "review-round-1", Labels: []string{"review-round", "attempt:99"}},
				{ID: "a-1", Title: "attempt: w", Labels: []string{"attempt", "attempt:1"}},
			},
			want: 1,
		},
		{
			name: "legacy attempts (no attempt:N label) → 0",
			in: []Bead{
				{ID: "a-old", Title: "attempt: w", Labels: []string{"attempt", "agent:wizard"}},
			},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaxAttemptNumberFromBeads(tt.in); got != tt.want {
				t.Errorf("MaxAttemptNumberFromBeads() = %d, want %d", got, tt.want)
			}
		})
	}
}
