package board

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"charm.land/lipgloss/v2"
)

// chromeHeight is the number of lines consumed by the tab bar (1) and footer (2).
const chromeHeight = 3

// RootOpts configures the RootModel.
type RootOpts struct {
	TowerName  string
	Identity   string
	BeadsDir   string
	Modes      []Mode
	TowerItems []TowerItem // available towers with beads dirs for the switcher
}

// RootModel is the top-level Bubble Tea model that owns shared state,
// renders chrome (tab bar + footer), and routes between modes.
type RootModel struct {
	modes         []Mode
	activeModeIdx int
	towerName     string
	identity      string
	beadsDir      string
	width, height int
	// tower switcher overlay state
	showTowerSwitcher bool
	towerItems        []TowerItem
	towerCursor       int
	// pending action for exit-relaunch
	pendingAction *PendingActionMsg
}

// NewRootModel creates a RootModel from the given options.
func NewRootModel(opts RootOpts) RootModel {
	return RootModel{
		modes:      opts.Modes,
		towerName:  opts.TowerName,
		identity:   opts.Identity,
		beadsDir:   opts.BeadsDir,
		towerItems: opts.TowerItems,
	}
}

// Init implements tea.Model. It calls Init() on all modes and batches
// the returned commands.
func (r RootModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	for _, m := range r.modes {
		cmd := m.Init()
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	// Activate the initially active mode.
	if len(r.modes) > 0 {
		if cmd := r.modes[r.activeModeIdx].OnActivate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// Update implements tea.Model.
func (r RootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return r.handleKey(msg)

	case tea.WindowSizeMsg:
		r.width = msg.Width
		r.height = msg.Height
		contentH := r.height - chromeHeight
		if contentH < 0 {
			contentH = 0
		}
		for _, m := range r.modes {
			m.SetSize(r.width, contentH)
		}
		return r, nil

	case PendingActionMsg:
		r.pendingAction = &msg
		return r, tea.Quit

	default:
		return r.routeMsg(msg)
	}
}

// routeMsg delivers a message to its owning mode based on type.
// Mode-specific tick/snapshot messages are routed to the correct mode so that
// background tick chains continue even when the mode is inactive.
// Unknown message types are forwarded to the active mode.
func (r RootModel) routeMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	target := -1
	switch msg.(type) {
	case tickMsg, snapshotMsg, actionResultMsg, inspectorDataMsg, termContentMsg, rejectDesignResultMsg:
		target = r.modeIndex(ModeBoard)
	case agentTickMsg, AgentSnapshot:
		target = r.modeIndex(ModeAgents)
	}
	if target >= 0 && target < len(r.modes) {
		updated, cmd := r.modes[target].Update(msg)
		r.modes[target] = updated
		return r, cmd
	}
	return r.updateActiveMode(msg)
}

// modeIndex returns the index of the mode with the given ID, or -1 if not found.
func (r RootModel) modeIndex(id ModeID) int {
	for i, m := range r.modes {
		if m.ID() == id {
			return i
		}
	}
	return -1
}

// handleKey processes all key messages with the priority order specified
// by the task: tower switcher > overlay passthrough > tab cycling > global keys > active mode.
func (r RootModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Tower switcher overlay absorbs all keys when open.
	if r.showTowerSwitcher {
		return r.handleTowerSwitcherKey(key)
	}

	// If the active mode has an overlay, pass ALL keys (including Tab) to it.
	if len(r.modes) > 0 && r.modes[r.activeModeIdx].HasOverlay() {
		return r.updateActiveMode(msg)
	}

	switch key {
	case "tab":
		return r.cycleMode(1)
	case "shift+tab":
		return r.cycleMode(-1)
	case "T":
		r.showTowerSwitcher = true
		r.towerCursor = 0
		return r, nil
	case "q", "ctrl+c":
		return r, tea.Quit
	default:
		return r.updateActiveMode(msg)
	}
}

// cycleMode advances activeModeIdx by delta (wrapping) and fires lifecycle hooks.
func (r RootModel) cycleMode(delta int) (tea.Model, tea.Cmd) {
	if len(r.modes) == 0 {
		return r, nil
	}
	old := r.activeModeIdx
	r.activeModeIdx = (r.activeModeIdx + delta + len(r.modes)) % len(r.modes)
	r.modes[old].OnDeactivate()
	cmd := r.modes[r.activeModeIdx].OnActivate()
	return r, cmd
}

// handleTowerSwitcherKey handles keys while the tower switcher overlay is visible.
func (r RootModel) handleTowerSwitcherKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "j", "down":
		if r.towerCursor < len(r.towerItems)-1 {
			r.towerCursor++
		}
		return r, nil
	case "k", "up":
		if r.towerCursor > 0 {
			r.towerCursor--
		}
		return r, nil
	case "enter":
		if r.towerCursor >= 0 && r.towerCursor < len(r.towerItems) {
			item := r.towerItems[r.towerCursor]
			r.towerName = item.Name
			r.beadsDir = item.BeadsDir
			r.showTowerSwitcher = false
			tc := TowerChanged{Name: item.Name, BeadsDir: item.BeadsDir}
			var cmds []tea.Cmd
			for _, m := range r.modes {
				if cmd := m.HandleTowerChanged(tc); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			return r, tea.Batch(cmds...)
		}
		r.showTowerSwitcher = false
		return r, nil
	case "esc":
		r.showTowerSwitcher = false
		return r, nil
	default:
		return r, nil
	}
}

