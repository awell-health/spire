//go:build unix

package main

import (
	"os"
	"syscall"
)

// callerPGID returns the process group ID of the calling process.
// dismissLocal uses this as the right-to-signal correlate for the
// PGID-sandbox check (spi-e16f5t).
func callerPGID() int {
	pgid, _ := syscall.Getpgid(os.Getpid())
	return pgid
}

// pgidForPID returns the process group ID of pid. A non-nil error
// means the process is gone or we can't see it — the caller treats
// that as "not ours, refuse to signal."
func pgidForPID(pid int) (int, error) {
	return syscall.Getpgid(pid)
}

// pgidCheckSupported reports whether the platform exposes process
// groups via syscall.Getpgid. Always true on unix.
func pgidCheckSupported() bool {
	return true
}
