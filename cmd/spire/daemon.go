// daemon.go provides the thin CLI adapter for the daemon command.
// Business logic lives in pkg/steward (cycle + strategies) and pkg/gateway
// (HTTP). This file just parses flags and wires them together.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/gateway"
	"github.com/awell-health/spire/pkg/process"
	"github.com/awell-health/spire/pkg/steward"
	"github.com/spf13/cobra"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run sync daemon (laptop or cluster mode; optional --serve gateway)",
	RunE: func(cmd *cobra.Command, _ []string) error {
		interval, _ := cmd.Flags().GetDuration("interval")
		debounce, _ := cmd.Flags().GetDuration("debounce")
		once, _ := cmd.Flags().GetBool("once")
		database, _ := cmd.Flags().GetString("database")
		remote, _ := cmd.Flags().GetString("remote")
		branch, _ := cmd.Flags().GetString("branch")
		serve, _ := cmd.Flags().GetString("serve")
		return runDaemon(interval, debounce, once, database, remote, branch, serve)
	},
}

func init() {
	daemonCmd.Flags().Duration("interval", time.Minute, "Sync ticker interval (e.g. 1m, 30s)")
	daemonCmd.Flags().Duration("debounce", 5*time.Second, "Minimum gap between triggered syncs (HTTP /sync coalesces within this window)")
	daemonCmd.Flags().Bool("once", false, "Run one cycle and exit")
	daemonCmd.Flags().String("database", "", "Cluster mode: single dolt database to sync via SQL (leave empty for laptop multi-tower mode)")
	daemonCmd.Flags().String("remote", "origin", "Cluster mode: dolt remote name (default origin)")
	daemonCmd.Flags().String("branch", "main", "Cluster mode: branch to pull/push")
	daemonCmd.Flags().String("serve", "", "If set (e.g. :8082), also start a gateway HTTP server that forwards POST /sync to this daemon")
}

// daemonDB is kept here for backward compatibility with doltSQL() in
// integration_bridge.go. pkg/steward sets steward.DaemonDB directly.
var daemonDB string

func runDaemon(interval, debounce time.Duration, once bool, database, remote, branch, serve string) error {
	log.Printf("[daemon] starting (interval=%s, debounce=%s, once=%v, database=%q, serve=%q)",
		interval, debounce, once, database, serve)

	// Prevent a second local daemon from racing — only meaningful in laptop
	// mode; in cluster the Deployment is k8s-managed.
	if database == "" {
		lockPath := filepath.Join(doltGlobalDir(), "spire-daemon.lock")
		lock, lockErr := process.AcquireLock(lockPath)
		if lockErr != nil {
			return fmt.Errorf("daemon already running: %s", lockErr)
		}
		defer lock.Release()
		writePID(daemonPIDPath(), os.Getpid())
	}

	// Construct the right daemon for this mode.
	var d *steward.Daemon
	if database != "" {
		d = steward.NewClusterDaemon(database, remote, branch, interval, debounce, nil)
	} else {
		d = steward.NewLocalDaemon(interval, debounce, nil)
	}

	// --once bypasses the Run loop: run a single sync synchronously, exit.
	if once {
		return d.RunOnce("once")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Start gateway if --serve was set. Runs in the same process so it can
	// call d.Trigger directly (no RPC boundary needed here).
	if serve != "" {
		go func() {
			srv := gateway.NewServer(serve, d, nil, "", "")
			if err := srv.Run(ctx); err != nil {
				log.Printf("[gateway] exited: %s", err)
			}
		}()
	}

	return d.Run(ctx)
}

// --- Backward-compatible wrappers for callers elsewhere in cmd/spire ---

func syncTowerDerivedConfigs(tower TowerConfig)    { steward.SyncTowerDerivedConfigs(tower) }
func runCycle()                                    { steward.DaemonCycle() }
func runTowerCycle(tower TowerConfig)              { steward.DaemonTowerCycle(tower) }
func runDoltSync(tower TowerConfig)                { steward.ExportRunDoltSync(tower) }
func processWebhookEvents() (int, int)             { return steward.ExportProcessWebhookEvents() }
func readSyncState(name string) *steward.SyncState { return steward.ReadSyncState(name) }
func deliverAgentInboxes() int                     { return steward.DeliverAgentInboxes() }
func reapDeadAgents(name string) int               { return steward.ReapDeadAgents(name) }
func ensureWebhookQueue()                          { steward.ExportEnsureWebhookQueue() }

// parseDurationOrSeconds accepts either "2m" or "120" (bare integer → seconds).
// Kept for any legacy callers; new callers should use cobra's Duration flag.
func parseDurationOrSeconds(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	secs, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return time.Duration(secs) * time.Second, nil
}
