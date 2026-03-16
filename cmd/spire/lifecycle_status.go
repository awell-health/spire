package main

import (
	"fmt"
	"os"
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

	return nil
}
