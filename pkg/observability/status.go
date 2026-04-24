package observability

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
)

// ServiceStatus holds pre-fetched service status data.
type ServiceStatus struct {
	DoltPID       int
	DoltRunning   bool
	DoltReachable bool
	DoltPort      string

	DaemonPID    int
	DaemonAlive  bool

	StewardPID   int
	StewardAlive bool
}

// SyncState holds sync state for a tower.
type SyncState struct {
	Remote string
	At     string
	Status string // "ok", "pull_failed", "push_failed", "no_remote"
	Error  string
}

// SyncInfo holds tower sync information for status display.
type SyncInfo struct {
	Name   string
	Remote string // DolthubRemote from tower config
	State  *SyncState
}

// WorkQueueData holds work queue summary counts.
type WorkQueueData struct {
	ReadyCount      int
	InProgressCount int
	BlockedCount    int
	Available       bool // true if at least one query succeeded
}

// StewardHealthData holds health information fetched from the steward's metrics server.
type StewardHealthData struct {
	LastCycleAt     time.Time
	CycleDuration   time.Duration
	ActiveAgents    int
	QueueDepth      int
	SchedulableWork int
	TrustLevels     []TrustLevelInfo // per-repo trust levels
}

// TrustLevelInfo holds per-repo trust level information for display.
type TrustLevelInfo struct {
	RepoPrefix       string
	Level            int
	LevelName        string
	ConsecutiveClean int
}

// StatusData holds all data needed to render the lifecycle status display.
type StatusData struct {
	Services      ServiceStatus
	SyncInfos     []SyncInfo
	Agents        []agent.Info
	AgentErr      error
	WorkQueue     WorkQueueData
	GlobalDir     string
	Backend       agent.Backend
	StewardHealth *StewardHealthData // nil if steward health unavailable

	// UnboundPrefixes lists registered repo prefixes that have no local
	// binding on this machine. Populated from the active tower's
	// LocalBindings. Rendered as a warning row so operators see the
	// misconfiguration (spi-rpuzs6) before a wizard writes to the wrong
	// repo.
	UnboundPrefixes []string
}

// RenderStatus renders the full lifecycle status display to stdout.
func RenderStatus(data StatusData) error {
	fmt.Printf("%sSPIRE STATUS%s\n\n", Bold, Reset)

	// --- Services ---
	fmt.Printf("%sServices%s\n", Bold, Reset)
	renderDoltStatus(data.Services)
	renderDaemonStatus(data.Services)
	renderStewardStatus(data.Services)

	// --- Unbound prefixes warning ---
	if len(data.UnboundPrefixes) > 0 {
		renderUnboundPrefixes(data.UnboundPrefixes)
	}

	// --- Steward Health ---
	if data.StewardHealth != nil {
		renderStewardHealth(data.StewardHealth)
	}

	// --- Sync ---
	if len(data.SyncInfos) > 0 {
		fmt.Printf("\n%sSync%s\n", Bold, Reset)
		for _, si := range data.SyncInfos {
			renderSyncInfo(si)
		}
	}

	// --- Agents ---
	if data.AgentErr == nil && len(data.Agents) > 0 {
		fmt.Printf("\n%sAgents%s\n", Bold, Reset)
		fmt.Printf("  %-20s %-12s %-10s %-8s %s\n",
			Dim+"NAME", "BEAD", "PHASE", "ID", "STATUS"+Reset)
		for _, a := range data.Agents {
			renderAgentInfo(a)
		}
	}

	// --- Work Queue ---
	if data.WorkQueue.Available {
		fmt.Printf("\n%sWork Queue%s\n", Bold, Reset)
		fmt.Printf("  %s%d%s ready  %s%d%s in-progress  %s%d%s blocked\n",
			Green, data.WorkQueue.ReadyCount, Reset,
			Cyan, data.WorkQueue.InProgressCount, Reset,
			Yellow, data.WorkQueue.BlockedCount, Reset)
	}

	// --- Log paths ---
	fmt.Printf("\n%sLogs%s\n", Bold, Reset)
	renderLogFiles(data.GlobalDir)
	if data.AgentErr == nil && data.Backend != nil {
		renderAgentLogs(data.Agents, data.Backend)
	}
	fmt.Printf("\n  %sTip: spire logs [name] to tail a log%s\n", Dim, Reset)

	return nil
}

