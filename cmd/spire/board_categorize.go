package main

import (
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// boardColDef is a display column with a name, color, and bead slice.
type boardColDef struct {
	name  string
	color lipgloss.Color
	beads []BoardBead
}

// boardActiveColumns returns the non-empty columns in board display order.
// This is the authoritative ordered list used by both navigation and rendering.
func boardActiveColumns(cols boardColumns) []boardColDef {
	all := []boardColDef{
		{"READY", lipgloss.Color("2"), cols.Ready},
		{"DESIGN", lipgloss.Color("4"), cols.Design},
		{"PLAN", lipgloss.Color("6"), cols.Plan},
		{"IMPLEMENT", lipgloss.Color("6"), cols.Implement},
		{"REVIEW", lipgloss.Color("3"), cols.Review},
		{"MERGE", lipgloss.Color("5"), cols.Merge},
		{"DONE", lipgloss.Color("8"), cols.Done},
	}
	var active []boardColDef
	for _, c := range all {
		if len(c.beads) > 0 {
			active = append(active, c)
		}
	}
	return active
}

// categorizeColumnsFromStore builds board columns from store API results.
// blockedBeads come from GetBlockedIssues and already have blocker metadata.
func categorizeColumnsFromStore(openBeads, closedBeads, blockedBeads []BoardBead, identity string) boardColumns {
	var c boardColumns

	isAlert := func(b BoardBead) bool {
		for _, l := range b.Labels {
			if l == "alert" || strings.HasPrefix(l, "alert:") {
				return true
			}
		}
		return false
	}

	skip := func(b BoardBead) bool {
		for _, l := range b.Labels {
			if strings.HasPrefix(l, "msg") || l == "template" || strings.HasPrefix(l, "agent") {
				return true
			}
		}
		// Skip attempt beads (internal tracking, not board-visible work)
		if isAttemptBoardBead(b) {
			return true
		}
		// Skip review-round beads (internal tracking, not board-visible work)
		if isReviewRoundBoardBead(b) {
			return true
		}
		// Skip workflow step beads (internal phase tracking, not board-visible work)
		if isStepBoardBead(b) {
			return true
		}
		return false
	}

	// Build a set of blocked IDs for fast lookup. Blocked beads are authoritative
	// from storeGetBlockedIssues (which includes blocker metadata); openBeads will
	// also contain these same beads (status=open), but we skip them via blockedIDs
	// so the Blocked column uses the richer representation with dependency info.
	blockedIDs := make(map[string]bool, len(blockedBeads))
	for _, b := range blockedBeads {
		if skip(b) {
			continue
		}
		blockedIDs[b.ID] = true
		c.Blocked = append(c.Blocked, b)
	}

	for _, b := range openBeads {
		if isAlert(b) && b.Status == "open" {
			c.Alerts = append(c.Alerts, b)
			continue
		}
		if skip(b) {
			continue
		}
		// Skip beads already in the Blocked column.
		if blockedIDs[b.ID] {
			continue
		}

		phase := getBoardBeadPhase(b)
		switch {
		case phase == "design":
			c.Design = append(c.Design, b)
		case phase == "plan":
			c.Plan = append(c.Plan, b)
		case phase == "implement":
			c.Implement = append(c.Implement, b)
		case strings.HasPrefix(phase, "review"):
			c.Review = append(c.Review, b)
		case phase == "merge":
			c.Merge = append(c.Merge, b)
		default:
			// No phase label → Ready.
			c.Ready = append(c.Ready, b)
		}
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	for _, b := range closedBeads {
		if skip(b) {
			continue
		}
		t, err := time.Parse(time.RFC3339, b.UpdatedAt)
		if err != nil {
			t, err = time.Parse("2006-01-02 15:04:05", b.UpdatedAt)
		}
		if err == nil && t.After(cutoff) {
			c.Done = append(c.Done, b)
		}
	}

	return c
}

func filterEpic(cols boardColumns, epicID string) boardColumns {
	match := func(b BoardBead) bool {
		return b.ID == epicID || b.Parent == epicID || strings.HasPrefix(b.ID, epicID+".")
	}
	return boardColumns{
		Alerts:    filterBeads(cols.Alerts, match),
		Ready:     filterBeads(cols.Ready, match),
		Design:    filterBeads(cols.Design, match),
		Plan:      filterBeads(cols.Plan, match),
		Implement: filterBeads(cols.Implement, match),
		Review:    filterBeads(cols.Review, match),
		Merge:     filterBeads(cols.Merge, match),
		Done:      filterBeads(cols.Done, match),
		Blocked:   filterBeads(cols.Blocked, match),
	}
}

func filterBoardTypeScope(cols boardColumns, scope boardTypeScope) boardColumns {
	if scope == boardTypeAll {
		return cols
	}
	match := func(b BoardBead) bool {
		return scope.match(b)
	}
	return boardColumns{
		Alerts:    filterBeads(cols.Alerts, match),
		Ready:     filterBeads(cols.Ready, match),
		Design:    filterBeads(cols.Design, match),
		Plan:      filterBeads(cols.Plan, match),
		Implement: filterBeads(cols.Implement, match),
		Review:    filterBeads(cols.Review, match),
		Merge:     filterBeads(cols.Merge, match),
		Done:      filterBeads(cols.Done, match),
		Blocked:   filterBeads(cols.Blocked, match),
	}
}

func filterBeads(beads []BoardBead, pred func(BoardBead) bool) []BoardBead {
	var out []BoardBead
	for _, b := range beads {
		if pred(b) {
			out = append(out, b)
		}
	}
	return out
}

func beadOwnerLabel(b BoardBead) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "owner:") {
			return l[6:]
		}
	}
	return ""
}

func isCurrentUser(claimedBy, identity string) bool {
	if claimedBy == "" || identity == "" {
		return false
	}
	return claimedBy == identity ||
		strings.EqualFold(claimedBy, identity) ||
		strings.Contains(claimedBy, identity)
}

func blockingDepIDs(b BoardBead) []string {
	var ids []string
	for _, d := range b.Dependencies {
		if d.Type == "blocks" {
			ids = append(ids, d.DependsOnID)
		}
	}
	return ids
}

func filterOwned(beads []BoardBead, identity string) []BoardBead {
	var out []BoardBead
	for _, b := range beads {
		claimedBy := beadOwnerLabel(b)
		if claimedBy == "" {
			claimedBy = b.Owner
		}
		if isCurrentUser(claimedBy, identity) {
			out = append(out, b)
		}
	}
	return out
}

func sortBeads(beads []BoardBead) {
	sort.Slice(beads, func(i, j int) bool {
		if beads[i].Priority != beads[j].Priority {
			return beads[i].Priority < beads[j].Priority
		}
		left := boardSortTime(beads[i])
		right := boardSortTime(beads[j])
		if !left.Equal(right) {
			return left.After(right)
		}
		return beads[i].ID < beads[j].ID
	})
}

func boardSortTime(b BoardBead) time.Time {
	if t, ok := parseBoardTime(b.UpdatedAt); ok {
		return t
	}
	if t, ok := parseBoardTime(b.CreatedAt); ok {
		return t
	}
	return time.Time{}
}

func parseBoardTime(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err == nil {
		return t, true
	}
	t, err = time.Parse("2006-01-02 15:04:05", ts)
	if err == nil {
		return t, true
	}
	return time.Time{}, false
}
