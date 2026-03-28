package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// claimGetBeadFunc is a test-replaceable wrapper around storeGetBead.
var claimGetBeadFunc = storeGetBead

// claimUpdateBeadFunc is a test-replaceable wrapper around storeUpdateBead.
var claimUpdateBeadFunc = storeUpdateBead

// claimCreateAttemptFunc is a test-replaceable wrapper around storeCreateAttemptBeadAtomic.
// cmdClaim creates the attempt bead atomically as part of the claim so that
// storeGetReadyWork and the steward see ownership immediately.
// The atomic variant checks for an existing active attempt before creating,
// narrowing the TOCTOU race window.
var claimCreateAttemptFunc = storeCreateAttemptBeadAtomic

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

	// Create or reclaim the attempt bead atomically.
	// The attempt bead is the real ownership marker — storeGetReadyWork and
	// the steward filter by attempt beads, not by in_progress status.
	// storeCreateAttemptBeadAtomic checks for an existing active attempt and
	// either reclaims it (same agent) or rejects the claim (different agent),
	// narrowing the TOCTOU race window.
	identity, _ := claimIdentityFunc("")
	branch := resolveClaimBranch(id)
	// Model is unknown at claim time — the executor updates the model label
	// later when it has formula context.
	attemptID, err := claimCreateAttemptFunc(id, identity, "", branch)
	if err != nil {
		return fmt.Errorf("claim %s: %w", id, err)
	}

	// Flip status to in_progress.
	if err := claimUpdateBeadFunc(id, map[string]interface{}{
		"status":   "in_progress",
		"assignee": identity,
	}); err != nil {
		return fmt.Errorf("claim %s: %w", id, err)
	}

	// Output result as JSON for easy consumption by spire-work
	result := map[string]string{
		"id":      target.ID,
		"title":   target.Title,
		"type":    target.Type,
		"status":  "in_progress",
		"attempt": attemptID,
	}
	out, _ := json.Marshal(result)
	fmt.Println(string(out))

	return nil
}

// resolveClaimBranch loads spire.yaml from the current directory and resolves
// the branch name for the given bead ID. Falls back to "feat/<id>" if the
// config cannot be loaded.
func resolveClaimBranch(beadID string) string {
	cfg, err := repoconfig.Load(".")
	if err != nil || cfg == nil {
		return "feat/" + beadID
	}
	return cfg.ResolveBranch(beadID)
}
