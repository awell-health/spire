package board

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// FilterColumns returns a new Columns struct with beads filtered by search query.
// If query is empty, returns cols unchanged.
func FilterColumns(cols Columns, query string) Columns {
	if query == "" {
		return cols
	}
	match := func(b BoardBead) bool {
		return matchesSearch(b, query)
	}
	return Columns{
		Alerts:      FilterBeads(cols.Alerts, match),
		Interrupted: FilterBeads(cols.Interrupted, match),
		Backlog:     FilterBeads(cols.Backlog, match),
		Ready:       FilterBeads(cols.Ready, match),
		Design:      FilterBeads(cols.Design, match),
		Plan:        FilterBeads(cols.Plan, match),
		Implement:   FilterBeads(cols.Implement, match),
		Review:      FilterBeads(cols.Review, match),
		Merge:       FilterBeads(cols.Merge, match),
		Done:        FilterBeads(cols.Done, match),
		Blocked:     FilterBeads(cols.Blocked, match),
	}
}

// matchesSearch returns true if the bead matches the query via case-insensitive
// substring match on ID, title, type, or any label.
func matchesSearch(bead BoardBead, query string) bool {
	q := strings.ToLower(query)
	if strings.Contains(strings.ToLower(bead.ID), q) {
		return true
	}
	if strings.Contains(strings.ToLower(bead.Title), q) {
		return true
	}
	if strings.Contains(strings.ToLower(bead.Type), q) {
		return true
	}
	for _, l := range bead.Labels {
		if strings.Contains(strings.ToLower(l), q) {
			return true
		}
	}
	return false
}

// renderSearchBar renders a styled search input bar for display at the top of the board.
func renderSearchBar(query string, active bool, width int) string {
	if query == "" && !active {
		return ""
	}
	prefixStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	prefix := prefixStyle.Render("/")
	if active {
		return prefix + " " + query + "█"
	}
	return prefix + " " + query + "  " + dimStyle.Render("(Esc to clear)")
}
