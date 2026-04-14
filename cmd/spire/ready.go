package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// readyGetBeadFunc is a test-replaceable wrapper around storeGetBead.
var readyGetBeadFunc = storeGetBead

// readyUpdateBeadFunc is a test-replaceable wrapper around storeUpdateBead.
var readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
	return storeUpdateBead(id, updates)
}

var readyCmd = &cobra.Command{
	Use:   "ready <bead-id> [bead-id...]",
	Short: "Mark beads as ready for agent pickup",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runReady,
}

func runReady(cmd *cobra.Command, args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	for _, id := range args {
		bead, err := readyGetBeadFunc(id)
		if err != nil {
			return fmt.Errorf("bead %s not found", id)
		}

		switch bead.Status {
		case "open":
			// valid transition — proceed
		case "ready":
			fmt.Fprintf(os.Stderr, "bead %s is already ready\n", id)
			continue
		case "in_progress":
			return fmt.Errorf("bead %s is already in progress", id)
		case "closed":
			return fmt.Errorf("bead %s is closed", id)
		case "deferred":
			return fmt.Errorf("bead %s is deferred — undefer it first", id)
		default:
			return fmt.Errorf("bead %s has unexpected status %q", id, bead.Status)
		}

		if err := readyUpdateBeadFunc(id, map[string]interface{}{
			"status": "ready",
		}); err != nil {
			return fmt.Errorf("ready %s: %w", id, err)
		}

		fmt.Printf("ready: %s\n", id)
	}

	return nil
}
