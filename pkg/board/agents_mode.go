package board

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/process"
)

// AgentSnapshot holds the latest fetched state of all registered agents.
type AgentSnapshot struct {
	Agents    []AgentInfo
	Error     string // non-empty if fetch failed
	FetchedAt time.Time
}

// AgentInfo holds display information for a single agent.
type AgentInfo struct {
	Name      string
	BeadID    string // empty if idle
	Phase     string
	Status    string // "running", "idle", "errored"
	StartedAt time.Time
	Duration  time.Duration
}

// agentTickMsg is sent by the tick command to trigger a registry re-read.
type agentTickMsg struct{}

// AgentsMode implements Mode for the Agents tab.
type AgentsMode struct {
	snapshot     AgentSnapshot
	width        int
	height       int
	cursor       int    // selected row
	towerName    string
	registryPath string // filesystem path to agent registry (unused — we use agent.LoadRegistry)
	active       bool   // whether this mode is the active tab
}

// NewAgentsMode creates an AgentsMode for the given tower.
func NewAgentsMode(towerName string) *AgentsMode {
	return &AgentsMode{
		towerName: towerName,
	}
}

// ID returns the mode identifier.
func (m *AgentsMode) ID() ModeID { return ModeAgents }

// Init returns the initial fetch command and schedules the first tick.
func (m *AgentsMode) Init() tea.Cmd {
	return tea.Batch(m.fetchAgents(), scheduleAgentTick())
}

// Update handles messages for the agents mode.
func (m *AgentsMode) Update(msg tea.Msg) (Mode, tea.Cmd) {
	switch msg := msg.(type) {
	case AgentSnapshot:
		m.snapshot = msg
		if m.cursor >= len(msg.Agents) {
			m.cursor = max(0, len(msg.Agents)-1)
		}
		return m, nil

	case agentTickMsg:
		return m, tea.Batch(m.fetchAgents(), scheduleAgentTick())

	case tea.KeyMsg:
		switch msg.String() {
		case "j", "down":
			if len(m.snapshot.Agents) > 0 {
				m.cursor = (m.cursor + 1) % len(m.snapshot.Agents)
			}
		case "k", "up":
			if len(m.snapshot.Agents) > 0 {
				m.cursor = (m.cursor - 1 + len(m.snapshot.Agents)) % len(m.snapshot.Agents)
			}
		}
		return m, nil
	}
	return m, nil
}

// View renders the agents table from the latest snapshot. No I/O.
func (m *AgentsMode) View() string {
	var s strings.Builder

	headerStyle := lipgloss.NewStyle().Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	warnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	cursorIcon := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")

	// Error banner.
	if m.snapshot.Error != "" {
		s.WriteString(warnStyle.Render("WARNING: "+m.snapshot.Error) + "\n\n")
	}

	// No agents.
	if len(m.snapshot.Agents) == 0 {
		empty := "No agents registered. Use 'spire register <name>' to register."
		if m.width > 0 {
			pad := (m.width - len(empty)) / 2
			if pad > 0 {
				empty = strings.Repeat(" ", pad) + empty
			}
		}
		s.WriteString(empty + "\n")
		return s.String()
	}

	// Compute column widths from data.
	colAgent, colBead, colPhase, colStatus, colDur := 5, 4, 5, 6, 8
	for _, a := range m.snapshot.Agents {
		if len(a.Name) > colAgent {
			colAgent = len(a.Name)
		}
		if len(a.BeadID) > colBead {
			colBead = len(a.BeadID)
		}
		if len(a.Phase) > colPhase {
			colPhase = len(a.Phase)
		}
		if len(a.Status) > colStatus {
			colStatus = len(a.Status)
		}
	}
	// Clamp widths if terminal is narrow.
	if m.width > 0 {
		total := colAgent + colBead + colPhase + colStatus + colDur + 12 // gaps
		if total > m.width {
			excess := total - m.width
			if colAgent > 12 {
				shrink := min(excess, colAgent-12)
				colAgent -= shrink
				excess -= shrink
			}
			if excess > 0 && colBead > 8 {
				shrink := min(excess, colBead-8)
				colBead -= shrink
			}
		}
	}

	// Header.
	hdr := fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %-*s",
		colAgent, "Agent", colBead, "Bead", colPhase, "Phase", colStatus, "Status", colDur, "Duration")
	s.WriteString(headerStyle.Render(hdr) + "\n")

	sepWidth := colAgent + colBead + colPhase + colStatus + colDur + 10
	if m.width > 0 && sepWidth > m.width {
		sepWidth = m.width
	}
	s.WriteString(dimStyle.Render("  "+strings.Repeat("─", sepWidth)) + "\n")

	// Rows.
	for i, a := range m.snapshot.Agents {
		prefix := "  "
		nameStr := Truncate(a.Name, colAgent)
		beadStr := Truncate(a.BeadID, colBead)
		if beadStr == "" {
			beadStr = dimStyle.Render("—")
			// Pad to match column width.
			if colBead > 1 {
				beadStr += strings.Repeat(" ", colBead-1)
			}
		} else {
			beadStr = fmt.Sprintf("%-*s", colBead, beadStr)
		}
		phaseStr := Truncate(a.Phase, colPhase)
		if phaseStr == "" {
			phaseStr = dimStyle.Render("—")
			if colPhase > 1 {
				phaseStr += strings.Repeat(" ", colPhase-1)
			}
		} else {
			phaseStr = fmt.Sprintf("%-*s", colPhase, phaseStr)
		}

		durStr := "—"
		if a.Duration > 0 {
			durStr = formatAgentDuration(a.Duration)
		}

		statusStr := renderAgentStatus(a.Status, colStatus)

		row := fmt.Sprintf("%-*s  %s  %s  %s  %-*s",
			colAgent, nameStr, beadStr, phaseStr, statusStr, colDur, durStr)

		if i == m.cursor {
			prefix = cursorIcon + " "
			row = selectedStyle.Render(row)
		}

		s.WriteString(prefix + row + "\n")
	}

	return s.String()
}

