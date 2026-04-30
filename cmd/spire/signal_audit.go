package main

import (
	"log"
	"os"
	"runtime"
)

// auditSendSignal logs a signal-send with goroutine stack trace before
// the actual signal delivery. Used at every signal-sending call site in
// cmd/spire so an operator can grep "[signal-audit]" in a log to find
// out which spire CLI command path delivered signals to a wizard /
// apprentice / dolt. The source argument is a short call-site label.
func auditSendSignal(proc *os.Process, sig os.Signal, source string) {
	if proc == nil {
		log.Printf("[signal-audit] %s: nil proc target", source)
		return
	}
	var buf [4096]byte
	n := runtime.Stack(buf[:], false)
	log.Printf("[signal-audit] sending %v to pid=%d source=%s\n%s", sig, proc.Pid, source, buf[:n])
}
