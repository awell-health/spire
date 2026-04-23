//go:build unix

package agent

import (
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
