package board

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Compile-time check: MetricsMode implements Mode.
var _ Mode = (*MetricsMode)(nil)

// MetricsMode is a stub Mode for the Metrics tab.
type MetricsMode struct {
	width, height int
}

// NewMetricsMode creates a new MetricsMode.
func NewMetricsMode() *MetricsMode {
	return &MetricsMode{}
}

func (m *MetricsMode) ID() ModeID                              { return ModeMetrics }
func (m *MetricsMode) Init() tea.Cmd                           { return nil }
func (m *MetricsMode) Update(tea.Msg) (Mode, tea.Cmd)          { return m, nil }
func (m *MetricsMode) SetSize(w, h int)                        { m.width, m.height = w, h }
func (m *MetricsMode) OnActivate() tea.Cmd                     { return nil }
func (m *MetricsMode) OnDeactivate()                           {}
func (m *MetricsMode) HandleTowerChanged(TowerChanged) tea.Cmd { return nil }
func (m *MetricsMode) HasOverlay() bool                        { return false }

func (m *MetricsMode) View() string {
	msg := "Metrics mode — coming soon"
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
