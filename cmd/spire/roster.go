package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// RosterAgent represents an agent registered in the tower.
type RosterAgent struct {
	Name         string        `json:"name"`
	Role         string        `json:"role"`           // "wizard", "artificer", "steward", "reviewer"
	Status       string        `json:"status"`         // "idle", "working", "offline"
	BeadID       string        `json:"bead_id"`        // current work (if working)
	BeadTitle    string        `json:"bead_title"`     // current work title
	Phase        string        `json:"phase"`          // current molecule phase (e.g. "implement", "review")
	Elapsed      time.Duration `json:"elapsed"`        // time on current work
	PhaseElapsed time.Duration `json:"phase_elapsed"`  // time in current phase
	Timeout      time.Duration `json:"timeout"`        // configured wizard timeout
	Remaining    time.Duration `json:"remaining"`      // time left (timeout - elapsed)
	RegisteredAt string        `json:"registered_at"`
}

// RosterSummary is the JSON output.
type RosterSummary struct {
	Agents  []RosterAgent `json:"agents"`
	Wizards int           `json:"wizards"`
	Busy    int           `json:"busy"`
	Idle    int           `json:"idle"`
	Offline int           `json:"offline"`
	Timeout time.Duration `json:"timeout"` // configured timeout from spire.yaml
}

// k8sPod is the subset of pod JSON we need.
type k8sPod struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Phase     string `json:"phase"`
		StartTime string `json:"startTime"`
	} `json:"status"`
}

func cmdRoster(args []string) error {
	flagJSON := false
	for _, arg := range args {
		switch arg {
		case "--json":
			flagJSON = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire roster [--json]", arg)
		}
	}

	// Load stale + timeout from repo config.
	cwd, _ := os.Getwd()
	cfg, _ := repoconfig.Load(cwd)
	stale := 10 * time.Minute    // guideline — when we flag it
	timeout := 15 * time.Minute  // kill — when we shut it down
	if cfg != nil {
		if cfg.Agent.Stale != "" {
			if d, err := time.ParseDuration(cfg.Agent.Stale); err == nil {
				stale = d
			}
		}
		if cfg.Agent.Timeout != "" {
			if d, err := time.ParseDuration(cfg.Agent.Timeout); err == nil {
				timeout = d
			}
		}
	}
	_ = stale // used in future enrichment for bar color thresholds

	// Try k8s first — it has real pod start times.
	if agents, err := rosterFromK8s(timeout); err == nil && len(agents) > 0 {
		summary := buildSummary(agents, timeout)
		if flagJSON {
			return jsonOut(summary)
		}
		printRoster(summary)
		return nil
	}

	// Local wizards from wizard registry (process mode).
	if localAgents := rosterFromLocalWizards(timeout); len(localAgents) > 0 {
		summary := buildSummary(localAgents, timeout)
		if flagJSON {
			return jsonOut(summary)
		}
		printRoster(summary)
		return nil
	}

	// Fallback: beads-based roster (no countdown, no pod times).
	agents := rosterFromBeads(timeout)
	summary := buildSummary(agents, timeout)
	if flagJSON {
		return jsonOut(summary)
	}
	printRoster(summary)
	return nil
}

