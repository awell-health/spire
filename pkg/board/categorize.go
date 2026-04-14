package board

import (
	"image/color"
	"sort"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"charm.land/lipgloss/v2"
)

// ColDef is a display column with a name, color, and bead slice.
type ColDef struct {
	Name  string
	Color color.Color
	Beads []BoardBead
}

// AllColumns returns all phase columns in board display order, including empty ones.
func AllColumns(cols Columns) []ColDef {
	return []ColDef{
		{"BACKLOG", lipgloss.Color("8"), cols.Backlog},
		{"READY", lipgloss.Color("2"), cols.Ready},
		{"DESIGN", lipgloss.Color("4"), cols.Design},
		{"PLAN", lipgloss.Color("6"), cols.Plan},
		{"IMPLEMENT", lipgloss.Color("6"), cols.Implement},
		{"REVIEW", lipgloss.Color("3"), cols.Review},
		{"MERGE", lipgloss.Color("5"), cols.Merge},
		{"DONE", lipgloss.Color("8"), cols.Done},
	}
}

// ActiveColumns returns the non-empty columns in board display order.
// This is the authoritative ordered list used by both navigation and rendering.
func ActiveColumns(cols Columns) []ColDef {
	var active []ColDef
	for _, c := range AllColumns(cols) {
		if len(c.Beads) > 0 {
			active = append(active, c)
		}
	}
	return active
}

// isWorkBoardBead mirrors store.IsWorkBead for BoardBead values.
func isWorkBoardBead(b BoardBead) bool {
	return !store.InternalTypes[b.Type] && b.Parent == ""
}

// skipBead returns true if a bead is an internal DAG artifact that should not
// appear in user-facing board columns (Ready, Implement, etc.). Alert beads
// are also caught here so they don't leak into phase columns when their status
// is not "open" or when they appear in the blocked-beads loop.
func skipBead(b BoardBead) bool {
	// Internal bead types (messages, steps, attempts, reviews) are hidden from the board.
	if store.InternalTypes[b.Type] {
		return true
	}
	// Recovery beads are shown unless they're closed (archmage may need to act).
	// Closed recovery beads are internal tracking — they've been resolved.
	if b.Type == "recovery" && b.Status == "closed" {
		return true
	}
	// Label-based fallback for beads not yet migrated to internal types.
	for _, l := range b.Labels {
		if l == "msg" || l == "attempt" || l == "review-round" || l == "workflow-step" {
			return true
		}
		if l == "template" || strings.HasPrefix(l, "agent") {
			return true
		}
		if l == "review-substep" {
			return true
		}
		if l == "alert" || strings.HasPrefix(l, "alert:") {
			return true
		}
	}
	if store.IsFormulaTemplateBoardBead(b) {
		return true
	}
	return false
}

// isAlertBead returns true if the bead has an alert or alert:* label.
func isAlertBead(b BoardBead) bool {
	for _, l := range b.Labels {
		if l == "alert" || strings.HasPrefix(l, "alert:") {
			return true
		}
	}
	return false
}

// isHookedBead returns true if the bead has status='hooked'.
// This replaces the old label-based interrupted:* check — hooked status is now
// set directly on beads by the executor when a step parks for human/external action.
func isHookedBead(b BoardBead) bool {
	return b.Status == "hooked"
}

// CategorizeColumnsFromStore builds board columns from store API results.
// blockedBeads come from GetBlockedIssues and already have blocker metadata.
func CategorizeColumnsFromStore(openBeads, closedBeads, blockedBeads []BoardBead, identity string) Columns {
	var c Columns

	blockedIDs := make(map[string]bool, len(blockedBeads))
	for _, b := range blockedBeads {
		if skipBead(b) {
			continue
		}
		blockedIDs[b.ID] = true
		c.Blocked = append(c.Blocked, b)
	}

	for _, b := range openBeads {
		if isAlertBead(b) && b.Status == "open" {
			c.Alerts = append(c.Alerts, b)
			continue
		}
		if skipBead(b) {
			continue
		}
		if blockedIDs[b.ID] {
			continue
		}
		// Hooked beads get their own section — they are parked waiting for a condition.
		if isHookedBead(b) {
			c.Hooked = append(c.Hooked, b)
			continue
		}

		// Deferred beads land in Backlog (sorted to bottom by SortBeads).
		if b.Status == "deferred" {
			if isWorkBoardBead(b) {
				c.Backlog = append(c.Backlog, b)
			}
			continue
		}

		// Non-work beads (internal types or epic children) are managed
		// elsewhere — skip them from phase columns and ready.
		if !isWorkBoardBead(b) {
			continue
		}

		// Open beads go to Backlog; ready beads go to Ready.
		// In-progress beads route by phase label.
		if b.Status == "open" {
			c.Backlog = append(c.Backlog, b)
			continue
		}
		if b.Status == "ready" {
			c.Ready = append(c.Ready, b)
			continue
		}

		phase := GetBoardBeadPhase(b)
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
			c.Ready = append(c.Ready, b)
		}
	}

	for _, b := range closedBeads {
		if skipBead(b) {
			continue
		}
		c.Done = append(c.Done, b)
	}
	// Most recently updated first.
	sort.Slice(c.Done, func(i, j int) bool {
		return c.Done[i].UpdatedAt > c.Done[j].UpdatedAt
	})

	return c
}

