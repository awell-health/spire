package board

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// PendingAction identifies an action to run after the TUI exits.
type PendingAction int

const (
	ActionNone   PendingAction = iota
	ActionFocus                // print cmdFocus output, then relaunch
	ActionLogs                 // tail wizard logs, then relaunch
	ActionSummon               // summon a wizard for the bead, then relaunch
	ActionClaim                // claim the bead, then relaunch
)

// Model is the Bubble Tea model for the board TUI.
type Model struct {
	Opts          Opts
	Cols          Columns
	Agents        []LocalAgent // alive local wizards from registry
	TypeScope     TypeScope
	ShowAllCols   bool // when true, show all phase columns including empty ones
	Width         int
	Height        int
	LastTick      time.Time
	Quitting      bool
	SelCol        int // selected column index into DisplayColumns()
	SelCard       int // selected card index within selCol
	PendingAction PendingAction
	PendingBeadID string

	// Inspector state.
	Inspecting      bool           // true when the inspector pane is visible
	InspectorData   InspectorData  // fetched detail data for the inspected bead
	InspectorScroll int            // scroll offset within the inspector

	// FetchBoardFn is called to refresh board data. Injected by the caller.
	FetchBoardFn func(opts Opts) (Columns, error)
	// FetchAgentsFn is called to refresh local agents. Injected by the caller.
	FetchAgentsFn func() []LocalAgent
}

// VisibleCols returns the columns filtered by the current type scope.
func (m Model) VisibleCols() Columns {
	return FilterTypeScope(m.Cols, m.TypeScope)
}

// DisplayColumns returns the columns to display, respecting ShowAllCols toggle.
func (m Model) DisplayColumns() []ColDef {
	vis := m.VisibleCols()
	if m.ShowAllCols {
		return AllColumns(vis)
	}
	return ActiveColumns(vis)
}

// ClampSelection keeps SelCol and SelCard within valid bounds.
func (m *Model) ClampSelection() {
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
		return
	}
	m.SelCard = ((m.SelCard % n) + n) % n
}

// SelectedBead returns a pointer to the currently selected bead, or nil.
func (m Model) SelectedBead() *BoardBead {
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

type tickMsg time.Time

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// RunBoardTUI runs the board TUI in a loop, executing pending actions between launches.
// actionFn is called when the TUI exits with a pending action; it returns true to relaunch.
func RunBoardTUI(opts Opts, fetchBoard func(Opts) (Columns, error), fetchAgents func() []LocalAgent, actionFn func(PendingAction, string) bool) error {
	for {
		cols, err := fetchBoard(opts)
		if err != nil {
			return err
		}
		m := Model{
			Opts:          opts,
			Cols:          cols,
			Agents:        fetchAgents(),
			LastTick:      time.Now(),
			FetchBoardFn:  fetchBoard,
			FetchAgentsFn: fetchAgents,
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
	return tickCmd(m.Opts.Interval)
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Inspector mode: handle keys differently.
		if m.Inspecting {
			switch msg.String() {
			case "esc", "q", "enter":
				m.Inspecting = false
				m.InspectorScroll = 0
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
				total := InspectorLineCount(m.InspectorData, m.Width)
				maxVisible := m.Height - 2
				if maxVisible < 5 {
					maxVisible = 5
				}
				m.InspectorScroll = total - maxVisible
				if m.InspectorScroll < 0 {
					m.InspectorScroll = 0
				}
			}
			return m, nil
		}

		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.Quitting = true
			return m, tea.Quit

		// Open inspector on Enter.
		case "enter":
			if bead := m.SelectedBead(); bead != nil {
				m.Inspecting = true
				m.InspectorScroll = 0
				m.InspectorData = FetchInspectorData(*bead)
			}

		// Column navigation.
		case "h", "left", "shift+tab":
			m.SelCol--
			m.SelCard = 0
			m.ClampSelection()
		case "l", "right", "tab":
			m.SelCol++
			m.SelCard = 0
			m.ClampSelection()

		// Card navigation.
		case "j", "down":
			m.SelCard++
			m.ClampSelection()
		case "k", "up":
			m.SelCard--
			m.ClampSelection()

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
			if m.FetchBoardFn != nil {
				if newCols, err := m.FetchBoardFn(m.Opts); err == nil {
					m.Cols = newCols
				}
			}
			m.ClampSelection()
		case "t":
			m.TypeScope = m.TypeScope.Next()
			m.ClampSelection()

		// Toggle showing all phase columns (including empty).
		case "H":
			m.ShowAllCols = !m.ShowAllCols
			m.ClampSelection()

		// Actions on the selected bead.
		case "f":
			if bead := m.SelectedBead(); bead != nil {
				m.PendingAction = ActionFocus
				m.PendingBeadID = bead.ID
				m.Quitting = true
				return m, tea.Quit
			}
		case "s":
			if bead := m.SelectedBead(); bead != nil {
				m.PendingAction = ActionSummon
				m.PendingBeadID = bead.ID
				m.Quitting = true
				return m, tea.Quit
			}
		case "c":
			if bead := m.SelectedBead(); bead != nil {
				m.PendingAction = ActionClaim
				m.PendingBeadID = bead.ID
				m.Quitting = true
				return m, tea.Quit
			}
		case "L":
			if bead := m.SelectedBead(); bead != nil {
				m.PendingAction = ActionLogs
				m.PendingBeadID = bead.ID
				m.Quitting = true
				return m, tea.Quit
			}
		}
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
	case tickMsg:
		if !m.Inspecting && m.FetchBoardFn != nil {
			if cols, err := m.FetchBoardFn(m.Opts); err == nil {
				m.Cols = cols
			}
		}
		if !m.Inspecting && m.FetchAgentsFn != nil {
			m.Agents = m.FetchAgentsFn()
		}
		m.LastTick = time.Now()
		if !m.Inspecting {
			m.ClampSelection()
		}
		return m, tickCmd(m.Opts.Interval)
	}
	return m, nil
}
