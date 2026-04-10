package board

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Compile-time check: WorkshopMode implements Mode.
var _ Mode = (*WorkshopMode)(nil)

// WorkshopMode is a stub Mode for the Workshop tab.
type WorkshopMode struct {
	width, height int
}

// NewWorkshopMode creates a new WorkshopMode.
func NewWorkshopMode() *WorkshopMode {
	return &WorkshopMode{}
}

func (m *WorkshopMode) ID() ModeID                              { return ModeWorkshop }
func (m *WorkshopMode) Init() tea.Cmd                           { return nil }
func (m *WorkshopMode) Update(tea.Msg) (Mode, tea.Cmd)          { return m, nil }
func (m *WorkshopMode) SetSize(w, h int)                        { m.width, m.height = w, h }
func (m *WorkshopMode) OnActivate() tea.Cmd                     { return nil }
func (m *WorkshopMode) OnDeactivate()                           {}
func (m *WorkshopMode) HandleTowerChanged(TowerChanged) tea.Cmd { return nil }
func (m *WorkshopMode) HasOverlay() bool                        { return false }

func (m *WorkshopMode) View() string {
	msg := "Workshop mode — coming soon"
	if m.width > 0 && m.height > 0 {
		pad := (m.width - len(msg)) / 2
		if pad < 0 {
			pad = 0
		}
		vpad := m.height / 2
		return strings.Repeat("\n", vpad) + strings.Repeat(" ", pad) + msg
	}
	return msg
}
