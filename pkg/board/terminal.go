package board

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// renderTerminalPane renders the terminal pane overlay content.
// width and height are the overlay dimensions (not the full terminal).
func renderTerminalPane(m *BoardMode, width, height int) string {
	if width < 20 {
		width = 20
	}
	if height < 6 {
		height = 6
	}

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	footerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	// Inner width is overlay width minus border (2) and padding (2).
	innerW := width - 4
	if innerW < 10 {
		innerW = 10
	}

	// Viewport height = overlay height minus title (1), separator (1), footer (1), borders (2).
	viewportH := height - 5
	if viewportH < 3 {
		viewportH = 3
	}

	var lines []string

	// Title bar with scroll indicator.
	titleText := m.TermTitle
	if m.TermLoading {
		titleText += "  (loading...)"
	}
	scrollInfo := ""
	if len(m.TermLines) > viewportH {
		scrollInfo = fmt.Sprintf(" [%d/%d]", m.TermScroll+1, len(m.TermLines))
	}
	titleLine := headerStyle.Render(titleText) + dimStyle.Render(scrollInfo)
	lines = append(lines, titleLine)

	// Separator.
	lines = append(lines, dimStyle.Render(strings.Repeat("─", innerW)))

	// Content area.
	if m.TermLoading || len(m.TermLines) == 0 {
		// Show loading or empty state centered in viewport.
		msg := "Loading..."
		if !m.TermLoading {
			msg = "(empty)"
		}
		padTop := viewportH / 2
		for i := 0; i < padTop; i++ {
			lines = append(lines, "")
		}
		lines = append(lines, dimStyle.Render(msg))
		for i := padTop + 1; i < viewportH; i++ {
			lines = append(lines, "")
		}
	} else {
		// Slice visible lines from scroll offset.
		start := m.TermScroll
		if start < 0 {
			start = 0
		}
		if start >= len(m.TermLines) {
			start = len(m.TermLines) - 1
		}
		end := start + viewportH
		if end > len(m.TermLines) {
			end = len(m.TermLines)
		}
		visible := m.TermLines[start:end]
		for _, l := range visible {
			// Truncate lines wider than inner width.
			if lipgloss.Width(l) > innerW {
				l = truncateAnsi(l, innerW)
			}
			lines = append(lines, l)
		}
		// Pad remaining viewport lines.
		for i := len(visible); i < viewportH; i++ {
			lines = append(lines, "")
		}
	}

	// Footer with key hints.
	lines = append(lines, footerStyle.Render("q/Esc close · j/k scroll · d/u page · g/G top/bottom · r refresh"))

	content := strings.Join(lines, "\n")

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("6")).
		Padding(0, 1).
		Width(width - 2) // account for border

	return boxStyle.Render(content)
}

// truncateAnsi truncates a string that may contain ANSI escape codes to fit
// within maxWidth visible characters. Uses lipgloss.Width for ANSI-aware measurement.
func truncateAnsi(s string, maxWidth int) string {
	if lipgloss.Width(s) <= maxWidth {
		return s
	}
	// Walk runes, tracking visible width.
	runes := []rune(s)
	inEscape := false
	visWidth := 0
	for i, r := range runes {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEscape = false
			}
			continue
		}
		visWidth++
		if visWidth >= maxWidth-1 {
			return string(runes[:i+1]) + "…\x1b[0m"
		}
	}
	return s
}
