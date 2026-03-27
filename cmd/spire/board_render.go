package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// heightBudgetResult holds the computed layout parameters for the board.
type heightBudgetResult struct {
	maxCards   int  // max cards to show per column
	compact    bool // use 1-line compact cards instead of 4-line cards
	maxAlerts  int  // max alert lines to show
	maxBlocked int  // max blocked lines to show
	maxAgents  int  // max agent lines to show in the agent panel (0 = hidden)
}

// calcHeightBudget computes card limits based on terminal height.
// Returns permissive defaults when termHeight is zero (non-TTY or unknown).
func calcHeightBudget(termHeight, alertCount, blockedCount, colCount, agentCount int) heightBudgetResult {
	if termHeight <= 0 {
		maxAg := agentCount
		if maxAg > 5 {
			maxAg = 5
		}
		return heightBudgetResult{maxCards: 99, maxAlerts: alertCount, maxBlocked: 8, maxAgents: maxAg}
	}

	// Fixed rows: header(2) + column-header+separator(2) + footer(3: blank+keys+bead).
	const fixed = 7
	available := termHeight - fixed
	if available < 4 {
		available = 4
	}

	// Allocate up to 20% of available rows for alerts (min 1, max alertCount).
	maxAlerts := 0
	if alertCount > 0 {
		maxAlerts = available / 5
		if maxAlerts < 1 {
			maxAlerts = 1
		}
		if maxAlerts > alertCount {
			maxAlerts = alertCount
		}
		available -= maxAlerts + 2 // header(1) + lines + blank(1)
		if available < 4 {
			available = 4
		}
	}

	// Allocate up to 20% of remaining rows for blocked (min 1, max blockedCount).
	maxBlocked := 0
	if blockedCount > 0 {
		maxBlocked = available / 5
		if maxBlocked < 1 {
			maxBlocked = 1
		}
		if maxBlocked > blockedCount {
			maxBlocked = blockedCount
		}
		available -= maxBlocked + 2 // blank(1) + header(1) + lines
		if available < 4 {
			available = 4
		}
	}

	// Allocate up to 20% of remaining rows for the agent panel (min 1, max 5).
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
		available -= maxAgents + 1 // header(1) + agent lines
		if available < 4 {
			available = 4
		}
	}

	// Try fitting cards in normal mode (4 lines each).
	maxCards := available / 4
	compact := false
	if maxCards < 2 {
		// Switch to compact (1 line per card).
		compact = true
		maxCards = available
		if maxCards < 1 {
			maxCards = 1
		}
	}

	return heightBudgetResult{
		maxCards:   maxCards,
		compact:    compact,
		maxAlerts:  maxAlerts,
		maxBlocked: maxBlocked,
		maxAgents:  maxAgents,
	}
}

// renderCompactCard renders a single bead as a one-line string for TUI columns.
// When selected is true, a ▶ cursor is prepended to indicate the active card.
func renderCompactCard(b BoardBead, color lipgloss.Color, width int, selected bool) string {
	titleWidth := width - 26
	if titleWidth < 15 {
		titleWidth = 15
	}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	owner := beadOwnerLabel(b)
	ownerStr := ""
	if owner != "" {
		ownerStr = " " + lipgloss.NewStyle().Foreground(color).Render(owner)
	}
	line := fmt.Sprintf("%s %s %s %s%s %s",
		priStr(b.Priority), b.ID, shortType(b.Type),
		truncate(b.Title, titleWidth), ownerStr,
		dimStyle.Render(timeAgo(b.UpdatedAt)))
	if selected {
		cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
		return cursor + " " + line + "\n"
	}
	return line + "\n"
}

