package observability

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/agent"
)

// TailFile tails a log file with the given line count and optional follow mode.
func TailFile(path string, lines int, follow bool) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("log file not found: %s", path)
	}

	fmt.Printf("%sTailing %s%s%s (%d lines)\n", Dim, Reset, path, Dim, lines)
	if follow {
		fmt.Printf("Press Ctrl-C to stop.%s\n\n", Reset)
	} else {
		fmt.Printf("%s\n\n", Reset)
	}

	tailArgs := []string{"-n", strconv.Itoa(lines)}
	if follow {
		tailArgs = append(tailArgs, "-f")
	}
	tailArgs = append(tailArgs, path)

	cmd := exec.Command("tail", tailArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
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

// StreamAgentLog copies an agent's log stream to stdout with a header.
func StreamAgentLog(name string, rc io.ReadCloser) error {
	defer rc.Close()
	fmt.Printf("%sStreaming logs for %s%s%s\n\n", Dim, Reset, name, Reset)
	_, err := io.Copy(os.Stdout, rc)
	return err
}
