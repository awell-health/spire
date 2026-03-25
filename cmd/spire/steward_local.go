package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
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

// loadLocalStewardConfig reads agent configuration from spire.yaml,
// walking up from the current working directory.
// Returns sensible defaults when no config file is found.
//
// Tower config (e.g. ~/.config/spire/towers/<name>.json) is intentionally not
// read here: its schema is not yet defined. When a tower config format is
// specified, this function should check it as a fallback after spire.yaml.
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
