package main

import (
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

	// 2. Check for existing molecule children
	var children []Bead
	_ = bdJSON(&children, "children", id)

	hasMolecule := false
	for _, c := range children {
		for _, l := range c.Labels {
			if l == "template" || l == "mol" {
				hasMolecule = true
				break
			}
		}
		if hasMolecule {
			break
		}
	}

	// 3. Bond formula if no molecule exists
	if !hasMolecule {
		_, bondErr := bd("mol", "bond", "spire-agent-work", id, "--var", fmt.Sprintf("task=%s", id))
		if bondErr != nil {
			// Non-fatal: formula may not exist or bond may not be available
			fmt.Fprintf(os.Stderr, "spire: warning: could not bond workflow: %s\n", bondErr)
		} else {
			// Re-fetch children after bonding
			_ = bdJSON(&children, "children", id)
		}
	}

	// 4. Get molecule progress if available
	var progressOut string
	progressOut, _ = bd("mol", "progress", id)

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
	} else if len(children) > 0 {
		fmt.Println("--- Workflow ---")
		for _, c := range children {
			check := "[ ]"
			if c.Status == "closed" {
				check = "[x]"
			}
			fmt.Printf("  %s %s — %s\n", check, c.ID, c.Title)
		}
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
