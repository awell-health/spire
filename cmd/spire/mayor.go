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

func cmdMayor(args []string) error {
	// Parse flags
	interval := 2 * time.Minute
	staleThreshold := 4 * time.Hour
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
			return fmt.Errorf("unknown flag: %s\nusage: spire mayor [--once] [--dry-run] [--interval 2m] [--stale-threshold 4h] [--agents a,b,c]", args[i])
		}
	}

	log.Printf("[mayor] starting (interval=%s, once=%v, dry-run=%v, stale-threshold=%s)", interval, once, dryRun, staleThreshold)
	if len(agentList) > 0 {
		log.Printf("[mayor] agents: %s", strings.Join(agentList, ", "))
	}

	// Run first cycle immediately
	mayorCycle(dryRun, staleThreshold, agentList)

	if once {
		log.Printf("[mayor] --once mode, exiting")
		return nil
	}

	// Set up signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			mayorCycle(dryRun, staleThreshold, agentList)
		case sig := <-sigCh:
			log.Printf("[mayor] received %s, shutting down", sig)
			return nil
		}
	}
}

// mayorCycle executes one mayor cycle: sync, find ready work, assign, check stale, push.
func mayorCycle(dryRun bool, staleThreshold time.Duration, agentList []string) {
	log.Printf("[mayor] cycle start")

	// Step 1: Pull latest state
	_, err := bd("dolt", "pull")
	if err != nil {
		if !strings.Contains(err.Error(), "no remotes") {
			log.Printf("[mayor] pull warning: %s", err)
		}
	}

	// Step 2: Find ready work (unblocked, unassigned)
	var ready []Bead
	err = bdJSON(&ready, "ready")
	if err != nil {
		log.Printf("[mayor] bd ready: %s", err)
		pushState()
		return
	}

	log.Printf("[mayor] found %d ready bead(s)", len(ready))

	// Step 3: Load roster of available agents
	roster := loadRoster(agentList)
	if len(roster) == 0 {
		log.Printf("[mayor] no agents available, skipping assignment")
		checkStaleBeads(staleThreshold, dryRun)
		pushState()
		return
	}

	// Step 4: Find busy agents (those with in_progress beads)
	busy := findBusyAgents()
	log.Printf("[mayor] roster: %d agent(s), %d busy", len(roster), len(busy))

	// Step 5: Assign ready beads to idle agents (round-robin)
	assigned := 0
	agentIdx := 0
	for _, bead := range ready {
		// Skip beads that already have an owner
		if hasLabel(bead, "owner:") != "" {
			continue
		}

		// Find next idle agent (round-robin)
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
			log.Printf("[mayor] no idle agents for %s: %s", bead.ID, bead.Title)
			continue
		}

		if dryRun {
			log.Printf("[mayor] [dry-run] would assign %s (%s) to %s", bead.ID, bead.Title, agent)
			assigned++
			continue
		}

		// Send assignment message
		msg := fmt.Sprintf("Please claim and work on %s: %s", bead.ID, bead.Title)
		sendArgs := []string{
			"send", agent, msg,
			"--ref", bead.ID,
			"-p", strconv.Itoa(bead.Priority),
			"--as", "mayor",
		}
		_, sendErr := runSpire(sendArgs...)
		if sendErr != nil {
			log.Printf("[mayor] send to %s for %s: %s", agent, bead.ID, sendErr)
			continue
		}

		log.Printf("[mayor] assigned %s (%s) to %s", bead.ID, bead.Title, agent)
		busy[agent] = true // mark busy for this cycle
		assigned++
	}

	log.Printf("[mayor] assigned %d bead(s)", assigned)

	// Step 6: Check for stale beads
	checkStaleBeads(staleThreshold, dryRun)

	// Step 7: Push state
	pushState()

	log.Printf("[mayor] cycle complete")
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
		log.Printf("[mayor] load roster: %s", err)
		return nil
	}

	var names []string
	for _, a := range agents {
		name := hasLabel(a, "name:")
		if name != "" {
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
		log.Printf("[mayor] find busy agents: %s", err)
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
func checkStaleBeads(threshold time.Duration, dryRun bool) {
	var inProgress []Bead
	err := bdJSON(&inProgress, "list", "--status=in_progress")
	if err != nil {
		log.Printf("[mayor] check stale: %s", err)
		return
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
				log.Printf("[mayor] [dry-run] stale: %s (%s) owner=%s age=%s", b.ID, b.Title, owner, age.Round(time.Minute))
			} else {
				log.Printf("[mayor] WARNING stale: %s (%s) owner=%s age=%s", b.ID, b.Title, owner, age.Round(time.Minute))
			}
			staleCount++
		}
	}

	if staleCount > 0 {
		log.Printf("[mayor] %d stale bead(s) detected", staleCount)
	}
}

// pushState pushes state to DoltHub, logging any errors.
func pushState() {
	_, err := bd("dolt", "push")
	if err != nil {
		if !strings.Contains(err.Error(), "no remotes") {
			log.Printf("[mayor] push warning: %s", err)
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
