package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/beads"
)

func cmdFocus(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	if len(args) < 1 {
		return fmt.Errorf("usage: spire focus <bead-id>")
	}
	id := args[0]

	// 1. Fetch the target bead
	target, err := storeGetBead(id)
	if err != nil {
		return fmt.Errorf("focus %s: %w", id, err)
	}

	// 2. Check if a molecule already exists for this task (via workflow: label)
	var molID string
	existingMols, _ := storeListBeads(beads.IssueFilter{IDPrefix: "spi-", Labels: []string{"workflow:" + id}, Status: statusPtr(beads.StatusOpen)})
	if len(existingMols) > 0 {
		molID = existingMols[0].ID
	}

	// 3. Pour molecule if none exists
	if molID == "" {
		// Ensure the proto exists (cook --persist is idempotent)
		bd("cook", "spire-agent-work", "--persist")
		pourOut, pourErr := bd("mol", "pour", "spire-agent-work", "--var", fmt.Sprintf("task=%s", id), "--json")
		if pourErr != nil {
			fmt.Fprintf(os.Stderr, "spire: warning: could not create workflow: %s\n", pourErr)
		} else {
			// Parse the poured molecule to get its root ID
			var pourResult struct {
				RootID string `json:"new_epic_id"`
			}
			if err := json.Unmarshal([]byte(pourOut), &pourResult); err == nil && pourResult.RootID != "" {
				molID = pourResult.RootID
			} else {
				// Fallback: find by label (pour may not return JSON we expect)
				existingMols, _ = storeListBeads(beads.IssueFilter{IDPrefix: "spi-", Labels: []string{"workflow:" + id}, Status: statusPtr(beads.StatusOpen)})
				if len(existingMols) > 0 {
					molID = existingMols[0].ID
				}
			}
			// Label the molecule to link it to the task
			if molID != "" {
				storeAddLabel(molID, "workflow:"+id)
			}
		}
	}

	// 4. Get molecule progress if available
	var progressOut string
	if molID != "" {
		progressOut, _ = bd("mol", "progress", molID)
	}

	// 5. Assemble output
	fmt.Printf("--- Task %s ---\n", target.ID)
	fmt.Printf("Title: %s\n", target.Title)
	fmt.Printf("Status: %s\n", target.Status)
	fmt.Printf("Priority: P%d\n", target.Priority)
	if target.Description != "" {
		fmt.Printf("Description: %s\n", target.Description)
	}
	fmt.Println()

	// Workflow progress
	if progressOut != "" {
		fmt.Println("--- Workflow (spire-agent-work) ---")
		fmt.Println(progressOut)
		fmt.Println()
	}

	// Referenced beads (from ref: labels)
	for _, l := range target.Labels {
		if strings.HasPrefix(l, "ref:") {
			refID := l[4:]
			refBead, refErr := storeGetBead(refID)
			if refErr != nil {
				continue
			}
			fmt.Printf("--- Referenced: %s ---\n", refBead.ID)
			fmt.Printf("Title: %s\n", refBead.Title)
			fmt.Printf("Status: %s\n", refBead.Status)
			if refBead.Description != "" {
				fmt.Printf("Description: %s\n", refBead.Description)
			}
			fmt.Println()
		}
	}

	// Also check for messages that reference this bead
	referrers, _ := storeListBeads(beads.IssueFilter{IDPrefix: "spi-", Labels: []string{"msg", "ref:" + id}, Status: statusPtr(beads.StatusOpen)})
	for _, m := range referrers {
		from := hasLabel(m, "from:")
		fmt.Printf("--- Referenced by %s ---\n", m.ID)
		if from != "" {
			fmt.Printf("From: %s\n", from)
		}
		fmt.Printf("Subject: %s\n", m.Title)
		fmt.Println()
	}

	// Thread context (parent + siblings)
	if target.Parent != "" {
		parentBead, parentErr := storeGetBead(target.Parent)
		if parentErr == nil {
			fmt.Printf("--- Thread (parent: %s) ---\n", parentBead.ID)
			fmt.Printf("Subject: %s\n", parentBead.Title)

			siblings, _ := storeGetChildren(target.Parent)
			for _, s := range siblings {
				if s.ID == target.ID {
					continue
				}
				from := hasLabel(s, "from:")
				fmt.Printf("  %s [%s]: %s\n", s.ID, from, s.Title)
			}
			fmt.Println()
		}
	}

	// Comments
	comments, commErr := storeGetComments(id)
	if commErr == nil && len(comments) > 0 {
		fmt.Printf("--- Comments (%d) ---\n", len(comments))
		for _, c := range comments {
			if c.Author != "" {
				fmt.Printf("[%s]: %s\n", c.Author, c.Text)
			} else {
				fmt.Println(c.Text)
			}
		}
		fmt.Println()
	}

	return nil
}
