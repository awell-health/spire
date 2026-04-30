package agent

import (
	"log"
	"os"
	"runtime"
	"syscall"
)

// auditSendSignal logs a signal-send with goroutine stack trace before
// the actual signal delivery. Used at every signal-sending call site in
// pkg/agent so an operator can grep "[signal-audit]" in a log to find
// out which spire-internal code path is delivering signals to a wizard
// or apprentice. The source argument is a short call-site identifier.
func auditSendSignal(proc *os.Process, sig os.Signal, source string) {
	if proc == nil {
		log.Printf("[signal-audit] %s: nil proc target", source)
		return
	}
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	log.Printf("[signal-audit] sending %v to pid=%d source=%s\n%s", sig, proc.Pid, source, buf[:n])
}

// auditKillPGID logs a process-group-wide kill before delivery.
func auditKillPGID(pgid int, sig syscall.Signal, source string) {
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	log.Printf("[signal-audit] sending %v to pgid=-%d source=%s\n%s", sig, pgid, source, buf[:n])
}
