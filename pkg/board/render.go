package board

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// HeightBudgetResult holds the computed layout parameters for the board.
type HeightBudgetResult struct {
	MaxCards    int  // max cards to show per column
	Compact     bool // use 1-line compact cards instead of 4-line cards
	MaxWarnings int  // max warning lines to show (system-level, above alerts)
	MaxAlerts   int  // max alert lines to show
	MaxHooked   int  // max parked lines to show in the lower section
	MaxBlocked  int  // max blocked lines to show
	MaxAgents   int  // max agent lines to show in the agent panel (0 = hidden)
}

// showPipelineForStatus reports whether a bead's status warrants the
// compact step-pipeline indicator. Beads beyond "ready" but before
// "closed" all benefit from showing where they are in their formula.
func showPipelineForStatus(status string) bool {
	switch status {
	case "in_progress", "dispatched",
		"awaiting_review", "needs_changes", "awaiting_human", "merge_pending":
		return true
	}
	return false
}

// CalcHeightBudget computes card limits based on terminal height.
// Returns permissive defaults when termHeight is zero (non-TTY or unknown).
func CalcHeightBudget(termHeight, warningCount, alertCount, parkedCount, blockedCount, colCount, agentCount int) HeightBudgetResult {
	if termHeight <= 0 {
		maxAg := agentCount
		if maxAg > 5 {
			maxAg = 5
		}
		return HeightBudgetResult{MaxCards: 99, MaxWarnings: warningCount, MaxAlerts: alertCount, MaxHooked: parkedCount, MaxBlocked: 8, MaxAgents: maxAg}
	}

	const fixed = 7
	available := termHeight - fixed
	if available < 4 {
		available = 4
	}

	// Warnings get priority allocation — they explain missing data.
	maxWarnings := 0
	if warningCount > 0 {
		maxWarnings = warningCount // show all warnings (typically 1-2)
		available -= maxWarnings + 2
		if available < 4 {
			available = 4
		}
	}

	maxAlerts := 0
	if alertCount > 0 {
		maxAlerts = available / 5
		if maxAlerts < 1 {
			maxAlerts = 1
		}
		if maxAlerts > alertCount {
			maxAlerts = alertCount
		}
		available -= maxAlerts + 2
		if available < 4 {
			available = 4
		}
	}

	// BLOCKED and PARKED share the same vertical space (rendered side-by-side).
	// Allocate a single pool and deduct from available only once.
	maxParked := 0
	maxBlocked := 0
	if blockedCount > 0 || parkedCount > 0 {
		maxLower := available / 5
		if maxLower < 1 {
			maxLower = 1
		}
		if blockedCount > 0 {
			maxBlocked = maxLower
			if maxBlocked > blockedCount {
				maxBlocked = blockedCount
			}
		}
		if parkedCount > 0 {
			maxParked = maxLower
			if maxParked > parkedCount {
				maxParked = parkedCount
			}
		}
		// Deduct the taller of the two from available (they share vertical space).
		deduct := maxBlocked
		if maxParked > deduct {
			deduct = maxParked
		}
		available -= deduct + 2
		if available < 4 {
			available = 4
		}
	}

	maxAgents := 0
	if agentCount > 0 {
		maxAgents = available / 5
		if maxAgents < 1 {
			maxAgents = 1
		}
		cap := agentCount
		if cap > 5 {
			cap = 5
		}
		if maxAgents > cap {
			maxAgents = cap
		}
		available -= maxAgents + 1
		if available < 4 {
			available = 4
		}
	}

	maxCards := available / 4
	compact := false
	if maxCards < 2 {
		compact = true
		maxCards = available
		if maxCards < 1 {
			maxCards = 1
		}
	}

	return HeightBudgetResult{
		MaxCards:    maxCards,
		Compact:     compact,
		MaxWarnings: maxWarnings,
		MaxAlerts:   maxAlerts,
		MaxHooked:   maxParked,
		MaxBlocked:  maxBlocked,
		MaxAgents:   maxAgents,
	}
}

