package main

import "fmt"

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
