package main

import (
	"fmt"
	"os"
	"time"
)

func cmdStatus(args []string) error {
	// Dolt server
	pid, running, reachable := doltServerStatus()
	if running && reachable {
		fmt.Printf("dolt server: running (pid %d, port %s, reachable)\n", pid, doltPort())
	} else if running {
		fmt.Printf("dolt server: running (pid %d, port %s, NOT reachable)\n", pid, doltPort())
	} else if reachable {
		fmt.Printf("dolt server: running externally (port %s reachable, no PID file)\n", doltPort())
	} else {
		fmt.Println("dolt server: not running")
	}

	// Daemon
	daemonPID := readPID(daemonPIDPath())
	if daemonPID > 0 && processAlive(daemonPID) {
		fmt.Printf("spire daemon: running (pid %d)\n", daemonPID)
	} else {
		if daemonPID > 0 {
			fmt.Println("spire daemon: not running (stale PID file cleaned)")
			os.Remove(daemonPIDPath())
		} else {
			fmt.Println("spire daemon: not running")
		}
	}

	// Sync status per tower
	towers, err := listTowerConfigs()
	if err == nil && len(towers) > 0 {
		fmt.Println()
		for _, t := range towers {
			if t.DolthubRemote == "" {
				fmt.Printf("sync [%s]: no remote configured\n", t.Name)
				continue
			}
			state := readSyncState(t.Name)
			if state == nil || state.Remote != t.DolthubRemote {
				// No state, or stale state from a previous remote config.
				fmt.Printf("sync [%s]: never synced (%s)\n", t.Name, t.DolthubRemote)
				continue
			}
			age := formatSyncAge(state.At)
			switch state.Status {
			case "ok":
				fmt.Printf("sync [%s]: ok (%s ago) — %s\n", t.Name, age, state.Remote)
			case "pull_failed":
				fmt.Printf("sync [%s]: pull failed (%s ago) — %s\n", t.Name, age, state.Error)
			case "push_failed":
				fmt.Printf("sync [%s]: push failed (%s ago) — %s\n", t.Name, age, state.Error)
			default:
				fmt.Printf("sync [%s]: %s (%s ago)\n", t.Name, state.Status, age)
			}
		}
	}

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