// RenderCompactCard renders a single bead as a one-line string for TUI columns.
func RenderCompactCard(b BoardBead, clr color.Color, width int, selected bool) string {
	titleWidth := width - 26
	if titleWidth < 15 {
		titleWidth = 15
	}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	isDeferred := b.Status == "deferred"
	owner := BeadOwnerLabel(b)
	ownerStr := ""
	if owner != "" {
		ownerStr = " " + lipgloss.NewStyle().Foreground(clr).Render(owner)
	}
	var line string
	if isDeferred {
		// Dim the entire line for deferred beads.
		titleWidth -= 11 // account for " [deferred]"
		if titleWidth < 10 {
			titleWidth = 10
		}
		line = fmt.Sprintf("%s %s %s %s%s %s %s",
			PriStr(b.Priority), dimStyle.Render(b.ID), dimStyle.Render(ShortType(b.Type)),
			dimStyle.Render(Truncate(b.Title, titleWidth)), ownerStr,
			dimStyle.Render(TimeAgo(b.UpdatedAt)),
			dimStyle.Render("[deferred]"))
	} else {
		line = fmt.Sprintf("%s %s %s %s%s %s",
			PriStr(b.Priority), b.ID, ShortType(b.Type),
			Truncate(b.Title, titleWidth), ownerStr,
			dimStyle.Render(TimeAgo(b.UpdatedAt)))
	}
	if selected {
		cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
		return cursor + " " + line + "\n"
	}
	return line + "\n"
}

// sidebarWidth is the fixed character width of the tab sidebar.
const sidebarWidth = 16

// renderTabSidebar renders a narrow vertical sidebar with tab labels.
// The active tab is bright white/bold; inactive tabs are dim gray.
func renderTabSidebar(mode ViewMode, alertCount, blockedCount, parkedCount int) string {
	type tabDef struct {
		label string
		mode  ViewMode
	}

	// Build a combined label for the lower tab showing separate counts.
	var lowerLabel string
	switch {
	case blockedCount > 0 && parkedCount > 0:
		lowerLabel = fmt.Sprintf("BLK(%d) PRK(%d)", blockedCount, parkedCount)
	case parkedCount > 0:
		lowerLabel = fmt.Sprintf("PARKED (%d)", parkedCount)
	case blockedCount > 0:
		lowerLabel = fmt.Sprintf("BLOCKED (%d)", blockedCount)
	default:
		lowerLabel = "BLOCKED"
	}

	tabs := []tabDef{
		{label: "ALERTS", mode: ViewAlerts},
		{label: "BOARD", mode: ViewBoard},
		{label: lowerLabel, mode: ViewLower},
	}

	activeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	inactiveStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	var lines []string
	for _, tab := range tabs {
		label := tab.label
		if tab.label == "ALERTS" && alertCount > 0 {
			label = fmt.Sprintf("ALERTS (%d)", alertCount)
		}
		if tab.mode == mode {
			lines = append(lines, activeStyle.Render(" ▸ "+label))
		} else {
			lines = append(lines, inactiveStyle.Render("   "+label))
		}
	}

	sidebar := strings.Join(lines, "\n")
	return lipgloss.NewStyle().Width(sidebarWidth).Render(sidebar)
}

