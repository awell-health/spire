package observability

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/awell-health/spire/pkg/agent"
	"golang.org/x/term"
)

// TailFile tails a log file with the given line count and optional follow mode.
// In follow mode, it accepts q or Ctrl-C to quit cleanly without killing the
// parent process (safe to call from the board TUI's L action).
func TailFile(path string, lines int, follow bool) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("log file not found: %s", path)
	}

	fmt.Printf("%sTailing %s%s%s (%d lines)\n", Dim, Reset, path, Dim, lines)
	if follow {
		fmt.Printf("Press q to quit.%s\n\n", Reset)
	} else {
		fmt.Printf("%s\n\n", Reset)
	}

	tailArgs := []string{"-n", strconv.Itoa(lines)}
	if follow {
		tailArgs = append(tailArgs, "-f")
	}
	tailArgs = append(tailArgs, path)

	cmd := exec.Command("tail", tailArgs...)
	cmd.Stderr = os.Stderr

	if !follow {
		cmd.Stdout = os.Stdout
		return cmd.Run()
	}

	// Follow mode: interactive quit via q/Ctrl-C with signal trapping.
	return runFollowCmd(cmd)
}

// runFollowCmd starts cmd and waits for it interactively. It puts the
// terminal in raw mode for single-keystroke quit (q/Ctrl-C) and traps
// SIGINT so the parent process isn't killed.
func runFollowCmd(cmd *exec.Cmd) error {
	// Run tail in its own process group so terminal SIGINT doesn't reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	tailOut, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	// Trap SIGINT so the parent process survives.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	quit := make(chan struct{}, 1)

	// Try raw mode for single-keystroke reading.
	fd := int(os.Stdin.Fd())
	rawMode := false
	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err == nil {
			rawMode = true
			defer term.Restore(fd, oldState)
			go readQuitKey(quit)
		}
	}

	// Pipe tail output to stdout, translating \n -> \r\n in raw mode.
	copyDone := make(chan struct{})
	go func() {
		copyOutput(tailOut, rawMode)
		close(copyDone)
	}()

	// Wait for a reason to stop.
	killed := false
	select {
	case <-copyDone:
		// tail exited on its own (output pipe closed).
	case <-sigCh:
		cmd.Process.Signal(syscall.SIGTERM)
		killed = true
	case <-quit:
		cmd.Process.Signal(syscall.SIGTERM)
		killed = true
	}

	<-copyDone // Ensure all output is flushed before reaping.
	waitErr := cmd.Wait()
	if killed {
		return nil
	}
	return waitErr
}

// ListAvailableLogs returns a formatted string listing all available log files.
// System logs are listed from globalDir; agent logs are discovered via the
// Backend so that process, docker, and future backends all work.
func ListAvailableLogs(globalDir string, backend agent.Backend) string {
	var sb strings.Builder

	// System logs (host services — always file-based).
	topLogs := []struct {
		flag string
		name string
		path string
	}{
		{"--daemon", "daemon", filepath.Join(globalDir, "daemon.log")},
		{"--daemon", "daemon (err)", filepath.Join(globalDir, "daemon.error.log")},
		{"--steward", "steward", filepath.Join(globalDir, "steward.log")},
		{"--steward", "steward (err)", filepath.Join(globalDir, "steward.error.log")},
		{"--dolt", "dolt", filepath.Join(globalDir, "dolt.log")},
		{"--dolt", "dolt (err)", filepath.Join(globalDir, "dolt.error.log")},
	}

	for _, l := range topLogs {
		info, err := os.Stat(l.path)
		if err != nil {
			continue
		}
		age := FormatSyncAge(info.ModTime().Format("2006-01-02T15:04:05Z07:00"))
		size := FormatFileSize(info.Size())
		sb.WriteString(fmt.Sprintf("  %-20s %6s  modified %s ago  %s%s%s\n",
			l.name, size, age, Dim, l.flag, Reset))
	}

	// Agent logs (discovered via backend).
	agents, err := backend.List()
	if err == nil {
		for _, a := range agents {
			rc, logErr := backend.Logs(a.Name)
			if logErr != nil {
				continue
			}
			// If it's a file, show size/age metadata.
			if f, ok := rc.(*os.File); ok {
				info, statErr := f.Stat()
				rc.Close()
				if statErr != nil {
					continue
				}
				age := FormatSyncAge(info.ModTime().Format("2006-01-02T15:04:05Z07:00"))
				size := FormatFileSize(info.Size())
				sb.WriteString(fmt.Sprintf("  %-20s %6s  modified %s ago  %sspire logs %s%s\n",
					a.Name, size, age, Dim, a.Name, Reset))
			} else {
				rc.Close()
				sb.WriteString(fmt.Sprintf("  %-20s %s(stream)%s  %sspire logs %s%s\n",
					a.Name, Dim, Reset, Dim, a.Name, Reset))
			}
		}
	}

	return sb.String()
}

// StreamAgentLog copies an agent's log stream to stdout with interactive quit
// support. Accepts q or Ctrl-C to quit cleanly.
func StreamAgentLog(name string, rc io.ReadCloser) error {
	fmt.Printf("%sStreaming logs for %s%s%s\n", Dim, Reset, name, Reset)
	fmt.Printf("%sPress q to quit.%s\n\n", Dim, Reset)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	done := make(chan error, 1)
	quit := make(chan struct{}, 1)

	fd := int(os.Stdin.Fd())
	rawMode := false
	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err == nil {
			rawMode = true
			defer term.Restore(fd, oldState)
			go readQuitKey(quit)
		}
	}

	go func() {
		done <- copyOutput(rc, rawMode)
	}()

	select {
	case err := <-done:
		rc.Close()
		return err
	case <-sigCh:
		rc.Close()
		return nil
	case <-quit:
		rc.Close()
		return nil
	}
}

// readQuitKey reads single keystrokes from stdin and sends on quit when
// q, Q, or Ctrl-C (0x03) is pressed. Must be called after entering raw mode.
func readQuitKey(quit chan<- struct{}) {
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return
		}
		if buf[0] == 'q' || buf[0] == 'Q' || buf[0] == 0x03 {
			quit <- struct{}{}
			return
		}
	}
}

// copyOutput reads from r and writes to stdout. When rawMode is true,
// it translates \n to \r\n so output renders correctly in raw terminal mode.
func copyOutput(r io.Reader, rawMode bool) error {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := buf[:n]
			if rawMode {
				data = bytes.ReplaceAll(data, []byte{'\n'}, []byte{'\r', '\n'})
			}
			os.Stdout.Write(data)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
