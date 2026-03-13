package main

import (
	"fmt"
	"strings"
)

func cmdRegister(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire register <name>")
	}
	name := args[0]

	// Check if already registered (idempotent)
	existingID, err := findAgentBead(name)
	if err == nil && existingID != "" {
		fmt.Println(existingID)
		return nil
	}

	// Create agent bead
	id, err := bdSilent(
		"create",
		"--rig=spi",
		"--type=task",
		"-p", "4",
		"--title", name,
		"--labels", fmt.Sprintf("agent,name:%s", name),
	)
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

	_, err = bd("close", id)
	if err != nil {
		return fmt.Errorf("unregister %s: %w", name, err)
	}

	fmt.Printf("unregistered %s (%s)\n", name, id)
	return nil
}

// findAgentBead returns the bead ID of a registered agent, or "" if not found.
func findAgentBead(name string) (string, error) {
	var beads []Bead
	err := bdJSON(&beads, "list", "--rig=spi", "--label", fmt.Sprintf("agent,name:%s", name), "--status=open")
	if err != nil {
		return "", err
	}
	for _, b := range beads {
		for _, l := range b.Labels {
			if l == "name:"+name {
				return b.ID, nil
			}
		}
	}
	return "", nil
}

// hasLabel checks if a bead has a specific label.
func hasLabel(b Bead, prefix string) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, prefix) {
			return l[len(prefix):]
		}
	}
	return ""
}
