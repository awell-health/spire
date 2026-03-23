package main

import (
	"fmt"
	"strconv"

	"github.com/steveyegge/beads"
)

// cmdAlert creates an alert bead that surfaces at the top of the board.
// Used by the steward, artificer, or archmage to flag things that need attention.
//
// Usage:
//
//	spire alert "Wizard-1 stale on spi-7v2.4" --ref spi-7v2.4 --type stale -p 1
//	spire alert "Epic spi-x2mk complete" --ref spi-x2mk --type epic-complete -p 1
//	spire alert "Review escalated for spi-7v2.2" --ref spi-7v2.2 --type escalation -p 0
func cmdAlert(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire alert <message> [--ref <bead-id>] [--type <alert-type>] [-p <priority>]")
	}

	message := args[0]
	var refBead, alertType string
	priority := 1

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--ref":
			if i+1 >= len(args) {
				return fmt.Errorf("--ref requires a bead ID")
			}
			i++
			refBead = args[i]
		case "--type":
			if i+1 >= len(args) {
				return fmt.Errorf("--type requires a value")
			}
			i++
			alertType = args[i]
		case "-p", "--priority":
			if i+1 >= len(args) {
				return fmt.Errorf("-p requires a priority (0-4)")
			}
			i++
			p, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("invalid priority: %q", args[i])
			}
			priority = p
		}
	}

	// Build labels.
	labels := []string{}
	if alertType != "" {
		labels = append(labels, "alert:"+alertType)
	} else {
		labels = append(labels, "alert")
	}
	if refBead != "" {
		labels = append(labels, "ref:"+refBead)
	}

	id, err := storeCreateBead(createOpts{
		Title:    message,
		Priority: priority,
		Type:     beads.TypeTask,
		Labels:   labels,
	})
	if err != nil {
		return fmt.Errorf("create alert: %w", err)
	}

	fmt.Printf("Alert created: %s\n", id)
	return nil
}
