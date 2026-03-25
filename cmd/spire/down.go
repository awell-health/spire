package main

import "fmt"

func cmdDown(args []string) error {
	// Stop steward first (if running)
	stewardStopped, _ := stopProcess(stewardPIDPath())
	if stewardStopped {
		fmt.Println("steward: stopped")
	} else {
		fmt.Println("steward: not running")
	}

	// Stop daemon
	stopped, _ := stopProcess(daemonPIDPath())
	if stopped {
		fmt.Println("daemon: stopped (dolt still running)")
	} else {
		fmt.Println("daemon: not running")
	}
	return nil
}
