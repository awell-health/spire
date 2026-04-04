package board

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// TowerItem represents a tower in the switcher popup.
type TowerItem struct {
	Name     string
	Database string
	Active   bool
}

// renderTowerSwitcher renders the tower switcher popup box.
func renderTowerSwitcher(items []TowerItem, cursor int, width int) string {
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	activeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))

	var lines []string
	lines = append(lines, headerStyle.Render("Switch Tower"))
	lines = append(lines, dimStyle.Render(strings.Repeat("─", width-4)))

	for i, item := range items {
		prefix := "  "
		if i == cursor {
			prefix = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶") + " "
		}
		label := item.Name
		if item.Active {
			label = fmt.Sprintf("%s %s", item.Name, activeStyle.Render("*"))
		}
		dbHint := ""
		if item.Database != "" {
			dbHint = "  " + dimStyle.Render(item.Database)
		}
		lines = append(lines, prefix+label+dbHint)
	}

	content := strings.Join(lines, "\n")
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("6")).
		Padding(0, 1)

	return boxStyle.Render(content)
}
