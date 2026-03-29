package board

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// PendingAction identifies an action to run after the TUI exits.
type PendingAction int

const (
	ActionNone     PendingAction = iota
	ActionFocus                  // print cmdFocus output, then relaunch
	ActionLogs                   // tail wizard logs, then relaunch
	ActionSummon                 // summon a wizard for the bead, then relaunch
	ActionClaim                  // claim the bead, then relaunch
	ActionResummon               // resummon a stuck bead (needs-human), then relaunch
	ActionClose                  // close/dismiss the bead (inline via tea.Cmd)
	ActionUnsummon               // dismiss active wizard (inline via tea.Cmd)
	ActionResetSoft              // soft reset (inline via tea.Cmd)
	ActionResetHard              // reset --hard (inline via tea.Cmd)
	ActionGrok                   // deep focus grok (inline via tea.Cmd)
	ActionTrace                  // DAG timeline trace (inline via tea.Cmd)
	ActionAdvance                // advance to next phase (inline via tea.Cmd)
)

// Section identifies which vertical zone of the board the cursor is in.
type Section int

const (
	SectionAlerts  Section = iota // alert beads above the columns
	SectionColumns                // the main phase columns
	SectionBlocked                // blocked beads below the columns
)

// Model is the Bubble Tea model for the board TUI.
type Model struct {
	Opts          Opts
	Cols          Columns
	Agents        []LocalAgent // alive local wizards from registry
	Identity      string       // user identity for snapshot fetching
	TypeScope     TypeScope
	ShowAllCols   bool // when true, show all phase columns including empty ones
	Width         int
	Height        int
	LastTick      time.Time
	Quitting      bool
	SelSection    Section // which vertical zone the cursor is in
	SelCol        int     // selected column index into DisplayColumns()
	SelCard       int     // selected card index within selCol
	ColScroll     int     // scroll offset for the selected column (beads above viewport)
	PendingAction PendingAction
	PendingBeadID string

	// Snapshot holds the pre-fetched board state assembled in the background.
	// View() reads from this struct — no I/O during render.
	Snapshot *BoardSnapshot

	// Inspector state.
	Inspecting      bool            // true when the inspector pane is visible
	InspectorData   *InspectorData  // fetched detail data (nil when loading)
	InspectorLoading bool           // true while async fetch is in progress
	InspectorScroll int             // scroll offset within the inspector

	// FetchAgentsFn is called to refresh local agents. Injected by the caller.
	FetchAgentsFn func() []LocalAgent

	// Action menu overlay state.
	ActionMenuOpen   bool
	ActionMenuItems  []MenuAction
	ActionMenuCursor int
	ActionMenuBeadID string

	// Search/filter state.
	SearchActive bool   // true when user is typing a search query
	SearchQuery  string // current search filter text

	// Inspector tab (0=details, 1=logs).
	InspectorTab int

	// Vim gg key sequence: true after first g press, waiting for second key.
	PendingG bool

	// Inline action execution state.
	ActionRunning    bool
	ActionStatus     string    // transient status message shown in footer
	ActionStatusTime time.Time // when status was set (for auto-clear)

	// InlineActionFn executes an action within the TUI via tea.Cmd.
	// Returns nil on success, error on failure.
	InlineActionFn func(PendingAction, string) error

	// Confirmation dialog state.
	ConfirmOpen   bool
	ConfirmAction PendingAction
	ConfirmBeadID string
	ConfirmPrompt string
	ConfirmDanger DangerLevel
}

// VisibleCols returns the columns filtered by the current type scope.
func (m Model) VisibleCols() Columns {
	return FilterTypeScope(m.Cols, m.TypeScope)
}

// DisplayColumns returns the columns to display, respecting ShowAllCols toggle
// and search filter. This is the single filtering point for search — both
// View() and navigation use these results.
func (m Model) DisplayColumns() []ColDef {
	vis := m.VisibleCols()
	if m.SearchQuery != "" {
		vis = FilterColumns(vis, m.SearchQuery)
	}
	if m.ShowAllCols {
		return AllColumns(vis)
	}
	return ActiveColumns(vis)
}

// ensureCardVisible adjusts ColScroll so SelCard is within the visible window.
func (m *Model) ensureCardVisible(maxCards int) {
	if maxCards <= 0 {
		return
	}
	if m.SelCard < m.ColScroll {
		m.ColScroll = m.SelCard
	}
	if m.SelCard >= m.ColScroll+maxCards {
		m.ColScroll = m.SelCard - maxCards + 1
	}
	if m.ColScroll < 0 {
		m.ColScroll = 0
	}
}

