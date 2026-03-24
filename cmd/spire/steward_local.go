package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// StewardMode controls how the steward manages agents.
type StewardMode string

const (
	StewardModeAuto  StewardMode = "auto"
	StewardModeLocal StewardMode = "local"
	StewardModeK8s   StewardMode = "k8s"
)

// LocalStewardConfig holds agent settings for local steward mode.
// Sourced from spire.yaml (preferred) or tower config — not from k8s CRs.
type LocalStewardConfig struct {
	Model         string
	MaxTurns      int
	Timeout       time.Duration
	BaseBranch    string
	BranchPattern string
}

// isInK8s returns true when running inside a Kubernetes cluster.
// Detected by the presence of the service-account token that k8s mounts
// into every pod.
func isInK8s() bool {
	_, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token")
	return err == nil
}

// resolveMode returns the effective StewardMode.
// Auto selects k8s when running inside a cluster, local otherwise.
func resolveMode(mode StewardMode) StewardMode {
	if mode == StewardModeAuto {
		if isInK8s() {
			return StewardModeK8s
		}
		return StewardModeLocal
	}
	return mode
}

// loadLocalStewardConfig reads agent configuration from spire.yaml,
// walking up from the current working directory.
// Returns sensible defaults when no config file is found.
func loadLocalStewardConfig() *LocalStewardConfig {
	cfg := &LocalStewardConfig{
		Model:         "claude-sonnet-4-6",
		MaxTurns:      30,
		Timeout:       15 * time.Minute,
		BaseBranch:    "main",
		BranchPattern: "feat/{bead-id}",
	}

	cwd, _ := os.Getwd()
	rc, err := repoconfig.Load(cwd)
	if err != nil || rc == nil {
		return cfg
	}

	if rc.Agent.Model != "" {
		cfg.Model = rc.Agent.Model
	}
	if rc.Agent.MaxTurns > 0 {
		cfg.MaxTurns = rc.Agent.MaxTurns
	}
	if rc.Agent.Timeout != "" {
		if d, err := time.ParseDuration(rc.Agent.Timeout); err == nil {
			cfg.Timeout = d
		}
	}
	if rc.Branch.Base != "" {
		cfg.BaseBranch = rc.Branch.Base
	}
	if rc.Branch.Pattern != "" {
		cfg.BranchPattern = rc.Branch.Pattern
	}

	return cfg
}

// wizardPIDPath returns the PID file path for a locally-running wizard.
// Stored in the global spire directory alongside the daemon and dolt PID files.
func wizardPIDPath(name string) string {
	return filepath.Join(doltGlobalDir(), fmt.Sprintf("wizard-%s.pid", name))
}

// isWizardRunning returns true if a locally-spawned wizard's process is alive.
func isWizardRunning(name string) bool {
	pid := readPID(wizardPIDPath(name))
	return pid > 0 && processAlive(pid)
}

// recordWizardPID writes the PID of a spawned wizard to its PID file.
func recordWizardPID(name string, pid int) {
	if err := writePID(wizardPIDPath(name), pid); err != nil {
		log.Printf("[steward] record wizard PID %s: %s", name, err)
	}
}

// clearWizardPID removes the PID file for a wizard (called on exit/kill).
func clearWizardPID(name string) {
	os.Remove(wizardPIDPath(name))
}

// killLocalWizard sends SIGTERM to a locally-running wizard and removes its PID file.
func killLocalWizard(agentName, beadID string) {
	pid := readPID(wizardPIDPath(agentName))
	if pid <= 0 {
		log.Printf("[steward] kill local wizard %s/%s: no PID file", agentName, beadID)
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		clearWizardPID(agentName)
		log.Printf("[steward] kill local wizard %s: process not found (pid %d)", agentName, pid)
		return
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		log.Printf("[steward] kill local wizard %s (pid %d): %s", agentName, pid, err)
	} else {
		log.Printf("[steward] killed local wizard %s (pid %d) for bead %s", agentName, pid, beadID)
	}
	clearWizardPID(agentName)
}

// spawnLocalAgent spawns an agent locally for the given bead.
// Returns the spawned process PID (>0) or 0 if spawning is deferred.
//
// The execution backend is determined by subsequent tasks:
//   - spi-1dl.2: Docker container per assignment
//   - spi-1dl.3: claude CLI subprocess (--exec=process)
//
// This stub records the intent and returns 0 until a backend is wired in.
func spawnLocalAgent(wizardName, beadID string, cfg *LocalStewardConfig) (int, error) {
	// TODO(spi-1dl.2): Docker backend — create container, inject bead env
	// TODO(spi-1dl.3): Process backend — exec claude CLI in repo worktree
	log.Printf("[steward] local spawn: %s → %s (model=%s, max-turns=%d, timeout=%s) [pending: spi-1dl.2/spi-1dl.3]",
		wizardName, beadID, cfg.Model, cfg.MaxTurns, cfg.Timeout)
	return 0, nil
}

// localRoster returns the names of wizards tracked in the local wizard registry.
// These are created by `spire summon`. Dead wizards (no live process) are included
// so the steward can see open slots and spawn new processes for them.
func localRoster() []string {
	reg := loadWizardRegistry()
	var names []string
	for _, w := range reg.Wizards {
		names = append(names, w.Name)
	}
	return names
}

// localBusyAgents returns a set of wizard names that are currently busy.
// A wizard is busy if either:
//   - it has a live PID file (process is running), or
//   - it owns an in_progress bead (survives crashes and the spawn stub case
//     where no PID file is written yet).
//
// Used in place of findBusyAgents() in local mode.
func localBusyAgents() map[string]bool {
	busy := make(map[string]bool)

	// Signal 1: live PID file.
	reg := loadWizardRegistry()
	for _, w := range reg.Wizards {
		if isWizardRunning(w.Name) {
			busy[w.Name] = true
		}
	}

	// Signal 2: bead ownership — a wizard assigned a bead is busy even if its
	// process hasn't started yet or its PID file was lost after a crash.
	inProgress, err := storeListBeads(beads.IssueFilter{Status: statusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] localBusyAgents: bead ownership check failed: %s", err)
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