// View implements Mode for the board TUI.
// This is a pure function — ZERO store/DB calls. All data comes from m.Snapshot.
func (m *BoardMode) View() string {
	if m.Quitting {
		return ""
	}

	// Show loading screen until first snapshot arrives.
	if m.Snapshot == nil {
		return "Loading..."
	}

	if m.Inspecting {
		b := BoardBead{}
		var dag *DAGProgress
		if m.InspectorData != nil {
			b = m.InspectorData.Bead
			dag = m.InspectorData.DAG
		} else if bead := m.SelectedBead(); bead != nil {
			b = *bead
			dag = m.Snapshot.DAGProgress[bead.ID]
		}
		inspectorHeight := m.Height
		if m.FeedbackActive {
			inspectorHeight -= 1 // reserve one line for feedback input bar
		}
		if m.ResolveActive {
			inspectorHeight -= 1 // reserve one line for resolve input bar
		}
		result := renderInspectorSnap(b, m.InspectorData, dag, m.Width, inspectorHeight, m.InspectorScroll, m.InspectorTab, m.InspectorLogIdx, m.InspectorLogMode, m.InspectorErrorsOnly, m.InspectorExpandAll)
		if m.FeedbackActive {
			result += "\n" + RenderFeedbackInput(m.FeedbackInput, m.Width)
		}
		if m.ResolveActive {
			result += "\n" + RenderResolveInput(m.ResolveInput, m.Width)
		}
		return result
	}

	visibleCols := m.VisibleCols()
	displayCols := m.DisplayColumns()

	// Full width available for board content (chrome rendered by RootModel).
	contentWidth := m.Width
	if contentWidth < 40 {
		contentWidth = 40
	}

	colWidth := 30
	if contentWidth > 0 && len(displayCols) > 0 {
		available := contentWidth - (len(displayCols)-1)*2
		cw := available / len(displayCols)
		if cw > 50 {
			cw = 50
		}
		if cw > 20 {
			colWidth = cw
		}
	}

	var s strings.Builder

	// Search bar.
	if searchBar := renderSearchBar(m.SearchQuery, m.SearchActive, m.Width); searchBar != "" {
		s.WriteString(searchBar + "\n")
	}

	// System warnings (dolt conflicts, etc.) — above alerts, red/bold.
	warningCount := len(m.Snapshot.Warnings)
	if warningCount > 0 {
		warnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(warnStyle.Render(fmt.Sprintf("⚠ SYSTEM WARNINGS (%d)", warningCount)) + "\n")
		for i, w := range m.Snapshot.Warnings {
			if i >= warningCount {
				break
			}
			s.WriteString(warnStyle.Render("  "+w) + "\n")
		}
		s.WriteString("\n")
	}

	alertCount := len(visibleCols.Alerts)

	// Compute height budget based on active view mode.
	var budget HeightBudgetResult
	switch m.ViewMode {
	case ViewBoard:
		budget = CalcHeightBudget(m.Height, warningCount, 0, 0, 0, len(displayCols), len(m.Agents))
	case ViewAlerts:
		budget = CalcHeightBudget(m.Height, warningCount, alertCount, 0, 0, 0, len(m.Agents))
	case ViewLower:
		budget = CalcHeightBudget(m.Height, warningCount, 0, len(visibleCols.ParkedBeads()), len(visibleCols.Blocked), 0, len(m.Agents))
	}

	// Render active view mode content.
	var mainContent strings.Builder
	switch m.ViewMode {
	case ViewAlerts:
		// Alerts fullscreen.
		if alertCount > 0 {
			SortBeads(visibleCols.Alerts)
			alertStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
			alertHeaderStr := fmt.Sprintf("⚠ ALERTS (%d)", alertCount)
			if m.SelSection == SectionAlerts {
				alertStyle = alertStyle.Underline(true)
			}
			mainContent.WriteString(alertStyle.Render(alertHeaderStr) + "\n")
			maxAlerts := budget.MaxAlerts
			if maxAlerts < alertCount {
				// In fullscreen mode, show more: use remaining height.
				maxAlerts = budget.MaxAlerts
			}
			for i, a := range visibleCols.Alerts {
				if i >= maxAlerts {
					dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
					mainContent.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", alertCount-maxAlerts)) + "\n")
					break
				}
				alertType := ""
				for _, l := range a.Labels {
					if strings.HasPrefix(l, "alert:") {
						alertType = "[" + l[6:] + "] "
					}
				}
				if m.SelSection == SectionAlerts && i == m.SelCard {
					cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
					mainContent.WriteString(fmt.Sprintf("%s %s %s%s\n", cursor, PriStr(a.Priority), alertType, a.Title))
				} else {
					mainContent.WriteString(fmt.Sprintf("  %s %s%s\n", PriStr(a.Priority), alertType, a.Title))
				}
			}
		} else {
			dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			mainContent.WriteString(dimStyle.Render("No alerts") + "\n")
		}

	case ViewBoard:
		// Compact attention line — shows counts for parked beads + alerts on one line.
		parkedCount := len(visibleCols.ParkedBeads())
		if parkedCount > 0 || alertCount > 0 {
			var parts []string
			if parkedCount > 0 {
				parkedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
				parts = append(parts, parkedStyle.Render(fmt.Sprintf("⏸ %d", parkedCount)))
			}
			if alertCount > 0 {
				alertStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
				parts = append(parts, alertStyle.Render(fmt.Sprintf("⚑ %d", alertCount)))
			}
			dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			mainContent.WriteString(strings.Join(parts, dimStyle.Render(" │ ")) + dimStyle.Render(" — v to toggle view") + "\n\n")
		}

		// Phase columns.
		if len(displayCols) > 0 {
			rendered := make([]string, len(displayCols))
			for i, c := range displayCols {
				var cb strings.Builder
				headerStyle := lipgloss.NewStyle().Bold(true).Foreground(c.Color)
				if m.SelSection == SectionColumns && i == m.SelCol {
					headerStyle = headerStyle.Underline(true)
				}
				cb.WriteString(headerStyle.Render(fmt.Sprintf("%s (%d)", c.Name, len(c.Beads))))
				cb.WriteString("\n")
				sepStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				cb.WriteString(sepStyle.Render(strings.Repeat("─", Min(colWidth, len(c.Name)+4))))
				cb.WriteString("\n")

				scrollOff := 0
				if m.SelSection == SectionColumns && i == m.SelCol {
					scrollOff = m.ColScroll
				}
				if scrollOff > len(c.Beads) {
					scrollOff = len(c.Beads)
				}
				if scrollOff > 0 {
					dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
					cb.WriteString(dimStyle.Render(fmt.Sprintf("  ↑%d more", scrollOff)))
					cb.WriteString("\n")
				}
				visible := c.Beads[scrollOff:]
				for j, b := range visible {
					if j >= budget.MaxCards {
						remaining := len(c.Beads) - scrollOff - budget.MaxCards
						if remaining > 0 {
							dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
							cb.WriteString(dimStyle.Render(fmt.Sprintf("  ↓%d more", remaining)))
							cb.WriteString("\n")
						}
						break
					}
					isSelected := (m.SelSection == SectionColumns && i == m.SelCol && (scrollOff+j) == m.SelCard)
					if budget.Compact {
						cb.WriteString(RenderCompactCard(b, c.Color, colWidth, isSelected))
					} else {
						phase := m.Snapshot.PhaseMap[b.ID]
						dag := m.Snapshot.DAGProgress[b.ID]
						cb.WriteString(RenderCardStrSnap(b, phase, dag, c.Color, colWidth, isSelected))
					}
				}
				rendered[i] = cb.String()
			}
			mainContent.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, AddGaps(rendered, colWidth)...))
			mainContent.WriteString("\n")
		}

	case ViewLower:
		// Blocked + parked (awaiting_review/needs_changes/awaiting_human/merge_pending) fullscreen.
		parked := visibleCols.ParkedBeads()
		hasBlocked := len(visibleCols.Blocked) > 0
		hasParked := len(parked) > 0
		if hasBlocked || hasParked {
			selLower := m.SelSection == SectionLower

			lowerWidth := contentWidth
			if lowerWidth <= 0 {
				lowerWidth = 80
			}
			bothPresent := hasBlocked && hasParked
			subColWidth := lowerWidth
			if bothPresent {
				subColWidth = lowerWidth/2 - 2
			}
			if subColWidth < 20 {
				subColWidth = 20
			}

			var blockedStr string
			if hasBlocked {
				var bb strings.Builder
				blockedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
				if selLower && m.SelLowerCol == 0 {
					blockedStyle = blockedStyle.Underline(true)
				}
				bb.WriteString(blockedStyle.Render(fmt.Sprintf("BLOCKED (%d)", len(visibleCols.Blocked))) + "\n")
				for i, b := range visibleCols.Blocked {
					if i >= budget.MaxBlocked {
						dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
						bb.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(visibleCols.Blocked)-budget.MaxBlocked)) + "\n")
						break
					}
					blockers := BlockingDepIDs(b)
					blockerStr := ""
					if len(blockers) > 0 {
						bStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
						blockerStr = " " + bStyle.Render("← "+strings.Join(blockers, ", "))
					}
					if selLower && m.SelLowerCol == 0 && i == m.SelCard {
						cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
						bb.WriteString(fmt.Sprintf("%s %s %s %s%s\n", cursor, PriStr(b.Priority), b.ID, Truncate(b.Title, 40), blockerStr))
					} else {
						bb.WriteString(fmt.Sprintf("  %s %s %s%s\n", PriStr(b.Priority), b.ID, Truncate(b.Title, 40), blockerStr))
					}
				}
				blockedStr = bb.String()
			}

			var parkedStr string
			if hasParked {
				SortBeads(parked)
				var hb strings.Builder
				parkedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
				if selLower && m.SelLowerCol == 1 {
					parkedStyle = parkedStyle.Underline(true)
				}
				hb.WriteString(parkedStyle.Render(fmt.Sprintf("⏸ PARKED (%d)", len(parked))) + "\n")
				for i, b := range parked {
					if i >= budget.MaxHooked {
						dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
						hb.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(parked)-budget.MaxHooked)) + "\n")
						break
					}
					// Show which step is parked from step:<name> labels on child step beads.
					parkedStep := ""
					for _, l := range b.Labels {
						if strings.HasPrefix(l, "hooked-step:") {
							parkedStep = "[" + l[len("hooked-step:"):] + "] "
							break
						}
					}
					if selLower && m.SelLowerCol == 1 && i == m.SelCard {
						cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
						hb.WriteString(fmt.Sprintf("%s %s %s%s: %s\n", cursor, PriStr(b.Priority), parkedStep, b.ID, Truncate(b.Title, 50)))
					} else {
						hb.WriteString(fmt.Sprintf("  %s %s%s: %s\n", PriStr(b.Priority), parkedStep, b.ID, Truncate(b.Title, 50)))
					}
					if m.Snapshot != nil && m.Snapshot.RecoveryRefs != nil {
						if ref := m.Snapshot.RecoveryRefs[b.ID]; ref != nil {
							recStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
							hb.WriteString(recStyle.Render(fmt.Sprintf("    recovery → %s", ref.ID)) + "\n")
						}
					}
				}
				parkedStr = hb.String()
			}

			if bothPresent {
				leftStyle := lipgloss.NewStyle().Width(subColWidth + 2)
				mainContent.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftStyle.Render(blockedStr), parkedStr))
				mainContent.WriteString("\n")
			} else if hasBlocked {
				mainContent.WriteString(blockedStr)
			} else {
				mainContent.WriteString(parkedStr)
			}
		} else {
			dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			mainContent.WriteString(dimStyle.Render("No blocked or parked beads") + "\n")
		}
	}

	// Write main content directly (chrome rendered by RootModel).
	s.WriteString(mainContent.String())
	s.WriteString("\n")

	// Agent panel (capped by budget).
	if budget.MaxAgents > 0 && len(m.Agents) > 0 {
		s.WriteString(RenderAgentPanelSnap(m.Agents, m.Snapshot.DAGProgress, budget.MaxAgents))
	}

	// Command line / action status (kept in mode, not chrome).
	if m.Cmdline.Active {
		s.WriteString(RenderCmdline(m.Cmdline, m.Width))
		s.WriteString("\n")
	} else if m.ActionStatus != "" {
		statusStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
		s.WriteString(statusStyle.Render(m.ActionStatus))
		s.WriteString("\n")
	}

	boardOutput := s.String()

	// Comment modal overlay: multiline textarea centered above the board.
	if m.CommentActive {
		popup := renderCommentModal(m)
		return overlayPopup(boardOutput, popup, m.Width, m.Height)
	}

	// Terminal pane overlay: large scrollable content viewer.
	if m.TermOpen {
		popW := m.Width * 9 / 10
		popH := m.Height * 85 / 100
		if popW < 80 {
			popW = 80
		}
		if popH < 24 {
			popH = 24
		}
		if popW > m.Width {
			popW = m.Width
		}
		if popH > m.Height {
			popH = m.Height
		}
		popup := renderTerminalPane(m, popW, popH)
		return overlayPopup(boardOutput, popup, m.Width, m.Height)
	}

	// Action menu overlay: composite popup OVER the board (not replacing it).
	if m.ActionMenuOpen {
		popup := renderActionMenu(m.ActionMenuItems, m.ActionMenuCursor, m.ActionMenuBeadID, 35)
		return overlayPopup(boardOutput, popup, m.Width, m.Height)
	}

	// Tower switcher overlay.
	if m.TowerSwitcherOpen {
		popup := renderTowerSwitcher(m.TowerSwitcherItems, m.TowerSwitcherCursor, 35)
		return overlayPopup(boardOutput, popup, m.Width, m.Height)
	}

	// Confirmation dialog overlay.
	if m.ConfirmOpen {
		popup := renderConfirmPopup(m.ConfirmPrompt, m.ConfirmDanger)
		return overlayPopup(boardOutput, popup, m.Width, m.Height)
	}

	return boardOutput
}