// colMaxCards computes MaxCards from the current board state.
func (m *Model) colMaxCards() int {
	vis := m.VisibleCols()
	displayCols := m.DisplayColumns()
	budget := CalcHeightBudget(m.Height, len(vis.Alerts), len(vis.Blocked), len(displayCols), len(m.Agents))
	return budget.MaxCards
}

// ClampSelection keeps SelSection, SelCol, and SelCard within valid bounds.
func (m *Model) ClampSelection() {
	vis := m.VisibleCols()

	// If current section is empty, fall through to columns.
	if m.SelSection == SectionAlerts && len(vis.Alerts) == 0 {
		m.SelSection = SectionColumns
	}
	if m.SelSection == SectionBlocked && len(vis.Blocked) == 0 {
		m.SelSection = SectionColumns
	}

	switch m.SelSection {
	case SectionAlerts:
		n := len(vis.Alerts)
		if m.SelCard < 0 {
			m.SelCard = 0
		}
		if m.SelCard >= n {
			m.SelCard = n - 1
		}
		return
	case SectionBlocked:
		n := len(vis.Blocked)
		if m.SelCard < 0 {
			m.SelCard = 0
		}
		if m.SelCard >= n {
			m.SelCard = n - 1
		}
		return
	}

	// SectionColumns
	active := m.DisplayColumns()
	if len(active) == 0 {
		m.SelCol = 0
		m.SelCard = 0
		return
	}
	if m.SelCol < 0 {
		m.SelCol = 0
	}
	if m.SelCol >= len(active) {
		m.SelCol = len(active) - 1
	}
	n := len(active[m.SelCol].Beads)
	if n == 0 {
		m.SelCard = 0
		m.ColScroll = 0
		return
	}
	m.SelCard = ((m.SelCard % n) + n) % n

	// Clamp ColScroll to valid range.
	if m.ColScroll > m.SelCard {
		m.ColScroll = m.SelCard
	}
	if m.ColScroll > n-1 {
		m.ColScroll = n - 1
	}
	if m.ColScroll < 0 {
		m.ColScroll = 0
	}
}

// SelectedBead returns a pointer to the currently selected bead, or nil.
func (m Model) SelectedBead() *BoardBead {
	vis := m.VisibleCols()
	switch m.SelSection {
	case SectionAlerts:
		if m.SelCard >= 0 && m.SelCard < len(vis.Alerts) {
			return &vis.Alerts[m.SelCard]
		}
		return nil
	case SectionBlocked:
		if m.SelCard >= 0 && m.SelCard < len(vis.Blocked) {
			return &vis.Blocked[m.SelCard]
		}
		return nil
	default: // SectionColumns
		active := m.DisplayColumns()
		if m.SelCol < 0 || m.SelCol >= len(active) {
			return nil
		}
		beads := active[m.SelCol].Beads
		if m.SelCard < 0 || m.SelCard >= len(beads) {
			return nil
		}
		return &beads[m.SelCard]
	}
}

type tickMsg time.Time

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// RunBoardTUI runs the board TUI in a loop, executing pending actions between launches.
// actionFn is called when the TUI exits with a pending action; it returns true to relaunch.
// inlineActionFn is used for actions that execute within the TUI via tea.Cmd (no exit-relaunch).
func RunBoardTUI(opts Opts, identity string, fetchAgents func() []LocalAgent, actionFn func(PendingAction, string) bool, inlineActionFn func(PendingAction, string) error) error {
	for {
		m := Model{
			Opts:           opts,
			Identity:       identity,
			LastTick:       time.Now(),
			SelSection:     SectionColumns,
			FetchAgentsFn:  fetchAgents,
			InlineActionFn: inlineActionFn,
		}
		p := tea.NewProgram(m, tea.WithAltScreen())
		result, err := p.Run()
		if err != nil {
			return err
		}

		final, ok := result.(Model)
		if !ok || final.PendingAction == ActionNone {
			break
		}

		if !actionFn(final.PendingAction, final.PendingBeadID) {
			break
		}
	}
	return nil
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
}

// actionResultMsg carries the result of an inline action executed via tea.Cmd.
type actionResultMsg struct {
	Action PendingAction
	BeadID string
	Err    error
}

