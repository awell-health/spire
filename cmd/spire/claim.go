package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/store"
	"github.com/google/uuid"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/spf13/cobra"
)

var claimCmd = &cobra.Command{
	Use:   "claim <bead-id>",
	Short: "Pull, verify, claim, push (atomic)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdClaim(args)
	},
}

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

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// Verify bead exists and check state
	target, err := claimGetBeadFunc(id)
	if err != nil {
		return fmt.Errorf("bead %s not found: %w", id, err)
	}

	// Check if already closed
	if target.Status == "closed" {
		return fmt.Errorf("bead %s is already closed", id)
	}

	// Seam 3: accept `ready` or `dispatched` as valid source statuses
	// for a fresh claim. `in_progress`/`hooked` flow through the reclaim
	// path below (same-agent identity check in CreateAttemptBeadAtomic).
	// `open` is allowed for legacy beads that never transitioned to ready.
	switch target.Status {
	case "ready", "dispatched", "in_progress", "hooked", "open", "":
		// ok
	default:
		return fmt.Errorf("bead %s has unclaimable status %q (expected ready or dispatched)", id, target.Status)
	}

	wasHooked := target.Status == "hooked"

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

	// Check instance ownership before proceeding (fail-closed for foreign instances).
	instanceID := config.InstanceID()
	owned, err := storeIsOwnedByInstanceFunc(attemptID, instanceID)
	if err != nil {
		return err
	}
	if !owned {
		meta, _ := storeGetAttemptInstanceFunc(attemptID)
		foreignName := "unknown"
		if meta != nil {
			foreignName = meta.InstanceName
		}
		return fmt.Errorf("attempt %s is owned by instance %q — cannot reclaim from this machine; use spire reset to release", attemptID, foreignName)
	}

	// Stamp instance metadata on the attempt bead.
	sessionID := uuid.New().String()
	instanceName := config.InstanceName()
	towerName := ""
	if tc, err := activeTowerConfig(); err == nil {
		towerName = tc.Name
	} else if tName := os.Getenv("SPIRE_TOWER"); tName != "" {
		towerName = tName
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if stampErr := storeStampAttemptInstanceFunc(attemptID, store.InstanceMeta{
		InstanceID:   instanceID,
		SessionID:    sessionID,
		InstanceName: instanceName,
		Backend:      "process",
		Tower:        towerName,
		StartedAt:    now,
		LastSeenAt:   now,
	}); stampErr != nil {
		return fmt.Errorf("stamp instance metadata on %s: %w", attemptID, stampErr)
	}

	// Flip status to in_progress.
	if err := claimUpdateBeadFunc(id, map[string]interface{}{
		"status":   "in_progress",
		"assignee": identity,
	}); err != nil {
		return fmt.Errorf("claim %s: %w", id, err)
	}

	// If the bead was hooked, a human is taking over — unhook all hooked step beads.
	if wasHooked {
		if children, err := storeGetChildren(id); err == nil {
			for _, child := range children {
				if isStepBead(child) && child.Status == "hooked" {
					if err := storeUnhookStepBead(child.ID); err != nil {
						fmt.Fprintf(os.Stderr, "  (note: could not unhook step %s: %s)\n", child.ID, err)
					}
				}
			}
		}
	}

	// Output result as JSON for easy consumption by spire-work
	result := map[string]string{
		"id":            target.ID,
		"title":         target.Title,
		"type":          target.Type,
		"status":        "in_progress",
		"attempt":       attemptID,
		"instance_name": instanceName,
		"instance_id":   instanceID,
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
