package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/steveyegge/beads"
)

// mockStorage embeds beads.Storage to satisfy the full interface.
// Only SearchIssues is overridden; all other methods delegate to the
// embedded nil interface and will panic if called (tests never call them).
type mockStorage struct {
	beads.Storage
	issues map[string][]*beads.Issue
}

func (m *mockStorage) SearchIssues(_ context.Context, _ string, filter beads.IssueFilter) ([]*beads.Issue, error) {
	if filter.ParentID == nil {
		return nil, nil
	}
	return m.issues[*filter.ParentID], nil
}

func (m *mockStorage) Close() error { return nil }

// setTestStore installs a mock store for the duration of a test and restores
// the original store when the test completes.
func setTestStore(t *testing.T, s beads.Storage) {
	t.Helper()
	prev := activeStore
	prevCtx := storeCtx
	activeStore = s
	storeCtx = context.Background()
	t.Cleanup(func() {
		activeStore = prev
		storeCtx = prevCtx
	})
}

func TestGetChildrenBatch_Grouped(t *testing.T) {
	mock := &mockStorage{
		issues: map[string][]*beads.Issue{
			"parent-1": {
				{ID: "child-1a", Title: "Child 1A", Status: beads.StatusOpen, IssueType: beads.TypeTask},
				{ID: "child-1b", Title: "Child 1B", Status: beads.StatusClosed, IssueType: beads.TypeTask},
			},
			"parent-2": {
				{ID: "child-2a", Title: "Child 2A", Status: beads.StatusInProgress, IssueType: beads.TypeBug},
			},
		},
	}
	setTestStore(t, mock)

	result, err := GetChildrenBatch([]string{"parent-1", "parent-2", "parent-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// parent-1 should have 2 children
	if len(result["parent-1"]) != 2 {
		t.Errorf("parent-1: expected 2 children, got %d", len(result["parent-1"]))
	}
	if result["parent-1"][0].ID != "child-1a" {
		t.Errorf("parent-1[0]: expected child-1a, got %s", result["parent-1"][0].ID)
	}
	if result["parent-1"][1].ID != "child-1b" {
		t.Errorf("parent-1[1]: expected child-1b, got %s", result["parent-1"][1].ID)
	}

	// parent-2 should have 1 child
	if len(result["parent-2"]) != 1 {
		t.Errorf("parent-2: expected 1 child, got %d", len(result["parent-2"]))
	}
	if result["parent-2"][0].ID != "child-2a" {
		t.Errorf("parent-2[0]: expected child-2a, got %s", result["parent-2"][0].ID)
	}

	// parent-3 should have nil/empty slice (no children in mock)
	if len(result["parent-3"]) != 0 {
		t.Errorf("parent-3: expected 0 children, got %d", len(result["parent-3"]))
	}

	// Verify Bead fields are populated correctly (IssueToBead conversion)
	child := result["parent-2"][0]
	if child.Title != "Child 2A" {
		t.Errorf("expected title 'Child 2A', got %q", child.Title)
	}
	if child.Status != "in_progress" {
		t.Errorf("expected status 'in_progress', got %q", child.Status)
	}
	if child.Type != "bug" {
		t.Errorf("expected type 'bug', got %q", child.Type)
	}
}

func TestGetChildrenBatch_Empty(t *testing.T) {
	// Should return empty map without querying — no store needed.
	result, err := GetChildrenBatch([]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil empty map, got nil")
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestGetChildrenBatch_NilInput(t *testing.T) {
	result, err := GetChildrenBatch(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil empty map, got nil")
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

// mockErrorStorage returns an error for a specific parent.
type mockErrorStorage struct {
	beads.Storage
}

func (m *mockErrorStorage) SearchIssues(_ context.Context, _ string, filter beads.IssueFilter) ([]*beads.Issue, error) {
	if filter.ParentID != nil && *filter.ParentID == "bad-parent" {
		return nil, fmt.Errorf("database connection lost")
	}
	return nil, nil
}

func (m *mockErrorStorage) Close() error { return nil }

func TestGetChildrenBatch_Error(t *testing.T) {
	setTestStore(t, &mockErrorStorage{})

	_, err := GetChildrenBatch([]string{"good-parent", "bad-parent"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	expected := "get children of bad-parent: database connection lost"
	if got := err.Error(); got != expected {
		t.Errorf("expected error %q, got %q", expected, got)
	}
}
