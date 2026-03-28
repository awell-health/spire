// steward.go provides the thin CLI adapter for the steward command.
// Business logic lives in pkg/steward.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/steward"
)

// agentNames delegates to pkg/steward for backward compatibility.
func agentNames(agents []AgentInfo, override []string) []string {
	return steward.AgentNames(agents, override)
}

// busySet delegates to pkg/steward for backward compatibility.
func busySet(agents []AgentInfo) map[string]bool {
	return steward.BusySet(agents)
}

func cmdSteward(args []string) error {
	// Parse flags — staleThreshold left at zero to detect "not overridden".
	interval := 2 * time.Minute
	var staleOverride time.Duration
	once := false
	dryRun := false
	noAssign := false // skip sending assignment messages (managed agents get work via operator)
	backendName := "" // default: auto-resolve from ResolveBackend
	var agentList []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Handle --flag=value syntax
		if strings.Contains(arg, "=") {
			parts := strings.SplitN(arg, "=", 2)
			arg = parts[0]
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
				return fmt.Errorf("--stale-threshold requires a value (e.g., 15m, 30m)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("--stale-threshold: invalid duration %q", args[i])
			}
			staleOverride = d
		case "--once":
			once = true
		case "--dry-run":
			dryRun = true
		case "--no-assign":
			noAssign = true
		case "--backend":
			if i+1 >= len(args) {
				return fmt.Errorf("--backend requires a value: process, docker, or k8s")
			}
			i++
			switch args[i] {
			case "process", "docker", "k8s":
				backendName = args[i]
			default:
				return fmt.Errorf("--backend: unknown value %q (use: process, docker, k8s)", args[i])
			}
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
			return fmt.Errorf("unknown flag: %s\nusage: spire steward [--once] [--dry-run] [--interval 2m] [--stale-threshold 15m] [--backend process|docker|k8s] [--agents a,b,c]", args[i])
		}
	}

	// Read stale + shutdown (timeout) from spire.yaml.
	var staleThreshold, shutdownThreshold time.Duration
	cwd, _ := os.Getwd()
	if cfg, err := repoconfig.Load(cwd); err == nil {
		if cfg.Agent.Stale != "" {
			if d, err := time.ParseDuration(cfg.Agent.Stale); err == nil {
				staleThreshold = d
			}
		}
		if cfg.Agent.Timeout != "" {
			if d, err := time.ParseDuration(cfg.Agent.Timeout); err == nil {
				shutdownThreshold = d
			}
		}
	}
	if staleThreshold == 0 {
		staleThreshold = 10 * time.Minute
	}
	if shutdownThreshold == 0 {
		shutdownThreshold = 15 * time.Minute
	}
	// Explicit flag overrides config.
	if staleOverride > 0 {
		staleThreshold = staleOverride
	}

	backend := ResolveBackend(backendName)
	log.Printf("[steward] starting (backend=%s, interval=%s, once=%v, dry-run=%v, stale=%s, shutdown=%s)",
		backendName, interval, once, dryRun, staleThreshold, shutdownThreshold)
	if backendName == "" {
		log.Printf("[steward] backend auto-resolved to process")
	}
	if len(agentList) > 0 {
		log.Printf("[steward] agents: %s", strings.Join(agentList, ", "))
	}

	// Align project_id — only for the CWD tower (legacy behavior).
	ensureProjectID()

	cfg := steward.StewardConfig{
		DryRun:            dryRun,
		NoAssign:          noAssign,
		Backend:           backend,
		StaleThreshold:    staleThreshold,
		ShutdownThreshold: shutdownThreshold,
		AgentList:         agentList,
	}

	cycleNum := 1

	// Run first cycle immediately.
	steward.Cycle(cycleNum, cfg)
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
			steward.Cycle(cycleNum, cfg)
			cycleNum++
		case sig := <-sigCh:
			log.Printf("[steward] received %s, shutting down after %d cycles", sig, cycleNum-1)
			return nil
		}
	}
}

// --- Backward-compatible wrappers for callers elsewhere in cmd/spire ---

func sanitizeK8sLabel(s string) string       { return steward.SanitizeK8sLabel(s) }
func pushState()                              {}
func runSpire(args ...string) (string, error) { return steward.RunSpire(args...) }
func reviewBeadVerdict(b Bead) string         { return steward.ReviewBeadVerdict(b) }
func beadsDirForTower(name string) string     { return steward.BeadsDirForTower(name) }
