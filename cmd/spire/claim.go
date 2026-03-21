package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// isNoRemoteError returns true for errors caused by a missing remote configuration,
// which are expected and non-fatal when no remote has been set up yet.
func isNoRemoteError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "no remotes") ||
		strings.Contains(s, "remote 'origin' not found") ||
		strings.Contains(s, "remote not found")
}

func cmdClaim(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	if len(args) < 1 {
		return fmt.Errorf("usage: spire claim <bead-id>")
	}
	id := args[0]

	// Verify bead exists and check state
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

	// Claim it — writes directly to the shared dolt server via SQL
	if _, err := bd("update", id, "--claim", "--status", "in_progress"); err != nil {
		return fmt.Errorf("claim %s: %w", id, err)
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
