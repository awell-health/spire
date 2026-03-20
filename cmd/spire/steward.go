package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func cmdSteward(args []string) error {
	// Parse flags
	interval := 2 * time.Minute
	staleThreshold := 15 * time.Minute
	once := false
	dryRun := false
	var agentList []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Handle --flag=value syntax
		if strings.Contains(arg, "=") {
			parts := strings.SplitN(arg, "=", 2)
			arg = parts[0]
			// Insert value as next arg for uniform handling
			args = append(args[:i+1], append([]string{parts[1]}, args[i+1:]...)...)
			args[i] = arg
		}

		switch arg {
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value (e.g., 2m, 30s, 5m)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				secs, serr := strconv.Atoi(args[i])
				if serr != nil {
					return fmt.Errorf("--interval: invalid duration %q", args[i])
				}
				d = time.Duration(secs) * time.Second
			}
			interval = d
		case "--stale-threshold":
			if i+1 >= len(args) {
				return fmt.Errorf("--stale-threshold requires a value (e.g., 4h, 30m)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("--stale-threshold: invalid duration %q", args[i])
			}
			staleThreshold = d
		case "--once":
			once = true
		case "--dry-run":
			dryRun = true
		case "--agents":
			if i+1 >= len(args) {
				return fmt.Errorf("--agents requires a comma-separated list of agent names")
			}
			i++
			for _, a := range strings.Split(args[i], ",") {
				a = strings.TrimSpace(a)
				if a != "" {
					agentList = append(agentList, a)
				}
			}
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire steward [--once] [--dry-run] [--interval 2m] [--stale-threshold 4h] [--agents a,b,c]", args[i])
		}
	}

	log.Printf("[steward] starting (interval=%s, once=%v, dry-run=%v, stale-threshold=%s)", interval, once, dryRun, staleThreshold)
	if len(agentList) > 0 {
		log.Printf("[steward] agents: %s", strings.Join(agentList, ", "))
	}

	cycleNum := 1

	// Run first cycle immediately.
	stewardCycle(cycleNum, dryRun, staleThreshold, agentList)
	cycleNum++

	if once {
		return nil
	}

	// Set up signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			stewardCycle(cycleNum, dryRun, staleThreshold, agentList)
			cycleNum++
		case sig := <-sigCh:
			log.Printf("[steward] received %s, shutting down after %d cycles", sig, cycleNum-1)
			return nil
		}
	}
}

// stewardCycle executes one steward cycle: commit, pull, assess, assign, stale check, push.
func stewardCycle(cycleNum int, dryRun bool, staleThreshold time.Duration, agentList []string) {
	start := time.Now()
	log.Printf("[steward] ═══ cycle %d ═══════════════════════════════", cycleNum)

	// Step 1: Commit + Pull.
	_, _ = bd("dolt", "commit", "steward cycle sync")
	_, err := bd("dolt", "pull")
	if err != nil {
		if !strings.Contains(err.Error(), "no remotes") && !strings.Contains(err.Error(), "nothing to commit") {
			log.Printf("[steward] pull: %s", err)
		}
	} else {
		log.Printf("[steward] pull: synced")
	}

	// Step 2: Assess — find ready work.
	var ready []Bead
	err = bdJSON(&ready, "ready")
	if err != nil {
		log.Printf("[steward] ready: error — %s", err)
		pushState()
		log.Printf("[steward] ═══ cycle %d complete (%.1fs) ════════════════", cycleNum, time.Since(start).Seconds())
		return
	}

	// Step 3: Load roster.
	roster := loadRoster(agentList)
	busy := findBusyAgents()
	idleCount := len(roster) - len(busy)
	if idleCount < 0 {
		idleCount = 0
	}

	log.Printf("[steward] ready: %d beads | roster: %d wizard(s) (%d busy, %d idle)",
		len(ready), len(roster), len(busy), idleCount)

	if len(roster) == 0 {
		checkStaleBeads(staleThreshold, dryRun)
		pushState()
		log.Printf("[steward] ═══ cycle %d complete (%.1fs) ════════════════", cycleNum, time.Since(start).Seconds())
		return
	}

	// Step 4: Assign ready beads to idle agents (round-robin).
	assigned := 0
	agentIdx := 0
	for _, bead := range ready {
		// Skip message, template, and already-owned beads.
		if hasLabel(bead, "msg") != "" || containsLabel(bead, "msg") {
			continue
		}
		if containsLabel(bead, "template") {
			continue
		}
		if hasLabel(bead, "owner:") != "" {
			continue
		}

		// Find next idle agent (round-robin).
		agent := ""
		for attempts := 0; attempts < len(roster); attempts++ {
			candidate := roster[agentIdx%len(roster)]
			agentIdx++
			if !busy[candidate] {
				agent = candidate
				break
			}
		}

		if agent == "" {
			continue // all agents busy
		}

		if dryRun {
			log.Printf("[steward] [dry-run] would assign %s → %s", bead.ID, agent)
			assigned++
			continue
		}

		// Send assignment message.
		msg := fmt.Sprintf("Please claim and work on %s: %s", bead.ID, bead.Title)
		sendArgs := []string{
			"send", agent, msg,
			"--ref", bead.ID,
			"-p", strconv.Itoa(bead.Priority),
			"--as", "steward",
		}
		_, sendErr := runSpire(sendArgs...)
		if sendErr != nil {
			log.Printf("[steward] send failed: %s → %s: %s", bead.ID, agent, sendErr)
			continue
		}

		log.Printf("[steward] assigned: %s → %s (P%d)", bead.ID, agent, bead.Priority)
		busy[agent] = true
		assigned++
	}

	if assigned > 0 {
		log.Printf("[steward] assigned: %d bead(s)", assigned)
	}

	// Step 5: Stale check.
	staleCount := checkStaleBeads(staleThreshold, dryRun)
	if staleCount > 0 {
		log.Printf("[steward] stale: %d bead(s)", staleCount)
	} else {
		log.Printf("[steward] stale: none")
	}

	// Step 6: Push.
	pushState()

	log.Printf("[steward] ═══ cycle %d complete (%.1fs) ════════════════", cycleNum, time.Since(start).Seconds())
}

