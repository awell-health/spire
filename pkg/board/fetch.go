package board

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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

	closedCutoff := time.Now().Add(-24 * time.Hour)
	closedBeads, _ := store.ListBoardBeads(beads.IssueFilter{
		Status:      store.StatusPtr(beads.StatusClosed),
		ClosedAfter: &closedCutoff,
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

// snapshotMsg carries the result of a background snapshot fetch.
type snapshotMsg struct {
	Snap *BoardSnapshot
	Err  error
}

// fetchSnapshotCmd returns a tea.Cmd that fetches a BoardSnapshot in the background.
func fetchSnapshotCmd(opts Opts, identity string, fetchAgents func() []LocalAgent) tea.Cmd {
	return func() tea.Msg {
		snap, err := fetchSnapshot(opts, identity, fetchAgents)
		return snapshotMsg{Snap: snap, Err: err}
	}
}

// fetchSnapshot assembles a complete BoardSnapshot in a single pass with minimal
// DB queries: 3 bulk bead queries + 1 GetChildrenBatch = 4 total.
func fetchSnapshot(opts Opts, identity string, fetchAgents func() []LocalAgent) (*BoardSnapshot, error) {
	// 1. Bulk-fetch beads — same store calls as FetchBoard.
	openBeads, err := store.ListBoardBeads(beads.IssueFilter{
		ExcludeStatus: []beads.Status{beads.StatusClosed},
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot: list open beads: %w", err)
	}

	closedCutoff := time.Now().Add(-24 * time.Hour)
	closedBeads, _ := store.ListBoardBeads(beads.IssueFilter{
		Status:      store.StatusPtr(beads.StatusClosed),
		ClosedAfter: &closedCutoff,
	})

	blockedBeads, _ := store.GetBlockedIssues(beads.WorkFilter{})

	// Build blockedMap: beadID -> list of blocker IDs.
	blockedMap := make(map[string][]string, len(blockedBeads))
	for _, b := range blockedBeads {
		blockedMap[b.ID] = BlockingDepIDs(b)
	}

	// 2. Collect all open bead IDs that need children.
	needChildren := make([]string, 0, len(openBeads))
	for _, b := range openBeads {
		needChildren = append(needChildren, b.ID)
	}

	// 3. Single query for all children.
	childrenMap, err := store.GetChildrenBatch(needChildren)
	if err != nil {
		return nil, fmt.Errorf("snapshot: batch children: %w", err)
	}

	// 4. Batch phase derivation.
	phaseMap := DerivePhaseMap(openBeads, childrenMap)

	// 5. Phase-map categorization.
	cols := CategorizeWithPhases(openBeads, closedBeads, blockedMap, phaseMap, identity)

	// Apply the same filters as FetchBoard.
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

	// 6. Batch DAG progress.
	allBeadIDs := make([]string, 0, len(openBeads))
	for _, b := range openBeads {
		allBeadIDs = append(allBeadIDs, b.ID)
	}
	dagProgress := BuildDAGProgressMap(allBeadIDs, childrenMap)

	// 7. Build EpicSummary for each epic-type bead.
	epicSummary := make(map[string]*EpicChildSummary)
	for _, b := range openBeads {
		if b.Type != "epic" {
			continue
		}
		children, ok := childrenMap[b.ID]
		if !ok || len(children) == 0 {
			continue
		}
		var s EpicChildSummary
		for _, c := range children {
			if store.IsStepBead(c) || store.IsAttemptBead(c) || store.IsReviewRoundBead(c) {
				continue
			}
			s.Total++
			switch c.Status {
			case "closed":
				s.Done++
			case "in_progress":
				s.Working++
			default:
				if _, blocked := blockedMap[c.ID]; blocked {
					s.Blocked++
				} else {
					s.Ready++
				}
			}
		}
		if s.Total > 0 {
			epicSummary[b.ID] = &s
		}
	}

	// 8-10. Assemble the snapshot.
	snap := &BoardSnapshot{
		Columns:     cols,
		DAGProgress: dagProgress,
		EpicSummary: epicSummary,
		Agents:      fetchAgents(),
		PhaseMap:    phaseMap,
		FetchedAt:   time.Now(),
	}

	return snap, nil
}
