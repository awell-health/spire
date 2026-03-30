package steward

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/repoconfig"
)

// LocalConfig holds agent settings for local steward mode.
// Sourced from spire.yaml (preferred) or tower config — not from k8s CRs.
type LocalConfig struct {
	Model         string
	MaxTurns      int
	Timeout       time.Duration
	BaseBranch    string
	BranchPattern string
}

// IsInK8s returns true when running inside a Kubernetes cluster.
// Detected by the presence of the service-account token that k8s mounts
// into every pod.
func IsInK8s() bool {
	_, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token")
	return err == nil
}

// LoadLocalConfig reads agent configuration from spire.yaml,
// walking up from the current working directory.
// Zero values mean "unset" — the consumer (wizard.go, repoconfig) decides defaults.
//
// Tower config (e.g. ~/.config/spire/towers/<name>.json) is intentionally not
// read here: its schema is not yet defined. When a tower config format is
// specified, this function should check it as a fallback after spire.yaml.
func LoadLocalConfig() *LocalConfig {
	cfg := &LocalConfig{}

	cwd, _ := os.Getwd()
	rc, err := repoconfig.Load(cwd)
	if err != nil || rc == nil {
		return cfg
	}

	cfg.Model = rc.Agent.Model
	cfg.MaxTurns = rc.Agent.MaxTurns
	if rc.Agent.Timeout != "" {
		if d, err := time.ParseDuration(rc.Agent.Timeout); err == nil {
			cfg.Timeout = d
		}
	}
	cfg.BaseBranch = rc.Branch.Base
	cfg.BranchPattern = rc.Branch.Pattern

	return cfg
}

// WizardPIDPath returns the PID file path for a locally-running wizard.
// Stored in the global spire directory alongside the daemon and dolt PID files.
func WizardPIDPath(name string) string {
	return filepath.Join(dolt.GlobalDir(), fmt.Sprintf("wizard-%s.pid", name))
}

// IsWizardRunning returns true if a locally-spawned wizard's process is alive.
func IsWizardRunning(name string) bool {
	pid := dolt.ReadPID(WizardPIDPath(name))
	return pid > 0 && dolt.ProcessAlive(pid)
}

// RecordWizardPID writes the PID of a spawned wizard to its PID file.
func RecordWizardPID(name string, pid int) {
	if err := dolt.WritePID(WizardPIDPath(name), pid); err != nil {
		log.Printf("[steward] record wizard PID %s: %s", name, err)
	}
}

// ClearWizardPID removes the PID file for a wizard (called on exit/kill).
func ClearWizardPID(name string) {
	os.Remove(WizardPIDPath(name))
}
