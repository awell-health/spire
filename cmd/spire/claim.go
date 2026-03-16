package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

func cmdClaim(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	if len(args) < 1 {
		return fmt.Errorf("usage: spire claim <bead-id>")
	}
	id := args[0]

	// Step 1: Pull latest state (non-fatal if no remote)
	if _, err := bd("dolt", "pull"); err != nil {
		if !strings.Contains(err.Error(), "no remotes") {
			fmt.Printf("  pull warning: %s\n", err)
		}
	}

	// Step 2: Verify bead exists and check state
	out, err := bd("show", id, "--json")
	if err != nil {
		return fmt.Errorf("bead %s not found: %w", id, err)
	}
	target, err := parseBead([]byte(out))
	if err != nil {
		return fmt.Errorf("parse bead %s: %w", id, err)
	}

	// Check if already closed
	if target.Status == "closed" {
		return fmt.Errorf("bead %s is already closed", id)
	}

	// Check if claimed by someone else
	owner := ""
	for _, l := range target.Labels {
		if strings.HasPrefix(l, "owner:") {
			owner = l[6:]
			break
		}
	}
	identity, _ := detectIdentity("")
	if owner != "" && owner != identity && target.Status == "in_progress" {
		return fmt.Errorf("bead %s is already in progress (owner: %s)", id, owner)
	}

	// Step 3: Claim it
	if _, err := bd("update", id, "--claim", "--status", "in_progress"); err != nil {
		return fmt.Errorf("claim %s: %w", id, err)
	}

	// Step 4: Push
	if _, err := bd("dolt", "push"); err != nil {
		if !strings.Contains(err.Error(), "no remotes") {
			fmt.Printf("  push warning: %s\n", err)
		}
	}

	// Output result as JSON for easy consumption by spire-work
	result := map[string]string{
		"id":     target.ID,
		"title":  target.Title,
		"type":   target.Type,
		"status": "in_progress",
	}
	out2, _ := json.Marshal(result)
	fmt.Println(string(out2))

	return nil
}
