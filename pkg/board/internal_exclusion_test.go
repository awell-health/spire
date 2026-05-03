package board

import (
	"context"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// internalExclusionStorage is a minimal beads.Storage implementation for the
// internal-type-exclusion regression test. It returns a fixed set of issues
// from SearchIssues (filtered by status), and never returns blocked issues.
type internalExclusionStorage struct {
	beads.Storage
	openIssues   []*beads.Issue
	closedIssues []*beads.Issue
}

func (s *internalExclusionStorage) SearchIssues(_ context.Context, _ string, filter beads.IssueFilter) ([]*beads.Issue, error) {
	// The closed-bead query sets filter.Status == StatusClosed.
	if filter.Status != nil && *filter.Status == beads.StatusClosed {
		return s.closedIssues, nil
	}
	// All other calls are for open beads (ExcludeStatus == [closed]).
	return s.openIssues, nil
}

func (s *internalExclusionStorage) GetBlockedIssues(_ context.Context, _ beads.WorkFilter) ([]*beads.BlockedIssue, error) {
	return nil, nil
}

func (s *internalExclusionStorage) Close() error { return nil }

// TestFetchBoard_InternalTypesExcluded asserts that beads with internal types
// (step, attempt, message, review) never surface in any board column. Goes
// through the public FetchBoard API so it catches regressions where someone
// adds a code path that bypasses skipBead.
func TestFetchBoard_InternalTypesExcluded(t *testing.T) {
	now := time.Now()
	mock := &internalExclusionStorage{
		openIssues: []*beads.Issue{
			// Public work beads — should appear on the board.
			{ID: "spi-task1", Title: "regular task", Status: beads.StatusOpen, IssueType: beads.TypeTask, Priority: 2, CreatedAt: now, UpdatedAt: now},
			{ID: "spi-ready1", Title: "ready task", Status: beads.Status("ready"), IssueType: beads.TypeTask, Priority: 1, CreatedAt: now, UpdatedAt: now},
			// Internal beads — must be excluded from every column.
			{ID: "spi-step1", Title: "step:implement", Status: beads.StatusOpen, IssueType: "step", Priority: 3, CreatedAt: now, UpdatedAt: now},
			{ID: "spi-attempt1", Title: "attempt:1", Status: beads.StatusInProgress, IssueType: "attempt", Priority: 3, CreatedAt: now, UpdatedAt: now},
			{ID: "spi-msg1", Title: "msg", Status: beads.StatusOpen, IssueType: "message", Priority: 4, CreatedAt: now, UpdatedAt: now},
			{ID: "spi-rev1", Title: "review round", Status: beads.StatusOpen, IssueType: "review", Priority: 3, CreatedAt: now, UpdatedAt: now},
		},
		closedIssues: []*beads.Issue{
			// Closed internal bead must not appear in Done either.
			{ID: "spi-step2", Title: "step:plan", Status: beads.StatusClosed, IssueType: "step", Priority: 3, CreatedAt: now, UpdatedAt: now},
		},
	}
	cleanup := store.SetTestStorage(mock)
	defer cleanup()

	result, err := FetchBoard(Opts{}, "me")
	if err != nil {
		t.Fatalf("FetchBoard: %v", err)
	}

	internalIDs := map[string]bool{
		"spi-step1":    true,
		"spi-step2":    true,
		"spi-attempt1": true,
		"spi-msg1":     true,
		"spi-rev1":     true,
	}

	// Walk every column slice and assert no internal bead leaked through.
	allColumns := map[string][]BoardBead{
		"Alerts":         result.Columns.Alerts,
		"AwaitingReview": result.Columns.AwaitingReview,
		"NeedsChanges":   result.Columns.NeedsChanges,
		"AwaitingHuman":  result.Columns.AwaitingHuman,
		"MergePending":   result.Columns.MergePending,
		"Backlog":        result.Columns.Backlog,
		"Ready":          result.Columns.Ready,
		"InProgress":     result.Columns.InProgress,
		"Design":         result.Columns.Design,
		"Plan":           result.Columns.Plan,
		"Implement":      result.Columns.Implement,
		"Review":         result.Columns.Review,
		"Merge":          result.Columns.Merge,
		"Done":           result.Columns.Done,
		"Blocked":        result.Columns.Blocked,
	}
	for colName, beads := range allColumns {
		for _, b := range beads {
			if internalIDs[b.ID] {
				t.Errorf("internal bead %s (type=%s) leaked into column %s", b.ID, b.Type, colName)
			}
		}
	}

	// Sanity-check that the public work beads still made it through —
	// otherwise we could be passing this test by filtering everything out.
	foundTask := false
	for _, b := range result.Columns.Backlog {
		if b.ID == "spi-task1" {
			foundTask = true
		}
	}
	if !foundTask {
		t.Errorf("expected spi-task1 in Backlog (sanity check), got %v", result.Columns.Backlog)
	}
	foundReady := false
	for _, b := range result.Columns.Ready {
		if b.ID == "spi-ready1" {
			foundReady = true
		}
	}
	if !foundReady {
		t.Errorf("expected spi-ready1 in Ready (sanity check), got %v", result.Columns.Ready)
	}
}
