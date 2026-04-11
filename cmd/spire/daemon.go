// daemon.go provides the thin CLI adapter for the daemon command.
// Business logic lives in pkg/steward.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/steward"
	"github.com/spf13/cobra"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run sync daemon (--interval, --once)",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if v, _ := cmd.Flags().GetString("interval"); v != "" {
			fullArgs = append(fullArgs, "--interval", v)
		}
		if once, _ := cmd.Flags().GetBool("once"); once {
			fullArgs = append(fullArgs, "--once")
		}
		return cmdDaemon(fullArgs)
	},
}

func init() {
	daemonCmd.Flags().String("interval", "", "Sync interval (e.g. 2m, 30s)")
	daemonCmd.Flags().Bool("once", false, "Run one cycle and exit")
}

// daemonDB is kept here for backward compatibility with doltSQL() in
// integration_bridge.go. pkg/steward sets steward.DaemonDB directly.
var daemonDB string

func init() {
	// Wire daemonDB so that integration_bridge.go's doltSQL sees per-tower state.
	// pkg/steward writes to steward.DaemonDB; we read it here for doltSQL.
	// This is a temporary bridge — eventually doltSQL callers should take db as param.
}

func cmdDaemon(args []string) error {
	// Parse flags
	interval := 2 * time.Minute
	once := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value (e.g., 2m, 30s, 5m)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				// Try parsing as plain seconds
				secs, serr := strconv.Atoi(args[i])
				if serr != nil {
					return fmt.Errorf("--interval: invalid duration %q", args[i])
				}
				d = time.Duration(secs) * time.Second
			}
			interval = d
		case "--once":
			once = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire daemon [--interval 2m] [--once]", args[i])
		}
	}

	log.Printf("[daemon] starting (interval=%s, once=%v)", interval, once)

	// Write our PID file so spire down can find us
	writePID(daemonPIDPath(), os.Getpid())

	// Run first cycle immediately
	steward.DaemonCycle()

	if once {
		log.Printf("[daemon] --once mode, exiting")
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
			steward.DaemonCycle()
		case sig := <-sigCh:
			log.Printf("[daemon] received %s, shutting down", sig)
			steward.StopOTLPReceiver()
			return nil
		}
	}
}

// --- Backward-compatible wrappers for callers elsewhere in cmd/spire ---

func syncTowerDerivedConfigs(tower TowerConfig) { steward.SyncTowerDerivedConfigs(tower) }
func runCycle()                                  { steward.DaemonCycle() }
func runTowerCycle(tower TowerConfig)            { steward.DaemonTowerCycle(tower) }
func runDoltSync(tower TowerConfig)              { steward.ExportRunDoltSync(tower) }
func processWebhookEvents() (int, int)           { return steward.ExportProcessWebhookEvents() }
func readSyncState(name string) *steward.SyncState { return steward.ReadSyncState(name) }
func deliverAgentInboxes() int                   { return steward.DeliverAgentInboxes() }
func reapDeadAgents(name string) int             { return steward.ReapDeadAgents(name) }
func ensureWebhookQueue()                        { steward.ExportEnsureWebhookQueue() }
