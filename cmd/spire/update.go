package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// updateGetBeadFunc is a test-replaceable wrapper around storeGetBead.
var updateGetBeadFunc = storeGetBead

// updateUpdateBeadFunc is a test-replaceable wrapper around storeUpdateBead.
var updateUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
	return storeUpdateBead(id, updates)
}

// updateAddLabelFunc is a test-replaceable wrapper around storeAddLabel.
var updateAddLabelFunc = storeAddLabel

// updateRemoveLabelFunc is a test-replaceable wrapper around storeRemoveLabel.
var updateRemoveLabelFunc = storeRemoveLabel

// updateIdentityFunc is a test-replaceable wrapper around detectIdentity.
var updateIdentityFunc = func(asFlag string) (string, error) { return detectIdentity(asFlag) }

var updateCmd = &cobra.Command{
	Use:   "update <bead-id> [flags]",
	Short: "Update bead fields (wraps bd update)",
	Long: `Update one or more fields on a bead. Passes through to the store API.

Note: --claim sets the assignee to the current identity and (unless --status
is also provided) flips the status to in_progress. Unlike "spire claim", it
does NOT create an attempt bead.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdUpdate(cmd, args)
	},
}

func init() {
	updateCmd.Flags().String("status", "", "Set status (open, ready, in_progress, deferred, closed)")
	updateCmd.Flags().String("title", "", "Set title")
	updateCmd.Flags().String("description", "", "Set description")
	updateCmd.Flags().IntP("priority", "p", 0, "Set priority (0-4)")
	updateCmd.Flags().String("assignee", "", "Set assignee")
	updateCmd.Flags().String("owner", "", "Set owner")
	updateCmd.Flags().Bool("claim", false, "Set assignee to current identity (and status to in_progress unless --status is set)")
	updateCmd.Flags().Bool("defer", false, "Set status to deferred")
	updateCmd.Flags().String("add-label", "", "Add a label")
	updateCmd.Flags().String("remove-label", "", "Remove a label")
}

func cmdUpdate(cmd *cobra.Command, args []string) error {
	id := args[0]

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// Validate bead exists and isn't closed.
	target, err := updateGetBeadFunc(id)
	if err != nil {
		return fmt.Errorf("bead %s not found: %w", id, err)
	}
	if target.Status == "closed" {
		return fmt.Errorf("bead %s is already closed", id)
	}

	// Reject conflicting --defer + --status.
	if cmd.Flags().Changed("defer") && cmd.Flags().Changed("status") {
		return fmt.Errorf("cannot use --defer with --status")
	}

	// Build updates map from explicitly-set flags only.
	updates := make(map[string]interface{})

	if cmd.Flags().Changed("status") {
		v, _ := cmd.Flags().GetString("status")
		updates["status"] = v
	}
	if cmd.Flags().Changed("title") {
		v, _ := cmd.Flags().GetString("title")
		updates["title"] = v
	}
	if cmd.Flags().Changed("description") {
		v, _ := cmd.Flags().GetString("description")
		updates["description"] = v
	}
	if cmd.Flags().Changed("priority") {
		v, _ := cmd.Flags().GetInt("priority")
		updates["priority"] = v
	}
	if cmd.Flags().Changed("assignee") {
		v, _ := cmd.Flags().GetString("assignee")
		updates["assignee"] = v
	}
	if cmd.Flags().Changed("owner") {
		v, _ := cmd.Flags().GetString("owner")
		updates["owner"] = v
	}

	// --claim: resolve identity, set assignee, auto-set status to in_progress
	// unless --status was explicitly provided.
	if cmd.Flags().Changed("claim") {
		identity, ierr := updateIdentityFunc("")
		if ierr != nil {
			return fmt.Errorf("update %s: cannot resolve identity: %w", id, ierr)
		}
		updates["assignee"] = identity
		if !cmd.Flags().Changed("status") {
			updates["status"] = "in_progress"
		}
	}

	// --defer: set status to deferred.
	if cmd.Flags().Changed("defer") {
		updates["status"] = "deferred"
	}

	// Handle label operations (separate store calls).
	if cmd.Flags().Changed("add-label") {
		label, _ := cmd.Flags().GetString("add-label")
		if err := updateAddLabelFunc(id, label); err != nil {
			return fmt.Errorf("update %s: add label: %w", id, err)
		}
	}
	if cmd.Flags().Changed("remove-label") {
		label, _ := cmd.Flags().GetString("remove-label")
		if err := updateRemoveLabelFunc(id, label); err != nil {
			return fmt.Errorf("update %s: remove label: %w", id, err)
		}
	}

	// Apply field updates if any.
	if len(updates) > 0 {
		if err := updateUpdateBeadFunc(id, updates); err != nil {
			return fmt.Errorf("update %s: %w", id, err)
		}
	}

	// Print result as JSON.
	result := map[string]interface{}{
		"id":      id,
		"updated": updates,
	}
	out, _ := json.Marshal(result)
	fmt.Println(string(out))

	return nil
}
