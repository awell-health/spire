package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// RosterAgent represents an agent registered in the tower.
type RosterAgent struct {
	Name       string `json:"name"`
	Role       string `json:"role"`       // "wizard", "artificer", "steward"
	Status     string `json:"status"`     // "idle", "working", "offline"
	BeadID     string `json:"bead_id"`    // current work (if working)
	BeadTitle  string `json:"bead_title"` // current work title
	Elapsed    string `json:"elapsed"`    // time on current work
	RegisteredAt string `json:"registered_at"`
}

// RosterSummary is the JSON output.
type RosterSummary struct {
	Agents    []RosterAgent `json:"agents"`
	Wizards   int           `json:"wizards"`
	Busy      int           `json:"busy"`
	Idle      int           `json:"idle"`
	Offline   int           `json:"offline"`
}

func cmdRoster(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	flagJSON := false
	for _, arg := range args {
		switch arg {
		case "--json":
			flagJSON = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire roster [--json]", arg)
		}
	}

	// Load registered agents (beads with label "agent").
	var agentBeads []BoardBead
	err := bdJSON(&agentBeads, "list", "--label", "agent", "--status=open")
	if err != nil {
		// Fallback: try without label filter if the rig doesn't support it.
		err = bdJSON(&agentBeads, "list", "--status=open")
		if err != nil {
			return fmt.Errorf("roster: %w", err)
		}
		// Filter manually for agent label.
		var filtered []BoardBead
		for _, b := range agentBeads {
			for _, l := range b.Labels {
				if l == "agent" {
					filtered = append(filtered, b)
					break
				}
			}
		}
		agentBeads = filtered
	}

	// Load in_progress beads to find who's working on what.
	var inProgress []BoardBead
	_ = bdJSON(&inProgress, "list", "--status=in_progress")

	// Build owner → bead map.
	ownerWork := make(map[string]BoardBead)
	for _, b := range inProgress {
		owner := beadOwnerLabel(b)
		if owner != "" {
			ownerWork[owner] = b
		}
	}

	// Exclude system agents.
	exclude := map[string]bool{
		"steward": true, "mayor": true,
		"spi": true, "awell": true,
	}

	var agents []RosterAgent
	wizards, busy, idle, offline := 0, 0, 0, 0

	for _, ab := range agentBeads {
		name := ""
		for _, l := range ab.Labels {
			if strings.HasPrefix(l, "name:") {
				name = l[5:]
				break
			}
		}
		if name == "" || exclude[name] {
			continue
		}

		role := "wizard"
		// Detect role from labels or name patterns.
		for _, l := range ab.Labels {
			if l == "artificer" || strings.Contains(l, "artificer") {
				role = "artificer"
			}
		}

		agent := RosterAgent{
			Name: name,
			Role: role,
		}

		// Check if this agent has active work.
		if work, ok := ownerWork[name]; ok {
			agent.Status = "working"
			agent.BeadID = work.ID
			agent.BeadTitle = work.Title
			agent.Elapsed = timeAgo(work.UpdatedAt)
			busy++
		} else {
			agent.Status = "idle"
			idle++
		}

		agent.RegisteredAt = ab.CreatedAt
		agents = append(agents, agent)
		wizards++
	}

	summary := RosterSummary{
		Agents:  agents,
		Wizards: wizards,
		Busy:    busy,
		Idle:    idle,
		Offline: offline,
	}

	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	}

	printRoster(summary)
	return nil
}

func printRoster(s RosterSummary) {
	if len(s.Agents) == 0 {
		fmt.Printf("%sTOWER ROSTER — empty%s\n", bold, reset)
		fmt.Printf("\n%sNo wizards summoned. Use %sspire summon N%s to conjure capacity.%s\n", dim, reset+bold, reset+dim, reset)
		return
	}

	fmt.Printf("%sTOWER ROSTER%s — %d wizard(s)", bold, reset, s.Wizards)
	fmt.Println()
	fmt.Println()

	for _, a := range s.Agents {
		icon := ""
		statusStr := ""
		switch a.Status {
		case "working":
			icon = cyan + "◐" + reset
			statusStr = fmt.Sprintf("%sworking%s", cyan, reset)
		case "idle":
			icon = green + "○" + reset
			statusStr = fmt.Sprintf("%sidle%s", green, reset)
		case "offline":
			icon = dim + "×" + reset
			statusStr = fmt.Sprintf("%soffline%s", dim, reset)
		}

		fmt.Printf("  %s %-12s %-10s", icon, a.Name, statusStr)

		if a.BeadID != "" {
			fmt.Printf("  %-12s %s", a.BeadID, truncate(a.BeadTitle, 30))
			if a.Elapsed != "" {
				fmt.Printf("  %s%s%s", dim, a.Elapsed, reset)
			}
		} else {
			fmt.Printf("  %s—%s", dim, reset)
		}
		fmt.Println()
	}

	fmt.Println()
	fmt.Printf("Capacity: %d/%d busy", s.Busy, s.Wizards)
	if s.Idle > 0 {
		fmt.Printf(" (%s%d idle%s)", green, s.Idle, reset)
	}
	fmt.Println()
}
