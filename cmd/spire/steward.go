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
	"sync"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/steward"
	"github.com/awell-health/spire/pkg/steward/dispatch"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"
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
		if v, _ := cmd.Flags().GetInt("max-concurrent"); v > 0 {
			fullArgs = append(fullArgs, "--max-concurrent", strconv.Itoa(v))
		}
		if v, _ := cmd.Flags().GetString("dispatched-timeout"); v != "" {
			fullArgs = append(fullArgs, "--dispatched-timeout", v)
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
	stewardCmd.Flags().Int("max-concurrent", 0, "Global cap on in-flight (dispatched+in_progress) beads; 0=unlimited")
	stewardCmd.Flags().String("dispatched-timeout", "", "Short stale timeout for dispatched beads (default 5m)")
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
	interval := 10 * time.Second
	var staleOverride time.Duration
	once := false
	dryRun := false
	noAssign := false // skip sending assignment messages (managed agents get work via operator)
	backendName := "" // default: auto-resolve from ResolveBackend
	metricsPort := 0  // 0 = disabled
	maxConcurrent := 0
	dispatchedTimeout := 5 * time.Minute
	var agentList []string

	// Env fallbacks (flags take precedence; parsed below).
	if v := os.Getenv("STEWARD_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxConcurrent = n
		}
	}
	if v := os.Getenv("STEWARD_DISPATCHED_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			dispatchedTimeout = d
		}
	}

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
		case "--max-concurrent":
			if i+1 >= len(args) {
				return fmt.Errorf("--max-concurrent requires a non-negative integer (0=unlimited)")
			}
			i++
			n, nErr := strconv.Atoi(args[i])
			if nErr != nil || n < 0 {
				return fmt.Errorf("--max-concurrent: invalid value %q", args[i])
			}
			maxConcurrent = n
		case "--dispatched-timeout":
			if i+1 >= len(args) {
				return fmt.Errorf("--dispatched-timeout requires a duration (e.g. 5m)")
			}
			i++
			d, dErr := time.ParseDuration(args[i])
			if dErr != nil {
				return fmt.Errorf("--dispatched-timeout: invalid duration %q", args[i])
			}
			dispatchedTimeout = d
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire steward [--once] [--dry-run] [--interval 2m] [--stale-threshold 15m] [--backend process|docker|k8s] [--agents a,b,c] [--metrics-port 9090]", args[i])
		}
	}

	// Read stale + shutdown (timeout) from spire.yaml.
	cwd, _ := os.Getwd()
	repoCfg, cfgErr := repoconfig.Load(cwd)
	if cfgErr != nil {
		log.Printf("[steward] config: repoconfig.Load(%s): %v — using threshold defaults unless flags override", cwd, cfgErr)
	}
	staleThreshold, shutdownThreshold, warnings := repoconfig.AgentTimeoutDurations(repoCfg)
	for _, warn := range warnings {
		log.Printf("[steward] config: %s", warn)
	}
	// Explicit flag overrides config.
	if staleOverride > 0 {
		staleThreshold = staleOverride
	}

	backend := ResolveBackend(backendName)
	log.Printf("[steward] starting (backend=%s, interval=%s, once=%v, dry-run=%v, stale=%s, shutdown=%s, dispatched-timeout=%s, max-concurrent=%d)",
		backendName, interval, once, dryRun, staleThreshold, shutdownThreshold, dispatchedTimeout, maxConcurrent)
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

	// Resolve the graph-state store using the active tower's identity.
	// The steward is a tower-scoped daemon — if no tower is bound, or if
	// a multi-prefix tower is ambiguous, we fall back to a local file
	// store and log the reason so the operator can fix it. (We do not
	// hard-fail: the steward should keep running in degraded
	// local-only mode rather than crash-looping.)
	//
	// Uses the global (empty-prefix) resolver explicitly — the steward
	// is tower-scoped, not bead-scoped. See spi-pwdhs5 Bug C: the
	// per-bead call sites use resolveGraphStateStoreForBeadOrLocal.
	graphStore, gsErr := resolveGlobalGraphStateStore()
	if gsErr != nil {
		log.Printf("[steward] graph-state store: %s (falling back to local file store)",
			friendlyIdentityError(gsErr))
		graphStore = &executor.FileGraphStateStore{ConfigDir: config.Dir}
	}

	cfg := steward.StewardConfig{
		DryRun:             dryRun,
		NoAssign:           noAssign,
		Backend:            backend,
		StaleThreshold:     staleThreshold,
		ShutdownThreshold:  shutdownThreshold,
		DispatchedTimeout:  dispatchedTimeout,
		AgentList:          agentList,
		MetricsPort:        metricsPort,
		GraphStateStore:    graphStore,
		ConcurrencyLimiter: concurrencyLimiter,
		MergeQueue:         mergeQueue,
		TrustChecker:       trustChecker,
		ABRouter:           abRouter,
		CycleStats:         cycleStats,
		// Lazy factory: cluster-native scheduler seams are built
		// against the per-tower DB that becomes available inside
		// TowerCycle (after StoreOpenAtFunc opens the tower's store).
		// Daemon startup does NOT populate ClusterDispatch itself —
		// capturing a store.ActiveDB() here would yield nil or a
		// stale connection across tower switches.
		BuildClusterDispatch: func(towerName string) *steward.ClusterDispatchConfig {
			cfg := buildClusterDispatch(towerName)
			if cfg != nil {
				cfg.MaxConcurrent = maxConcurrent
			}
			return cfg
		},
	}

	// Start metrics server if configured.
	var metricsServer *steward.MetricsServer
	if metricsPort > 0 {
		dsn := fmt.Sprintf("root:@tcp(%s:%s)/", doltHost(), doltPort())
		metricsDB, dbErr := sql.Open("mysql", dsn)
		if dbErr != nil {
			log.Printf("[steward] metrics db open: %s (metrics disabled)", dbErr)
		} else {
			metricsServer = steward.NewMetricsServer(metricsPort, metricsDB,
				steward.WithCycleStats(cycleStats),
				steward.WithMergeQueue(mergeQueue),
			)
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

func sanitizeK8sLabel(s string) string    { return steward.SanitizeK8sLabel(s) }
func pushState()                          {}
func reviewBeadVerdict(b Bead) string     { return steward.ReviewBeadVerdict(b) }
func beadsDirForTower(name string) string { return steward.BeadsDirForTower(name) }

// intentTableOnce guards EnsureWorkloadIntentsTable per-tower so the
// factory only issues the CREATE TABLE IF NOT EXISTS once per tower
// name across the steward's lifetime. Concurrent tower cycles on the
// same tower name therefore pay the DDL round-trip exactly once.
var intentTableOnce sync.Map // map[string]*sync.Once

func intentTableOnceFor(towerName string) *sync.Once {
	if v, ok := intentTableOnce.Load(towerName); ok {
		return v.(*sync.Once)
	}
	v, _ := intentTableOnce.LoadOrStore(towerName, &sync.Once{})
	return v.(*sync.Once)
}

// buildClusterDispatch is the lazy factory plumbed into
// steward.StewardConfig.BuildClusterDispatch. It runs inside
// TowerCycle's per-tower scope so store.ActiveDB() returns the
// connection for the tower currently being cycled (the cmdSteward
// caller does NOT own that connection — it's opened by StoreOpenAtFunc
// a few lines above the factory invocation).
//
// When the active DB is not available (e.g. the backing store is a
// test mock), the factory returns nil and the cycle falls back to the
// existing "ClusterDispatch is not configured" skip path — matching
// the shape the panic-fakes in mode_routing_test.go already rely on.
//
// The first call per tower also ensures the workload_intents table
// exists, so a freshly-deployed cluster does not need operator-side
// migrations to have completed first. EnsureWorkloadIntentsTable is
// idempotent; the sync.Once just avoids redundant DDL per cycle.
func buildClusterDispatch(towerName string) *steward.ClusterDispatchConfig {
	db, ok := store.ActiveDB()
	if !ok || db == nil {
		return nil
	}
	intentTableOnceFor(towerName).Do(func() {
		if err := intent.EnsureWorkloadIntentsTable(db); err != nil {
			log.Printf("[steward] cluster-native: ensure workload_intents table for tower %q: %s", towerName, err)
		}
	})
	agentName := "steward-" + config.InstanceID()
	return &steward.ClusterDispatchConfig{
		Resolver: &identity.DefaultClusterIdentityResolver{
			Registry: identity.NewSQLRegistryStore(db),
		},
		Claimer: &dispatch.StoreClaimer{
			AgentName: agentName,
		},
		Publisher: intent.NewDoltPublisher(db),
	}
}