// RenderAgentPanel renders a compact live agent status panel.
func RenderAgentPanel(agents []LocalAgent, maxAgents int) string {
	var s strings.Builder
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	phaseStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("4"))

	shown := len(agents)
	if shown > maxAgents {
		shown = maxAgents
	}
	s.WriteString(headerStyle.Render(fmt.Sprintf("AGENTS (%d)", len(agents))) + "\n")

	greenLG := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	cyanLG := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	yellowLG := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	dimLG := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	for i := 0; i < shown; i++ {
		w := agents[i]
		phase := "working"
		elapsed := ""
		if t, err := time.Parse(time.RFC3339, w.StartedAt); err == nil {
			d := time.Since(t).Round(time.Second)
			h := int(d.Hours())
			m := int(d.Minutes()) % 60
			sec := int(d.Seconds()) % 60
			if h > 0 {
				elapsed = fmt.Sprintf("%dh%02dm", h, m)
			} else if m > 0 {
				elapsed = fmt.Sprintf("%dm%02ds", m, sec)
			} else {
				elapsed = fmt.Sprintf("%ds", sec)
			}
		}
		name := w.Name
		if len(name) > 28 {
			name = name[:27] + "…"
		}
		beadPart := ""
		if w.BeadID != "" {
			beadPart = "  " + w.BeadID
		}

		// Show compact DAG pipeline if available.
		pipelineStr := ""
		if w.BeadID != "" {
			if dag := FetchDAGProgress(w.BeadID); dag != nil && len(dag.Steps) > 0 {
				var icons []string
				for _, step := range dag.Steps {
					switch step.Status {
					case "closed":
						icons = append(icons, greenLG.Render("✓"))
					case "in_progress":
						icons = append(icons, cyanLG.Render("▶"))
					case "dispatched":
						icons = append(icons, cyanLG.Render("⏳"))
					case "hooked":
						icons = append(icons, yellowLG.Render("⏸"))
					case "awaiting_review":
						icons = append(icons, yellowLG.Render("◆"))
					default:
						icons = append(icons, dimLG.Render("○"))
					}
				}
				pipelineStr = "  " + strings.Join(icons, " ")
			}
		}

		line := fmt.Sprintf("  %-28s%s  %s  %s%s",
			name,
			beadPart,
			phaseStyle.Render(phase),
			dimStyle.Render(elapsed),
			pipelineStr,
		)
		s.WriteString(line + "\n")
	}
	if len(agents) > maxAgents {
		s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(agents)-maxAgents)) + "\n")
	}
	return s.String()
}

