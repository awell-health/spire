package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func cmdFocus(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire focus <bead-id>")
	}
	id := args[0]

	// 1. Fetch the target bead
	out, err := bd("show", id, "--json")
	if err != nil {
		return fmt.Errorf("focus %s: %w", id, err)
	}
	target, err := parseBead([]byte(out))
	if err != nil {
		return fmt.Errorf("focus %s: parse bead: %w", id, err)
	}

	// 2. Check if a molecule already exists for this task (via workflow: label)
	var molID string
	var existingMols []Bead
	_ = bdJSON(&existingMols, "list", "--rig=spi", "--label", fmt.Sprintf("workflow:%s", id), "--status=open")
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
				_ = bdJSON(&existingMols, "list", "--rig=spi", "--label", fmt.Sprintf("workflow:%s", id), "--status=open")
				if len(existingMols) > 0 {
					molID = existingMols[0].ID
				}
			}
			// Label the molecule to link it to the task
			if molID != "" {
				bd("update", molID, "--add-label", fmt.Sprintf("workflow:%s", id))
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
			refOut, refErr := bd("show", refID, "--json")
			if refErr != nil {
				continue
			}
			refBead, refParseErr := parseBead([]byte(refOut))
			if refParseErr == nil {
				fmt.Printf("--- Referenced: %s ---\n", refBead.ID)
				fmt.Printf("Title: %s\n", refBead.Title)
				fmt.Printf("Status: %s\n", refBead.Status)
				if refBead.Description != "" {
					fmt.Printf("Description: %s\n", refBead.Description)
				}
				fmt.Println()
			}
		}
	}

	// Also check for messages that reference this bead
	var referrers []Bead
	_ = bdJSON(&referrers, "list", "--rig=spi", "--label", fmt.Sprintf("msg,ref:%s", id), "--status=open")
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
		parentOut, parentErr := bd("show", target.Parent, "--json")
		if parentErr == nil {
			parentBead, parseErr := parseBead([]byte(parentOut))
			if parseErr == nil {
				fmt.Printf("--- Thread (parent: %s) ---\n", parentBead.ID)
				fmt.Printf("Subject: %s\n", parentBead.Title)

				var siblings []Bead
				_ = bdJSON(&siblings, "children", target.Parent)
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
	}

	// Comments
	var comments []struct {
		Author string `json:"author"`
		Body   string `json:"body"`
	}
	commErr := bdJSON(&comments, "comments", id)
	if commErr == nil && len(comments) > 0 {
		fmt.Printf("--- Comments (%d) ---\n", len(comments))
		for _, c := range comments {
			if c.Author != "" {
				fmt.Printf("[%s]: %s\n", c.Author, c.Body)
			} else {
				fmt.Println(c.Body)
			}
		}
		fmt.Println()
	}

	return nil
}
