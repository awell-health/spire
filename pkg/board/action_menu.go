package board

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// DangerLevel indicates how dangerous an action is (for UI styling and confirmation).
type DangerLevel int

const (
	DangerNone        DangerLevel = iota
	DangerConfirm                         // show confirmation before executing
	DangerDestructive                     // red highlight + confirmation
)

// ActionReady is defined in the consolidated iota block in tui.go.

// MenuAction represents a single item in the action menu popup.
type MenuAction struct {
	Key        rune
	Label      string
	Danger     DangerLevel
	ActionType PendingAction
}

// BuildActionMenu builds a context-sensitive list of actions based on bead status.
// Routing is status-based, with a label-aware approve entry (key `y`) appended
// when the bead carries `needs-human` or `awaiting-approval`.
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

	var items []MenuAction

	switch bead.Status {
	case "open":
		items = append(items,
			MenuAction{Key: 's', Label: "Summon wizard", Danger: DangerNone, ActionType: ActionSummon},
			MenuAction{Key: 'r', Label: "Ready", Danger: DangerNone, ActionType: ActionReady},
			MenuAction{Key: 'd', Label: "Defer", Danger: DangerNone, ActionType: ActionDefer},
			MenuAction{Key: 'c', Label: "Comment", Danger: DangerNone, ActionType: ActionComment},
			MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
		)
	case "ready":
		items = append(items,
			MenuAction{Key: 's', Label: "Summon wizard", Danger: DangerNone, ActionType: ActionSummon},
			MenuAction{Key: 'd', Label: "Defer", Danger: DangerNone, ActionType: ActionDefer},
			MenuAction{Key: 'c', Label: "Comment", Danger: DangerNone, ActionType: ActionComment},
			MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
		)
	case "in_progress":
		if hasWizard {
			items = append(items,
				MenuAction{Key: 'L', Label: "Tail logs", Danger: DangerNone, ActionType: ActionLogs},
				MenuAction{Key: 'u', Label: "Unsummon wizard", Danger: DangerConfirm, ActionType: ActionUnsummon},
				MenuAction{Key: 'c', Label: "Comment", Danger: DangerNone, ActionType: ActionComment},
				MenuAction{Key: 'r', Label: "Reset", Danger: DangerConfirm, ActionType: ActionResetSoft},
				MenuAction{Key: 'R', Label: "Reset --hard", Danger: DangerDestructive, ActionType: ActionResetHard},
				MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
			)
		} else {
			items = append(items,
				MenuAction{Key: 's', Label: "Summon (resume)", Danger: DangerNone, ActionType: ActionSummon},
				MenuAction{Key: 'c', Label: "Comment", Danger: DangerNone, ActionType: ActionComment},
				MenuAction{Key: 'r', Label: "Reset", Danger: DangerConfirm, ActionType: ActionResetSoft},
				MenuAction{Key: 'R', Label: "Reset --hard", Danger: DangerDestructive, ActionType: ActionResetHard},
				MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
			)
		}
	case "hooked":
		items = append(items,
			MenuAction{Key: 'S', Label: "Resume", Danger: DangerNone, ActionType: ActionResume},
			MenuAction{Key: 'c', Label: "Comment", Danger: DangerNone, ActionType: ActionComment},
			MenuAction{Key: 'r', Label: "Reset", Danger: DangerConfirm, ActionType: ActionResetSoft},
			MenuAction{Key: 'R', Label: "Reset --hard", Danger: DangerDestructive, ActionType: ActionResetHard},
			MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
		)
	case "awaiting_review":
		items = append(items,
			MenuAction{Key: 'c', Label: "Comment", Danger: DangerNone, ActionType: ActionComment},
			MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
		)
	case "deferred":
		items = append(items,
			MenuAction{Key: 'd', Label: "Undefer", Danger: DangerNone, ActionType: ActionDefer},
			MenuAction{Key: 'c', Label: "Comment", Danger: DangerNone, ActionType: ActionComment},
			MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
		)
	case "closed":
		// Closed beads: only Grok and Trace (appended below).
	}

	// Label-aware approve entry on `y`. Precedence (first match wins):
	//   1. awaiting-approval → Approve gate
	//   2. design + needs-human → Approve design
	//   3. needs-human (non-design) → Approve
	switch {
	case bead.HasLabel("awaiting-approval"):
		items = append(items, MenuAction{Key: 'y', Label: "Approve gate", Danger: DangerConfirm, ActionType: ActionApproveGate})
	case bead.Type == "design" && bead.HasLabel("needs-human"):
		items = append(items, MenuAction{Key: 'y', Label: "Approve design", Danger: DangerConfirm, ActionType: ActionApproveDesign})
	case bead.HasLabel("needs-human"):
		items = append(items, MenuAction{Key: 'y', Label: "Approve", Danger: DangerConfirm, ActionType: ActionApprove})
	}

	// Always available.
	items = append(items,
		MenuAction{Key: 'g', Label: "Grok (deep focus)", Danger: DangerNone, ActionType: ActionGrok},
		MenuAction{Key: 't', Label: "Trace timeline", Danger: DangerNone, ActionType: ActionTrace},
	)

	return items
}

// BuildAgentActionMenu builds a context-sensitive action menu for an agent row.
// The agent's status and bead state determine which actions are available.
func BuildAgentActionMenu(agent AgentInfo) []MenuAction {
	var items []MenuAction

	if agent.BeadID == "" {
		// Idle agent — no bead-specific actions.
		return items
	}

	switch agent.Status {
	case "running":
		items = append(items,
			MenuAction{Key: 'u', Label: "Unsummon wizard", Danger: DangerConfirm, ActionType: ActionUnsummon},
			MenuAction{Key: 'r', Label: "Reset", Danger: DangerConfirm, ActionType: ActionResetSoft},
			MenuAction{Key: 'R', Label: "Reset --hard", Danger: DangerDestructive, ActionType: ActionResetHard},
			MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
		)
	case "errored":
		items = append(items,
			MenuAction{Key: 'r', Label: "Reset", Danger: DangerConfirm, ActionType: ActionResetSoft},
			MenuAction{Key: 'R', Label: "Reset --hard", Danger: DangerDestructive, ActionType: ActionResetHard},
			MenuAction{Key: 's', Label: "Resummon", Danger: DangerNone, ActionType: ActionResummon},
			MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
		)
	default: // idle with bead
		items = append(items,
			MenuAction{Key: 's', Label: "Summon wizard", Danger: DangerNone, ActionType: ActionSummon},
			MenuAction{Key: 'r', Label: "Reset", Danger: DangerConfirm, ActionType: ActionResetSoft},
			MenuAction{Key: 'x', Label: "Close", Danger: DangerConfirm, ActionType: ActionClose},
		)
	}

	// Always available for agents with beads.
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
