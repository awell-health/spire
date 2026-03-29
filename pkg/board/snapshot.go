package board

import "time"

// BoardSnapshot holds a complete, pre-fetched board state assembled in a single
// background pass. View() reads from this struct — no I/O allowed during render.
type BoardSnapshot struct {
	Columns     Columns
	DAGProgress map[string]*DAGProgress
	EpicSummary map[string]*EpicChildSummary
	Agents      []LocalAgent
	PhaseMap    map[string]string // beadID -> phase string (for render)
	FetchedAt   time.Time
}
