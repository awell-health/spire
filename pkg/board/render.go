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
	MaxCards       int  // max cards to show per column
	Compact        bool // use 1-line compact cards instead of 4-line cards
	MaxWarnings    int  // max warning lines to show (system-level, above alerts)
	MaxAlerts      int  // max alert lines to show
	MaxInterrupted int  // max interrupted lines to show
	MaxBlocked     int  // max blocked lines to show
	MaxAgents      int  // max agent lines to show in the agent panel (0 = hidden)
}

// CalcHeightBudget computes card limits based on terminal height.
// Returns permissive defaults when termHeight is zero (non-TTY or unknown).
func CalcHeightBudget(termHeight, warningCount, alertCount, interruptedCount, blockedCount, colCount, agentCount int) HeightBudgetResult {
	if termHeight <= 0 {
		maxAg := agentCount
		if maxAg > 5 {
			maxAg = 5
		}
		return HeightBudgetResult{MaxCards: 99, MaxWarnings: warningCount, MaxAlerts: alertCount, MaxInterrupted: interruptedCount, MaxBlocked: 8, MaxAgents: maxAg}
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

	// BLOCKED and INTERRUPTED share the same vertical space (rendered side-by-side).
	// Allocate a single pool and deduct from available only once.
	maxInterrupted := 0
	maxBlocked := 0
	if blockedCount > 0 || interruptedCount > 0 {
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
		if interruptedCount > 0 {
			maxInterrupted = maxLower
			if maxInterrupted > interruptedCount {
				maxInterrupted = interruptedCount
			}
		}
		// Deduct the taller of the two from available (they share vertical space).
		deduct := maxBlocked
		if maxInterrupted > deduct {
			deduct = maxInterrupted
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
		MaxCards:       maxCards,
		Compact:        compact,
		MaxWarnings:    maxWarnings,
		MaxAlerts:      maxAlerts,
		MaxInterrupted: maxInterrupted,
		MaxBlocked:     maxBlocked,
		MaxAgents:      maxAgents,
	}
}

// RenderCompactCard renders a single bead as a one-line string for TUI columns.
func RenderCompactCard(b BoardBead, clr color.Color, width int, selected bool) string {
	titleWidth := width - 26
	if titleWidth < 15 {
		titleWidth = 15
	}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	owner := BeadOwnerLabel(b)
	ownerStr := ""
	if owner != "" {
		ownerStr = " " + lipgloss.NewStyle().Foreground(clr).Render(owner)
	}
	line := fmt.Sprintf("%s %s %s %s%s %s",
		PriStr(b.Priority), b.ID, ShortType(b.Type),
		Truncate(b.Title, titleWidth), ownerStr,
		dimStyle.Render(TimeAgo(b.UpdatedAt)))
	if selected {
		cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
		return cursor + " " + line + "\n"
	}
	return line + "\n"
}

// View implements tea.Model for the board TUI.
// This is a pure function — ZERO store/DB calls. All data comes from m.Snapshot.
func (m Model) View() string {
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
		result := renderInspectorSnap(b, m.InspectorData, dag, m.Width, inspectorHeight, m.InspectorScroll, m.InspectorTab)
		if m.FeedbackActive {
			result += "\n" + RenderFeedbackInput(m.FeedbackInput, m.Width)
		}
		return result
	}

	visibleCols := m.VisibleCols()
	displayCols := m.DisplayColumns()
	colWidth := 30
	if m.Width > 0 && len(displayCols) > 0 {
		available := m.Width - (len(displayCols)-1)*2
		cw := available / len(displayCols)
		if cw > 50 {
			cw = 50
		}
		if cw > 20 {
			colWidth = cw
		}
	}

	var s strings.Builder

	budget := CalcHeightBudget(m.Height, len(m.Snapshot.Warnings), len(visibleCols.Alerts), len(visibleCols.Interrupted), len(visibleCols.Blocked), len(displayCols), len(m.Agents))

	// Header.
	headerTitle := "Spire Board"
	if m.Opts.TowerName != "" {
		headerTitle = "Spire Board \u2022 " + m.Opts.TowerName
	}
	header := lipgloss.NewStyle().Bold(true).Render(headerTitle)
	ts := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(m.LastTick.Format("15:04:05"))
	s.WriteString(header + "  " + ts + "\n\n")

	// Search bar.
	if searchBar := renderSearchBar(m.SearchQuery, m.SearchActive, m.Width); searchBar != "" {
		s.WriteString(searchBar + "\n")
	}

	// System warnings (dolt conflicts, etc.) — above alerts, red/bold.
	if len(m.Snapshot.Warnings) > 0 {
		warnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(warnStyle.Render(fmt.Sprintf("⚠ SYSTEM WARNINGS (%d)", len(m.Snapshot.Warnings))) + "\n")
		for i, w := range m.Snapshot.Warnings {
			if i >= budget.MaxWarnings {
				break
			}
			s.WriteString(warnStyle.Render("  "+w) + "\n")
		}
		s.WriteString("\n")
	}

	// Alerts (capped by budget).
	if len(visibleCols.Alerts) > 0 {
		SortBeads(visibleCols.Alerts)
		alertStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		alertHeaderStr := fmt.Sprintf("⚠ ALERTS (%d)", len(visibleCols.Alerts))
		if m.SelSection == SectionAlerts {
			alertHeaderStr = fmt.Sprintf("⚠ ALERTS (%d)", len(visibleCols.Alerts))
			alertStyle = alertStyle.Underline(true)
		}
		s.WriteString(alertStyle.Render(alertHeaderStr) + "\n")
		for i, a := range visibleCols.Alerts {
			if i >= budget.MaxAlerts {
				dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(visibleCols.Alerts)-budget.MaxAlerts)) + "\n")
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
				s.WriteString(fmt.Sprintf("%s %s %s%s\n", cursor, PriStr(a.Priority), alertType, a.Title))
			} else {
				s.WriteString(fmt.Sprintf("  %s %s%s\n", PriStr(a.Priority), alertType, a.Title))
			}
		}
		s.WriteString("\n")
	}

	// Build column content.
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

			// Apply scroll offset for the selected column only.
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

		s.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, AddGaps(rendered, colWidth)...))
		s.WriteString("\n")
	}

	// Lower area: BLOCKED and INTERRUPTED side-by-side.
	hasBlocked := len(visibleCols.Blocked) > 0
	hasInterrupted := len(visibleCols.Interrupted) > 0
	if hasBlocked || hasInterrupted {
		selLower := m.SelSection == SectionLower

		// Compute sub-column width.
		lowerWidth := m.Width
		if lowerWidth <= 0 {
			lowerWidth = 80
		}
		bothPresent := hasBlocked && hasInterrupted
		subColWidth := lowerWidth
		if bothPresent {
			subColWidth = lowerWidth/2 - 2
		}
		if subColWidth < 20 {
			subColWidth = 20
		}

		// Build BLOCKED column.
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

		// Build INTERRUPTED column.
		var interruptedStr string
		if hasInterrupted {
			SortBeads(visibleCols.Interrupted)
			var ib strings.Builder
			intStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
			if selLower && m.SelLowerCol == 1 {
				intStyle = intStyle.Underline(true)
			}
			ib.WriteString(intStyle.Render(fmt.Sprintf("⚠ INTERRUPTED (%d)", len(visibleCols.Interrupted))) + "\n")
			for i, b := range visibleCols.Interrupted {
				if i >= budget.MaxInterrupted {
					dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
					ib.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(visibleCols.Interrupted)-budget.MaxInterrupted)) + "\n")
					break
				}
				intType := ""
				for _, l := range b.Labels {
					if strings.HasPrefix(l, "interrupted:") {
						intType = "[" + l[len("interrupted:"):] + "] "
						break
					}
				}
				if selLower && m.SelLowerCol == 1 && i == m.SelCard {
					cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
					ib.WriteString(fmt.Sprintf("%s %s %s%s: %s\n", cursor, PriStr(b.Priority), intType, b.ID, Truncate(b.Title, 50)))
				} else {
					ib.WriteString(fmt.Sprintf("  %s %s%s: %s\n", PriStr(b.Priority), intType, b.ID, Truncate(b.Title, 50)))
				}
				// Show linked open recovery bead (if any) as an indented sub-line.
				if m.Snapshot != nil && m.Snapshot.RecoveryRefs != nil {
					if ref := m.Snapshot.RecoveryRefs[b.ID]; ref != nil {
						recStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
						ib.WriteString(recStyle.Render(fmt.Sprintf("    recovery → %s", ref.ID)) + "\n")
					}
				}
			}
			interruptedStr = ib.String()
		}

		// Join side-by-side or render single section.
		if bothPresent {
			leftStyle := lipgloss.NewStyle().Width(subColWidth + 2)
			s.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftStyle.Render(blockedStr), interruptedStr))
			s.WriteString("\n")
		} else if hasBlocked {
			s.WriteString(blockedStr)
		} else {
			s.WriteString(interruptedStr)
		}
	}

	// Agent panel (capped by budget).
	if budget.MaxAgents > 0 && len(m.Agents) > 0 {
		s.WriteString(RenderAgentPanelSnap(m.Agents, m.Snapshot.DAGProgress, budget.MaxAgents))
	}

	// Footer.
	s.WriteString("\n")
	footerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	scopeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	epicInfo := ""
	if m.Opts.Epic != "" {
		epicInfo = " • epic:" + m.Opts.Epic
	}
	colsHint := ""
	if m.ShowAllCols {
		colsHint = " [all]"
	}
	leftFooter := footerStyle.Render("j/k ↕  h/l ↔  Enter inspect  / search  s summon  u unsummon  r reset  x close  a actions  e epic" + epicInfo + "  T tower  H cols" + colsHint + " • q quit • ↻ " + m.Opts.Interval.String())
	rightFooter := scopeStyle.Render("showing " + m.TypeScope.Label())
	if m.Width > 0 {
		gap := m.Width - lipgloss.Width(leftFooter) - lipgloss.Width(rightFooter)
		if gap > 1 {
			s.WriteString(leftFooter)
			s.WriteString(strings.Repeat(" ", gap))
			s.WriteString(rightFooter)
		} else {
			s.WriteString(leftFooter + "  " + rightFooter)
		}
	} else {
		s.WriteString(leftFooter + "  " + rightFooter)
	}
	s.WriteString("\n")
	if m.Cmdline.Active {
		s.WriteString(RenderCmdline(m.Cmdline, m.Width))
	} else {
		var footerParts []string
		if bead := m.SelectedBead(); bead != nil {
			beadStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
			footerParts = append(footerParts, beadStyle.Render(bead.ID+"  "+Truncate(bead.Title, 60)))
		}
		// Show inline action status.
		if m.ActionStatus != "" {
			statusStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
			footerParts = append(footerParts, statusStyle.Render(m.ActionStatus))
		}
		if len(footerParts) > 0 {
			s.WriteString(strings.Join(footerParts, footerStyle.Render("  •  ")))
		}
	}

	boardOutput := s.String()

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
	dimLG := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	for i := 0; i < shown; i++ {
		w := agents[i]
		phase := w.Phase
		if phase == "" {
			phase = "working"
		}
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

	// Show compact DAG pipeline for in-progress beads.
	if b.Status == "in_progress" {
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
	dimLGStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	var parts []string
	for _, step := range dag.Steps {
		switch step.Status {
		case "closed":
			parts = append(parts, greenStyle.Render("✓"))
		case "in_progress":
			parts = append(parts, cyanStyle.Render("▶"))
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
	dimLGStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	var parts []string
	for _, step := range dag.Steps {
		switch step.Status {
		case "closed":
			parts = append(parts, greenStyle.Render("✓"))
		case "in_progress":
			parts = append(parts, cyanStyle.Render("▶"))
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

	typeStr := ShortType(b.Type)
	if phase != "" {
		typeStr += " [" + phase + "]"
	}

	// Visual tag for beads awaiting human approval.
	needsHumanTag := ""
	if b.HasLabel("needs-human") {
		needsHumanTag = " " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3")).Render("[needs-human]")
	}

	isSel := len(selected) > 0 && selected[0]
	var s strings.Builder
	if isSel {
		cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
		s.WriteString(fmt.Sprintf("%s %s %s %s%s\n", cursor, PriStr(b.Priority), b.ID, typeStr, needsHumanTag))
	} else {
		s.WriteString(fmt.Sprintf("%s %s %s%s\n", PriStr(b.Priority), b.ID, typeStr, needsHumanTag))
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

	// Show compact DAG pipeline for in-progress beads.
	if b.Status == "in_progress" {
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
	dimLG := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	for i := 0; i < shown; i++ {
		w := agents[i]
		phase := w.Phase
		if phase == "" {
			phase = "working"
		}
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
