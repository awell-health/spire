package main

import (
	"fmt"
	"os"

	"github.com/awell-health/spire/pkg/observability"
	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show services, agents, and work queue",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			fullArgs = append(fullArgs, "--json")
		}
		return cmdStatus(fullArgs)
	},
}

func init() {
	statusCmd.Flags().Bool("json", false, "Output as JSON")
}

func cmdStatus(args []string) error {
	// Parse flags.
	flagJSON := false
	for _, arg := range args {
		switch arg {
		case "--json":
			flagJSON = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire status [--json]", arg)
		}
	}

	_ = flagJSON // reserved for future JSON output

	// --- Gather service status ---
	pid, running, reachable := doltServerStatus()

	daemonPID := readPID(daemonPIDPath())
	daemonAlive := daemonPID > 0 && processAlive(daemonPID)
	if !daemonAlive && daemonPID > 0 {
		os.Remove(daemonPIDPath())
	}

	stewardPID := readPID(stewardPIDPath())
	stewardAlive := stewardPID > 0 && processAlive(stewardPID)
	if !stewardAlive && stewardPID > 0 {
		os.Remove(stewardPIDPath())
	}

	// --- Gather sync info ---
	var syncInfos []observability.SyncInfo
	towers, err := listTowerConfigs()
	if err == nil && len(towers) > 0 {
		for _, t := range towers {
			si := observability.SyncInfo{Name: t.Name, Remote: t.DolthubRemote}
			state := readSyncState(t.Name)
			if state != nil {
				si.State = &observability.SyncState{
					Remote: state.Remote,
					At:     state.At,
					Status: state.Status,
					Error:  state.Error,
				}
			}
			syncInfos = append(syncInfos, si)
		}
	}

	// --- Gather agents ---
	backend := ResolveBackend("")
	agents, agentErr := backend.List()

	// --- Gather work queue ---
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	readyBeads, readyErr := storeGetReadyWork(beads.WorkFilter{})
	inProgressBeads, ipErr := storeListBeads(beads.IssueFilter{
		Status: statusPtr(beads.StatusInProgress),
	})
	blockedBeads, blockedErr := storeGetBlockedIssues(beads.WorkFilter{})

	wq := observability.WorkQueueData{}
	if readyErr == nil || ipErr == nil || blockedErr == nil {
		wq.Available = true
		if readyErr == nil {
			wq.ReadyCount = len(readyBeads)
		}
		if ipErr == nil {
			for _, b := range inProgressBeads {
				if store.IsInternalBead(b) {
					continue
				}
				wq.InProgressCount++
			}
		}
		if blockedErr == nil {
			wq.BlockedCount = len(blockedBeads)
		}
	}

	return observability.RenderStatus(observability.StatusData{
		Services: observability.ServiceStatus{
			DoltPID:       pid,
			DoltRunning:   running,
			DoltReachable: reachable,
			DoltPort:      doltPort(),
			DaemonPID:     daemonPID,
			DaemonAlive:   daemonAlive,
			StewardPID:    stewardPID,
			StewardAlive:  stewardAlive,
		},
		SyncInfos: syncInfos,
		Agents:    agents,
		AgentErr:  agentErr,
		WorkQueue: wq,
		GlobalDir: doltGlobalDir(),
		Backend:   backend,
	})
}
