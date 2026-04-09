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

// skipBead returns true if a bead is an internal DAG artifact that should not
// appear in user-facing board columns (Ready, Implement, etc.). Alert beads
// are also caught here so they don't leak into phase columns when their status
// is not "open" or when they appear in the blocked-beads loop.
func skipBead(b BoardBead) bool {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "msg") || l == "template" || strings.HasPrefix(l, "agent") {
			return true
		}
		if l == "review-substep" {
			return true
		}
		if l == "alert" || strings.HasPrefix(l, "alert:") {
			return true
		}
	}
	if store.IsAttemptBoardBead(b) {
		return true
	}
	if store.IsReviewRoundBoardBead(b) {
		return true
	}
	if store.IsStepBoardBead(b) {
		return true
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

// isInterruptedBead returns true if the bead has an interrupted:* label.
// This is the explicit failure signal set by executor escalation functions,
// distinct from alert beads (which are separate linked artifacts) and from
// needs-human alone (which is used for design approval gates).
func isInterruptedBead(b BoardBead) bool {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			return true
		}
	}
	return false
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
		// Interrupted beads get their own section — they must not fall into READY
		// when step beads are closed after a failed executor run.
		if isInterruptedBead(b) {
			c.Interrupted = append(c.Interrupted, b)
			continue
		}

		// Deferred beads land in Ready (sorted to bottom by SortBeads).
		if b.Status == "deferred" {
			if b.Parent == "" {
				c.Ready = append(c.Ready, b)
			}
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
			// Subtasks belong to their parent epic — don't surface them
			// independently on the ready board.
			if b.Parent == "" {
				c.Ready = append(c.Ready, b)
			}
		}
	}

	for _, b := range closedBeads {
		if skipBead(b) {
			continue
		}
		c.Done = append(c.Done, b)
	}
	// Most recently updated first, capped at 10.
	sort.Slice(c.Done, func(i, j int) bool {
		return c.Done[i].UpdatedAt > c.Done[j].UpdatedAt
	})
	if len(c.Done) > 10 {
		c.Done = c.Done[:10]
	}

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
		if isInterruptedBead(b) {
			c.Interrupted = append(c.Interrupted, b)
			continue
		}

		// Deferred beads land in Ready (sorted to bottom by SortBeads).
		if b.Status == "deferred" {
			if b.Parent == "" {
				c.Ready = append(c.Ready, b)
			}
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
			if b.Parent == "" {
				c.Ready = append(c.Ready, b)
			}
		}
	}

	for _, b := range closedBeads {
		if skipBead(b) {
			continue
		}
		c.Done = append(c.Done, b)
	}
	// Most recently updated first, capped at 10.
	sort.Slice(c.Done, func(i, j int) bool {
		return c.Done[i].UpdatedAt > c.Done[j].UpdatedAt
	})
	if len(c.Done) > 10 {
		c.Done = c.Done[:10]
	}

	return c
}

// FilterEpic filters columns to only contain beads matching the epic ID and its children.
func FilterEpic(cols Columns, epicID string) Columns {
	match := func(b BoardBead) bool {
		return b.ID == epicID || b.Parent == epicID || strings.HasPrefix(b.ID, epicID+".")
	}
	return Columns{
		Alerts:      FilterBeads(cols.Alerts, match),
		Interrupted: FilterBeads(cols.Interrupted, match),
		Ready:       FilterBeads(cols.Ready, match),
		Design:      FilterBeads(cols.Design, match),
		Plan:        FilterBeads(cols.Plan, match),
		Implement:   FilterBeads(cols.Implement, match),
		Review:      FilterBeads(cols.Review, match),
		Merge:       FilterBeads(cols.Merge, match),
		Done:        FilterBeads(cols.Done, match),
		Blocked:     FilterBeads(cols.Blocked, match),
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
		Alerts:      FilterBeads(cols.Alerts, match),
		Interrupted: FilterBeads(cols.Interrupted, match),
		Ready:       FilterBeads(cols.Ready, match),
		Design:      FilterBeads(cols.Design, match),
		Plan:        FilterBeads(cols.Plan, match),
		Implement:   FilterBeads(cols.Implement, match),
		Review:      FilterBeads(cols.Review, match),
		Merge:       FilterBeads(cols.Merge, match),
		Done:        FilterBeads(cols.Done, match),
		Blocked:     FilterBeads(cols.Blocked, match),
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
