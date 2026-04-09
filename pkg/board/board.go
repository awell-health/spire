// Package board provides board data shaping, categorization, Bubble Tea / Lip Gloss
// rendering, watch views, and roster summaries for the Spire TUI.
package board

import (
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

// Type aliases for convenience within pkg/board.
type BoardBead = store.BoardBead
type BoardDep = store.BoardDep
type Bead = store.Bead

// LocalAgent is an alias for agent.Entry used for the agent panel.
type LocalAgent = agent.Entry

// Columns holds beads categorized into board columns.
type Columns struct {
	Alerts      []BoardBead
	Interrupted []BoardBead // parent beads with an interrupted:* label (escalated failures)
	Ready       []BoardBead
	Design      []BoardBead
	Plan        []BoardBead
	Implement   []BoardBead
	Review      []BoardBead
	Merge       []BoardBead
	Done        []BoardBead
	Blocked     []BoardBead
}

// RecoveryRef is an alias for recovery.RecoveryRef, avoiding duplicate definitions.
type RecoveryRef = recovery.RecoveryRef

// DepRecord holds dependent metadata needed for recovery ref lookup.
type DepRecord struct {
	ID             string
	Title          string
	Status         string
	DependencyType string
}

// GetDependentsFunc retrieves dependents with metadata for a bead ID.
type GetDependentsFunc func(beadID string) ([]DepRecord, error)

// BoardBeadJSON wraps a BoardBead with optional DAG progress for JSON output.
type BoardBeadJSON struct {
	BoardBead
	DAG          *DAGProgress      `json:"dag,omitempty"`
	EpicSub      *EpicChildSummary `json:"epic_subtasks,omitempty"`
	RecoveryBead *RecoveryRef      `json:"recovery_bead,omitempty"`
}

// BoardJSON is the top-level JSON envelope for board output.
// It wraps column data with optional system-level warnings.
type BoardJSON struct {
	ColumnsJSON
	Warnings []string `json:"warnings,omitempty"`
}

// ColumnsJSON is the JSON-serializable version of Columns.
type ColumnsJSON struct {
	Alerts      []BoardBeadJSON `json:"alerts"`
	Interrupted []BoardBeadJSON `json:"interrupted"`
	Ready       []BoardBeadJSON `json:"ready"`
	Design      []BoardBeadJSON `json:"design"`
	Plan        []BoardBeadJSON `json:"plan"`
	Implement   []BoardBeadJSON `json:"implement"`
	Review      []BoardBeadJSON `json:"review"`
	Merge       []BoardBeadJSON `json:"merge"`
	Done        []BoardBeadJSON `json:"done"`
	Blocked     []BoardBeadJSON `json:"blocked"`
}

// Opts holds board command options shared between JSON output and TUI mode.
type Opts struct {
	Mine      bool
	Ready     bool
	Epic      string
	Interval  time.Duration
	RootCmd   *cobra.Command // root cobra command for command mode completion/execution
	TowerName string         // current tower name (shown in header)

	// ListTowersFn returns available towers for the T-key switcher. Injected by caller.
	ListTowersFn func() []TowerItem
	// SwitchTowerFn handles env changes when switching towers. Returns new name or error.
	SwitchTowerFn func(towerName string) (string, error)
	// ResolveFn resolves a needs-human bead with a recovery learning comment.
	ResolveFn func(beadID, comment string) error
}

// ANSI color codes for static terminal output (used by watch, roster, actions).
const (
	Bold    = "\033[1m"
	Dim     = "\033[2m"
	Red     = "\033[31m"
	Yellow  = "\033[33m"
	Green   = "\033[32m"
	Cyan    = "\033[36m"
	Magenta = "\033[35m"
	Reset   = "\033[0m"
)

// NonNil converts a nil slice to an empty slice for JSON serialization.
func NonNil(beads []BoardBead) []BoardBead {
	if beads == nil {
		return []BoardBead{}
	}
	return beads
}

// enrichBeadsJSON enriches BoardBeads with DAG progress for in_progress beads.
func enrichBeadsJSON(beads []BoardBead) []BoardBeadJSON {
	out := make([]BoardBeadJSON, len(beads))
	for i, b := range beads {
		out[i] = BoardBeadJSON{BoardBead: b}
		if b.Status == "in_progress" {
			out[i].DAG = FetchDAGProgress(b.ID)
			if b.Type == "epic" {
				out[i].EpicSub = FetchEpicChildSummary(b.ID)
			}
		}
	}
	return out
}

// nonNilJSON converts a nil slice to an empty slice for JSON serialization.
func nonNilJSON(beads []BoardBeadJSON) []BoardBeadJSON {
	if beads == nil {
		return []BoardBeadJSON{}
	}
	return beads
}

// ShortType converts a bead type to a short display label.
func ShortType(t string) string {
	switch t {
	case "feature":
		return "feat"
	case "task":
		return "task"
	case "bug":
		return "bug"
	case "epic":
		return "epic"
	case "chore":
		return "chore"
	case "decision":
		return "dec"
	default:
		return t
	}
}

// Truncate shortens a string to max characters, appending an ellipsis if truncated.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// TimeAgo returns a human-readable relative time string.
func TimeAgo(ts string) string {
	t, ok := ParseBoardTime(ts)
	if !ok {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw ago", int(d.Hours()/(24*7)))
	}
}

// ParseBoardTime parses a timestamp in RFC3339 or "2006-01-02 15:04:05" format.
func ParseBoardTime(ts string) (time.Time, bool) {
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

// Min returns the smaller of two ints.
func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// PriorityStr returns a priority label with ANSI color codes.
func PriorityStr(p int) string {
	label := fmt.Sprintf("P%d", p)
	switch p {
	case 0, 1:
		return Bold + Red + label + Reset
	case 2:
		return Yellow + label + Reset
	default:
		return Dim + label + Reset
	}
}

// ClearScreen sends ANSI escape sequences to clear the terminal.
func ClearScreen() {
	fmt.Print("\033[2J\033[H")
}

// FetchRecoveryRef looks up the first open recovery-for dependent for a bead.
// getDeps is injected for testability — use StoreDeps() for production callers.
// Returns nil if no open recovery bead exists.
func FetchRecoveryRef(beadID string, getDeps GetDependentsFunc) *RecoveryRef {
	deps, err := getDeps(beadID)
	if err != nil {
		return nil
	}
	for _, dep := range deps {
		if dep.DependencyType != "recovery-for" {
			continue
		}
		if dep.Status == "closed" {
			continue
		}
		return &RecoveryRef{ID: dep.ID, Title: dep.Title}
	}
	return nil
}

// StoreDeps returns a GetDependentsFunc backed by the store package.
func StoreDeps() GetDependentsFunc {
	return func(beadID string) ([]DepRecord, error) {
		deps, err := store.GetDependentsWithMeta(beadID)
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

// ToJSON converts Columns to the JSON-serializable ColumnsJSON with DAG progress.
// recoveryRefs provides pre-fetched recovery refs for interrupted beads (may be nil).
func (c Columns) ToJSON(recoveryRefs map[string]*RecoveryRef) ColumnsJSON {
	enrich := func(beads []BoardBead) []BoardBeadJSON {
		return nonNilJSON(enrichBeadsJSON(NonNil(beads)))
	}
	cj := ColumnsJSON{
		Alerts:      enrich(c.Alerts),
		Interrupted: enrich(c.Interrupted),
		Ready:       enrich(c.Ready),
		Design:      enrich(c.Design),
		Plan:        enrich(c.Plan),
		Implement:   enrich(c.Implement),
		Review:      enrich(c.Review),
		Merge:       enrich(c.Merge),
		Done:        enrich(c.Done),
		Blocked:     enrich(c.Blocked),
	}
	// Enrich interrupted beads with pre-fetched recovery refs.
	if recoveryRefs != nil {
		for i := range cj.Interrupted {
			cj.Interrupted[i].RecoveryBead = recoveryRefs[cj.Interrupted[i].ID]
		}
	}
	return cj
}
