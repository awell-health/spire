package process

import (
	"log"
	"os"
	"runtime"
	"syscall"
)

// AuditSendSignal wraps proc.Signal with a stack-trace log so an
// operator can identify which spire-internal code path delivered a
// given signal to a given PID. Used at every signal-sending site in
// spire so when an unexpected signal lands on a wizard/apprentice/dolt
// we can grep daemon.error.log + the CLI's stderr for "[signal-audit]"
// and see exactly where it came from. The source argument is a
// short string identifying the call site.
func AuditSendSignal(proc *os.Process, sig os.Signal, source string) error {
	if proc == nil {
		log.Printf("[signal-audit] %s: nil proc", source)
		return nil
	}
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	log.Printf("[signal-audit] sending %v to pid=%d source=%s\n%s", sig, proc.Pid, source, buf[:n])
	return proc.Signal(sig)
}

// AuditKillPGID wraps syscall.Kill(-pgid, sig) with the same audit
// log. Used by killPGID-equivalent paths to record process-group-wide
// signals.
func AuditKillPGID(pgid int, sig syscall.Signal, source string) error {
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	log.Printf("[signal-audit] sending %v to pgid=-%d source=%s\n%s", sig, pgid, source, buf[:n])
	return syscall.Kill(-pgid, sig)
}
