package main

import (
	"fmt"
	"strings"

	"github.com/steveyegge/beads"
)

func cmdRegister(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire register <name> [context]")
	}
	name := args[0]

	var context string
	if len(args) > 1 {
		context = strings.Join(args[1:], " ")
	}

	// Check if already registered (idempotent)
	existingID, err := findAgentBead(name)
	if err == nil && existingID != "" {
		fmt.Println(existingID)
		return nil
	}

	// Create agent bead
	id, err := storeCreateBead(createOpts{
		Title:       name,
		Description: context,
		Priority:    4,
		Type:        beads.TypeTask,
		Prefix:      "spi",
		Labels:      []string{"agent", "name:" + name},
	})
	if err != nil {
		return fmt.Errorf("register %s: %w", name, err)
	}

	fmt.Println(id)
	return nil
}

func cmdUnregister(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire unregister <name>")
	}
	name := args[0]

	id, err := findAgentBead(name)
	if err != nil {
		return fmt.Errorf("unregister %s: %w", name, err)
	}
	if id == "" {
		return fmt.Errorf("unregister %s: no registered agent found", name)
	}

	if err := storeCloseBead(id); err != nil {
		return fmt.Errorf("unregister %s: %w", name, err)
	}

	fmt.Printf("unregistered %s (%s)\n", name, id)
	return nil
}

// findAgentBead returns the bead ID of a registered agent, or "" if not found.
func findAgentBead(name string) (string, error) {
	results, err := storeListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"agent", "name:" + name},
		Status:   statusPtr(beads.StatusOpen),
	})
	if err != nil {
		return "", err
	}
	for _, b := range results {
		for _, l := range b.Labels {
			if l == "name:"+name {
				return b.ID, nil
			}
		}
	}
	return "", nil
}

// hasLabel checks if a bead has a label with the given prefix, returning the suffix.
func hasLabel(b Bead, prefix string) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, prefix) {
			return l[len(prefix):]
		}
	}
	return ""
}

// containsLabel checks if a bead has an exact label match.
func containsLabel(b Bead, label string) bool {
	for _, l := range b.Labels {
		if l == label {
			return true
		}
	}
	return false
}
