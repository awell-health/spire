package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/observability"
)

func cmdLogs(args []string) error {
	// Parse flags.
	var target string
	lines := 50
	follow := true
	flagDaemon := false
	flagDolt := false
	flagSteward := false

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--daemon":
			flagDaemon = true
		case "--dolt":
			flagDolt = true
		case "--steward":
			flagSteward = true
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

	// System logs (daemon, steward, dolt) are always file-based.
	switch {
	case flagDaemon:
		return observability.TailFile(filepath.Join(gd, "daemon.log"), lines, follow)
	case flagSteward:
		return observability.TailFile(filepath.Join(gd, "steward.log"), lines, follow)
	case flagDolt:
		return observability.TailFile(filepath.Join(gd, "dolt.log"), lines, follow)
	case target != "":
		// Agent log: resolve via AgentBackend.
		backend := ResolveBackend("")
		rc, err := backend.Logs(target)
		if err != nil {
			return fmt.Errorf("no logs for %q: %w\n\nAvailable logs:\n%s",
				target, err, observability.ListAvailableLogs(gd, backend))
		}
		defer rc.Close()

		// If the backend returned a file, use tail for --lines and --follow.
		if f, ok := rc.(*os.File); ok {
			return observability.TailFile(f.Name(), lines, follow)
		}
		// Otherwise stream directly (e.g. docker pipe).
		return observability.StreamAgentLog(target, rc)
	default:
		// No args: list available logs.
		backend := ResolveBackend("")
		fmt.Printf("%sAvailable logs%s\n\n", bold, reset)
		available := observability.ListAvailableLogs(gd, backend)
		if available == "" {
			fmt.Printf("  %sNo log files found in %s%s\n", dim, gd, reset)
		} else {
			fmt.Print(available)
		}
		fmt.Printf("\n%sUsage:%s\n", bold, reset)
		fmt.Printf("  spire logs <agent-name>   Tail an agent log\n")
		fmt.Printf("  spire logs --daemon       Tail the daemon log\n")
		fmt.Printf("  spire logs --steward      Tail the steward log\n")
		fmt.Printf("  spire logs --dolt         Tail the dolt server log\n")
		return nil
	}
}

func printLogsUsage() {
	fmt.Println(`Usage: spire logs [name] [flags]

Tail agent and system logs.

Arguments:
  <name>           Agent name or bead ID to tail (substring match)

Flags:
  --daemon         Tail the daemon log
  --steward        Tail the steward log
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
