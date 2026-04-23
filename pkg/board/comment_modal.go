package board

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// renderCommentModal renders the comment-input overlay as a centered bordered
// box that matches the style of other board modals (terminal pane, action
// menu, tower switcher).
//
// The bubbles textarea itself is rendered via m.CommentTextarea.View(); this
// function is responsible for sizing the widget, wrapping it with a title
// bar and footer, and painting the border.
func renderCommentModal(m *BoardMode) string {
	popW, popH := commentModalSize(m.Width, m.Height)

	// Inner dimensions are the popup minus the rounded border (2) and
	// horizontal padding (2 = Padding(0, 1) on both sides).
	innerW := popW - 4
	if innerW < 20 {
		innerW = 20
	}

	// Reserve rows for: title (1), separator (1), footer (1), top/bottom
	// border (2). What's left is the textarea body height.
	const chrome = 5
	bodyH := popH - chrome
	if bodyH < 3 {
		bodyH = 3
	}

	// Keep the textarea model in sync with the current popup size. Doing it
	// here (render time) means we don't need a WindowSizeMsg hook.
	m.CommentTextarea.SetWidth(innerW)
	m.CommentTextarea.SetHeight(bodyH)

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	footerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	title := headerStyle.Render("Comment on " + m.CommentBeadID)
	separator := dimStyle.Render(strings.Repeat("─", innerW))
	footer := footerStyle.Render("ctrl+d submit   esc cancel   enter newline")

	body := m.CommentTextarea.View()

	content := strings.Join([]string{title, separator, body, footer}, "\n")

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("6")).
		Padding(0, 1).
		Width(popW - 2) // account for border

	return boxStyle.Render(content)
}

// commentModalSize computes the popup dimensions from the terminal size.
// Target is ~65% width and ~40% height, clamped to sensible minimums so the
// modal is usable even on small terminals.
func commentModalSize(termW, termH int) (int, int) {
	popW := termW * 65 / 100
	popH := termH * 40 / 100
	if popW < 50 {
		popW = 50
	}
	if popH < 12 {
		popH = 12
	}
	if termW > 0 && popW > termW {
		popW = termW
	}
	if termH > 0 && popH > termH {
		popH = termH
	}
	return popW, popH
}