// runInlineActionCmd returns a tea.Cmd that executes an action in a goroutine.
func runInlineActionCmd(fn func(PendingAction, string) error, action PendingAction, beadID string) tea.Cmd {
	return func() tea.Msg {
		err := fn(action, beadID)
		return actionResultMsg{Action: action, BeadID: beadID, Err: err}
	}
}

// actionLabel returns a human-readable label for an action.
func actionLabel(a PendingAction) string {
	switch a {
	case ActionSummon:
		return "Summon"
	case ActionResummon:
		return "Resummon"
	case ActionUnsummon:
		return "Unsummon"
	case ActionResetSoft:
		return "Reset"
	case ActionResetHard:
		return "Reset --hard"
	case ActionGrok:
		return "Grok"
	case ActionTrace:
		return "Trace"
	case ActionAdvance:
		return "Advance"
	case ActionClose:
		return "Close"
	default:
		return "Action"
	}
}

// isInlineAction returns true if the action should execute within the TUI.
func isInlineAction(a PendingAction) bool {
	switch a {
	case ActionSummon, ActionResummon, ActionUnsummon, ActionResetSoft, ActionResetHard, ActionGrok, ActionTrace, ActionAdvance, ActionClose:
		return true
	}
	return false
}

// confirmPromptForAction returns the confirmation prompt text for an action.
func confirmPromptForAction(action PendingAction, beadID string) string {
	switch action {
	case ActionClose:
		return fmt.Sprintf("Close %s?", beadID)
	case ActionUnsummon:
		return fmt.Sprintf("Dismiss wizard for %s?", beadID)
	case ActionResetSoft:
		return fmt.Sprintf("Reset %s?", beadID)
	case ActionResetHard:
		return fmt.Sprintf("Hard reset %s? This is destructive.", beadID)
	default:
		return fmt.Sprintf("%s %s?", actionLabel(action), beadID)
	}
}

// dangerForAction returns the danger level for an action.
func dangerForAction(action PendingAction) DangerLevel {
	switch action {
	case ActionResetHard:
		return DangerDestructive
	case ActionClose, ActionUnsummon, ActionResetSoft:
		return DangerConfirm
	default:
		return DangerNone
	}
}

// updateConfirm handles key input in the confirmation dialog.
func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		action := m.ConfirmAction
		beadID := m.ConfirmBeadID
		m.ConfirmOpen = false
		if m.ActionRunning {
			return m, nil
		}
		m.ActionRunning = true
		m.ActionStatus = actionLabel(action) + "..."
		m.ActionStatusTime = time.Now()
		return m, runInlineActionCmd(m.InlineActionFn, action, beadID)
	case "n", "N", "esc":
		m.ConfirmOpen = false
		return m, nil
	}
	return m, nil
}

