package store

import (
	"context"
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