// CategorizeWithPhases builds board columns using a pre-computed phase map and blocked map
// instead of making per-bead DB calls. Same logic as CategorizeColumnsFromStore.
func CategorizeWithPhases(openBeads, closedBeads []BoardBead, blockedMap map[string][]string, phaseMap map[string]string, identity string) Columns {
	var c Columns

	// Build blocked set from blockedMap keys.
	blockedIDs := make(map[string]bool, len(blockedMap))
	for id := range blockedMap {
		blockedIDs[id] = true
	}

	// Add blocked beads from open beads that appear in blockedMap.
	for _, b := range openBeads {
		if skipBead(b) {
			continue
		}
		if blockedIDs[b.ID] {
			c.Blocked = append(c.Blocked, b)
		}
	}

	for _, b := range openBeads {
		if isAlertBead(b) && b.Status == "open" {
			c.Alerts = append(c.Alerts, b)
			continue
		}
		if skipBead(b) {
			continue
		}
		if blockedIDs[b.ID] {
			continue
		}
		if isHookedBead(b) {
			c.Hooked = append(c.Hooked, b)
			continue
		}

		// Deferred beads land in Backlog (sorted to bottom by SortBeads).
		if b.Status == "deferred" {
			if isWorkBoardBead(b) {
				c.Backlog = append(c.Backlog, b)
			}
			continue
		}

		// Non-work beads (internal types or epic children) are managed
		// elsewhere — skip them from phase columns and ready.
		if !isWorkBoardBead(b) {
			continue
		}

		// Open beads go to Backlog; ready beads go to Ready.
		// In-progress beads route by phase label.
		if b.Status == "open" {
			c.Backlog = append(c.Backlog, b)
			continue
		}
		if b.Status == "ready" {
			c.Ready = append(c.Ready, b)
			continue
		}

		phase := phaseMap[b.ID]
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
			c.Ready = append(c.Ready, b)
		}
	}

	for _, b := range closedBeads {
		if skipBead(b) {
			continue
		}
		c.Done = append(c.Done, b)
	}
	// Most recently updated first.
	sort.Slice(c.Done, func(i, j int) bool {
		return c.Done[i].UpdatedAt > c.Done[j].UpdatedAt
	})

	return c
}

// CapDone trims the Done column to at most n entries.
// Call this after FilterEpic so the cap applies to the filtered set, not before.
func CapDone(cols *Columns, n int) {
	if len(cols.Done) > n {
		cols.Done = cols.Done[:n]
	}
}

// FilterEpic filters columns to only contain beads matching the epic ID, its
// children, beads linked via discovered-from dependencies (e.g. design beads),
// and beads that depend on the epic (e.g. alert and recovery beads via caused-by).
func FilterEpic(cols Columns, epicID string) Columns {
	linkedIDs := make(map[string]bool)
	allSlices := [][]BoardBead{
		cols.Alerts, cols.Hooked, cols.Backlog, cols.Ready, cols.Design, cols.Plan,
		cols.Implement, cols.Review, cols.Merge, cols.Done, cols.Blocked,
	}

	// Collect IDs of beads the epic depends on via discovered-from (design beads).
	for _, slice := range allSlices {
		for _, b := range slice {
			if b.ID == epicID {
				for _, dep := range b.Dependencies {
					if dep.Type == "discovered-from" {
						linkedIDs[dep.DependsOnID] = true
					}
				}
				break
			}
		}
	}

	// Collect beads that depend on the epic (reverse refs). This catches
	// alert beads (caused-by), recovery beads, and other beads linked to
	// the epic that don't use the parent field.
	for _, slice := range allSlices {
		for _, b := range slice {
			for _, dep := range b.Dependencies {
				if dep.DependsOnID == epicID {
					linkedIDs[b.ID] = true
					break
				}
			}
		}
	}

	match := func(b BoardBead) bool {
		return b.ID == epicID || b.Parent == epicID || strings.HasPrefix(b.ID, epicID+".") || linkedIDs[b.ID]
	}
	return Columns{
		Alerts:    FilterBeads(cols.Alerts, match),
		Hooked:    FilterBeads(cols.Hooked, match),
		Backlog:   FilterBeads(cols.Backlog, match),
		Ready:     FilterBeads(cols.Ready, match),
		Design:    FilterBeads(cols.Design, match),
		Plan:      FilterBeads(cols.Plan, match),
		Implement: FilterBeads(cols.Implement, match),
		Review:    FilterBeads(cols.Review, match),
		Merge:     FilterBeads(cols.Merge, match),
		Done:      FilterBeads(cols.Done, match),
		Blocked:   FilterBeads(cols.Blocked, match),
	}
}

