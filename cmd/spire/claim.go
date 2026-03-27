package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// claimGetActiveAttemptFunc is a test-replaceable wrapper around storeGetActiveAttempt.
var claimGetActiveAttemptFunc = storeGetActiveAttempt

// claimGetBeadFunc is a test-replaceable wrapper around storeGetBead.
var claimGetBeadFunc = storeGetBead

// claimUpdateBeadFunc is a test-replaceable wrapper around storeUpdateBead.
var claimUpdateBeadFunc = storeUpdateBead

// claimIdentityFunc is a test-replaceable wrapper around detectIdentity.
var claimIdentityFunc = func(asFlag string) (string, error) { return detectIdentity(asFlag) }

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
	target, err := claimGetBeadFunc(id)
	if err != nil {
		return fmt.Errorf("bead %s not found: %w", id, err)
	}

	// Check if already closed
	if target.Status == "closed" {
		return fmt.Errorf("bead %s is already closed", id)
	}

	// Check if claimed by someone else via attempt bead.
	identity, _ := claimIdentityFunc("")
	attempt, err := claimGetActiveAttemptFunc(id)
	if err != nil {
		return fmt.Errorf("claim %s: checking active attempt: %w", id, err)
	}
	if attempt != nil {
		// An active attempt bead exists — bead is already claimed.
		// Allow reclaim only if the attempt belongs to the same identity.
		owner := ""
		for _, l := range attempt.Labels {
			if strings.HasPrefix(l, "agent:") {
				owner = l[6:]
				break
			}
		}
		if owner != identity {
			return fmt.Errorf("bead %s is already claimed (attempt: %s)", id, attempt.ID)
		}
	}

	// Claim it
	if err := claimUpdateBeadFunc(id, map[string]interface{}{
		"status":   "in_progress",
		"assignee": identity,
	}); err != nil {
		return fmt.Errorf("claim %s: %w", id, err)
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
