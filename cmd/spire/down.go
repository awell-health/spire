package main

import "fmt"

func cmdDown(args []string) error {
	stopped, _ := stopProcess(daemonPIDPath())
	if stopped {
		fmt.Println("daemon: stopped (dolt still running)")
	} else {
		fmt.Println("daemon: not running")
	}
	return nil
}
