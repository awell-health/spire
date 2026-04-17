package executor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runClaude invokes the injected ClaudeRunner with a per-invocation log file
// at <AgentResultDir>/claude/<label>-<ts>.log. The file path is echoed into
// the wizard log BEFORE the call so an operator can tail it if the wizard
// dies mid-invocation. Visibility is best-effort: if AgentResultDir is nil
// or the file can't be created we fall through to io.Discard and still run
// the subprocess, so a broken log dir never blocks real work.
func (e *Executor) runClaude(args []string, label string) ([]byte, error) {
	logOut := io.Discard
	var logPath string
	var logFile *os.File

	if e.deps != nil && e.deps.AgentResultDir != nil {
		base := e.deps.AgentResultDir(e.agentName)
		if base != "" {
			dir := filepath.Join(base, "claude")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				e.log("warning: mkdir claude log dir %s: %v", dir, err)
			} else {
				ts := time.Now().UTC().Format("20060102-150405")
				name := fmt.Sprintf("%s-%s.log", sanitizeLogLabel(label), ts)
				logPath = filepath.Join(dir, name)
				f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
				if err != nil {
					e.log("warning: open claude log %s: %v", logPath, err)
					logPath = ""
				} else {
					logFile = f
					logOut = f
				}
			}
		}
	}

	dir := e.effectiveRepoPath()
	if logPath != "" {
		e.log("claude invocation [%s] logging to %s", label, logPath)
	}

	if logFile != nil {
		fmt.Fprintf(logFile, "=== claude invocation ===\n")
		fmt.Fprintf(logFile, "label:  %s\n", label)
		fmt.Fprintf(logFile, "agent:  %s\n", e.agentName)
		fmt.Fprintf(logFile, "bead:   %s\n", e.beadID)
		fmt.Fprintf(logFile, "dir:    %s\n", dir)
		fmt.Fprintf(logFile, "time:   %s\n", time.Now().UTC().Format(time.RFC3339))
		fmt.Fprintf(logFile, "args:\n")
		for _, a := range args {
			fmt.Fprintln(logFile, a)
		}
		fmt.Fprintln(logFile)
		fmt.Fprintf(logFile, "=== stream ===\n")
	}

	started := time.Now()
	out, err := e.deps.ClaudeRunner(args, dir, logOut)
	dur := time.Since(started)

	if logFile != nil {
		fmt.Fprintf(logFile, "\n=== end (err=%v, duration=%s) ===\n", err, dur)
		logFile.Close()
	}

	return out, err
}

// sanitizeLogLabel normalizes a semantic label into a filesystem-safe token.
// Slashes, spaces, and colons become dashes; everything else is passed
// through (labels are caller-controlled and short).
func sanitizeLogLabel(s string) string {
	r := strings.NewReplacer("/", "-", " ", "-", ":", "-")
	out := r.Replace(s)
	if out == "" {
		return "claude"
	}
	return out
}
