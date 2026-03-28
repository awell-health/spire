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
	Width         int
	Height        int
	LastTick      time.Time
	Quitting      bool
	SelCol        int // selected column index into ActiveColumns(cols)
	SelCard       int // selected card index within selCol
	PendingAction PendingAction
	PendingBeadID string

	// FetchBoardFn is called to refresh board data. Injected by the caller.
	FetchBoardFn func(opts Opts) (Columns, error)
	// FetchAgentsFn is called to refresh local agents. Injected by the caller.
	FetchAgentsFn func() []LocalAgent
}

// VisibleCols returns the columns filtered by the current type scope.
func (m Model) VisibleCols() Columns {
	return FilterTypeScope(m.Cols, m.TypeScope)
}

// ClampSelection keeps SelCol and SelCard within valid bounds.
func (m *Model) ClampSelection() {
	active := ActiveColumns(m.VisibleCols())
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
	active := ActiveColumns(m.VisibleCols())
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
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.Quitting = true
			return m, tea.Quit

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
		if m.FetchBoardFn != nil {
			if cols, err := m.FetchBoardFn(m.Opts); err == nil {
				m.Cols = cols
			}
		}
		if m.FetchAgentsFn != nil {
			m.Agents = m.FetchAgentsFn()
		}
		m.LastTick = time.Now()
		m.ClampSelection()
		return m, tickCmd(m.Opts.Interval)
	}
	return m, nil
}
