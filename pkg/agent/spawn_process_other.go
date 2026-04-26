//go:build !unix

package agent

import (
	"errors"
	"os/exec"
	"syscall"
)

// applyDetachAttrs is a no-op on non-unix platforms. Setpgid has no
// equivalent on Windows; the wizard registry tracks the PID instead and
// reset falls back to per-PID signalling there.
func applyDetachAttrs(_ *exec.Cmd) {}

// pgidOf is unimplemented on non-unix platforms. Returns 0 so callers
// fall through to the per-PID terminate path. spi-w65pr1's
// process-group reap is unix-only by design — the bug only reproduces
// on platforms that actually detach child apprentices via Setpgid.
func pgidOf(_ int) (int, error) {
	return 0, errors.New("pgidOf: not supported on this platform")
}

// killPGID is unimplemented on non-unix platforms. Returns an error so
// the TerminateBead caller falls through to the per-PID fallback in
// signalTerminate / signalKill.
func killPGID(_ int, _ syscall.Signal) error {
	return errors.New("killPGID: not supported on this platform")
}

// pgidAlive is unimplemented on non-unix platforms. Returns false so
// the per-PID liveness probe in isAlive runs instead.
func pgidAlive(_ int) bool {
	return false
}
