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
	if len(args) < 1 {
		return fmt.Errorf("usage: spire claim <bead-id>")
	}
	id := args[0]

	// Verify bead exists and check state
	target, err := storeGetBead(id)
	if err != nil {
		return fmt.Errorf("bead %s not found: %w", id, err)
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

	// Claim it
	if err := storeUpdateBead(id, map[string]interface{}{
		"status":   "in_progress",
		"assignee": identity,
	}); err != nil {
		return fmt.Errorf("claim %s: %w", id, err)
	}

	// Add owner label so spire watch can identify who is working the bead.
	if identity != "" {
		storeAddLabel(id, "owner:"+identity)
	}

	// Output result as JSON for easy consumption by spire-work
	result := map[string]string{
		"id":     target.ID,
		"title":  target.Title,
		"type":   target.Type,
		"status": "in_progress",
	}
	out, _ := json.Marshal(result)
	fmt.Println(string(out))

	return nil
}