// updateActiveMode passes a message to the currently active mode.
func (r RootModel) updateActiveMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	if len(r.modes) == 0 {
		return r, nil
	}
	updated, cmd := r.modes[r.activeModeIdx].Update(msg)
	r.modes[r.activeModeIdx] = updated
	return r, cmd
}

// View implements tea.Model.
func (r RootModel) View() string {
	var b strings.Builder

	// Tab bar (line 1).
	b.WriteString(r.renderTabBar())
	b.WriteString("\n")

	// Mode content (lines 2..N-2).
	if len(r.modes) > 0 {
		b.WriteString(r.modes[r.activeModeIdx].View())
	}
	b.WriteString("\n")

	// Footer (last 2 lines).
	b.WriteString(r.renderFooter())

	// Tower switcher overlay.
	if r.showTowerSwitcher {
		return r.overlayTowerSwitcher(b.String())
	}

	return b.String()
}

// PendingAction returns the pending action if the TUI quit due to one,
// or nil if the user quit normally.
func (r RootModel) PendingAction() *PendingActionMsg {
	return r.pendingAction
}

// renderTabBar builds the tab bar showing mode names with the active mode highlighted.
func (r RootModel) renderTabBar() string {
	activeStyle := lipgloss.NewStyle().Bold(true).Reverse(true)
	normalStyle := lipgloss.NewStyle()

	var parts []string
	for i, m := range r.modes {
		name := m.ID().String()
		if i == r.activeModeIdx {
			parts = append(parts, activeStyle.Render(fmt.Sprintf("[%s]", name)))
		} else {
			parts = append(parts, normalStyle.Render(name))
		}
	}
	return strings.Join(parts, "  ")
}

// renderFooter builds the 2-line status footer.
func (r RootModel) renderFooter() string {
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	line1 := dimStyle.Render(fmt.Sprintf(" %s  %s", r.towerName, r.identity))
	line2 := dimStyle.Render(" Tab: switch mode  T: tower  q: quit")
	return line1 + "\n" + line2
}

// overlayTowerSwitcher renders the tower switcher centered over the base view.
func (r RootModel) overlayTowerSwitcher(base string) string {
	// Mark the active tower for rendering.
	items := make([]TowerItem, len(r.towerItems))
	for i, item := range r.towerItems {
		items[i] = item
		items[i].Active = item.Name == r.towerName
	}
	overlay := renderTowerSwitcher(items, r.towerCursor, r.width)

	// Simple overlay: replace middle lines of base with the switcher box.
	baseLines := strings.Split(base, "\n")
	overlayLines := strings.Split(overlay, "\n")

	startY := (r.height - len(overlayLines)) / 2
	if startY < 0 {
		startY = 0
	}

	// Pad base lines to fill the screen height if needed.
	for len(baseLines) < r.height {
		baseLines = append(baseLines, "")
	}

	for i, ol := range overlayLines {
		row := startY + i
		if row >= 0 && row < len(baseLines) {
			// Center horizontally.
			pad := (r.width - lipgloss.Width(ol)) / 2
			if pad < 0 {
				pad = 0
			}
			baseLines[row] = strings.Repeat(" ", pad) + ol
		}
	}

	return strings.Join(baseLines, "\n")
}