// SetSize stores the available dimensions.
func (m *AgentsMode) SetSize(w, h int) {
	m.width = w
	m.height = h
}

// OnActivate sets active=true and triggers an immediate re-fetch.
func (m *AgentsMode) OnActivate() tea.Cmd {
	m.active = true
	return m.fetchAgents()
}

// OnDeactivate sets active=false.
func (m *AgentsMode) OnDeactivate() {
	m.active = false
}

// HandleTowerChanged updates the tower and triggers a re-fetch.
func (m *AgentsMode) HandleTowerChanged(tc TowerChanged) tea.Cmd {
	m.towerName = tc.Name
	return m.fetchAgents()
}

// HasOverlay returns false — no overlays in v1.
func (m *AgentsMode) HasOverlay() bool { return false }

// fetchAgents returns a tea.Cmd that reads the agent registry and produces an AgentSnapshot.
func (m *AgentsMode) fetchAgents() tea.Cmd {
	tower := m.towerName
	return func() tea.Msg {
		reg := agent.LoadRegistry()
		entries := agent.WizardsForTower(reg, tower)

		now := time.Now()
		agents := make([]AgentInfo, 0, len(entries))
		for _, e := range entries {
			info := AgentInfo{
				Name:   e.Name,
				BeadID: e.BeadID,
				Phase:  e.Phase,
			}

			// Determine status from PID liveness.
			alive := e.PID > 0 && process.ProcessAlive(e.PID)
			if alive {
				if e.BeadID != "" {
					info.Status = "running"
				} else {
					info.Status = "idle"
				}
			} else {
				info.Status = "idle"
			}

			// Parse start time for duration.
			if e.PhaseStartedAt != "" {
				if t, err := time.Parse(time.RFC3339, e.PhaseStartedAt); err == nil {
					info.StartedAt = t
					info.Duration = now.Sub(t).Round(time.Second)
				}
			} else if e.StartedAt != "" {
				if t, err := time.Parse(time.RFC3339, e.StartedAt); err == nil {
					info.StartedAt = t
					info.Duration = now.Sub(t).Round(time.Second)
				}
			}

			agents = append(agents, info)
		}

		return AgentSnapshot{
			Agents:    agents,
			FetchedAt: now,
		}
	}
}

// scheduleAgentTick returns a tea.Cmd that fires an agentTickMsg after 2 seconds.
func scheduleAgentTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return agentTickMsg{}
	})
}

// renderAgentStatus renders the status string with color.
func renderAgentStatus(status string, width int) string {
	var style lipgloss.Style
	switch status {
	case "running":
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // cyan
	case "idle":
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	case "errored":
		style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")) // red
	default:
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // dim
	}
	return style.Render(fmt.Sprintf("%-*s", width, status))
}

// formatAgentDuration formats a duration as compact string (e.g. "12m", "1h3m").
func formatAgentDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm", m)
	}
	s := int(d.Seconds())
	return fmt.Sprintf("%ds", s)
}

