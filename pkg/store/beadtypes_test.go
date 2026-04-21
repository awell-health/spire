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
