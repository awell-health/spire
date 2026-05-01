// Package process provides general-purpose PID utilities and a shared
// background-process spawning helper used by dolt server, daemon, and steward.
package process

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ReadPID reads a PID from a file. Returns 0 if file does not exist or is invalid.
func ReadPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// WritePID writes a PID to a file.
func WritePID(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0644)
}

// ProcessAlive checks if a process with the given PID is running and
// not in a zombie/defunct state. Plain kill -0 (Signal 0) returns success
// for zombies because the kernel keeps an entry until the parent reaps
// it; that masked dead wizards as alive on the board and in the orphan
// sweep. We additionally consult a platform-specific isZombie probe
// (Linux: /proc/<pid>/stat; macOS: sysctl kern.proc.pid) and report dead
// when the process is zombified.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. Use kill -0 to check.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return !isZombie(pid)
}

// StopProcess stops a process by PID with SIGTERM then SIGKILL.
// Removes the PID file when done.
func StopProcess(pidPath string) (bool, error) {
	pid := ReadPID(pidPath)
	if pid <= 0 || !ProcessAlive(pid) {
		if pid > 0 {
			os.Remove(pidPath)
		}
		return false, nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(pidPath)
		return false, nil
	}

	_ = proc.Signal(syscall.SIGTERM)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessAlive(pid) {
			os.Remove(pidPath)
			return true, nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	_ = proc.Signal(syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	os.Remove(pidPath)
	return true, nil
}
