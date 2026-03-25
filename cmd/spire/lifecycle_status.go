package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/beads"
)

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

	fmt.Printf("%sSPIRE STATUS%s\n\n", bold, reset)

	// --- Services ---
	fmt.Printf("%sServices%s\n", bold, reset)

	// Dolt server
	pid, running, reachable := doltServerStatus()
	if running && reachable {
		fmt.Printf("  %s●%s dolt server    %srunning%s  pid %d  port %s\n", green, reset, green, reset, pid, doltPort())
	} else if running {
		fmt.Printf("  %s●%s dolt server    %srunning (unreachable)%s  pid %d  port %s\n", yellow, reset, yellow, reset, pid, doltPort())
	} else if reachable {
		fmt.Printf("  %s●%s dolt server    %sexternal%s  port %s\n", green, reset, green, reset, doltPort())
	} else {
		fmt.Printf("  %s○%s dolt server    %sstopped%s\n", dim, reset, dim, reset)
	}

	// Daemon
	daemonPID := readPID(daemonPIDPath())
	daemonAlive := daemonPID > 0 && processAlive(daemonPID)
	if daemonAlive {
		fmt.Printf("  %s●%s daemon         %srunning%s  pid %d\n", green, reset, green, reset, daemonPID)
	} else {
		if daemonPID > 0 {
			os.Remove(daemonPIDPath())
		}
		fmt.Printf("  %s○%s daemon         %sstopped%s\n", dim, reset, dim, reset)
	}

	// Steward
	stewardPID := readPID(stewardPIDPath())
	stewardAlive := stewardPID > 0 && processAlive(stewardPID)
	if stewardAlive {
		fmt.Printf("  %s●%s steward        %srunning%s  pid %d\n", green, reset, green, reset, stewardPID)
	} else {
		if stewardPID > 0 {
			os.Remove(stewardPIDPath())
		}
		fmt.Printf("  %s○%s steward        %sstopped%s\n", dim, reset, dim, reset)
	}

	// --- Sync ---
	towers, err := listTowerConfigs()
	if err == nil && len(towers) > 0 {
		fmt.Printf("\n%sSync%s\n", bold, reset)
		for _, t := range towers {
			if t.DolthubRemote == "" {
				fmt.Printf("  %s—%s [%s]  no remote configured\n", dim, reset, t.Name)
				continue
			}
			state := readSyncState(t.Name)
			if state == nil || state.Remote != t.DolthubRemote {
				fmt.Printf("  %s?%s [%s]  never synced  %s%s%s\n", yellow, reset, t.Name, dim, t.DolthubRemote, reset)
				continue
			}
			age := formatSyncAge(state.At)
			switch state.Status {
			case "ok":
				fmt.Printf("  %s●%s [%s]  %sok%s  %s ago  %s%s%s\n", green, reset, t.Name, green, reset, age, dim, state.Remote, reset)
			case "pull_failed":
				fmt.Printf("  %s●%s [%s]  %spull failed%s  %s ago  %s\n", red, reset, t.Name, red, reset, age, state.Error)
			case "push_failed":
				fmt.Printf("  %s●%s [%s]  %spush failed%s  %s ago  %s\n", red, reset, t.Name, red, reset, age, state.Error)
			default:
				fmt.Printf("  %s?%s [%s]  %s  %s ago\n", yellow, reset, t.Name, state.Status, age)
			}
		}
	}

	// --- Agents ---
	backend := ResolveBackend("")
	agents, agentErr := backend.List()
	if agentErr == nil && len(agents) > 0 {
		fmt.Printf("\n%sAgents%s\n", bold, reset)
		fmt.Printf("  %-20s %-12s %-10s %-8s %s\n",
			dim+"NAME", "BEAD", "PHASE", "ID", "STATUS"+reset)
		for _, a := range agents {
			statusStr := fmt.Sprintf("%sdead%s", red, reset)
			statusIcon := red + "●" + reset
			if a.Alive {
				statusStr = fmt.Sprintf("%salive%s", green, reset)
				statusIcon = green + "●" + reset

				// Show elapsed time if we have start time.
				if !a.StartedAt.IsZero() {
					elapsed := time.Since(a.StartedAt).Round(time.Second)
					statusStr = fmt.Sprintf("%salive%s %s(%s)%s", green, reset, dim, formatDurationShort(elapsed), reset)
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
	}

	// --- Work Queue ---
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	readyBeads, readyErr := storeGetReadyWork(beads.WorkFilter{})
	inProgressBeads, ipErr := storeListBeads(beads.IssueFilter{
		Status: statusPtr(beads.StatusInProgress),
	})
	blockedBeads, blockedErr := storeGetBlockedIssues(beads.WorkFilter{})

	if readyErr == nil || ipErr == nil || blockedErr == nil {
		fmt.Printf("\n%sWork Queue%s\n", bold, reset)
		readyCount := 0
		if readyErr == nil {
			readyCount = len(readyBeads)
		}
		ipCount := 0
		if ipErr == nil {
			// Filter out message beads and workflow steps from in_progress count.
			for _, b := range inProgressBeads {
				if containsLabel(b, "msg") {
					continue
				}
				ipCount++
			}
		}
		blockedCount := 0
		if blockedErr == nil {
			blockedCount = len(blockedBeads)
		}
		fmt.Printf("  %s%d%s ready  %s%d%s in-progress  %s%d%s blocked\n",
			green, readyCount, reset,
			cyan, ipCount, reset,
			yellow, blockedCount, reset)
	}

	// --- Log paths ---
	gd := doltGlobalDir()
	fmt.Printf("\n%sLogs%s\n", bold, reset)

	// System logs (host services — always file-based).
	sysLogs := []struct {
		name string
		path string
	}{
		{"daemon", filepath.Join(gd, "daemon.log")},
		{"daemon (err)", filepath.Join(gd, "daemon.error.log")},
		{"steward", filepath.Join(gd, "steward.log")},
		{"steward (err)", filepath.Join(gd, "steward.error.log")},
		{"dolt", filepath.Join(gd, "dolt.log")},
	}
	for _, lf := range sysLogs {
		info, err := os.Stat(lf.path)
		if err != nil {
			fmt.Printf("  %s—%s %-16s %s(not found)%s\n", dim, reset, lf.name, dim, reset)
			continue
		}
		age := formatSyncAge(info.ModTime().Format(time.RFC3339))
		size := formatFileSize(info.Size())
		fmt.Printf("  %s●%s %-16s %s  modified %s ago\n", dim, reset, lf.name, size, age)
	}

	// Agent logs (discovered via backend).
	if agentErr == nil {
		for _, a := range agents {
			rc, logErr := backend.Logs(a.Name)
			if logErr != nil {
				continue
			}
			// If it's a file, show size/age info.
			if f, ok := rc.(*os.File); ok {
				info, err := f.Stat()
				rc.Close()
				if err != nil {
					continue
				}
				age := formatSyncAge(info.ModTime().Format(time.RFC3339))
				size := formatFileSize(info.Size())
				fmt.Printf("  %s●%s %-16s %s  modified %s ago\n", dim, reset, a.Name, size, age)
			} else {
				rc.Close()
				fmt.Printf("  %s●%s %-16s %s(stream)%s\n", dim, reset, a.Name, dim, reset)
			}
		}
	}

	fmt.Printf("\n  %sTip: spire logs [name] to tail a log%s\n", dim, reset)

	return nil
}

// formatSyncAge returns a human-readable duration since the given RFC3339 timestamp.
func formatSyncAge(timestamp string) string {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// formatDurationShort returns a compact duration string like "2m30s" or "1h5m".
func formatDurationShort(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// formatFileSize returns a human-readable file size.
func formatFileSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(bytes)/1024)
	case bytes < 1024*1024*1024:
		return fmt.Sprintf("%.1fM", float64(bytes)/(1024*1024))
	default:
		return fmt.Sprintf("%.1fG", float64(bytes)/(1024*1024*1024))
	}
}