func (m boardModel) View() string {
	if m.quitting {
		return ""
	}

	visibleCols := m.visibleCols()
	colWidth := 30
	if m.width > 0 {
		// Fit columns to terminal width.
		activeCols := countActiveCols(visibleCols)
		if activeCols > 0 {
			available := m.width - (activeCols-1)*2 // 2-char gap between columns
			cw := available / activeCols
			if cw > 50 {
				cw = 50
			}
			if cw > 20 {
				colWidth = cw
			}
		}
	}

	var s strings.Builder

	// Compute height budget before rendering any sections.
	budget := calcHeightBudget(m.height, len(visibleCols.Alerts), len(visibleCols.Blocked), countActiveCols(visibleCols), len(m.agents))

	// Header.
	header := lipgloss.NewStyle().Bold(true).Render("Spire Board")
	ts := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(m.lastTick.Format("15:04:05"))
	s.WriteString(header + "  " + ts + "\n\n")

	// Alerts (capped by budget).
	if len(visibleCols.Alerts) > 0 {
		sortBeads(visibleCols.Alerts)
		alertStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(alertStyle.Render(fmt.Sprintf("⚠ ALERTS (%d)", len(visibleCols.Alerts))) + "\n")
		for i, a := range visibleCols.Alerts {
			if i >= budget.maxAlerts {
				dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(visibleCols.Alerts)-budget.maxAlerts)) + "\n")
				break
			}
			alertType := ""
			for _, l := range a.Labels {
				if strings.HasPrefix(l, "alert:") {
					alertType = "[" + l[6:] + "] "
				}
			}
			s.WriteString(fmt.Sprintf("  %s %s%s\n", priStr(a.Priority), alertType, a.Title))
		}
		s.WriteString("\n")
	}

	// Build column content.
	active := boardActiveColumns(visibleCols)

	if len(active) > 0 {
		// Render each column as a string.
		rendered := make([]string, len(active))
		for i, c := range active {
			var cb strings.Builder
			headerStyle := lipgloss.NewStyle().Bold(true).Foreground(c.color)
			if i == m.selCol {
				headerStyle = headerStyle.Underline(true)
			}
			cb.WriteString(headerStyle.Render(fmt.Sprintf("%s (%d)", c.name, len(c.beads))))
			cb.WriteString("\n")
			sepStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			cb.WriteString(sepStyle.Render(strings.Repeat("─", min(colWidth, len(c.name)+4))))
			cb.WriteString("\n")

			for j, b := range c.beads {
				if j >= budget.maxCards {
					dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
					cb.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(c.beads)-budget.maxCards)))
					cb.WriteString("\n")
					break
				}
				isSelected := (i == m.selCol && j == m.selCard)
				if budget.compact {
					cb.WriteString(renderCompactCard(b, c.color, colWidth, isSelected))
				} else {
					cb.WriteString(renderCardStr(b, c.color, colWidth, isSelected))
				}
			}
			rendered[i] = cb.String()
		}

		// Join columns horizontally.
		s.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, addGaps(rendered, colWidth)...))
		s.WriteString("\n")
	}

	// Blocked (capped by budget).
	if len(visibleCols.Blocked) > 0 {
		blockedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(blockedStyle.Render(fmt.Sprintf("BLOCKED (%d)", len(visibleCols.Blocked))) + "\n")

		for i, b := range visibleCols.Blocked {
			if i >= budget.maxBlocked {
				dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(visibleCols.Blocked)-budget.maxBlocked)) + "\n")
				break
			}
			blockers := blockingDepIDs(b)
			blockerStr := ""
			if len(blockers) > 0 {
				bStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				blockerStr = " " + bStyle.Render("← "+strings.Join(blockers, ", "))
			}
			s.WriteString(fmt.Sprintf("  %s %s %s%s\n", priStr(b.Priority), b.ID, truncate(b.Title, 40), blockerStr))
		}
	}

	// Agent panel (capped by budget).
	if budget.maxAgents > 0 && len(m.agents) > 0 {
		s.WriteString(renderAgentPanel(m.agents, budget.maxAgents))
	}

	// Footer: two lines — keybindings + contextual bead info.
	s.WriteString("\n")
	footerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	scopeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	epicInfo := ""
	if m.opts.epic != "" {
		epicInfo = " • epic:" + m.opts.epic
	}
	leftFooter := footerStyle.Render("j/k ↕  h/l ↔  tab  t type  f focus  s summon  c claim  L logs  e epic" + epicInfo + " • q quit • ↻ " + m.opts.interval.String())
	rightFooter := scopeStyle.Render("showing " + m.typeScope.label())
	if m.width > 0 {
		gap := m.width - lipgloss.Width(leftFooter) - lipgloss.Width(rightFooter)
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
	// Second footer line: selected bead context.
	var footerParts []string
	if bead := m.selectedBead(); bead != nil {
		beadStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
		footerParts = append(footerParts, beadStyle.Render(bead.ID+"  "+truncate(bead.Title, 60)))
	}
	if len(footerParts) > 0 {
		s.WriteString(strings.Join(footerParts, footerStyle.Render("  •  ")))
	}

	return s.String()
}

// renderAgentPanel renders a compact live agent status panel.
// agents must already be alive (cleaned from registry).
func renderAgentPanel(agents []localWizard, maxAgents int) string {
	var s strings.Builder
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	phaseStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("4"))

	shown := len(agents)
	if shown > maxAgents {
		shown = maxAgents
	}
	s.WriteString(headerStyle.Render(fmt.Sprintf("AGENTS (%d)", len(agents))) + "\n")
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
		line := fmt.Sprintf("  %-28s%s  %s  %s",
			name,
			beadPart,
			phaseStyle.Render(phase),
			dimStyle.Render(elapsed),
		)
		s.WriteString(line + "\n")
	}
	if len(agents) > maxAgents {
		s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(agents)-maxAgents)) + "\n")
	}
	return s.String()
}

// renderCardStr renders a single bead card as a multi-line string for a column.
// When selected is true, a ▶ cursor is prepended to indicate the active card.
func renderCardStr(b BoardBead, color lipgloss.Color, width int, selected ...bool) string {
	titleWidth := width - 4
	if titleWidth < 10 {
		titleWidth = 10
	}

	typeStr := shortType(b.Type)
	if phase := getBoardBeadPhase(b); phase != "" {
		typeStr += " [" + phase + "]"
	}

	isSel := len(selected) > 0 && selected[0]
	var s strings.Builder
	if isSel {
		cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
		s.WriteString(fmt.Sprintf("%s %s %s %s\n", cursor, priStr(b.Priority), b.ID, typeStr))
	} else {
		s.WriteString(fmt.Sprintf("%s %s %s\n", priStr(b.Priority), b.ID, typeStr))
	}
	s.WriteString(fmt.Sprintf("  %s\n", truncate(b.Title, titleWidth)))

	// Third line: context (owner, time, etc.)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	colorStyle := lipgloss.NewStyle().Foreground(color)

	owner := beadOwnerLabel(b)
	if owner != "" {
		s.WriteString(fmt.Sprintf("  %s %s\n", colorStyle.Render(owner), dimStyle.Render(timeAgo(b.UpdatedAt))))
	} else {
		s.WriteString(fmt.Sprintf("  %s\n", dimStyle.Render(timeAgo(b.CreatedAt))))
	}

	s.WriteString("\n") // blank line between cards
	return s.String()
}

// addGaps pads each column string to colWidth and adds 2-char gaps.
func addGaps(columns []string, colWidth int) []string {
	style := lipgloss.NewStyle().Width(colWidth + 2)
	out := make([]string, len(columns))
	for i, c := range columns {
		out[i] = style.Render(c)
	}
	return out
}

func countActiveCols(cols boardColumns) int {
	return len(boardActiveColumns(cols))
}

func priStr(p int) string {
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
