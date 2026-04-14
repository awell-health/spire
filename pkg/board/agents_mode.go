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
	Status    string // "running", "idle", "errored"
	StartedAt time.Time
	Duration  time.Duration
}

// agentTickMsg is sent by the tick command to trigger a registry re-read.
type agentTickMsg struct{}

// agentActionResultMsg carries the result of an inline action in agents mode.
type agentActionResultMsg struct {
	Action PendingAction
	BeadID string
	Err    error
}

// AgentsMode implements Mode for the Agents tab.
type AgentsMode struct {
	snapshot     AgentSnapshot
	width        int
	height       int
	cursor    int    // selected row
	towerName string

	// InlineActionFn executes an action within the TUI via tea.Cmd.
	InlineActionFn func(PendingAction, string) error

	// Action menu overlay state.
	ActionMenuOpen    bool
	ActionMenuItems   []MenuAction
	ActionMenuCursor  int
	ActionMenuBeadID  string
	ActionMenuBeadTitle string

	// Confirmation dialog state.
	ConfirmOpen   bool
	ConfirmAction PendingAction
	ConfirmBeadID string
	ConfirmPrompt string
	ConfirmDanger DangerLevel

	// Inline action execution state.
	ActionRunning    bool
	ActionStatus     string
	ActionStatusTime time.Time
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

// selectedAgent returns the currently selected agent, or nil if none.
func (m *AgentsMode) selectedAgent() *AgentInfo {
	if m.cursor >= 0 && m.cursor < len(m.snapshot.Agents) {
		return &m.snapshot.Agents[m.cursor]
	}
	return nil
}

// dispatchInlineAction dispatches an inline action via tea.Cmd.
func (m *AgentsMode) dispatchInlineAction(action PendingAction, beadID string) (Mode, tea.Cmd) {
	if m.ActionRunning {
		return m, nil
	}
	if m.InlineActionFn == nil {
		// No inline fn — emit PendingActionMsg for exit-relaunch.
		return m, func() tea.Msg {
			return PendingActionMsg{Action: actionLabel(action), Args: []string{beadID}}
		}
	}
	m.ActionRunning = true
	m.ActionStatus = actionLabel(action) + "..."
	m.ActionStatusTime = time.Now()
	fn := m.InlineActionFn
	return m, func() tea.Msg {
		err := fn(action, beadID)
		return agentActionResultMsg{Action: action, BeadID: beadID, Err: err}
	}
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
		// Auto-clear action status after 5 seconds.
		if m.ActionStatus != "" && time.Since(m.ActionStatusTime) > 5*time.Second {
			m.ActionStatus = ""
		}
		return m, tea.Batch(m.fetchAgents(), scheduleAgentTick())

	case agentActionResultMsg:
		m.ActionRunning = false
		if msg.Err != nil {
			m.ActionStatus = fmt.Sprintf("%s failed: %v", actionLabel(msg.Action), msg.Err)
		} else {
			m.ActionStatus = fmt.Sprintf("%s: done", actionLabel(msg.Action))
		}
		m.ActionStatusTime = time.Now()
		// Refresh agents after action.
		return m, m.fetchAgents()

	case tea.KeyMsg:
		// Confirmation dialog absorbs all keys.
		if m.ConfirmOpen {
			return m.updateConfirm(msg)
		}

		// Action menu absorbs all keys.
		if m.ActionMenuOpen {
			return m.updateActionMenu(msg)
		}

		switch msg.String() {
		case "j", "down":
			if len(m.snapshot.Agents) > 0 {
				m.cursor = (m.cursor + 1) % len(m.snapshot.Agents)
			}
		case "k", "up":
			if len(m.snapshot.Agents) > 0 {
				m.cursor = (m.cursor - 1 + len(m.snapshot.Agents)) % len(m.snapshot.Agents)
			}

		// Unsummon — confirm first.
		case "u":
			if a := m.selectedAgent(); a != nil && a.BeadID != "" && a.Status == "running" {
				m.ConfirmOpen = true
				m.ConfirmAction = ActionUnsummon
				m.ConfirmBeadID = a.BeadID
				m.ConfirmPrompt = confirmPromptForAction(ActionUnsummon, a.BeadID, a.Name)
				m.ConfirmDanger = DangerConfirm
				return m, nil
			}

		// Reset — confirm first.
		case "r":
			if a := m.selectedAgent(); a != nil && a.BeadID != "" {
				m.ConfirmOpen = true
				m.ConfirmAction = ActionResetSoft
				m.ConfirmBeadID = a.BeadID
				m.ConfirmPrompt = confirmPromptForAction(ActionResetSoft, a.BeadID, a.Name)
				m.ConfirmDanger = DangerConfirm
				return m, nil
			}

		// Close — confirm first.
		case "x":
			if a := m.selectedAgent(); a != nil && a.BeadID != "" {
				m.ConfirmOpen = true
				m.ConfirmAction = ActionClose
				m.ConfirmBeadID = a.BeadID
				m.ConfirmPrompt = confirmPromptForAction(ActionClose, a.BeadID, a.Name)
				m.ConfirmDanger = DangerConfirm
				return m, nil
			}

		// Action menu.
		case "a":
			if a := m.selectedAgent(); a != nil && a.BeadID != "" {
				m.ActionMenuBeadID = a.BeadID
				m.ActionMenuBeadTitle = a.Name
				m.ActionMenuItems = BuildAgentActionMenu(*a)
				m.ActionMenuCursor = 0
				m.ActionMenuOpen = true
				return m, nil
			}

		// Enter — inspect (emit PendingAction for focus).
		case "enter":
			if a := m.selectedAgent(); a != nil && a.BeadID != "" {
				return m, func() tea.Msg {
					return PendingActionMsg{Action: "focus", Args: []string{a.BeadID}}
				}
			}

		// Copy bead ID to clipboard.
		case "y":
			if a := m.selectedAgent(); a != nil && a.BeadID != "" {
				if err := copyToClipboard(a.BeadID); err != nil {
					m.ActionStatus = fmt.Sprintf("clipboard error: %v", err)
				} else {
					m.ActionStatus = fmt.Sprintf("copied: %s", a.BeadID)
				}
				m.ActionStatusTime = time.Now()
			}
			return m, nil
		}
		return m, nil
	}
	return m, nil
}