// RenderCardStr renders a single bead card as a multi-line string for a column.
func RenderCardStr(b BoardBead, clr color.Color, width int, selected ...bool) string {
	titleWidth := width - 4
	if titleWidth < 10 {
		titleWidth = 10
	}

	typeStr := ShortType(b.Type)
	if phase := GetBoardBeadPhase(b); phase != "" {
		typeStr += " [" + phase + "]"
	}

	isSel := len(selected) > 0 && selected[0]
	var s strings.Builder
	if isSel {
		cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
		s.WriteString(fmt.Sprintf("%s %s %s %s\n", cursor, PriStr(b.Priority), b.ID, typeStr))
	} else {
		s.WriteString(fmt.Sprintf("%s %s %s\n", PriStr(b.Priority), b.ID, typeStr))
	}
	s.WriteString(fmt.Sprintf("  %s\n", Truncate(b.Title, titleWidth)))

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	colorStyle := lipgloss.NewStyle().Foreground(clr)

	owner := BeadOwnerLabel(b)
	if owner != "" {
		s.WriteString(fmt.Sprintf("  %s %s\n", colorStyle.Render(owner), dimStyle.Render(TimeAgo(b.UpdatedAt))))
	} else {
		s.WriteString(fmt.Sprintf("  %s\n", dimStyle.Render(TimeAgo(b.CreatedAt))))
	}

	// Show compact DAG pipeline for beads with active or parked execution
	// state — anything past "ready" but before "closed" benefits from the
	// step icons.
	if showPipelineForStatus(b.Status) {
		if pipeline := RenderPipelineLipgloss(b.ID); pipeline != "" {
			s.WriteString(fmt.Sprintf("  %s\n", pipeline))
		}
	}

	s.WriteString("\n")
	return s.String()
}

