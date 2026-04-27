package board

import (
	"context"
	"fmt"
	"log"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// BoardResult wraps board columns with optional system warnings.
type BoardResult struct {
	Columns  Columns
	Warnings []string
}

// checkDoltConflicts detects unresolved Dolt conflicts and returns a warning
// string if any exist. Returns "", nil when there are no conflicts or when the
// database name cannot be resolved (non-fatal).
//
// Callers gate the check via Opts.SkipLocalConflictCheck — the function
// itself never branches on deployment mode or sync transport, since
// docs/ARCHITECTURE.md keeps those axes orthogonal. The skip is for
// callers that demonstrably do not own a writable local Dolt mirror
// (e.g. an HTTP gateway server whose backing Dolt is the cluster's,
// not the request-serving pod's).
func checkDoltConflicts() (string, error) {
	dbName, err := config.DetectDBName()
	if err != nil || dbName == "" {
		return "", nil // can't check — not an error
	}
	count, err := dolt.HasUnresolvedConflicts(dbName)
	if err != nil {
		return "", err
	}
	if count > 0 {
		return fmt.Sprintf("dolt-conflict: %d unresolved conflict(s) in issues table — run `spire pull` to resolve", count), nil
	}
	return "", nil
}

// FetchBoard loads beads from the store and categorizes them into board columns.
// Detects unresolved Dolt conflicts and surfaces them as warnings instead of
// failing the entire load.
func FetchBoard(opts Opts, identity string) (BoardResult, error) {
	var warnings []string

	// Check for Dolt conflicts before store reads. Callers without a
	// writable local Dolt mirror (gateway HTTP server) opt out via
	// Opts.SkipLocalConflictCheck — the check fork-execs `dolt sql`
	// and is a per-request hot path that adds latency and accumulates
	// subprocess zombies on those callers. See docs/k8s-v1-punchlist.md
	// item #6 for the diagnosis.
	if !opts.SkipLocalConflictCheck {
		if w, err := checkDoltConflicts(); err != nil {
			log.Printf("[board] conflict check: %s", err)
		} else if w != "" {
			warnings = append(warnings, w)
		}
	}

	openBeads, err := store.ListBoardBeads(beads.IssueFilter{
		ExcludeStatus: []beads.Status{beads.StatusClosed},
	})
	if err != nil {
		// If conflicts exist, degrade gracefully instead of hard-failing.
		if len(warnings) > 0 {
			return BoardResult{Warnings: warnings}, nil
		}
		return BoardResult{}, fmt.Errorf("board: list open beads: %w", err)
	}

	closedCutoff := time.Now().Add(-7 * 24 * time.Hour)
	closedBeads, _ := store.ListBoardBeads(beads.IssueFilter{
		Status:      store.StatusPtr(beads.StatusClosed),
		ClosedAfter: &closedCutoff,
	})

	blockedBeads, _ := store.GetBlockedIssues(beads.WorkFilter{})

	cols := CategorizeColumnsFromStore(openBeads, closedBeads, blockedBeads, identity)

	if opts.Epic != "" {
		cols = FilterEpic(cols, opts.Epic)
	}
	CapDone(&cols, 10)
	if opts.Mine {
		cols.Backlog = nil
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

	SortBeads(cols.Backlog)
	SortBeads(cols.Ready)
	SortBeads(cols.Design)
	SortBeads(cols.Plan)
	SortBeads(cols.Implement)
	SortBeads(cols.Review)
	SortBeads(cols.Merge)
	SortBeads(cols.Done)
	SortBeads(cols.Blocked)

	return BoardResult{Columns: cols, Warnings: warnings}, nil
}

// snapshotMsg carries the result of a background snapshot fetch.
type snapshotMsg struct {
	Snap *BoardSnapshot
	Err  error
}

// fetchSnapshotCmd returns a tea.Cmd that fetches a BoardSnapshot in the background.
// db is the owned beads.Storage connection (not the singleton).
func fetchSnapshotCmd(db beads.Storage, opts Opts, identity string, fetchAgents func() []LocalAgent) tea.Cmd {
	return func() tea.Msg {
		snap, err := fetchSnapshot(db, opts, identity, fetchAgents)
		return snapshotMsg{Snap: snap, Err: err}
	}
}

// fetchSnapshot assembles a complete BoardSnapshot in a single pass with minimal
// DB queries: 3 bulk bead queries + 1 GetChildrenBatch = 4 total.
// db is the owned beads.Storage connection (not the singleton).
func fetchSnapshot(db beads.Storage, opts Opts, identity string, fetchAgents func() []LocalAgent) (*BoardSnapshot, error) {
	var warnings []string

	// Same SkipLocalConflictCheck gate as FetchBoard — see comment there.
	if !opts.SkipLocalConflictCheck {
		if w, err := checkDoltConflicts(); err != nil {
			log.Printf("[board] snapshot conflict check: %s", err)
		} else if w != "" {
			warnings = append(warnings, w)
		}
	}

	ctx := context.Background()

	// 1. Bulk-fetch beads using the owned db connection.
	openBeads, err := listBoardBeadsDB(ctx, db, beads.IssueFilter{
		ExcludeStatus: []beads.Status{beads.StatusClosed},
	})
	if err != nil {
		// If conflicts detected, return a minimal snapshot with the warning
		// instead of nil (which leaves the TUI stuck on "Loading...").
		if len(warnings) > 0 {
			return &BoardSnapshot{Warnings: warnings, FetchedAt: time.Now()}, nil
		}
		return nil, fmt.Errorf("snapshot: list open beads: %w", err)
	}

	closedCutoff := time.Now().Add(-7 * 24 * time.Hour)
	closedBeads, _ := listBoardBeadsDB(ctx, db, beads.IssueFilter{
		Status:      store.StatusPtr(beads.StatusClosed),
		ClosedAfter: &closedCutoff,
	})

	blockedBeads, _ := getBlockedIssuesDB(ctx, db, beads.WorkFilter{})

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
	childrenMap, err := getChildrenBatchDB(ctx, db, needChildren)
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
	CapDone(&cols, 10)
	if opts.Mine {
		cols.Backlog = nil
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

	SortBeads(cols.Backlog)
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
			if store.IsStepBead(c) || store.IsAttemptBead(c) || store.IsReviewRoundBead(c) || store.IsFormulaTemplateBead(c) {
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

	// 8. Fetch recovery refs for hooked beads.
	recoveryRefs := make(map[string]*RecoveryRef)
	getDeps := storeDepsWith(ctx, db)
	for _, b := range cols.Hooked {
		if ref := FetchRecoveryRef(b.ID, getDeps); ref != nil {
			recoveryRefs[b.ID] = ref
		}
	}

	// 9-10. Assemble the snapshot.
	snap := &BoardSnapshot{
		Columns:      cols,
		DAGProgress:  dagProgress,
		EpicSummary:  epicSummary,
		RecoveryRefs: recoveryRefs,
		Agents:       fetchAgents(),
		PhaseMap:     phaseMap,
		Warnings:     warnings,
		FetchedAt:    time.Now(),
	}

	return snap, nil
}

// --- DB-aware helpers for fetchSnapshot (use owned connection, not singleton) ---

// listBoardBeadsDB is like store.ListBoardBeads but uses the provided db connection.
func listBoardBeadsDB(ctx context.Context, db beads.Storage, filter beads.IssueFilter) ([]BoardBead, error) {
	if filter.Status == nil && len(filter.ExcludeStatus) == 0 {
		filter.ExcludeStatus = []beads.Status{beads.StatusClosed}
	}
	issues, err := db.SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, fmt.Errorf("list board beads: %w", err)
	}
	store.PopulateDependencies(ctx, db, issues)
	return store.IssuesToBoardBeads(issues), nil
}

// getBlockedIssuesDB is like store.GetBlockedIssues but uses the provided db connection.
func getBlockedIssuesDB(ctx context.Context, db beads.Storage, filter beads.WorkFilter) ([]BoardBead, error) {
	blocked, err := db.GetBlockedIssues(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("get blocked issues: %w", err)
	}
	result := make([]BoardBead, 0, len(blocked))
	for _, bi := range blocked {
		bb := BoardBead{
			ID:              bi.ID,
			Title:           bi.Title,
			Description:     bi.Description,
			Status:          string(bi.Status),
			Priority:        bi.Priority,
			Type:            string(bi.IssueType),
			Owner:           bi.Owner,
			CreatedAt:       bi.CreatedAt.Format(time.RFC3339),
			UpdatedAt:       bi.UpdatedAt.Format(time.RFC3339),
			Labels:          bi.Labels,
			DependencyCount: bi.BlockedByCount,
		}
		for _, blockerID := range bi.BlockedBy {
			bb.Dependencies = append(bb.Dependencies, BoardDep{
				IssueID:     bi.ID,
				DependsOnID: blockerID,
				Type:        "blocks",
			})
		}
		result = append(result, bb)
	}
	return result, nil
}

// getChildrenBatchDB is like store.GetChildrenBatch but uses the provided db connection.
func getChildrenBatchDB(ctx context.Context, db beads.Storage, parentIDs []string) (map[string][]store.Bead, error) {
	if len(parentIDs) == 0 {
		return map[string][]store.Bead{}, nil
	}
	result := make(map[string][]store.Bead, len(parentIDs))
	for _, pid := range parentIDs {
		pid := pid
		issues, err := db.SearchIssues(ctx, "", beads.IssueFilter{
			ParentID: &pid,
		})
		if err != nil {
			return nil, fmt.Errorf("get children of %s: %w", pid, err)
		}
		result[pid] = store.IssuesToBeads(issues)
	}
	return result, nil
}

// storeDepsWith returns a GetDependentsFunc backed by the provided db connection.
func storeDepsWith(ctx context.Context, db beads.Storage) GetDependentsFunc {
	return func(beadID string) ([]DepRecord, error) {
		deps, err := db.GetDependentsWithMetadata(ctx, beadID)
		if err != nil {
			return nil, err
		}
		out := make([]DepRecord, len(deps))
		for i, d := range deps {
			out[i] = DepRecord{
				ID:             d.ID,
				Title:          d.Title,
				Status:         string(d.Status),
				DependencyType: string(d.DependencyType),
			}
		}
		return out, nil
	}
}