func renderDoltStatus(s ServiceStatus) {
	if s.DoltRunning && s.DoltReachable {
		fmt.Printf("  %s●%s dolt server    %srunning%s  pid %d  port %s\n",
			Green, Reset, Green, Reset, s.DoltPID, s.DoltPort)
	} else if s.DoltRunning {
		fmt.Printf("  %s●%s dolt server    %srunning (unreachable)%s  pid %d  port %s\n",
			Yellow, Reset, Yellow, Reset, s.DoltPID, s.DoltPort)
	} else if s.DoltReachable {
		fmt.Printf("  %s●%s dolt server    %sexternal%s  port %s\n",
			Green, Reset, Green, Reset, s.DoltPort)
	} else {
		fmt.Printf("  %s○%s dolt server    %sstopped%s\n", Dim, Reset, Dim, Reset)
	}
}

func renderDaemonStatus(s ServiceStatus) {
	if s.DaemonAlive {
		fmt.Printf("  %s●%s daemon         %srunning%s  pid %d\n",
			Green, Reset, Green, Reset, s.DaemonPID)
	} else {
		fmt.Printf("  %s○%s daemon         %sstopped%s\n", Dim, Reset, Dim, Reset)
	}
}

func renderStewardStatus(s ServiceStatus) {
	if s.StewardAlive {
		fmt.Printf("  %s●%s steward        %srunning%s  pid %d\n",
			Green, Reset, Green, Reset, s.StewardPID)
	} else {
		fmt.Printf("  %s○%s steward        %sstopped%s\n", Dim, Reset, Dim, Reset)
	}
}

// renderUnboundPrefixes surfaces prefixes with no local binding as a
// visible warning row. Wizards can't summon against these prefixes —
// the summon pre-flight will refuse, and the executor guard will fail
// closed. See spi-rpuzs6 for the underlying silent-fallback bug.
func renderUnboundPrefixes(prefixes []string) {
	fmt.Printf("  %s⚠%s unbound prefix%s  %s%s%s\n",
		Yellow, Reset, pluralize(len(prefixes)),
		Bold, strings.Join(prefixes, ", "), Reset)
	fmt.Printf("    %srun `spire repo bind <prefix> <path>` to register a local checkout%s\n",
		Dim, Reset)
}