// loadRoster returns a list of registered agent names.
// If agentList is non-empty, use that directly. Otherwise query beads for agent registrations.
func loadRoster(agentList []string) []string {
	if len(agentList) > 0 {
		return agentList
	}

	// Query for registered agents (beads with label "agent", status open)
	var agents []Bead
	err := bdJSON(&agents, "list", "--rig=spi", "--label", "agent", "--status=open")
	if err != nil {
		log.Printf("[steward] load roster: %s", err)
		return nil
	}

	// Agents to exclude from assignment (steward itself, prefix artifacts)
	exclude := map[string]bool{
		"steward": true, "mayor": true, // coordinator — not a worker
		"spi":     true, // prefix artifact
		"awell":   true, // prefix artifact
	}

	var names []string
	for _, a := range agents {
		name := hasLabel(a, "name:")
		if name != "" && !exclude[name] {
			names = append(names, name)
		}
	}

	return names
}

// findBusyAgents returns a set of agent names that currently have in_progress beads.
func findBusyAgents() map[string]bool {
	busy := make(map[string]bool)

	var inProgress []Bead
	err := bdJSON(&inProgress, "list", "--status=in_progress")
	if err != nil {
		log.Printf("[steward] find busy agents: %s", err)
		return busy
	}

	for _, b := range inProgress {
		owner := hasLabel(b, "owner:")
		if owner != "" {
			busy[owner] = true
		}
	}

	return busy
}

// checkStaleBeads warns about beads that have been in_progress longer than the threshold.
func checkStaleBeads(threshold time.Duration, dryRun bool) int {
	var inProgress []Bead
	err := bdJSON(&inProgress, "list", "--status=in_progress")
	if err != nil {
		log.Printf("[steward] check stale: %s", err)
		return 0
	}

	now := time.Now()
	staleCount := 0

	for _, b := range inProgress {
		updatedAt := hasLabel(b, "updated:")
		if updatedAt == "" {
			continue
		}

		t, err := time.Parse(time.RFC3339, updatedAt)
		if err != nil {
			// Try other common formats
			t, err = time.Parse("2006-01-02 15:04:05", updatedAt)
			if err != nil {
				continue
			}
		}

		age := now.Sub(t)
		if age > threshold {
			owner := hasLabel(b, "owner:")
			if dryRun {
				log.Printf("[steward] [dry-run] stale: %s (%s) owner=%s age=%s", b.ID, b.Title, owner, age.Round(time.Minute))
			} else {
				log.Printf("[steward] WARNING stale: %s (%s) owner=%s age=%s", b.ID, b.Title, owner, age.Round(time.Minute))
			}
			staleCount++
		}
	}

	return staleCount
}

// pushState pushes state to DoltHub, logging any errors.
func pushState() {
	_, err := bd("dolt", "push")
	if err != nil {
		if !strings.Contains(err.Error(), "no remotes") {
			log.Printf("[steward] push warning: %s", err)
		}
	}
}

// runSpire runs a spire subcommand by calling the spire binary.
func runSpire(args ...string) (string, error) {
	// Find our own binary path to call ourselves
	exe, err := os.Executable()
	if err != nil {
		exe = "spire"
	}

	cmd := exec.Command(exe, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		return "", fmt.Errorf("spire %s: %s\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}
