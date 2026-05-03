// Package board provides board data shaping, categorization, Bubble Tea / Lip Gloss
// rendering, watch views, and roster summaries for the Spire TUI.
package board

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/spf13/cobra"
)

// Type aliases for convenience within pkg/board.
type BoardBead = store.BoardBead
type BoardDep = store.BoardDep
type Bead = store.Bead

// LocalAgent is an alias for agent.Entry used for the agent panel.
type LocalAgent = agent.Entry

// Columns holds beads categorized into board columns.
// Categorization is purely status-based: open/deferred→Backlog, ready→Ready,
// in_progress/dispatched→InProgress, awaiting_review→AwaitingReview,
// needs_changes→NeedsChanges, awaiting_human→AwaitingHuman,
// merge_pending→MergePending, closed→Done.
// The legacy phase fields (Design, Plan, Implement, Review, Merge) are retained
// for compilation compatibility with fetch.go/search.go but are no longer populated
// by the categorization functions.
type Columns struct {
	Alerts         []BoardBead
	AwaitingReview []BoardBead // beads with status='awaiting_review' (sage handoff)
	NeedsChanges   []BoardBead // beads with status='needs_changes' (sage requested changes)
	AwaitingHuman  []BoardBead // beads with status='awaiting_human' (parked on human action)
	MergePending   []BoardBead // beads with status='merge_pending' (queued for merge)
	Backlog        []BoardBead // open + deferred beads (not yet ready for agents)
	Ready          []BoardBead
	InProgress     []BoardBead // all in_progress beads (regardless of phase)
	Design         []BoardBead // legacy — no longer populated by categorization
	Plan           []BoardBead // legacy — no longer populated by categorization
	Implement      []BoardBead // legacy — no longer populated by categorization
	Review         []BoardBead // legacy — no longer populated by categorization
	Merge          []BoardBead // legacy — no longer populated by categorization
	Done           []BoardBead
	Blocked        []BoardBead
}

// ParkedBeads returns the union of beads parked in the per-status
// columns (AwaitingReview, NeedsChanges, AwaitingHuman, MergePending)
// in display order. Used by the lower-section renderer that groups all
// "stuck on something" beads into a single attention list.
func (c Columns) ParkedBeads() []BoardBead {
	out := make([]BoardBead, 0,
		len(c.AwaitingReview)+len(c.NeedsChanges)+len(c.AwaitingHuman)+len(c.MergePending))
	out = append(out, c.AwaitingReview...)
	out = append(out, c.NeedsChanges...)
	out = append(out, c.AwaitingHuman...)
	out = append(out, c.MergePending...)
	return out
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
	Alerts         []BoardBeadJSON `json:"alerts"`
	AwaitingReview []BoardBeadJSON `json:"awaiting_review"`
	NeedsChanges   []BoardBeadJSON `json:"needs_changes"`
	AwaitingHuman  []BoardBeadJSON `json:"awaiting_human"`
	MergePending   []BoardBeadJSON `json:"merge_pending"`
	Backlog        []BoardBeadJSON `json:"backlog"`
	Ready          []BoardBeadJSON `json:"ready"`
	InProgress     []BoardBeadJSON `json:"in_progress"`
	Design         []BoardBeadJSON `json:"design"`
	Plan           []BoardBeadJSON `json:"plan"`
	Implement      []BoardBeadJSON `json:"implement"`
	Review         []BoardBeadJSON `json:"review"`
	Merge          []BoardBeadJSON `json:"merge"`
	Done           []BoardBeadJSON `json:"done"`
	Blocked        []BoardBeadJSON `json:"blocked"`
}

// Opts holds board command options shared between JSON output and TUI mode.
type Opts struct {
	Mine      bool
	Ready     bool
	Epic      string
	Interval  time.Duration
	RootCmd   *cobra.Command // root cobra command for command mode completion/execution
	TowerName string         // current tower name (shown in header)

	// SkipLocalConflictCheck disables FetchBoard's call into
	// dolt.HasUnresolvedConflicts. The conflict check fork-execs
	// `dolt sql` and is a per-request hot path on dolt-touching
	// callers — fine for a TUI that runs on a laptop with a writable
	// local Dolt mirror that the user is expected to reconcile, dead
	// weight for HTTP gateway servers that do not own a local Dolt.
	// Caller-controlled by design: deciding this from the deployment
	// mode would conflate sync-transport semantics with topology, and
	// docs/ARCHITECTURE.md is explicit those axes are orthogonal.
	SkipLocalConflictCheck bool

	// ListTowersFn returns available towers for the T-key switcher. Injected by caller.
	ListTowersFn func() []TowerItem
	// ResolveFn resolves a parked bead (e.g. awaiting_human) with a
	// recovery learning comment.
	ResolveFn func(beadID, comment string) error
	// TermContentFn fetches content for the terminal pane overlay.
	// Takes beadID and returns rendered content string.
	TermContentFn func(beadID string) (string, error)
	// TowerItems lists available towers with their beads dirs for the RootModel tower switcher.
	TowerItems []TowerItem
	// AgentRegistry powers the Agents tab. wizardregistry/local in local mode,
	// wizardregistry/cluster in cluster mode. Nil renders the tab empty.
	AgentRegistry wizardregistry.Registry
}

