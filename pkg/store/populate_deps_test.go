package store

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/beads"
)

// mockDepFetcher implements depBatchFetcher for testing.
type mockDepFetcher struct {
	deps map[string][]*beads.Dependency
	err  error
}

func (m *mockDepFetcher) GetDependencyRecordsForIssues(_ context.Context, _ []string) (map[string][]*beads.Dependency, error) {
	return m.deps, m.err
}

// plainStorage is a beads.Storage that does NOT implement depBatchFetcher.
type plainStorage struct{ beads.Storage }

func TestPopulateDependencies(t *testing.T) {
	parentDep := &beads.Dependency{
		IssueID:     "child-1",
		DependsOnID: "parent-1",
		Type:        beads.DepParentChild,
	}

	tests := []struct {
		name       string
		storage    beads.Storage
		issues     []*beads.Issue
		wantParent map[string]string // issueID -> expected FindParentID result
	}{
		{
			name:    "empty slice is no-op",
			storage: &mockDepFetcher{},
			issues:  nil,
		},
		{
			name:    "store without depBatchFetcher is no-op",
			storage: plainStorage{},
			issues:  []*beads.Issue{{ID: "a"}},
			wantParent: map[string]string{
				"a": "",
			},
		},
		{
			name: "fetch error leaves deps unpopulated",
			storage: &mockDepFetcher{
				err: errors.New("db gone"),
			},
			issues: []*beads.Issue{{ID: "b"}},
			wantParent: map[string]string{
				"b": "",
			},
		},
		{
			name: "happy path populates dependencies",
			storage: &mockDepFetcher{
				deps: map[string][]*beads.Dependency{
					"child-1": {parentDep},
				},
			},
			issues: []*beads.Issue{
				{ID: "child-1"},
				{ID: "orphan"},
			},
			wantParent: map[string]string{
				"child-1": "parent-1",
				"orphan":  "",
			},
		},
	}

	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			PopulateDependencies(ctx, tt.storage, tt.issues)
			for _, issue := range tt.issues {
				want := tt.wantParent[issue.ID]
				got := FindParentID(issue.Dependencies)
				if got != want {
					t.Errorf("issue %s: FindParentID = %q, want %q", issue.ID, got, want)
				}
			}
		})
	}
}
