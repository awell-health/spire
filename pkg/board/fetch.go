package board

import (
	"fmt"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// FetchBoard loads beads from the store and categorizes them into board columns.
func FetchBoard(opts Opts, identity string) (Columns, error) {
	openBeads, err := store.ListBoardBeads(beads.IssueFilter{
		ExcludeStatus: []beads.Status{beads.StatusClosed},
	})
	if err != nil {
		return Columns{}, fmt.Errorf("board: list open beads: %w", err)
	}

	closedBeads, _ := store.ListBoardBeads(beads.IssueFilter{
		Status: store.StatusPtr(beads.StatusClosed),
	})

	blockedBeads, _ := store.GetBlockedIssues(beads.WorkFilter{})

	cols := CategorizeColumnsFromStore(openBeads, closedBeads, blockedBeads, identity)

	if opts.Epic != "" {
		cols = FilterEpic(cols, opts.Epic)
	}
	if opts.Mine {
		cols.Ready = nil
		cols.Design = FilterOwned(cols.Design, identity)
		cols.Plan = FilterOwned(cols.Plan, identity)
		cols.Implement = FilterOwned(cols.Implement, identity)
		cols.Review = FilterOwned(cols.Review, identity)
		cols.Merge = FilterOwned(cols.Merge, identity)
		cols.Blocked = FilterOwned(cols.Blocked, identity)
	}
	if opts.Ready {
		cols.Design = nil
		cols.Plan = nil
		cols.Implement = nil
		cols.Review = nil
		cols.Merge = nil
		cols.Done = nil
		cols.Blocked = nil
	}

	SortBeads(cols.Ready)
	SortBeads(cols.Design)
	SortBeads(cols.Plan)
	SortBeads(cols.Implement)
	SortBeads(cols.Review)
	SortBeads(cols.Merge)
	SortBeads(cols.Done)
	SortBeads(cols.Blocked)

	return cols, nil
}
