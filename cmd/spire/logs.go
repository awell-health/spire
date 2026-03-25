package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func cmdLogs(args []string) error {
	// Parse flags.
	var target string
	lines := 50
	follow := true
	flagDaemon := false
	flagDolt := false

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--daemon":
			flagDaemon = true
		case "--dolt":
			flagDolt = true
		case "--lines", "-n":
			if i+1 >= len(args) {
				return fmt.Errorf("--lines requires a number")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("--lines: invalid number %q", args[i])
			}
			lines = n
		case "--no-follow":
			follow = false
		case "--help", "-h":
			printLogsUsage()
			return nil
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag: %s\n\nRun 'spire logs --help' for usage.", args[i])
			}
			target = args[i]
		}
		i++
	}

	gd := doltGlobalDir()

	// System logs (daemon, dolt) are always file-based.
	switch {
	case flagDaemon:
		return tailFile(filepath.Join(gd, "daemon.log"), lines, follow)
	case flagDolt:
		return tailFile(filepath.Join(gd, "dolt.log"), lines, follow)
	case target != "":
		// Agent log: resolve via AgentBackend.
		backend := ResolveBackend("")
		rc, err := backend.Logs(target)
		if err != nil {
			return fmt.Errorf("no logs for %q: %w\n\nAvailable logs:\n%s", target, err, listAvailableLogs(gd))
		}
		defer rc.Close()

		// If the backend returned a file, use tail for --lines and --follow.
		if f, ok := rc.(*os.File); ok {
			return tailFile(f.Name(), lines, follow)
		}
		// Otherwise stream directly (e.g. docker pipe).
		fmt.Printf("%sStreaming logs for %s%s%s\n\n", dim, reset, target, reset)
		_, err = io.Copy(os.Stdout, rc)
		return err
	default:
		// No args: list available logs.
		fmt.Printf("%sAvailable logs%s\n\n", bold, reset)
		available := listAvailableLogs(gd)
		if available == "" {
			fmt.Printf("  %sNo log files found in %s%s\n", dim, gd, reset)
		} else {
			fmt.Print(available)
		}
		fmt.Printf("\n%sUsage:%s\n", bold, reset)
		fmt.Printf("  spire logs <agent-name>   Tail an agent log\n")
		fmt.Printf("  spire logs --daemon       Tail the daemon log\n")
		fmt.Printf("  spire logs --dolt         Tail the dolt server log\n")
		return nil
	}
}

// tailFile tails a log file with the given line count and optional follow mode.
func tailFile(path string, lines int, follow bool) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("log file not found: %s", path)
	}

	fmt.Printf("%sTailing %s%s%s (%d lines)\n", dim, reset, path, dim, lines)
	if follow {
		fmt.Printf("Press Ctrl-C to stop.%s\n\n", reset)
	} else {
		fmt.Printf("%s\n\n", reset)
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

// listAvailableLogs returns a formatted string listing all available log files.
// System logs are listed from the global dir; agent logs are discovered via
// the AgentBackend so that process, docker, and future backends all work.
func listAvailableLogs(globalDir string) string {
	var sb strings.Builder

	// System logs (host services — always file-based).
	topLogs := []struct {
		flag string
		name string
		path string
	}{
		{"--daemon", "daemon", filepath.Join(globalDir, "daemon.log")},
		{"--daemon", "daemon (err)", filepath.Join(globalDir, "daemon.error.log")},
		{"--dolt", "dolt", filepath.Join(globalDir, "dolt.log")},
		{"--dolt", "dolt (err)", filepath.Join(globalDir, "dolt.error.log")},
	}

	for _, l := range topLogs {
		info, err := os.Stat(l.path)
		if err != nil {
			continue
		}
		age := formatSyncAge(info.ModTime().Format("2006-01-02T15:04:05Z07:00"))
		size := formatFileSize(info.Size())
		sb.WriteString(fmt.Sprintf("  %-20s %6s  modified %s ago  %s%s%s\n",
			l.name, size, age, dim, l.flag, reset))
	}

	// Agent logs (discovered via backend).
	backend := ResolveBackend("")
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
				age := formatSyncAge(info.ModTime().Format("2006-01-02T15:04:05Z07:00"))
				size := formatFileSize(info.Size())
				sb.WriteString(fmt.Sprintf("  %-20s %6s  modified %s ago  %sspire logs %s%s\n",
					a.Name, size, age, dim, a.Name, reset))
			} else {
				rc.Close()
				sb.WriteString(fmt.Sprintf("  %-20s %s(stream)%s  %sspire logs %s%s\n",
					a.Name, dim, reset, dim, a.Name, reset))
			}
		}
	}

	return sb.String()
}

func printLogsUsage() {
	fmt.Println(`Usage: spire logs [name] [flags]

Tail agent and system logs.

Arguments:
  <name>           Agent name or bead ID to tail (substring match)

Flags:
  --daemon         Tail the daemon log
  --dolt           Tail the dolt server log
  --lines N, -n N  Number of historical lines to show (default: 50)
  --no-follow      Print lines and exit (don't follow)
  --help           Show this help

Examples:
  spire logs                     List available logs
  spire logs wizard-spi-abc      Tail a specific wizard log
  spire logs spi-abc             Tail log matching a bead ID
  spire logs --daemon            Tail the daemon log
  spire logs --daemon -n 100     Show last 100 lines of daemon log`)
}