// ViewMode identifies which tabbed view is active on the board.
type ViewMode int

const (
	ViewBoard  ViewMode = iota // main phase columns (default)
	ViewAlerts                 // alerts fullscreen
	ViewLower                  // blocked + parked fullscreen
)

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
// recoveryRefs provides pre-fetched recovery refs for parked beads (may be nil).
func (c Columns) ToJSON(recoveryRefs map[string]*RecoveryRef) ColumnsJSON {
	enrich := func(beads []BoardBead) []BoardBeadJSON {
		return nonNilJSON(enrichBeadsJSON(NonNil(beads)))
	}
	cj := ColumnsJSON{
		Alerts:         enrich(c.Alerts),
		AwaitingReview: enrich(c.AwaitingReview),
		NeedsChanges:   enrich(c.NeedsChanges),
		AwaitingHuman:  enrich(c.AwaitingHuman),
		MergePending:   enrich(c.MergePending),
		Backlog:        enrich(c.Backlog),
		Ready:          enrich(c.Ready),
		InProgress:     enrich(c.InProgress),
		Design:         enrich(c.Design),
		Plan:           enrich(c.Plan),
		Implement:      enrich(c.Implement),
		Review:         enrich(c.Review),
		Merge:          enrich(c.Merge),
		Done:           enrich(c.Done),
		Blocked:        enrich(c.Blocked),
	}
	// Enrich parked beads with pre-fetched recovery refs.
	if recoveryRefs != nil {
		attach := func(beads []BoardBeadJSON) {
			for i := range beads {
				if ref, ok := recoveryRefs[beads[i].ID]; ok {
					beads[i].RecoveryBead = ref
				}
			}
		}
		attach(cj.AwaitingReview)
		attach(cj.NeedsChanges)
		attach(cj.AwaitingHuman)
		attach(cj.MergePending)
	}
	return cj
}

// RunBoard runs the multi-mode board TUI with RootModel wrapping all modes.
// actionFn is called when the TUI exits with a pending action; it returns true to relaunch.
func RunBoard(opts Opts, identity string, fetchAgents func() []LocalAgent, actionFn func(PendingAction, string) bool, inlineActionFn func(PendingAction, string) error, rejectDesignFn ...func(string, string) error) error {
	var rejectFn func(string, string) error
	if len(rejectDesignFn) > 0 {
		rejectFn = rejectDesignFn[0]
	}

	for {
		beadsDir := resolveBeadsDirForBoard()
		boardMode, err := NewBoardMode(BoardModeOpts{
			BeadsDir:       beadsDir,
			Opts:           opts,
			Identity:       identity,
			FetchAgentsFn:  fetchAgents,
			InlineActionFn: inlineActionFn,
			RejectDesignFn: rejectFn,
		})
		if err != nil {
			return err
		}

		agentsMode := NewAgentsMode(opts.TowerName)
		agentsMode.InlineActionFn = inlineActionFn
		agentsMode.Registry = opts.AgentRegistry
		workshopMode := NewWorkshopMode()
		messagesMode := NewMessagesMode()
		metricsMode := NewMetricsMode()

		root := NewRootModel(RootOpts{
			TowerName:  opts.TowerName,
			Identity:   identity,
			BeadsDir:   beadsDir,
			Modes:      []Mode{boardMode, agentsMode, workshopMode, messagesMode, metricsMode},
			TowerItems: opts.TowerItems,
		})

		p := tea.NewProgram(root, tea.WithAltScreen())
		finalModel, err := p.Run()
		if err != nil {
			boardMode.Close()
			return err
		}
		boardMode.Close()

		// Recover the final RootModel from p.Run() so PendingAction() reflects mutations.
		if rm, ok := finalModel.(RootModel); ok {
			root = rm
		}

		// Check RootModel for pending action (exit-relaunch pattern).
		if pa := root.PendingAction(); pa != nil {
			action := parsePendingAction(pa.Action)
			beadID := ""
			if len(pa.Args) > 0 {
				beadID = pa.Args[0]
			}
			if action != ActionNone && actionFn(action, beadID) {
				continue
			}
			break
		}

		// Also check BoardMode's own pending action for backward compatibility.
		if boardMode.PendingAction != ActionNone {
			if !actionFn(boardMode.PendingAction, boardMode.PendingBeadID) {
				break
			}
			continue
		}

		break
	}
	return nil
}

// ActionComment and ActionResume are defined in the consolidated iota block in tui.go.

// parsePendingAction converts a string action name to a PendingAction.
func parsePendingAction(s string) PendingAction {
	switch strings.ToLower(s) {
	case "focus":
		return ActionFocus
	case "logs":
		return ActionLogs
	case "summon":
		return ActionSummon
	case "claim":
		return ActionClaim
	case "resummon":
		return ActionResummon
	case "close":
		return ActionClose
	case "grok":
		return ActionGrok
	case "trace":
		return ActionTrace
	case "comment":
		return ActionComment
	case "resume":
		return ActionResume
	default:
		return ActionNone
	}
}
