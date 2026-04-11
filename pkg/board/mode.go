package board

import tea "github.com/charmbracelet/bubbletea"

// ModeID identifies which TUI mode is active.
type ModeID int

const (
	ModeBoard    ModeID = iota
	ModeAgents
	ModeWorkshop
	ModeMessages
	ModeMetrics
)

// String returns the display name for a ModeID.
func (m ModeID) String() string {
	switch m {
	case ModeBoard:
		return "Board"
	case ModeAgents:
		return "Agents"
	case ModeWorkshop:
		return "Workshop"
	case ModeMessages:
		return "Messages"
	case ModeMetrics:
		return "Metrics"
	default:
		return "Unknown"
	}
}

// TowerChanged is sent by RootModel when the user switches towers.
type TowerChanged struct {
	Name     string // tower name
	BeadsDir string // resolved beads directory path
}

// Mode is the interface each TUI mode must implement.
type Mode interface {
	// Init returns the initial command (e.g. start fetch ticker).
	// Called once when the mode is created.
	Init() tea.Cmd

	// Update is the standard Bubble Tea update loop. Returns (Mode, tea.Cmd)
	// — the concrete mode type is returned as the Mode interface value,
	// not (tea.Model, tea.Cmd).
	Update(msg tea.Msg) (Mode, tea.Cmd)

	// View renders the mode content. Must do zero I/O; render from cached
	// snapshot only.
	View() string

	// SetSize receives dimensions AFTER RootModel's chrome (tab bar + status
	// footer). The mode must not subtract further.
	SetSize(w, h int)

	// OnActivate is called when the tab switches to this mode. Return a
	// tea.Cmd to resume fetching or refresh data.
	OnActivate() tea.Cmd

	// OnDeactivate is called when the user tabs away. Pause expensive
	// operations. No return value.
	OnDeactivate()

	// HandleTowerChanged is called when the user switches towers. Close the
	// current data connection, reopen against tc.BeadsDir, and re-fetch.
	// Return a tea.Cmd for the initial fetch.
	HandleTowerChanged(tc TowerChanged) tea.Cmd

	// HasOverlay reports whether the mode has an active overlay (inspector,
	// command palette, etc.). When true, RootModel passes the Tab keypress
	// to the mode instead of switching tabs.
	HasOverlay() bool

	// ID returns the mode's ModeID constant. Used by RootModel for tab bar
	// rendering and message routing.
	ID() ModeID
}

// PendingActionMsg wraps an action string. RootModel detects this from mode
// Update returns to trigger TUI exit-and-relaunch.
type PendingActionMsg struct {
	Action string
	Args   []string
}
