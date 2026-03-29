package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var readCmd = &cobra.Command{
	Use:   "read <bead-id>",
	Short: "Mark a message as read",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdRead(args)
	},
}

func cmdRead(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire read <bead-id>")
	}
	id := args[0]

	// Check if already closed
	b, err := storeGetBead(id)
	if err != nil {
		return fmt.Errorf("read %s: %w", id, err)
	}

	if b.Status == "closed" {
		fmt.Printf("%s already read.\n", id)
		return nil
	}

	if err := storeCloseBead(id); err != nil {
		return fmt.Errorf("read %s: %w", id, err)
	}

	fmt.Printf("%s marked as read.\n", id)
	return nil
}
