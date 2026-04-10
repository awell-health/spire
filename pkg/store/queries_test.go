package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

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

// --- GetChildrenBoardBatch tests ---

func TestGetChildrenBoardBatch_Empty(t *testing.T) {
	// Should return empty map without querying — no store needed.
	result, err := GetChildrenBoardBatch([]string{})
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

func TestGetChildrenBoardBatch_NilInput(t *testing.T) {
	result, err := GetChildrenBoardBatch(nil)
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

func TestGetChildrenBoardBatch_Grouped(t *testing.T) {
	now := time.Now()
	earlier := now.Add(-1 * time.Hour)
	closed := now.Add(-30 * time.Minute)

	mock := &mockStorage{
		issues: map[string][]*beads.Issue{
			"parent-1": {
				{
					ID:        "child-1a",
					Title:     "attempt: wizard",
					Status:    beads.StatusClosed,
					IssueType: beads.TypeTask,
					CreatedAt: earlier,
					UpdatedAt: now,
					ClosedAt:  &closed,
					Labels:    []string{"attempt", "result:success"},
				},
			},
			"parent-2": {
				{
					ID:        "child-2a",
					Title:     "review-round-1",
					Status:    beads.StatusOpen,
					IssueType: beads.TypeTask,
					CreatedAt: earlier,
					UpdatedAt: now,
				},
			},
		},
	}
	setTestStore(t, mock)

	result, err := GetChildrenBoardBatch([]string{"parent-1", "parent-2", "parent-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// parent-1 should have 1 child with timestamps populated.
	if len(result["parent-1"]) != 1 {
		t.Fatalf("parent-1: expected 1 child, got %d", len(result["parent-1"]))
	}
	bb := result["parent-1"][0]
	if bb.ID != "child-1a" {
		t.Errorf("expected ID child-1a, got %s", bb.ID)
	}
	if bb.CreatedAt == "" {
		t.Error("expected CreatedAt to be populated")
	}
	if bb.ClosedAt == "" {
		t.Error("expected ClosedAt to be populated for closed issue")
	}
	if bb.Title != "attempt: wizard" {
		t.Errorf("expected title 'attempt: wizard', got %q", bb.Title)
	}

	// parent-2 should have 1 child.
	if len(result["parent-2"]) != 1 {
		t.Fatalf("parent-2: expected 1 child, got %d", len(result["parent-2"]))
	}
	if result["parent-2"][0].ClosedAt != "" {
		t.Errorf("expected empty ClosedAt for open issue, got %q", result["parent-2"][0].ClosedAt)
	}

	// parent-3 should have empty slice.
	if len(result["parent-3"]) != 0 {
		t.Errorf("parent-3: expected 0 children, got %d", len(result["parent-3"]))
	}
}

func TestGetChildrenBoardBatch_Error(t *testing.T) {
	setTestStore(t, &mockErrorStorage{})

	_, err := GetChildrenBoardBatch([]string{"good-parent", "bad-parent"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bad-parent") {
		t.Errorf("expected error to mention bad-parent, got %q", err.Error())
	}
}

// --- GetReadyWork parent filtering tests ---

// readyWorkMockStorage overrides GetReadyWork and SearchIssues for testing
// the post-filter logic in GetReadyWork.
type readyWorkMockStorage struct {
	beads.Storage
	readyIssues []*beads.Issue
}

func (m *readyWorkMockStorage) GetReadyWork(_ context.Context, _ beads.WorkFilter) ([]*beads.Issue, error) {
	return m.readyIssues, nil
}

func (m *readyWorkMockStorage) SearchIssues(_ context.Context, _ string, _ beads.IssueFilter) ([]*beads.Issue, error) {
	// Used by GetChildren inside GetActiveAttempt — return nothing by default.
	return nil, nil
}

func (m *readyWorkMockStorage) Close() error { return nil }

func TestGetReadyWork_ParentFiltering(t *testing.T) {
	now := time.Now()
	mock := &readyWorkMockStorage{
		readyIssues: []*beads.Issue{
			{
				ID:        "spi-top",
				Title:     "Top-level task",
				Status:    beads.StatusOpen,
				IssueType: beads.TypeTask,
				CreatedAt: now,
				UpdatedAt: now,
			},
			{
				ID:        "spi-epic.1",
				Title:     "Epic child task",
				Status:    beads.StatusOpen,
				IssueType: beads.TypeTask,
				CreatedAt: now,
				UpdatedAt: now,
				Dependencies: []*beads.Dependency{
					{
						IssueID:     "spi-epic.1",
						DependsOnID: "spi-epic",
						Type:        beads.DepParentChild,
					},
				},
			},
			{
				ID:        "spi-another",
				Title:     "Another top-level",
				Status:    beads.StatusOpen,
				IssueType: beads.TypeTask,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
	setTestStore(t, mock)

	result, err := GetReadyWork(beads.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// spi-epic.1 has a parent — it should be filtered out.
	for _, b := range result {
		if b.ID == "spi-epic.1" {
			t.Errorf("epic child spi-epic.1 should be filtered from GetReadyWork results")
		}
	}

	// The two top-level beads should remain.
	ids := make(map[string]bool)
	for _, b := range result {
		ids[b.ID] = true
	}
	if !ids["spi-top"] {
		t.Error("expected spi-top in results")
	}
	if !ids["spi-another"] {
		t.Error("expected spi-another in results")
	}
	if len(result) != 2 {
		t.Errorf("expected 2 results, got %d", len(result))
	}
}
