// steward.go provides the thin CLI adapter for the steward command.
// Business logic lives in pkg/steward.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/steward"
	"github.com/spf13/cobra"
)

var stewardCmd = &cobra.Command{
	Use:   "steward",
	Short: "Run work coordinator (--once, --dry-run)",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if v, _ := cmd.Flags().GetString("interval"); v != "" {
			fullArgs = append(fullArgs, "--interval", v)
		}
		if v, _ := cmd.Flags().GetString("stale-threshold"); v != "" {
			fullArgs = append(fullArgs, "--stale-threshold", v)
		}
		if once, _ := cmd.Flags().GetBool("once"); once {
			fullArgs = append(fullArgs, "--once")
		}
		if dryRun, _ := cmd.Flags().GetBool("dry-run"); dryRun {
			fullArgs = append(fullArgs, "--dry-run")
		}
		if noAssign, _ := cmd.Flags().GetBool("no-assign"); noAssign {
			fullArgs = append(fullArgs, "--no-assign")
		}
		if v, _ := cmd.Flags().GetString("backend"); v != "" {
			fullArgs = append(fullArgs, "--backend", v)
		}
		if v, _ := cmd.Flags().GetString("agents"); v != "" {
			fullArgs = append(fullArgs, "--agents", v)
		}
		if v, _ := cmd.Flags().GetInt("metrics-port"); v > 0 {
			fullArgs = append(fullArgs, "--metrics-port", strconv.Itoa(v))
		}
		return cmdSteward(fullArgs)
	},
}

func init() {
	stewardCmd.Flags().String("interval", "", "Cycle interval (e.g. 2m, 30s)")
	stewardCmd.Flags().String("stale-threshold", "", "Stale agent threshold")
	stewardCmd.Flags().Bool("once", false, "Run one cycle and exit")
	stewardCmd.Flags().Bool("dry-run", false, "Print actions without executing")
	stewardCmd.Flags().Bool("no-assign", false, "Skip sending assignment messages")
	stewardCmd.Flags().String("backend", "", "Agent backend: process, docker, or k8s")
	stewardCmd.Flags().String("agents", "", "Comma-separated agent names")
	stewardCmd.Flags().Int("metrics-port", 0, "Expose Prometheus metrics on this port (0=disabled)")
}

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
	metricsPort := 0  // 0 = disabled
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
		case "--metrics-port":
			if i+1 >= len(args) {
				return fmt.Errorf("--metrics-port requires a port number")
			}
			i++
			p, pErr := strconv.Atoi(args[i])
			if pErr != nil || p < 0 || p > 65535 {
				return fmt.Errorf("--metrics-port: invalid port %q", args[i])
			}
			metricsPort = p
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire steward [--once] [--dry-run] [--interval 2m] [--stale-threshold 15m] [--backend process|docker|k8s] [--agents a,b,c] [--metrics-port 9090]", args[i])
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

	// Initialize wave-0 modules.
	concurrencyLimiter := steward.NewConcurrencyLimiter()
	mergeQueue := steward.NewMergeQueue()
	trustChecker := steward.NewTrustChecker()
	abRouter := steward.NewABRouter()
	cycleStats := steward.NewCycleStats()

	cfg := steward.StewardConfig{
		DryRun:             dryRun,
		NoAssign:           noAssign,
		Backend:            backend,
		StaleThreshold:     staleThreshold,
		ShutdownThreshold:  shutdownThreshold,
		AgentList:          agentList,
		MetricsPort:        metricsPort,
		ConcurrencyLimiter: concurrencyLimiter,
		MergeQueue:         mergeQueue,
		TrustChecker:       trustChecker,
		ABRouter:           abRouter,
		CycleStats:         cycleStats,
	}

	// Start metrics server if configured.
	var metricsServer *steward.MetricsServer
	if metricsPort > 0 {
		dsn := fmt.Sprintf("root:@tcp(%s:%s)/", doltHost(), doltPort())
		metricsDB, dbErr := sql.Open("mysql", dsn)
		if dbErr != nil {
			log.Printf("[steward] metrics db open: %s (metrics disabled)", dbErr)
		} else {
			metricsServer = steward.NewMetricsServer(metricsPort, metricsDB)
			if err := metricsServer.Start(); err != nil {
				log.Printf("[steward] metrics server: %s (metrics disabled)", err)
				metricsDB.Close()
				metricsServer = nil
			}
		}
	}

	cycleNum := 1

	// Run first cycle immediately.
	steward.Cycle(cycleNum, cfg)
	cycleNum++

	if once {
		if metricsServer != nil {
			metricsServer.Stop(context.Background())
		}
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
			if metricsServer != nil {
				metricsServer.Stop(context.Background())
			}
			return nil
		}
	}
}

// --- Backward-compatible wrappers for callers elsewhere in cmd/spire ---

func sanitizeK8sLabel(s string) string { return steward.SanitizeK8sLabel(s) }
func pushState()                       {}
func reviewBeadVerdict(b Bead) string  { return steward.ReviewBeadVerdict(b) }
func beadsDirForTower(name string) string     { return steward.BeadsDirForTower(name) }