// RenderPipelineLipgloss renders a compact step pipeline using lipgloss styles.
// Returns "" if the bead has no step beads.
func RenderPipelineLipgloss(beadID string) string {
	dag := FetchDAGProgress(beadID)
	if dag == nil || len(dag.Steps) == 0 {
		return ""
	}

	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	cyanStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	dimLGStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	var parts []string
	for _, step := range dag.Steps {
		switch step.Status {
		case "closed":
			parts = append(parts, greenStyle.Render("✓"))
		case "in_progress":
			parts = append(parts, cyanStyle.Render("▶"))
		case "dispatched":
			parts = append(parts, cyanStyle.Render("⏳"))
		case "hooked":
			parts = append(parts, yellowStyle.Render("⏸"))
		case "awaiting_review":
			parts = append(parts, yellowStyle.Render("◆"))
		default:
			parts = append(parts, dimLGStyle.Render("○"))
		}
	}
	return strings.Join(parts, " ")
}

// AddGaps pads each column string to colWidth and adds 2-char gaps.
func AddGaps(columns []string, colWidth int) []string {
	style := lipgloss.NewStyle().Width(colWidth + 2)
	out := make([]string, len(columns))
	for i, c := range columns {
		out[i] = style.Render(c)
	}
	return out
}

// CountActiveCols returns the number of non-empty columns.
func CountActiveCols(cols Columns) int {
	return len(ActiveColumns(cols))
}

