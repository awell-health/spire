package board

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Compile-time check: MessagesMode implements Mode.
var _ Mode = (*MessagesMode)(nil)

// MessagesMode is a stub Mode for the Messages tab.
type MessagesMode struct {
	width, height int
}

// NewMessagesMode creates a new MessagesMode.
func NewMessagesMode() *MessagesMode {
	return &MessagesMode{}
}

func (m *MessagesMode) ID() ModeID                              { return ModeMessages }
func (m *MessagesMode) Init() tea.Cmd                           { return nil }
func (m *MessagesMode) Update(tea.Msg) (Mode, tea.Cmd)          { return m, nil }
func (m *MessagesMode) SetSize(w, h int)                        { m.width, m.height = w, h }
func (m *MessagesMode) OnActivate() tea.Cmd                     { return nil }
func (m *MessagesMode) OnDeactivate()                           {}
func (m *MessagesMode) HandleTowerChanged(TowerChanged) tea.Cmd { return nil }
func (m *MessagesMode) HasOverlay() bool                        { return false }

func (m *MessagesMode) View() string {
	msg := "Messages mode — coming soon"
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
