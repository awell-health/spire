package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

var alertCmd = &cobra.Command{
	Use:   "alert <message> [flags]",
	Short: "Alert on bead state changes",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if v, _ := cmd.Flags().GetString("ref"); v != "" {
			fullArgs = append(fullArgs, "--ref", v)
		}
		if v, _ := cmd.Flags().GetString("type"); v != "" {
			fullArgs = append(fullArgs, "--type", v)
		}
		if cmd.Flags().Changed("priority") {
			p, _ := cmd.Flags().GetInt("priority")
			fullArgs = append(fullArgs, "-p", strconv.Itoa(p))
		}
		fullArgs = append(fullArgs, args...)
		return cmdAlert(fullArgs)
	},
}

func init() {
	alertCmd.Flags().String("ref", "", "Bead ID to link to")
	alertCmd.Flags().String("type", "", "Alert type")
	alertCmd.Flags().IntP("priority", "p", 1, "Priority (0-4)")
}

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

	id, err := storeCreateBead(createOpts{
		Title:    message,
		Priority: priority,
		Type:     beads.TypeTask,
		Labels:   labels,
	})
	if err != nil {
		return fmt.Errorf("create alert: %w", err)
	}

	// Link alert to referenced bead via related dep (not ref: label).
	if refBead != "" {
		if derr := storeAddDepTyped(id, refBead, "related"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: add related dep %s→%s: %s\n", id, refBead, derr)
		}
	}

	fmt.Printf("Alert created: %s\n", id)
	return nil
}
