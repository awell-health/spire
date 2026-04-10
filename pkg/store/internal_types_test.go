package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/steveyegge/beads"
)

func TestIsWorkBead(t *testing.T) {
	tests := []struct {
		name string
		bead Bead
		want bool
	}{
		{
			name: "task with no parent is work bead",
			bead: Bead{Type: "task", Parent: ""},
			want: true,
		},
		{
			name: "bug with no parent is work bead",
			bead: Bead{Type: "bug", Parent: ""},
			want: true,
		},
		{
			name: "epic with no parent is work bead",
			bead: Bead{Type: "epic", Parent: ""},
			want: true,
		},
		{
			name: "task with parent is not work bead",
			bead: Bead{Type: "task", Parent: "spi-abc"},
			want: false,
		},
		{
			name: "message type is not work bead",
			bead: Bead{Type: "message", Parent: ""},
			want: false,
		},
		{
			name: "step type is not work bead",
			bead: Bead{Type: "step", Parent: ""},
			want: false,
		},
		{
			name: "attempt type is not work bead",
			bead: Bead{Type: "attempt", Parent: ""},
			want: false,
		},
		{
			name: "review type is not work bead",
			bead: Bead{Type: "review", Parent: ""},
			want: false,
		},
		{
			name: "internal type with parent is not work bead",
			bead: Bead{Type: "step", Parent: "spi-abc"},
			want: false,
		},
		{
			name: "empty type with no parent is work bead",
			bead: Bead{Type: "", Parent: ""},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsWorkBead(tt.bead); got != tt.want {
				t.Errorf("IsWorkBead() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsInternalBead(t *testing.T) {
	tests := []struct {
		name string
		bead Bead
		want bool
	}{
		{"message is internal", Bead{Type: "message"}, true},
		{"step is internal", Bead{Type: "step"}, true},
		{"attempt is internal", Bead{Type: "attempt"}, true},
		{"review is internal", Bead{Type: "review"}, true},
		{"task is not internal", Bead{Type: "task"}, false},
		{"bug is not internal", Bead{Type: "bug"}, false},
		{"epic is not internal", Bead{Type: "epic"}, false},
		{"empty type is not internal", Bead{Type: ""}, false},
		{"design is not internal", Bead{Type: "design"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsInternalBead(tt.bead); got != tt.want {
				t.Errorf("IsInternalBead() = %v, want %v", got, tt.want)
			}
		})
	}
}

// migrateMockStorage tracks SearchIssues and UpdateIssue calls for MigrateInternalTypes tests.
type migrateMockStorage struct {
	beads.Storage
	// issues keyed by label for SearchIssues
	issuesByLabel map[string][]*beads.Issue
	// track updates: id -> map of updates applied
	updates []migrateUpdate
}

type migrateUpdate struct {
	ID      string
	Updates map[string]interface{}
}

func (m *migrateMockStorage) SearchIssues(_ context.Context, _ string, filter beads.IssueFilter) ([]*beads.Issue, error) {
	if len(filter.Labels) > 0 {
		return m.issuesByLabel[filter.Labels[0]], nil
	}
	return nil, nil
}

func (m *migrateMockStorage) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	m.updates = append(m.updates, migrateUpdate{ID: id, Updates: updates})
	return nil
}

func (m *migrateMockStorage) Close() error { return nil }

func TestMigrateInternalTypes_HappyPath(t *testing.T) {
	mock := &migrateMockStorage{
		issuesByLabel: map[string][]*beads.Issue{
			"msg": {
				{ID: "msg-1", IssueType: "task"},
				{ID: "msg-2", IssueType: "task"},
			},
			"workflow-step": {
				{ID: "step-1", IssueType: "task"},
			},
			"attempt": {
				{ID: "att-1", IssueType: "task"},
			},
			"review-round": {
				{ID: "rev-1", IssueType: "task"},
			},
		},
	}
	setTestStore(t, mock)

	if err := MigrateInternalTypes(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect 5 updates total: 2 messages + 1 step + 1 attempt + 1 review
	if len(mock.updates) != 5 {
		t.Fatalf("expected 5 updates, got %d", len(mock.updates))
	}

	// Verify each update set the correct type
	expected := map[string]string{
		"msg-1":  "message",
		"msg-2":  "message",
		"step-1": "step",
		"att-1":  "attempt",
		"rev-1":  "review",
	}
	for _, u := range mock.updates {
		wantType, ok := expected[u.ID]
		if !ok {
			t.Errorf("unexpected update for id %s", u.ID)
			continue
		}
		gotType, _ := u.Updates["issue_type"].(string)
		if gotType != wantType {
			t.Errorf("update for %s: issue_type = %q, want %q", u.ID, gotType, wantType)
		}
	}
}

func TestMigrateInternalTypes_Idempotent(t *testing.T) {
	mock := &migrateMockStorage{
		issuesByLabel: map[string][]*beads.Issue{
			"msg": {
				{ID: "msg-1", IssueType: "message"}, // already correct
			},
			"workflow-step": {
				{ID: "step-1", IssueType: "step"}, // already correct
			},
			"attempt": {
				{ID: "att-1", IssueType: "attempt"}, // already correct
			},
			"review-round": {
				{ID: "rev-1", IssueType: "review"}, // already correct
			},
		},
	}
	setTestStore(t, mock)

	if err := MigrateInternalTypes(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No updates should be made — all beads already have correct types
	if len(mock.updates) != 0 {
		t.Errorf("expected 0 updates (idempotent), got %d", len(mock.updates))
	}
}

func TestMigrateInternalTypes_PartialMigration(t *testing.T) {
	mock := &migrateMockStorage{
		issuesByLabel: map[string][]*beads.Issue{
			"msg": {
				{ID: "msg-1", IssueType: "message"}, // already correct
				{ID: "msg-2", IssueType: "task"},     // needs migration
			},
			"workflow-step": {},
			"attempt":       {},
			"review-round":  {},
		},
	}
	setTestStore(t, mock)

	if err := MigrateInternalTypes(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only msg-2 should be updated
	if len(mock.updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(mock.updates))
	}
	if mock.updates[0].ID != "msg-2" {
		t.Errorf("expected update for msg-2, got %s", mock.updates[0].ID)
	}
	gotType, _ := mock.updates[0].Updates["issue_type"].(string)
	if gotType != "message" {
		t.Errorf("expected issue_type = %q, got %q", "message", gotType)
	}
}

func TestMigrateInternalTypes_NoBeads(t *testing.T) {
	mock := &migrateMockStorage{
		issuesByLabel: map[string][]*beads.Issue{},
	}
	setTestStore(t, mock)

	if err := MigrateInternalTypes(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.updates) != 0 {
		t.Errorf("expected 0 updates, got %d", len(mock.updates))
	}
}

// migrateMockErrorStorage returns an error from SearchIssues for a specific label.
type migrateMockErrorStorage struct {
	beads.Storage
	errLabel string
}

func (m *migrateMockErrorStorage) SearchIssues(_ context.Context, _ string, filter beads.IssueFilter) ([]*beads.Issue, error) {
	if len(filter.Labels) > 0 && filter.Labels[0] == m.errLabel {
		return nil, fmt.Errorf("database error")
	}
	return nil, nil
}

func (m *migrateMockErrorStorage) Close() error { return nil }

func TestMigrateInternalTypes_SearchError(t *testing.T) {
	mock := &migrateMockErrorStorage{errLabel: "msg"}
	setTestStore(t, mock)

	err := MigrateInternalTypes()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != `migrate internal types: search label "msg": database error` {
		t.Errorf("unexpected error message: %s", got)
	}
}

// migrateMockUpdateErrorStorage returns an error from UpdateIssue.
type migrateMockUpdateErrorStorage struct {
	beads.Storage
}

func (m *migrateMockUpdateErrorStorage) SearchIssues(_ context.Context, _ string, filter beads.IssueFilter) ([]*beads.Issue, error) {
	if len(filter.Labels) > 0 && filter.Labels[0] == "msg" {
		return []*beads.Issue{{ID: "msg-1", IssueType: "task"}}, nil
	}
	return nil, nil
}

func (m *migrateMockUpdateErrorStorage) UpdateIssue(_ context.Context, _ string, _ map[string]interface{}, _ string) error {
	return fmt.Errorf("write failed")
}

func (m *migrateMockUpdateErrorStorage) Close() error { return nil }

func TestMigrateInternalTypes_UpdateError(t *testing.T) {
	mock := &migrateMockUpdateErrorStorage{}
	setTestStore(t, mock)

	err := MigrateInternalTypes()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != `migrate internal types: update msg-1 to type "message": write failed` {
		t.Errorf("unexpected error message: %s", got)
	}
}
