//go:build unix

package agent

import (
	"errors"
	"os/exec"
	"syscall"
)

// applyDetachAttrs configures SysProcAttr so the child survives parent
// exit. Setpgid puts the child in its own process group; when the
// parent's controlling terminal closes (e.g. the shell that ran
// `spire summon` exits), SIGHUP is delivered to the parent's process
// group — not the child's — so the detached wizard keeps running.
func applyDetachAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// pgidOf returns the process group ID of pid. Must be called AFTER
// cmd.Start() returns — Setpgid only takes effect at exec, so the PGID
// is not stable until the child has been started. On detached spawns,
// the leader's PID equals its PGID once Setpgid is honored on
// macOS/Linux, but we still ask the kernel rather than assuming so the
// path also works for spawns that did not request Setpgid.
//
// Returns 0 and a non-nil error when the lookup fails (process gone,
// permission denied). Callers tolerate the zero value by skipping the
// PGID-keyed terminate path and falling through to the per-PID
// fallback.
func pgidOf(pid int) (int, error) {
	if pid <= 0 {
		return 0, errors.New("pgidOf: invalid pid")
	}
	return syscall.Getpgid(pid)
}

// killPGID sends sig to every process in the group identified by pgid.
// The negative-PID kill targets the entire group on both macOS and
// Linux. ESRCH (no such process) is folded into nil — by the time
// reset escalates from SIGTERM to SIGKILL the group may already be
// gone, and that's a success, not an error.
func killPGID(pgid int, sig syscall.Signal) error {
	if pgid <= 0 {
		return errors.New("killPGID: invalid pgid")
	}
	err := syscall.Kill(-pgid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// pgidAlive reports whether any process in the group identified by
// pgid is still running. Implemented as `kill(-pgid, 0)`: Signal 0
// performs no action but still validates that there exists at least
// one process in the group. ESRCH means the group is empty (success
// from the caller's perspective).
func pgidAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil
}
