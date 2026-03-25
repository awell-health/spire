package main

import (
	"fmt"
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

	// Determine which log file to tail.
	var logPath string

	switch {
	case flagDaemon:
		logPath = filepath.Join(gd, "daemon.log")
	case flagDolt:
		logPath = filepath.Join(gd, "dolt.log")
	case target != "":
		// Look for agent log in wizards directory.
		logPath = resolveAgentLogPath(gd, target)
		if logPath == "" {
			return fmt.Errorf("no log found for %q\n\nAvailable logs:\n%s", target, listAvailableLogs(gd))
		}
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

	// Verify the log file exists.
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		return fmt.Errorf("log file not found: %s", logPath)
	}

	fmt.Printf("%sTailing %s%s%s (%d lines)\n", dim, reset, logPath, dim, lines)
	if follow {
		fmt.Printf("Press Ctrl-C to stop.%s\n\n", reset)
	} else {
		fmt.Printf("%s\n\n", reset)
	}

	// Use tail to display the log.
	tailArgs := []string{"-n", strconv.Itoa(lines)}
	if follow {
		tailArgs = append(tailArgs, "-f")
	}
	tailArgs = append(tailArgs, logPath)

	cmd := exec.Command("tail", tailArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// resolveAgentLogPath finds the log file for an agent name or bead ID.
// Checks the wizards/ log directory for matching files.
func resolveAgentLogPath(globalDir, target string) string {
	wizardLogDir := filepath.Join(globalDir, "wizards")

	// Direct match: wizard-<target>.log
	candidates := []string{
		filepath.Join(wizardLogDir, target+".log"),
		filepath.Join(wizardLogDir, "wizard-"+target+".log"),
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// Substring match: any log containing the target string.
	entries, err := os.ReadDir(wizardLogDir)
	if err != nil {
		return ""
	}

	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			if strings.Contains(e.Name(), target) {
				return filepath.Join(wizardLogDir, e.Name())
			}
		}
	}

	// Also check top-level logs (daemon, dolt) by name.
	topLevel := filepath.Join(globalDir, target+".log")
	if _, err := os.Stat(topLevel); err == nil {
		return topLevel
	}

	return ""
}

// listAvailableLogs returns a formatted string listing all available log files.
func listAvailableLogs(globalDir string) string {
	var sb strings.Builder

	// Top-level logs.
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

	// Wizard logs.
	wizardLogDir := filepath.Join(globalDir, "wizards")
	entries, err := os.ReadDir(wizardLogDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".log")
			age := formatSyncAge(info.ModTime().Format("2006-01-02T15:04:05Z07:00"))
			size := formatFileSize(info.Size())
			sb.WriteString(fmt.Sprintf("  %-20s %6s  modified %s ago  %sspire logs %s%s\n",
				name, size, age, dim, name, reset))
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
