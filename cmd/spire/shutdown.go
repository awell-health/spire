package main

import "fmt"

func cmdShutdown(args []string) error {
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
		fmt.Println("daemon: stopped")
	} else {
		fmt.Println("daemon: not running")
	}

	// Stop dolt
	err := doltStop()
	if err != nil {
		fmt.Printf("dolt server: %s\n", err)
	} else {
		fmt.Println("dolt server: stopped")
	}

	return nil
}
