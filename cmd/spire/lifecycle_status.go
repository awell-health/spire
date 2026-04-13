package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/awell-health/spire/pkg/observability"
	"github.com/awell-health/spire/pkg/steward"
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

	// --- Gather steward health (from metrics server) ---
	var stewardHealth *observability.StewardHealthData
	if stewardAlive {
		metricsPort := steward.ReadMetricsPort()
		if health, err := queryStewardHealth(metricsPort); err == nil {
			stewardHealth = health
		}
	}

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
		SyncInfos:     syncInfos,
		Agents:        agents,
		AgentErr:      agentErr,
		WorkQueue:     wq,
		GlobalDir:     doltGlobalDir(),
		Backend:       backend,
		StewardHealth: stewardHealth,
	})
}

// queryStewardHealth fetches /health/detailed from the steward's metrics server
// and parses it into StewardHealthData.
func queryStewardHealth(port int) (*observability.StewardHealthData, error) {
	if port <= 0 {
		return nil, fmt.Errorf("no metrics port configured")
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health/detailed", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health endpoint returned %d", resp.StatusCode)
	}

	var raw map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	health := &observability.StewardHealthData{}

	if v, ok := raw["last_cycle_at"].(string); ok && v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			health.LastCycleAt = t
		}
	}
	if v, ok := raw["cycle_duration_ms"].(float64); ok {
		health.CycleDuration = time.Duration(int64(v)) * time.Millisecond
	}
	if v, ok := raw["active_agents"].(float64); ok {
		health.ActiveAgents = int(v)
	}
	if v, ok := raw["merge_queue_depth"].(float64); ok {
		health.QueueDepth = int(v)
	}
	if v, ok := raw["schedulable_work"].(float64); ok {
		health.SchedulableWork = int(v)
	}

	return health, nil
}