// RenderPipelineFromDAG renders a compact step pipeline from a pre-fetched DAGProgress.
// Same visual output as RenderPipelineLipgloss but without calling FetchDAGProgress.
// Returns "" if dag is nil or has no steps.
func RenderPipelineFromDAG(dag *DAGProgress) string {
	if dag == nil || len(dag.Steps) == 0 {
		return ""
	}

	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	cyanStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	dimLGStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	var parts []string
	for _, step := range dag.Steps {
		switch step.Status {
		case "closed":
			parts = append(parts, greenStyle.Render("✓"))
		case "in_progress":
			parts = append(parts, cyanStyle.Render("▶"))
		case "dispatched":
			parts = append(parts, cyanStyle.Render("⏳"))
		case "hooked":
			parts = append(parts, yellowStyle.Render("⏸"))
		case "awaiting_review":
			parts = append(parts, yellowStyle.Render("◆"))
		default:
			parts = append(parts, dimLGStyle.Render("○"))
		}
	}
	return strings.Join(parts, " ")
}

// RenderCardStrSnap renders a single bead card as a multi-line string for a column,
// using pre-fetched phase and DAG progress instead of calling the database.
// Same visual output as RenderCardStr.
func RenderCardStrSnap(b BoardBead, phase string, dag *DAGProgress, clr color.Color, width int, selected ...bool) string {
	titleWidth := width - 4
	if titleWidth < 10 {
		titleWidth = 10
	}

	isDeferred := b.Status == "deferred"
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	typeStr := ShortType(b.Type)
	if phase != "" {
		typeStr += " [" + phase + "]"
	}

	// Visual tags for special states.
	statusTag := ""
	if isDeferred {
		statusTag = " " + dimStyle.Render("[deferred]")
	} else if b.HasLabel("needs-human") {
		statusTag = " " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3")).Render("[needs-human]")
	}

	isSel := len(selected) > 0 && selected[0]
	var s strings.Builder
	if isSel {
		cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
		if isDeferred {
			s.WriteString(fmt.Sprintf("%s %s %s %s%s\n", cursor, PriStr(b.Priority), dimStyle.Render(b.ID), dimStyle.Render(typeStr), statusTag))
		} else {
			s.WriteString(fmt.Sprintf("%s %s %s %s%s\n", cursor, PriStr(b.Priority), b.ID, typeStr, statusTag))
		}
	} else {
		if isDeferred {
			s.WriteString(fmt.Sprintf("%s %s %s%s\n", PriStr(b.Priority), dimStyle.Render(b.ID), dimStyle.Render(typeStr), statusTag))
		} else {
			s.WriteString(fmt.Sprintf("%s %s %s%s\n", PriStr(b.Priority), b.ID, typeStr, statusTag))
		}
	}
	if isDeferred {
		s.WriteString(fmt.Sprintf("  %s\n", dimStyle.Render(Truncate(b.Title, titleWidth))))
	} else {
		s.WriteString(fmt.Sprintf("  %s\n", Truncate(b.Title, titleWidth)))
	}

	colorStyle := lipgloss.NewStyle().Foreground(clr)

	owner := BeadOwnerLabel(b)
	if owner != "" {
		s.WriteString(fmt.Sprintf("  %s %s\n", colorStyle.Render(owner), dimStyle.Render(TimeAgo(b.UpdatedAt))))
	} else {
		s.WriteString(fmt.Sprintf("  %s\n", dimStyle.Render(TimeAgo(b.CreatedAt))))
	}

	// Show compact DAG pipeline for beads with active or parked execution
	// state. Dispatched beads haven't been claimed yet, so their DAG is
	// usually empty — rendering still works and shows "○ ○ ○" to signal
	// the bead is scheduled but unstarted.
	if showPipelineForStatus(b.Status) {
		if pipeline := RenderPipelineFromDAG(dag); pipeline != "" {
			s.WriteString(fmt.Sprintf("  %s\n", pipeline))
		}
	}

	s.WriteString("\n")
	return s.String()
}

