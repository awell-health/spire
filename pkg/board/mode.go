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
	Init() tea.Cmd
	Update(msg tea.Msg) (Mode, tea.Cmd)
	View() string
	SetSize(w, h int)                          // dimensions AFTER Root's chrome
	OnActivate() tea.Cmd                       // called when this tab becomes active
	OnDeactivate()                             // called when user tabs away
	HandleTowerChanged(tc TowerChanged) tea.Cmd
	HasOverlay() bool                          // true = Root passes Tab to mode instead of switching
	ID() ModeID
}

// PendingActionMsg wraps an action string. RootModel detects this from mode
// Update returns to trigger TUI exit-and-relaunch.
type PendingActionMsg struct {
	Action string
	Args   []string
}