// updateConfirm handles key input in the confirmation dialog.
func (m *AgentsMode) updateConfirm(msg tea.KeyMsg) (Mode, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		action := m.ConfirmAction
		beadID := m.ConfirmBeadID
		m.ConfirmOpen = false
		if m.ActionRunning {
			return m, nil
		}
		return m.dispatchInlineAction(action, beadID)
	case "n", "N", "esc":
		m.ConfirmOpen = false
		return m, nil
	}
	return m, nil
}

// updateActionMenu handles key input in the action menu overlay.
func (m *AgentsMode) updateActionMenu(msg tea.KeyMsg) (Mode, tea.Cmd) {
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
			return m.dispatchAgentMenuAction(item)
		}
		return m, nil
	default:
		// Shortcut key match.
		for _, item := range m.ActionMenuItems {
			if msg.String() == string(item.Key) {
				m.ActionMenuOpen = false
				return m.dispatchAgentMenuAction(item)
			}
		}
		return m, nil
	}
}

// dispatchAgentMenuAction handles an action selected from the agent action menu.
func (m *AgentsMode) dispatchAgentMenuAction(item MenuAction) (Mode, tea.Cmd) {
	if item.ActionType == ActionTrace || item.ActionType == ActionGrok {
		// These use exit-relaunch pattern.
		return m, func() tea.Msg {
			return PendingActionMsg{Action: actionLabel(item.ActionType), Args: []string{m.ActionMenuBeadID}}
		}
	}
	if isInlineAction(item.ActionType) {
		if item.Danger != DangerNone {
			m.ConfirmOpen = true
			m.ConfirmAction = item.ActionType
			m.ConfirmBeadID = m.ActionMenuBeadID
			m.ConfirmPrompt = confirmPromptForAction(item.ActionType, m.ActionMenuBeadID, m.ActionMenuBeadTitle)
			m.ConfirmDanger = item.Danger
			return m, nil
		}
		return m.dispatchInlineAction(item.ActionType, m.ActionMenuBeadID)
	}
	// Non-inline: exit-relaunch.
	return m, func() tea.Msg {
		return PendingActionMsg{Action: actionLabel(item.ActionType), Args: []string{m.ActionMenuBeadID}}
	}
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
	colAgent, colBead, colStatus, colDur := 5, 4, 6, 8
	for _, a := range m.snapshot.Agents {
		if len(a.Name) > colAgent {
			colAgent = len(a.Name)
		}
		if len(a.BeadID) > colBead {
			colBead = len(a.BeadID)
		}
		if len(a.Status) > colStatus {
			colStatus = len(a.Status)
		}
	}
	// Clamp widths if terminal is narrow.
	if m.width > 0 {
		total := colAgent + colBead + colStatus + colDur + 10 // gaps
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
	hdr := fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s",
		colAgent, "Agent", colBead, "Bead", colStatus, "Status", colDur, "Duration")
	s.WriteString(headerStyle.Render(hdr) + "\n")

	sepWidth := colAgent + colBead + colStatus + colDur + 8
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
		durStr := "—"
		if a.Duration > 0 {
			durStr = formatAgentDuration(a.Duration)
		}

		statusStr := renderAgentStatus(a.Status, colStatus)

		row := fmt.Sprintf("%-*s  %s  %s  %-*s",
			colAgent, nameStr, beadStr, statusStr, colDur, durStr)

		if i == m.cursor {
			prefix = cursorIcon + " "
			row = selectedStyle.Render(row)
		}

		s.WriteString(prefix + row + "\n")
	}

	// Action status line.
	if m.ActionStatus != "" {
		statusStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
		s.WriteString("\n" + statusStyle.Render(m.ActionStatus) + "\n")
	}

	// Confirmation overlay.
	if m.ConfirmOpen {
		s.WriteString("\n")
		promptStyle := lipgloss.NewStyle().Bold(true)
		if m.ConfirmDanger == DangerDestructive {
			promptStyle = promptStyle.Foreground(lipgloss.Color("1"))
		} else {
			promptStyle = promptStyle.Foreground(lipgloss.Color("3"))
		}
		s.WriteString(promptStyle.Render(m.ConfirmPrompt+" [y/n]") + "\n")
	}

	// Action menu overlay.
	if m.ActionMenuOpen && len(m.ActionMenuItems) > 0 {
		s.WriteString("\n")
		menuWidth := m.width
		if menuWidth <= 0 {
			menuWidth = 40
		}
		s.WriteString(renderActionMenu(m.ActionMenuItems, m.ActionMenuCursor, m.ActionMenuBeadID, menuWidth))
		s.WriteString("\n")
	}

	return s.String()
}

