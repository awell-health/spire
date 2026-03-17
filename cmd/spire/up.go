package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func cmdUp(args []string) error {
	// Parse flags
	interval := "2m"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value")
			}
			i++
			interval = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire up [--interval 2m]", args[i])
		}
	}

	// Pre-check: .beads must exist
	if _, err := os.Stat(".beads"); os.IsNotExist(err) {
		return fmt.Errorf("no .beads directory — run `spire init` first")
	}

	// Step 1: Start dolt server
	fmt.Print("dolt server: ")
	pid, running, reachable := doltServerStatus()
	if running && reachable {
		fmt.Printf("already running (pid %d, port %s)\n", pid, doltPort())
	} else if reachable {
		fmt.Printf("running externally (port %s)\n", doltPort())
	} else {
		newPID, err := doltStart()
		if err != nil {
			fmt.Printf("error: %s\n", err)
			return fmt.Errorf("cannot start dolt server: %w", err)
		}
		fmt.Printf("started (pid %d, port %s)\n", newPID, doltPort())
	}

	// Step 1b: Ensure beads database exists on the dolt server
	dbName := detectDBName()
	if err := ensureDatabase(dbName); err != nil {
		// Non-fatal: bd init may handle this, or db may already exist
		fmt.Printf("  warning: could not ensure database %q: %s\n", dbName, err)
	}

	// Step 2: Start daemon
	fmt.Print("spire daemon: ")
	daemonPID := readPID(daemonPIDPath())
	if daemonPID > 0 && processAlive(daemonPID) {
		fmt.Printf("already running (pid %d)\n", daemonPID)
		return nil
	}

	// Remove stale PID file
	if daemonPID > 0 {
		os.Remove(daemonPIDPath())
	}

	// Find spire binary
	spireBin, err := os.Executable()
	if err != nil {
		spireBin, err = exec.LookPath("spire")
		if err != nil {
			return fmt.Errorf("cannot find spire binary")
		}
	}

	cmd := exec.Command(spireBin, "daemon", "--interval", interval)
	cmd.Dir, _ = os.Getwd()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()

	// Redirect daemon output to log files
	sd, _ := spireDir()
	logFile, _ := os.OpenFile(sd+"/daemon.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	errFile, _ := os.OpenFile(sd+"/daemon.error.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	cmd.Stdout = logFile
	cmd.Stderr = errFile

	if err := cmd.Start(); err != nil {
		fmt.Printf("error: %s\n", err)
		return fmt.Errorf("cannot start daemon: %w", err)
	}

	newPID := cmd.Process.Pid
	writePID(daemonPIDPath(), newPID)
	cmd.Process.Release()

	if logFile != nil {
		logFile.Close()
	}
	if errFile != nil {
		errFile.Close()
	}

	// Brief wait to confirm it stayed alive
	time.Sleep(500 * time.Millisecond)
	if processAlive(newPID) {
		fmt.Printf("started (pid %d, interval %s)\n", newPID, interval)
	} else {
		fmt.Printf("started but may have exited (pid %d)\n", newPID)
	}

	return nil
}