// rosterFromK8s queries k8s for wizard pods and their start times.
func rosterFromK8s(timeout time.Duration) ([]RosterAgent, error) {
	cmd := exec.Command("kubectl", "get", "pods", "-n", "spire",
		"-l", "spire.awell.io/managed=true",
		"-o", "json")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var podList struct {
		Items []k8sPod `json:"items"`
	}
	if err := json.Unmarshal(out, &podList); err != nil {
		return nil, err
	}

	// Also get SpireAgent CRs for idle agents (those without pods).
	var agentNames []string
	if names, err := exec.Command("kubectl", "get", "spireagent", "-n", "spire",
		"-o", "jsonpath={.items[*].metadata.name}").Output(); err == nil {
		for _, n := range strings.Fields(strings.TrimSpace(string(names))) {
			if strings.HasPrefix(n, "wizard-") || strings.HasPrefix(n, "artificer") {
				agentNames = append(agentNames, n)
			}
		}
	}

	// Build agents from pods (working agents).
	podAgents := make(map[string]RosterAgent)
	for _, pod := range podList.Items {
		agentName := pod.Metadata.Labels["spire.awell.io/agent"]
		beadID := pod.Metadata.Labels["spire.awell.io/bead"]
		role := pod.Metadata.Labels["spire.awell.io/role"]
		if role == "" {
			role = "wizard"
		}

		agent := RosterAgent{
			Name:    agentName,
			Role:    role,
			Status:  "working",
			BeadID:  beadID,
			Timeout: timeout,
		}

		// Parse pod start time for countdown.
		if pod.Status.StartTime != "" {
			if t, err := time.Parse(time.RFC3339, pod.Status.StartTime); err == nil {
				agent.Elapsed = time.Since(t).Round(time.Second)
				agent.Remaining = timeout - agent.Elapsed
				if agent.Remaining < 0 {
					agent.Remaining = 0
				}
			}
		}

		// Pod phase.
		switch pod.Status.Phase {
		case "Succeeded", "Failed":
			agent.Status = "done"
		case "Pending":
			agent.Status = "provisioning"
		}

		podAgents[agentName] = agent
	}

	// Add idle agents (CRs without pods).
	var agents []RosterAgent
	for _, name := range agentNames {
		if a, ok := podAgents[name]; ok {
			agents = append(agents, a)
		} else {
			agents = append(agents, RosterAgent{
				Name:    name,
				Role:    "wizard",
				Status:  "idle",
				Timeout: timeout,
			})
		}
	}

	return agents, nil
}

// rosterFromBeads builds a roster from bead state (no k8s).
func rosterFromBeads(timeout time.Duration) []RosterAgent {
	agentBeads, err := storeListBoardBeads(beads.IssueFilter{
		Labels: []string{"agent"},
		Status: statusPtr(beads.StatusOpen),
	})
	if err != nil {
		allOpen, _ := storeListBoardBeads(beads.IssueFilter{
			Status: statusPtr(beads.StatusOpen),
		})
		var filtered []BoardBead
		for _, b := range allOpen {
			for _, l := range b.Labels {
				if l == "agent" {
					filtered = append(filtered, b)
					break
				}
			}
		}
		agentBeads = filtered
	}

	inProgress, _ := storeListBoardBeads(beads.IssueFilter{
		Status: statusPtr(beads.StatusInProgress),
	})

	ownerWork := make(map[string]BoardBead)
	for _, b := range inProgress {
		owner := beadOwnerLabel(b)
		if owner != "" {
			ownerWork[owner] = b
		}
	}

	exclude := map[string]bool{
		"steward": true, "mayor": true,
		"spi": true, "awell": true,
	}

	var agents []RosterAgent
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
		for _, l := range ab.Labels {
			if strings.Contains(l, "artificer") {
				role = "artificer"
			}
		}

		agent := RosterAgent{
			Name:         name,
			Role:         role,
			Timeout:      timeout,
			RegisteredAt: ab.CreatedAt,
		}

		if work, ok := ownerWork[name]; ok {
			agent.Status = "working"
			agent.BeadID = work.ID
			agent.BeadTitle = work.Title
			// Best effort elapsed from bead updated_at.
			if t, err := time.Parse(time.RFC3339, work.UpdatedAt); err == nil {
				agent.Elapsed = time.Since(t).Round(time.Second)
				agent.Remaining = timeout - agent.Elapsed
				if agent.Remaining < 0 {
					agent.Remaining = 0
				}
			}
		} else {
			agent.Status = "idle"
		}

		agents = append(agents, agent)
	}

	return agents
}

// rosterFromLocalWizards builds a roster from the local wizard registry (process mode).
func rosterFromLocalWizards(timeout time.Duration) []RosterAgent {
	reg := loadWizardRegistry()
	reg = cleanDeadWizards(reg)

	if len(reg.Wizards) == 0 {
		return nil
	}

	var agents []RosterAgent
	for _, w := range reg.Wizards {
		// Determine role from wizard name or phase.
		role := "wizard"
		if strings.Contains(w.Name, "-review") || w.Phase == "review" {
			role = "reviewer"
		}

		agent := RosterAgent{
			Name:    w.Name,
			Role:    role,
			BeadID:  w.BeadID,
			Timeout: timeout,
			Phase:   w.Phase, // empty string if old registry format
		}

		// Check if process is alive.
		isAlive := w.PID > 0 && processAlive(w.PID)

		if isAlive {
			agent.Status = "working"
			if t, err := time.Parse(time.RFC3339, w.StartedAt); err == nil {
				agent.Elapsed = time.Since(t).Round(time.Second)
				agent.Remaining = timeout - agent.Elapsed
				if agent.Remaining < 0 {
					agent.Remaining = 0
				}
			}
			// Phase elapsed time.
			if w.PhaseStartedAt != "" {
				if t, err := time.Parse(time.RFC3339, w.PhaseStartedAt); err == nil {
					agent.PhaseElapsed = time.Since(t).Round(time.Second)
				}
			}
		} else {
			agent.Status = "done"
		}

		agents = append(agents, agent)
	}
	return agents
}