func pluralize(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

func renderSyncInfo(si SyncInfo) {
	if si.Remote == "" {
		fmt.Printf("  %s—%s [%s]  no remote configured\n", Dim, Reset, si.Name)
		return
	}
	if si.State == nil || si.State.Remote != si.Remote {
		fmt.Printf("  %s?%s [%s]  never synced  %s%s%s\n",
			Yellow, Reset, si.Name, Dim, si.Remote, Reset)
		return
	}
	age := FormatSyncAge(si.State.At)
	switch si.State.Status {
	case "ok":
		fmt.Printf("  %s●%s [%s]  %sok%s  %s ago  %s%s%s\n",
			Green, Reset, si.Name, Green, Reset, age, Dim, si.State.Remote, Reset)
	case "pull_failed":
		fmt.Printf("  %s●%s [%s]  %spull failed%s  %s ago  %s\n",
			Red, Reset, si.Name, Red, Reset, age, si.State.Error)
	case "push_failed":
		fmt.Printf("  %s●%s [%s]  %spush failed%s  %s ago  %s\n",
			Red, Reset, si.Name, Red, Reset, age, si.State.Error)
	default:
		fmt.Printf("  %s?%s [%s]  %s  %s ago\n",
			Yellow, Reset, si.Name, si.State.Status, age)
	}
}

func renderAgentInfo(a agent.Info) {
	statusStr := fmt.Sprintf("%sdead%s", Red, Reset)
	statusIcon := Red + "●" + Reset
	if a.Alive {
		statusStr = fmt.Sprintf("%salive%s", Green, Reset)
		statusIcon = Green + "●" + Reset

		if !a.StartedAt.IsZero() {
			elapsed := time.Since(a.StartedAt).Round(time.Second)
			statusStr = fmt.Sprintf("%salive%s %s(%s)%s",
				Green, Reset, Dim, FormatDurationShort(elapsed), Reset)
		}
	}

	phase := a.Phase
	if phase == "" {
		phase = "-"
	}
	beadID := a.BeadID
	if beadID == "" {
		beadID = "-"
	}
	idStr := a.Identifier
	if idStr == "" || idStr == "0" {
		idStr = "-"
	}

	fmt.Printf("  %s %-18s %-12s %-10s %-8s %s\n",
		statusIcon, a.Name, beadID, phase, idStr, statusStr)
}

func renderLogFiles(globalDir string) {
	sysLogs := []struct {
		name string
		path string
	}{
		{"daemon", filepath.Join(globalDir, "daemon.log")},
		{"daemon (err)", filepath.Join(globalDir, "daemon.error.log")},
		{"steward", filepath.Join(globalDir, "steward.log")},
		{"steward (err)", filepath.Join(globalDir, "steward.error.log")},
		{"dolt", filepath.Join(globalDir, "dolt.log")},
	}
	for _, lf := range sysLogs {
		info, err := os.Stat(lf.path)
		if err != nil {
			fmt.Printf("  %s—%s %-16s %s(not found)%s\n", Dim, Reset, lf.name, Dim, Reset)
			continue
		}
		age := FormatSyncAge(info.ModTime().Format(time.RFC3339))
		size := FormatFileSize(info.Size())
		fmt.Printf("  %s●%s %-16s %s  modified %s ago\n", Dim, Reset, lf.name, size, age)
	}
}

func renderStewardHealth(h *StewardHealthData) {
	fmt.Printf("\n%sSteward Health%s\n", Bold, Reset)

	// Last cycle timing
	if !h.LastCycleAt.IsZero() {
		ago := time.Since(h.LastCycleAt).Round(time.Second)
		fmt.Printf("  last cycle:     %s ago (took %s)\n",
			FormatDurationShort(ago), FormatDurationShort(h.CycleDuration))
	} else {
		fmt.Printf("  last cycle:     %snever%s\n", Dim, Reset)
	}

	fmt.Printf("  active agents:  %d\n", h.ActiveAgents)
	fmt.Printf("  merge queue:    %d pending\n", h.QueueDepth)
	fmt.Printf("  schedulable:    %d beads\n", h.SchedulableWork)

	if len(h.TrustLevels) > 0 {
		fmt.Printf("  trust:\n")
		for _, t := range h.TrustLevels {
			fmt.Printf("    %-4s level %d (%s)   %d consecutive clean\n",
				t.RepoPrefix, t.Level, t.LevelName, t.ConsecutiveClean)
		}
	}
}

func renderAgentLogs(agents []agent.Info, backend agent.Backend) {
	for _, a := range agents {
		rc, logErr := backend.Logs(a.Name)
		if logErr != nil {
			continue
		}
		if f, ok := rc.(*os.File); ok {
			info, err := f.Stat()
			rc.Close()
			if err != nil {
				continue
			}
			age := FormatSyncAge(info.ModTime().Format(time.RFC3339))
			size := FormatFileSize(info.Size())
			fmt.Printf("  %s●%s %-16s %s  modified %s ago\n", Dim, Reset, a.Name, size, age)
		} else {
			rc.Close()
			fmt.Printf("  %s●%s %-16s %s(stream)%s\n", Dim, Reset, a.Name, Dim, Reset)
		}
	}
}
