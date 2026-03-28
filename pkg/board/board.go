// Package board provides board data shaping, categorization, Bubble Tea / Lip Gloss
// rendering, watch views, and roster summaries for the Spire TUI.
package board

import (
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/store"
)

// Type aliases for convenience within pkg/board.
type BoardBead = store.BoardBead
type BoardDep = store.BoardDep
type Bead = store.Bead

// LocalAgent is an alias for agent.Entry used for the agent panel.
type LocalAgent = agent.Entry

// Columns holds beads categorized into board columns.
type Columns struct {
	Alerts    []BoardBead
	Ready     []BoardBead
	Design    []BoardBead
	Plan      []BoardBead
	Implement []BoardBead
	Review    []BoardBead
	Merge     []BoardBead
	Done      []BoardBead
	Blocked   []BoardBead
}

// ColumnsJSON is the JSON-serializable version of Columns.
type ColumnsJSON struct {
	Alerts    []BoardBead `json:"alerts"`
	Ready     []BoardBead `json:"ready"`
	Design    []BoardBead `json:"design"`
	Plan      []BoardBead `json:"plan"`
	Implement []BoardBead `json:"implement"`
	Review    []BoardBead `json:"review"`
	Merge     []BoardBead `json:"merge"`
	Done      []BoardBead `json:"done"`
	Blocked   []BoardBead `json:"blocked"`
}

// Opts holds board command options shared between JSON output and TUI mode.
type Opts struct {
	Mine     bool
	Ready    bool
	Epic     string
	Interval time.Duration
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

// ToJSON converts Columns to the JSON-serializable ColumnsJSON.
func (c Columns) ToJSON() ColumnsJSON {
	return ColumnsJSON{
		Alerts:    NonNil(c.Alerts),
		Ready:     NonNil(c.Ready),
		Design:    NonNil(c.Design),
		Plan:      NonNil(c.Plan),
		Implement: NonNil(c.Implement),
		Review:    NonNil(c.Review),
		Merge:     NonNil(c.Merge),
		Done:      NonNil(c.Done),
		Blocked:   NonNil(c.Blocked),
	}
}
