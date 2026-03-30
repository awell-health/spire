package board

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// DangerLevel indicates how dangerous an action is (for UI styling and confirmation).
type DangerLevel int

const (
	DangerNone        DangerLevel = iota
	DangerConfirm                         // show confirmation before executing
	DangerDestructive                     // red highlight + confirmation
)

// MenuAction represents a single item in the action menu popup.
type MenuAction struct {
	Key        rune
	Label      string
	Danger     DangerLevel
	ActionType PendingAction
}

// BuildActionMenu builds a context-sensitive list of actions based on bead state.
func BuildActionMenu(bead *BoardBead, agents []LocalAgent) []MenuAction {
	if bead == nil {
		return nil
	}

	hasWizard := false
	for _, a := range agents {
		if a.BeadID == bead.ID {
			hasWizard = true
			break
		}
	}

	needsHuman := bead.HasLabel("needs-human")

	var items []MenuAction

	isDesign := bead.Type == "design"

	switch bead.Status {
	case "open":
		if isDesign && needsHuman {
			items = append(items, MenuAction{Key: 'y', Label: "Approve design", Danger: DangerConfirm, ActionType: ActionApproveDesign})
			items = append(items, MenuAction{Key: 'n', Label: "Reject with feedback", Danger: DangerNone, ActionType: ActionRejectDesign})
		}
		items = append(items, MenuAction{Key: 's', Label: "Summon wizard", Danger: DangerNone, ActionType: ActionSummon})
		items = append(items, MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose})
	case "in_progress":
		if isDesign && needsHuman {
			items = append(items, MenuAction{Key: 'y', Label: "Approve design", Danger: DangerConfirm, ActionType: ActionApproveDesign})
			items = append(items, MenuAction{Key: 'n', Label: "Reject with feedback", Danger: DangerNone, ActionType: ActionRejectDesign})
		}
		if needsHuman {
			items = append(items, MenuAction{Key: 'Y', Label: "Approve (close)", Danger: DangerConfirm, ActionType: ActionApprove})
			items = append(items, MenuAction{Key: 'S', Label: "Resummon", Danger: DangerNone, ActionType: ActionResummon})
		}
		if hasWizard {
			items = append(items, MenuAction{Key: 'L', Label: "Tail logs", Danger: DangerNone, ActionType: ActionLogs})
			items = append(items, MenuAction{Key: 'u', Label: "Unsummon wizard", Danger: DangerConfirm, ActionType: ActionUnsummon})
		}
		if !hasWizard {
			items = append(items, MenuAction{Key: 's', Label: "Summon wizard", Danger: DangerNone, ActionType: ActionSummon})
		}
		items = append(items,
			MenuAction{Key: 'r', Label: "Reset", Danger: DangerConfirm, ActionType: ActionResetSoft},
			MenuAction{Key: 'R', Label: "Reset --hard", Danger: DangerDestructive, ActionType: ActionResetHard},
			MenuAction{Key: 'A', Label: "Advance phase", Danger: DangerNone, ActionType: ActionAdvance},
			MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
		)
	case "closed":
		// minimal actions for closed beads
	}

	// Always available.
	items = append(items,
		MenuAction{Key: 'g', Label: "Grok (deep focus)", Danger: DangerNone, ActionType: ActionGrok},
		MenuAction{Key: 't', Label: "Trace timeline", Danger: DangerNone, ActionType: ActionTrace},
	)

	return items
}

// renderActionMenu renders the popup box as a lipgloss-styled string.
func renderActionMenu(items []MenuAction, cursor int, beadID string, width int) string {
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	keyStyle := lipgloss.NewStyle().Bold(true)
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	var lines []string
	lines = append(lines, headerStyle.Render("Actions: "+beadID))
	lines = append(lines, dimStyle.Render(strings.Repeat("─", width-4)))

	for i, item := range items {
		prefix := "  "
		if i == cursor {
			prefix = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶") + " "
		}
		label := item.Label
		switch item.Danger {
		case DangerConfirm:
			label = yellowStyle.Render(label)
		case DangerDestructive:
			label = redStyle.Render(label)
		}
		lines = append(lines, fmt.Sprintf("%s%s  %s", prefix, keyStyle.Render(string(item.Key)), label))
	}

	content := strings.Join(lines, "\n")
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("6")).
		Padding(0, 1)

	return boxStyle.Render(content)
}