// TypeScope filters beads by their type.
type TypeScope int

const (
	TypeAll TypeScope = iota
	TypeTask
	TypeBug
	TypeEpic
	TypeDesign
	TypeDecision
	TypeOther
)

// TypeScopeOrder is the ordered list of scopes for cycling.
var TypeScopeOrder = []TypeScope{
	TypeAll,
	TypeTask,
	TypeBug,
	TypeEpic,
	TypeDesign,
	TypeDecision,
	TypeOther,
}

// Next returns the next type scope in the cycle.
func (s TypeScope) Next() TypeScope {
	for i, candidate := range TypeScopeOrder {
		if candidate == s {
			return TypeScopeOrder[(i+1)%len(TypeScopeOrder)]
		}
	}
	return TypeAll
}

// Label returns a human-readable label for the scope.
func (s TypeScope) Label() string {
	switch s {
	case TypeTask:
		return "tasks"
	case TypeBug:
		return "bugs"
	case TypeEpic:
		return "epics"
	case TypeDesign:
		return "designs"
	case TypeDecision:
		return "decisions"
	case TypeOther:
		return "other"
	default:
		return "all"
	}
}

// Match checks if a bead matches the type scope.
func (s TypeScope) Match(b BoardBead) bool {
	switch s {
	case TypeAll:
		return true
	case TypeTask:
		return b.Type == "task"
	case TypeBug:
		return b.Type == "bug"
	case TypeEpic:
		return b.Type == "epic"
	case TypeDesign:
		return b.Type == "design"
	case TypeDecision:
		return b.Type == "decision"
	case TypeOther:
		switch b.Type {
		case "task", "bug", "epic", "design", "decision":
			return false
		default:
			return true
		}
	default:
		return true
	}
}

// FilterTypeScope filters columns to only contain beads matching the scope.
func FilterTypeScope(cols Columns, scope TypeScope) Columns {
	if scope == TypeAll {
		return cols
	}
	match := func(b BoardBead) bool {
		return scope.Match(b)
	}
	return Columns{
		Alerts:    FilterBeads(cols.Alerts, match),
		Hooked:    FilterBeads(cols.Hooked, match),
		Backlog:   FilterBeads(cols.Backlog, match),
		Ready:     FilterBeads(cols.Ready, match),
		Design:    FilterBeads(cols.Design, match),
		Plan:      FilterBeads(cols.Plan, match),
		Implement: FilterBeads(cols.Implement, match),
		Review:    FilterBeads(cols.Review, match),
		Merge:     FilterBeads(cols.Merge, match),
		Done:      FilterBeads(cols.Done, match),
		Blocked:   FilterBeads(cols.Blocked, match),
	}
}

// FilterBeads returns beads matching the predicate.
func FilterBeads(beads []BoardBead, pred func(BoardBead) bool) []BoardBead {
	var out []BoardBead
	for _, b := range beads {
		if pred(b) {
			out = append(out, b)
		}
	}
	return out
}

// BeadOwnerLabel extracts the owner name from a bead's labels.
func BeadOwnerLabel(b BoardBead) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "owner:") {
			return l[6:]
		}
	}
	return ""
}

// IsCurrentUser checks if a claimed-by identity matches the current user.
func IsCurrentUser(claimedBy, identity string) bool {
	if claimedBy == "" || identity == "" {
		return false
	}
	return claimedBy == identity ||
		strings.EqualFold(claimedBy, identity) ||
		strings.Contains(claimedBy, identity)
}

// BlockingDepIDs returns the IDs of beads that block the given bead.
func BlockingDepIDs(b BoardBead) []string {
	var ids []string
	for _, d := range b.Dependencies {
		if d.Type == "blocks" {
			ids = append(ids, d.DependsOnID)
		}
	}
	return ids
}

// FilterOwned filters to beads owned by the given identity.
func FilterOwned(beads []BoardBead, identity string) []BoardBead {
	var out []BoardBead
	for _, b := range beads {
		claimedBy := BeadOwnerLabel(b)
		if claimedBy == "" {
			claimedBy = b.Owner
		}
		if IsCurrentUser(claimedBy, identity) {
			out = append(out, b)
		}
	}
	return out
}

// SortBeads sorts beads by priority (ascending) then update time (most recent first).
// Deferred beads are always sorted to the bottom, after all non-deferred beads.
func SortBeads(beads []BoardBead) {
	sort.Slice(beads, func(i, j int) bool {
		iDeferred := beads[i].Status == "deferred"
		jDeferred := beads[j].Status == "deferred"
		if iDeferred != jDeferred {
			return !iDeferred // non-deferred sorts before deferred
		}
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
	if t, ok := ParseBoardTime(b.UpdatedAt); ok {
		return t
	}
	if t, ok := ParseBoardTime(b.CreatedAt); ok {
		return t
	}
	return time.Time{}
}
