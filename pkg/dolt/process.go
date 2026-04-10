package dolt

import "github.com/awell-health/spire/pkg/process"

// ReadPID reads a PID from a file. Returns 0 if file does not exist or is invalid.
func ReadPID(path string) int { return process.ReadPID(path) }

// WritePID writes a PID to a file.
func WritePID(path string, pid int) error { return process.WritePID(path, pid) }

// ProcessAlive checks if a process with the given PID is running.
func ProcessAlive(pid int) bool { return process.ProcessAlive(pid) }

// StopProcess stops a process by PID with SIGTERM then SIGKILL.
// Removes the PID file when done.
func StopProcess(pidPath string) (bool, error) { return process.StopProcess(pidPath) }