// RenderAgentPanelSnap renders a compact live agent status panel using pre-fetched
// DAG progress data instead of calling FetchDAGProgress per agent.
// Same visual output as RenderAgentPanel.
func RenderAgentPanelSnap(agents []LocalAgent, dagMap map[string]*DAGProgress, maxAgents int) string {
	var s strings.Builder
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	phaseStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("4"))

	shown := len(agents)
	if shown > maxAgents {
		shown = maxAgents
	}
	s.WriteString(headerStyle.Render(fmt.Sprintf("AGENTS (%d)", len(agents))) + "\n")

	greenLG := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	cyanLG := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	yellowLG := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	dimLG := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	for i := 0; i < shown; i++ {
		w := agents[i]
		phase := "working"
		elapsed := ""
		if t, err := time.Parse(time.RFC3339, w.StartedAt); err == nil {
			d := time.Since(t).Round(time.Second)
			h := int(d.Hours())
			m := int(d.Minutes()) % 60
			sec := int(d.Seconds()) % 60
			if h > 0 {
				elapsed = fmt.Sprintf("%dh%02dm", h, m)
			} else if m > 0 {
				elapsed = fmt.Sprintf("%dm%02ds", m, sec)
			} else {
				elapsed = fmt.Sprintf("%ds", sec)
			}
		}
		name := w.Name
		if len(name) > 28 {
			name = name[:27] + "…"
		}
		beadPart := ""
		if w.BeadID != "" {
			beadPart = "  " + w.BeadID
		}

		// Show compact DAG pipeline if available (from pre-fetched dagMap).
		pipelineStr := ""
		if w.BeadID != "" {
			if dag, ok := dagMap[w.BeadID]; ok && dag != nil && len(dag.Steps) > 0 {
				var icons []string
				for _, step := range dag.Steps {
					switch step.Status {
					case "closed":
						icons = append(icons, greenLG.Render("✓"))
					case "in_progress":
						icons = append(icons, cyanLG.Render("▶"))
					case "dispatched":
						icons = append(icons, cyanLG.Render("⏳"))
					case "hooked":
						icons = append(icons, yellowLG.Render("⏸"))
					case "awaiting_review":
						icons = append(icons, yellowLG.Render("◆"))
					default:
						icons = append(icons, dimLG.Render("○"))
					}
				}
				pipelineStr = "  " + strings.Join(icons, " ")
			}
		}

		line := fmt.Sprintf("  %-28s%s  %s  %s%s",
			name,
			beadPart,
			phaseStyle.Render(phase),
			dimStyle.Render(elapsed),
			pipelineStr,
		)
		s.WriteString(line + "\n")
	}
	if len(agents) > maxAgents {
		s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(agents)-maxAgents)) + "\n")
	}
	return s.String()
}

// renderConfirmPopup renders a small confirmation dialog box.
func renderConfirmPopup(prompt string, danger DangerLevel) string {
	borderColor := lipgloss.Color("6")
	promptStyle := lipgloss.NewStyle()
	if danger == DangerDestructive {
		borderColor = lipgloss.Color("1")
		promptStyle = promptStyle.Foreground(lipgloss.Color("1"))
	}

	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	content := promptStyle.Render(prompt) + "\n" + hintStyle.Render("[y/n]")

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1)

	return boxStyle.Render(content)
}

// overlayPopup composites a popup string over a background string, centering the
// popup. This is used for the action menu overlay so the board remains visible.
func overlayPopup(background, popup string, width, height int) string {
	bgLines := strings.Split(background, "\n")
	popupLines := strings.Split(popup, "\n")

	// Pad background to fill height.
	for len(bgLines) < height {
		bgLines = append(bgLines, "")
	}

	popupW := 0
	for _, pl := range popupLines {
		if w := lipgloss.Width(pl); w > popupW {
			popupW = w
		}
	}

	// Center the popup.
	startRow := (height - len(popupLines)) / 2
	startCol := (width - popupW) / 2
	if startRow < 0 {
		startRow = 0
	}
	if startCol < 0 {
		startCol = 0
	}

	for i, pl := range popupLines {
		row := startRow + i
		if row >= len(bgLines) {
			break
		}
		// Replace portion of the background line with popup line.
		bg := bgLines[row]
		// Use padding to position popup.
		padded := strings.Repeat(" ", startCol) + pl
		// If background is wider, keep the tail.
		if lipgloss.Width(padded) < lipgloss.Width(bg) {
			bgLines[row] = padded
		} else {
			bgLines[row] = padded
		}
	}

	return strings.Join(bgLines, "\n")
}

// PriStr returns a priority label with lipgloss styling (for TUI rendering).
func PriStr(p int) string {
	label := fmt.Sprintf("P%d", p)
	switch p {
	case 0, 1:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")).Render(label)
	case 2:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render(label)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(label)
	}
}