// dispatchInlineAction dispatches an inline action via tea.Cmd if the Model has an InlineActionFn.
func (m *Model) dispatchInlineAction(action PendingAction, beadID string) (Model, tea.Cmd) {
	if m.ActionRunning {
		return *m, nil
	}
	if m.InlineActionFn == nil {
		// Fallback to exit-relaunch pattern if no inline fn provided.
		m.PendingAction = action
		m.PendingBeadID = beadID
		m.Quitting = true
		return *m, tea.Quit
	}
	m.ActionRunning = true
	m.ActionStatus = actionLabel(action) + "..."
	m.ActionStatusTime = time.Now()
	return *m, runInlineActionCmd(m.InlineActionFn, action, beadID)
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Confirmation dialog: absorb all keys.
		if m.ConfirmOpen {
			return m.updateConfirm(msg)
		}

		// Action menu mode: absorb all keys.
		if m.ActionMenuOpen {
			switch msg.String() {
			case "esc", "q":
				m.ActionMenuOpen = false
				return m, nil
			case "j", "down":
				if m.ActionMenuCursor < len(m.ActionMenuItems)-1 {
					m.ActionMenuCursor++
				}
				return m, nil
			case "k", "up":
				if m.ActionMenuCursor > 0 {
					m.ActionMenuCursor--
				}
				return m, nil
			case "enter":
				if m.ActionMenuCursor >= 0 && m.ActionMenuCursor < len(m.ActionMenuItems) {
					item := m.ActionMenuItems[m.ActionMenuCursor]
					m.ActionMenuOpen = false
					if isInlineAction(item.ActionType) {
						if item.Danger != DangerNone {
							m.ConfirmOpen = true
							m.ConfirmAction = item.ActionType
							m.ConfirmBeadID = m.ActionMenuBeadID
							m.ConfirmPrompt = confirmPromptForAction(item.ActionType, m.ActionMenuBeadID)
							m.ConfirmDanger = item.Danger
							return m, nil
						}
						mm, cmd := m.dispatchInlineAction(item.ActionType, m.ActionMenuBeadID)
						return mm, cmd
					}
					m.PendingAction = item.ActionType
					m.PendingBeadID = m.ActionMenuBeadID
					m.Quitting = true
					return m, tea.Quit
				}
				return m, nil
			default:
				// Check shortcut key match.
				for _, item := range m.ActionMenuItems {
					if msg.String() == string(item.Key) {
						m.ActionMenuOpen = false
						if isInlineAction(item.ActionType) {
							if item.Danger != DangerNone {
								m.ConfirmOpen = true
								m.ConfirmAction = item.ActionType
								m.ConfirmBeadID = m.ActionMenuBeadID
								m.ConfirmPrompt = confirmPromptForAction(item.ActionType, m.ActionMenuBeadID)
								m.ConfirmDanger = item.Danger
								return m, nil
							}
							mm, cmd := m.dispatchInlineAction(item.ActionType, m.ActionMenuBeadID)
							return mm, cmd
						}
						m.PendingAction = item.ActionType
						m.PendingBeadID = m.ActionMenuBeadID
						m.Quitting = true
						return m, tea.Quit
					}
				}
				return m, nil
			}
		}

		// Search mode: absorb all keys.
		if m.SearchActive {
			switch msg.String() {
			case "esc":
				m.SearchActive = false
				m.SearchQuery = ""
				m.SelCard = 0
				m.ColScroll = 0
				m.ClampSelection()
				return m, nil
			case "enter":
				m.SearchActive = false
				return m, nil
			case "backspace":
				if len(m.SearchQuery) > 0 {
					m.SearchQuery = m.SearchQuery[:len(m.SearchQuery)-1]
				}
				m.SelCard = 0
				m.ColScroll = 0
				m.ClampSelection()
				return m, nil
			case "ctrl+u":
				m.SearchQuery = ""
				m.SelCard = 0
				m.ColScroll = 0
				m.ClampSelection()
				return m, nil
			default:
				// Append printable runes.
				if len(msg.String()) == 1 && msg.String()[0] >= 32 {
					m.SearchQuery += msg.String()
					m.SelCard = 0
					m.ColScroll = 0
					m.ClampSelection()
				}
				return m, nil
			}
		}

		// Inspector mode: handle keys differently.
		if m.Inspecting {
			switch msg.String() {
			case "esc", "q", "enter":
				m.Inspecting = false
				m.InspectorScroll = 0
				m.InspectorTab = 0
				m.InspectorData = nil
				m.InspectorLoading = false
			case "ctrl+c":
				m.Quitting = true
				return m, tea.Quit
			case "j", "down":
				m.InspectorScroll++
			case "k", "up":
				m.InspectorScroll--
				if m.InspectorScroll < 0 {
					m.InspectorScroll = 0
				}
			case "g":
				m.InspectorScroll = 0
			case "G":
				if m.InspectorData != nil {
					var dag *DAGProgress
					if m.Snapshot != nil {
						dag = m.Snapshot.DAGProgress[m.InspectorData.Bead.ID]
					}
					total := inspectorLineCountSnap(m.InspectorData, dag, m.Width, m.InspectorTab)
					maxVisible := m.Height - 2
					if maxVisible < 5 {
						maxVisible = 5
					}
					m.InspectorScroll = total - maxVisible
					if m.InspectorScroll < 0 {
						m.InspectorScroll = 0
					}
				}
			case "tab":
				m.InspectorTab++
				if m.InspectorTab > 1 {
					m.InspectorTab = 0
				}
				m.InspectorScroll = 0
			case "shift+tab":
				m.InspectorTab--
				if m.InspectorTab < 0 {
					m.InspectorTab = 1
				}
				m.InspectorScroll = 0
			}
			return m, nil
		}

		// Board mode: handle pending G for gg sequence.
		if m.PendingG {
			m.PendingG = false
			if msg.String() == "g" {
				m.SelCard = 0
				m.ColScroll = 0
				return m, nil
			}
			// Not gg — fall through to handle the key normally.
		}

		switch msg.String() {
		case "q", "ctrl+c", "esc":
			if m.SearchQuery != "" {
				m.SearchQuery = ""
				m.SelCard = 0
				m.ColScroll = 0
				m.ClampSelection()
				return m, nil
			}
			m.Quitting = true
			return m, tea.Quit

		// Open inspector on Enter or i.
		case "enter", "i":
			if bead := m.SelectedBead(); bead != nil {
				m.Inspecting = true
				m.InspectorScroll = 0
				m.InspectorTab = 0
				m.InspectorLoading = true
				m.InspectorData = nil
				return m, fetchInspectorCmd(*bead)
			}

		// Column navigation.
		case "h", "left":
			if m.SelSection != SectionColumns {
				m.SelSection = SectionColumns
				m.SelCard = 0
				m.ColScroll = 0
				m.ClampSelection()
			} else {
				m.SelCol--
				m.ColScroll = 0
				m.ClampSelection()
				m.ensureCardVisible(m.colMaxCards())
			}
		case "l", "right":
			if m.SelSection != SectionColumns {
				m.SelSection = SectionColumns
				m.SelCard = 0
				m.ColScroll = 0
				m.ClampSelection()
			} else {
				m.SelCol++
				m.ColScroll = 0
				m.ClampSelection()
				m.ensureCardVisible(m.colMaxCards())
			}

		// Card navigation (section-aware).
		case "j", "down":
			vis := m.VisibleCols()
			if m.SearchQuery != "" {
				vis = FilterColumns(vis, m.SearchQuery)
			}
			switch m.SelSection {
			case SectionAlerts:
				if m.SelCard+1 < len(vis.Alerts) {
					m.SelCard++
				} else {
					m.SelSection = SectionColumns
					m.SelCard = 0
					m.ColScroll = 0
					m.ClampSelection()
				}
			case SectionColumns:
				active := m.DisplayColumns()
				maxCard := 0
				if m.SelCol >= 0 && m.SelCol < len(active) {
					maxCard = len(active[m.SelCol].Beads)
				}
				if m.SelCard+1 < maxCard {
					m.SelCard++
					m.ClampSelection()
					m.ensureCardVisible(m.colMaxCards())
				} else if len(vis.Blocked) > 0 {
					m.SelSection = SectionBlocked
					m.SelCard = 0
					m.ClampSelection()
				}
			case SectionBlocked:
				if m.SelCard+1 < len(vis.Blocked) {
					m.SelCard++
				}
			}
		case "k", "up":
			vis := m.VisibleCols()
			if m.SearchQuery != "" {
				vis = FilterColumns(vis, m.SearchQuery)
			}
			switch m.SelSection {
			case SectionAlerts:
				if m.SelCard > 0 {
					m.SelCard--
				}
			case SectionColumns:
				if m.SelCard > 0 {
					m.SelCard--
					m.ClampSelection()
					m.ensureCardVisible(m.colMaxCards())
				} else if len(vis.Alerts) > 0 {
					m.SelSection = SectionAlerts
					m.SelCard = len(vis.Alerts) - 1
					m.ClampSelection()
				}
			case SectionBlocked:
				if m.SelCard > 0 {
					m.SelCard--
				} else {
					m.SelSection = SectionColumns
					active := m.DisplayColumns()
					if m.SelCol >= 0 && m.SelCol < len(active) {
						lastCard := len(active[m.SelCol].Beads) - 1
						if lastCard < 0 {
							lastCard = 0
						}
						m.SelCard = lastCard
					} else {
						m.SelCard = 0
					}
					m.ClampSelection()
					m.ensureCardVisible(m.colMaxCards())
				}
			}

		// Vim gg: first g sets PendingG.
		case "g":
			m.PendingG = true
			return m, nil

		// Vim G: go to bottom of current column.
		case "G":
			active := m.DisplayColumns()
			if m.SelSection == SectionColumns && m.SelCol >= 0 && m.SelCol < len(active) {
				lastCard := len(active[m.SelCol].Beads) - 1
				if lastCard < 0 {
					lastCard = 0
				}
				m.SelCard = lastCard
				m.ensureCardVisible(m.colMaxCards())
			}

		// Epic scoping toggle.
		case "e":
			if m.Opts.Epic != "" {
				m.Opts.Epic = ""
			} else if bead := m.SelectedBead(); bead != nil {
				if bead.Type == "epic" {
					m.Opts.Epic = bead.ID
				} else if bead.Parent != "" {
					m.Opts.Epic = bead.Parent
				}
			}
			m.ClampSelection()
			return m, fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
		case "t":
			m.TypeScope = m.TypeScope.Next()
			m.ClampSelection()

		// Toggle showing all phase columns (including empty).
		case "H":
			m.ShowAllCols = !m.ShowAllCols
			m.ClampSelection()

		// Summon wizard — inline.
		case "s":
			if bead := m.SelectedBead(); bead != nil {
				mm, cmd := m.dispatchInlineAction(ActionSummon, bead.ID)
				return mm, cmd
			}

		// Unsummon wizard — confirm, then inline (only if bead has a wizard).
		case "u":
			if bead := m.SelectedBead(); bead != nil {
				for _, a := range m.Agents {
					if a.BeadID == bead.ID {
						m.ConfirmOpen = true
						m.ConfirmAction = ActionUnsummon
						m.ConfirmBeadID = bead.ID
						m.ConfirmPrompt = confirmPromptForAction(ActionUnsummon, bead.ID)
						m.ConfirmDanger = DangerConfirm
						return m, nil
					}
				}
			}

		// Resummon — inline.
		case "S":
			if bead := m.SelectedBead(); bead != nil && bead.HasLabel("needs-human") {
				mm, cmd := m.dispatchInlineAction(ActionResummon, bead.ID)
				return mm, cmd
			}

		// Reset — confirm, then inline.
		case "r":
			if bead := m.SelectedBead(); bead != nil && bead.Status == "in_progress" {
				m.ConfirmOpen = true
				m.ConfirmAction = ActionResetSoft
				m.ConfirmBeadID = bead.ID
				m.ConfirmPrompt = confirmPromptForAction(ActionResetSoft, bead.ID)
				m.ConfirmDanger = DangerConfirm
				return m, nil
			}

		// Reset --hard — confirm, then inline.
		case "R":
			if bead := m.SelectedBead(); bead != nil && bead.Status == "in_progress" {
				m.ConfirmOpen = true
				m.ConfirmAction = ActionResetHard
				m.ConfirmBeadID = bead.ID
				m.ConfirmPrompt = confirmPromptForAction(ActionResetHard, bead.ID)
				m.ConfirmDanger = DangerDestructive
				return m, nil
			}

		// Close — confirm, then inline.
		case "x":
			if bead := m.SelectedBead(); bead != nil {
				m.ConfirmOpen = true
				m.ConfirmAction = ActionClose
				m.ConfirmBeadID = bead.ID
				m.ConfirmPrompt = confirmPromptForAction(ActionClose, bead.ID)
				m.ConfirmDanger = DangerConfirm
				return m, nil
			}

		// Action menu.
		case "a":
			if bead := m.SelectedBead(); bead != nil {
				m.ActionMenuBeadID = bead.ID
				m.ActionMenuItems = BuildActionMenu(bead, m.Agents)
				m.ActionMenuCursor = 0
				m.ActionMenuOpen = true
				return m, nil
			}

		// Search.
		case "/":
			m.SearchActive = true
			m.SearchQuery = ""
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		if m.SelSection == SectionColumns {
			m.ensureCardVisible(m.colMaxCards())
		}
	case tickMsg:
		m.LastTick = time.Now()
		// Auto-clear action status after 5 seconds.
		if m.ActionStatus != "" && time.Since(m.ActionStatusTime) > 5*time.Second {
			m.ActionStatus = ""
		}
		if !m.Inspecting {
			return m, fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
		}
		return m, tickCmd(m.Opts.Interval)
	case snapshotMsg:
		if msg.Err == nil && msg.Snap != nil {
			m.Snapshot = msg.Snap
			m.Cols = msg.Snap.Columns
			m.Agents = msg.Snap.Agents
		}
		if !m.Inspecting {
			m.ClampSelection()
		}
		return m, tickCmd(m.Opts.Interval)
	case inspectorDataMsg:
		if msg.Err == nil && msg.Data != nil {
			m.InspectorData = msg.Data
		}
		m.InspectorLoading = false
		return m, nil
	case actionResultMsg:
		m.ActionRunning = false
		if msg.Err != nil {
			m.ActionStatus = fmt.Sprintf("%s failed: %v", actionLabel(msg.Action), msg.Err)
		} else {
			m.ActionStatus = fmt.Sprintf("%s: done", actionLabel(msg.Action))
		}
		m.ActionStatusTime = time.Now()
		// Refresh board data after action completes.
		return m, fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
	}
	return m, nil
}
