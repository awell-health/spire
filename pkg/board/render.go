package board

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// HeightBudgetResult holds the computed layout parameters for the board.
type HeightBudgetResult struct {
	MaxCards   int  // max cards to show per column
	Compact    bool // use 1-line compact cards instead of 4-line cards
	MaxAlerts  int  // max alert lines to show
	MaxBlocked int  // max blocked lines to show
	MaxAgents  int  // max agent lines to show in the agent panel (0 = hidden)
}

// CalcHeightBudget computes card limits based on terminal height.
// Returns permissive defaults when termHeight is zero (non-TTY or unknown).
func CalcHeightBudget(termHeight, alertCount, blockedCount, colCount, agentCount int) HeightBudgetResult {
	if termHeight <= 0 {
		maxAg := agentCount
		if maxAg > 5 {
			maxAg = 5
		}
		return HeightBudgetResult{MaxCards: 99, MaxAlerts: alertCount, MaxBlocked: 8, MaxAgents: maxAg}
	}

	const fixed = 7
	available := termHeight - fixed
	if available < 4 {
		available = 4
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

	maxBlocked := 0
	if blockedCount > 0 {
		maxBlocked = available / 5
		if maxBlocked < 1 {
			maxBlocked = 1
		}
		if maxBlocked > blockedCount {
			maxBlocked = blockedCount
		}
		available -= maxBlocked + 2
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
		MaxCards:   maxCards,
		Compact:    compact,
		MaxAlerts:  maxAlerts,
		MaxBlocked: maxBlocked,
		MaxAgents:  maxAgents,
	}
}

// RenderCompactCard renders a single bead as a one-line string for TUI columns.
func RenderCompactCard(b BoardBead, color lipgloss.Color, width int, selected bool) string {
	titleWidth := width - 26
	if titleWidth < 15 {
		titleWidth = 15
	}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	owner := BeadOwnerLabel(b)
	ownerStr := ""
	if owner != "" {
		ownerStr = " " + lipgloss.NewStyle().Foreground(color).Render(owner)
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
func (m Model) View() string {
	if m.Quitting {
		return ""
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

	budget := CalcHeightBudget(m.Height, len(visibleCols.Alerts), len(visibleCols.Blocked), len(displayCols), len(m.Agents))

	// Header.
	header := lipgloss.NewStyle().Bold(true).Render("Spire Board")
	ts := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(m.LastTick.Format("15:04:05"))
	s.WriteString(header + "  " + ts + "\n\n")

	// Alerts (capped by budget).
	if len(visibleCols.Alerts) > 0 {
		SortBeads(visibleCols.Alerts)
		alertStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(alertStyle.Render(fmt.Sprintf("⚠ ALERTS (%d)", len(visibleCols.Alerts))) + "\n")
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
			s.WriteString(fmt.Sprintf("  %s %s%s\n", PriStr(a.Priority), alertType, a.Title))
		}
		s.WriteString("\n")
	}

	// Build column content.
	if len(displayCols) > 0 {
		rendered := make([]string, len(displayCols))
		for i, c := range displayCols {
			var cb strings.Builder
			headerStyle := lipgloss.NewStyle().Bold(true).Foreground(c.Color)
			if i == m.SelCol {
				headerStyle = headerStyle.Underline(true)
			}
			cb.WriteString(headerStyle.Render(fmt.Sprintf("%s (%d)", c.Name, len(c.Beads))))
			cb.WriteString("\n")
			sepStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			cb.WriteString(sepStyle.Render(strings.Repeat("─", Min(colWidth, len(c.Name)+4))))
			cb.WriteString("\n")

			for j, b := range c.Beads {
				if j >= budget.MaxCards {
					dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
					cb.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(c.Beads)-budget.MaxCards)))
					cb.WriteString("\n")
					break
				}
				isSelected := (i == m.SelCol && j == m.SelCard)
				if budget.Compact {
					cb.WriteString(RenderCompactCard(b, c.Color, colWidth, isSelected))
				} else {
					cb.WriteString(RenderCardStr(b, c.Color, colWidth, isSelected))
				}
			}
			rendered[i] = cb.String()
		}

		s.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, AddGaps(rendered, colWidth)...))
		s.WriteString("\n")
	}

	// Blocked (capped by budget).
	if len(visibleCols.Blocked) > 0 {
		blockedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(blockedStyle.Render(fmt.Sprintf("BLOCKED (%d)", len(visibleCols.Blocked))) + "\n")

		for i, b := range visibleCols.Blocked {
			if i >= budget.MaxBlocked {
				dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(visibleCols.Blocked)-budget.MaxBlocked)) + "\n")
				break
			}
			blockers := BlockingDepIDs(b)
			blockerStr := ""
			if len(blockers) > 0 {
				bStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				blockerStr = " " + bStyle.Render("← "+strings.Join(blockers, ", "))
			}
			s.WriteString(fmt.Sprintf("  %s %s %s%s\n", PriStr(b.Priority), b.ID, Truncate(b.Title, 40), blockerStr))
		}
	}

	// Agent panel (capped by budget).
	if budget.MaxAgents > 0 && len(m.Agents) > 0 {
		s.WriteString(RenderAgentPanel(m.Agents, budget.MaxAgents))
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
	leftFooter := footerStyle.Render("j/k ↕  h/l ↔  tab  t type  H cols" + colsHint + "  f focus  s summon  c claim  L logs  e epic" + epicInfo + " • q quit • ↻ " + m.Opts.Interval.String())
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
	var footerParts []string
	if bead := m.SelectedBead(); bead != nil {
		beadStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
		footerParts = append(footerParts, beadStyle.Render(bead.ID+"  "+Truncate(bead.Title, 60)))
	}
	if len(footerParts) > 0 {
		s.WriteString(strings.Join(footerParts, footerStyle.Render("  •  ")))
	}

	return s.String()
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
func RenderCardStr(b BoardBead, color lipgloss.Color, width int, selected ...bool) string {
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
	colorStyle := lipgloss.NewStyle().Foreground(color)

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