// SetSize stores the available dimensions.
func (m *AgentsMode) SetSize(w, h int) {
	m.width = w
	m.height = h
}

// OnActivate triggers an immediate re-fetch and restarts the tick chain.
func (m *AgentsMode) OnActivate() tea.Cmd {
	return tea.Batch(m.fetchAgents(), scheduleAgentTick())
}

// OnDeactivate is a no-op; required by the Mode interface.
func (m *AgentsMode) OnDeactivate() {}

// HandleTowerChanged updates the tower and triggers a re-fetch.
func (m *AgentsMode) HandleTowerChanged(tc TowerChanged) tea.Cmd {
	m.towerName = tc.Name
	return m.fetchAgents()
}

// HasOverlay returns true when confirmation dialog or action menu is open.
func (m *AgentsMode) HasOverlay() bool {
	return m.ConfirmOpen || m.ActionMenuOpen
}

// FooterHints implements Mode. Returns context-sensitive keybinding hints.
func (m *AgentsMode) FooterHints() string {
	if m.ConfirmOpen {
		return "y confirm  n/esc cancel"
	}
	if m.ActionMenuOpen {
		return "j/k navigate  enter select  esc close"
	}
	if len(m.snapshot.Agents) == 0 {
		return ""
	}
	return "u unsummon  r reset  x close  a actions  enter inspect  y copy"
}

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
			if e.StartedAt != "" {
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
