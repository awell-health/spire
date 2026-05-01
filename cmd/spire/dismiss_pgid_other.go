//go:build !unix

package main

// callerPGID returns 0 on platforms without process groups.
// dismissLocal treats this as "PGID check disabled" and signals
// every wizard regardless of group.
func callerPGID() int {
	return 0
}

// pgidForPID returns 0, nil on platforms without process groups.
func pgidForPID(_ int) (int, error) {
	return 0, nil
}

// pgidCheckSupported reports whether the platform exposes process
// groups via syscall.Getpgid. False on Windows / Plan 9.
func pgidCheckSupported() bool {
	return false
}