func buildSummary(agents []RosterAgent, timeout time.Duration) RosterSummary {
	s := RosterSummary{Agents: agents, Timeout: timeout}
	for _, a := range agents {
		s.Wizards++
		switch a.Status {
		case "working", "provisioning":
			s.Busy++
		case "idle":
			s.Idle++
		case "offline":
			s.Offline++
		}
	}
	return s
}

func jsonOut(s RosterSummary) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func printRoster(s RosterSummary) {
	if len(s.Agents) == 0 {
		fmt.Printf("%sTOWER ROSTER — empty%s\n", bold, reset)
		fmt.Printf("\n%sNo wizards summoned. Use %sspire summon N%s to conjure capacity.%s\n", dim, reset+bold, reset+dim, reset)
		return
	}

	fmt.Printf("%sTOWER ROSTER%s — %d wizard(s), timeout %s\n", bold, reset, s.Wizards, s.Timeout)
	fmt.Println()

	for _, a := range s.Agents {
		icon := ""
		statusStr := ""
		switch a.Status {
		case "working":
			icon = cyan + "◐" + reset
			statusStr = fmt.Sprintf("%sworking%s", cyan, reset)
		case "provisioning":
			icon = yellow + "◔" + reset
			statusStr = fmt.Sprintf("%sprovisioning%s", yellow, reset)
		case "idle":
			icon = green + "○" + reset
			statusStr = fmt.Sprintf("%sidle%s", green, reset)
		case "done":
			icon = dim + "✓" + reset
			statusStr = fmt.Sprintf("%sdone%s", dim, reset)
		case "offline":
			icon = dim + "×" + reset
			statusStr = fmt.Sprintf("%soffline%s", dim, reset)
		}

		fmt.Printf("  %s %-12s %-14s", icon, a.Name, statusStr)

		if a.BeadID != "" {
			phaseStr := ""
			if a.Phase != "" {
				phaseStr = fmt.Sprintf("%s[%s]%s ", yellow, a.Phase, reset)
			}
			// Adjust title width based on phase label width.
			titleMax := 24
			if a.Phase != "" {
				titleMax = titleMax - len(a.Phase) - 3 // brackets + space
				if titleMax < 10 {
					titleMax = 10
				}
			}
			fmt.Printf("%-12s %s%s", a.BeadID, phaseStr, truncate(a.BeadTitle, titleMax))

			// Countdown bar.
			if a.Timeout > 0 && a.Elapsed > 0 {
				fmt.Printf("  %s", renderCountdown(a.Elapsed, a.Timeout))
			}
		} else {
			fmt.Printf("%s—%s", dim, reset)
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

// renderCountdown renders an elapsed/timeout bar like: 7m12s / 10m  ███████░░░
func renderCountdown(elapsed, timeout time.Duration) string {
	barWidth := 10
	ratio := float64(elapsed) / float64(timeout)
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}

	// Color: green < 70%, yellow 70-90%, red > 90%.
	barColor := green
	if ratio > 0.9 {
		barColor = red
	} else if ratio > 0.7 {
		barColor = yellow
	}

	remaining := timeout - elapsed
	if remaining < 0 {
		remaining = 0
	}

	elapsedStr := formatDuration(elapsed)
	timeoutStr := formatDuration(timeout)

	bar := fmt.Sprintf("%s%s%s%s",
		barColor, strings.Repeat("█", filled), reset,
		strings.Repeat("░", barWidth-filled))

	if remaining == 0 {
		return fmt.Sprintf("%s%s / %s%s  %s  %sOVERTIME%s", red, elapsedStr, timeoutStr, reset, bar, bold+red, reset)
	}

	return fmt.Sprintf("%s / %s  %s", elapsedStr, timeoutStr, bar)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
