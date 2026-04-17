package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/observability"
	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs [name]",
	Short: "Tail agent/system logs (--daemon, --dolt, --claude)",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if daemon, _ := cmd.Flags().GetBool("daemon"); daemon {
			fullArgs = append(fullArgs, "--daemon")
		}
		if dolt, _ := cmd.Flags().GetBool("dolt"); dolt {
			fullArgs = append(fullArgs, "--dolt")
		}
		if steward, _ := cmd.Flags().GetBool("steward"); steward {
			fullArgs = append(fullArgs, "--steward")
		}
		if claude, _ := cmd.Flags().GetBool("claude"); claude {
			fullArgs = append(fullArgs, "--claude")
		}
		if cf, _ := cmd.Flags().GetString("claude-file"); cf != "" {
			fullArgs = append(fullArgs, "--claude-file", cf)
		}
		if cmd.Flags().Changed("lines") {
			n, _ := cmd.Flags().GetInt("lines")
			fullArgs = append(fullArgs, "--lines", strconv.Itoa(n))
		}
		if noFollow, _ := cmd.Flags().GetBool("no-follow"); noFollow {
			fullArgs = append(fullArgs, "--no-follow")
		}
		fullArgs = append(fullArgs, args...)
		return cmdLogs(fullArgs)
	},
}

func init() {
	logsCmd.Flags().Bool("daemon", false, "Tail the daemon log")
	logsCmd.Flags().Bool("dolt", false, "Tail the dolt server log")
	logsCmd.Flags().Bool("steward", false, "Tail the steward log")
	logsCmd.Flags().Bool("claude", false, "List/tail per-invocation claude subprocess logs for a wizard")
	logsCmd.Flags().String("claude-file", "", "Tail a specific claude log file by absolute path (must live under <dolt-global>/wizards/)")
	logsCmd.Flags().IntP("lines", "n", 50, "Number of historical lines to show")
	logsCmd.Flags().Bool("no-follow", false, "Print lines and exit (don't follow)")
}

func cmdLogs(args []string) error {
	// Parse flags.
	var positionals []string
	lines := 50
	follow := true
	flagDaemon := false
	flagDolt := false
	flagSteward := false
	flagClaude := false
	flagClaudeFile := ""

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--daemon":
			flagDaemon = true
		case "--dolt":
			flagDolt = true
		case "--steward":
			flagSteward = true
		case "--claude":
			flagClaude = true
		case "--claude-file":
			if i+1 >= len(args) {
				return fmt.Errorf("--claude-file requires a path")
			}
			i++
			flagClaudeFile = args[i]
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
			positionals = append(positionals, args[i])
		}
		i++
	}

	gd := doltGlobalDir()

	// --claude-file: tail a specific file after validating it lives under wizards/.
	if flagClaudeFile != "" {
		return tailClaudeFile(gd, flagClaudeFile, lines, follow)
	}

	// --claude: list invocations for a wizard, or tail by label.
	if flagClaude {
		if len(positionals) == 0 {
			return fmt.Errorf("--claude requires a wizard name: spire logs <wizard-name> --claude [label]")
		}
		wizardName := positionals[0]
		var label string
		if len(positionals) > 1 {
			label = positionals[1]
		}
		if label == "" {
			return listClaudeLogs(os.Stdout, gd, wizardName)
		}
		path, err := resolveClaudeLog(gd, wizardName, label)
		if err != nil {
			return err
		}
		return observability.TailFile(path, lines, follow)
	}

	var target string
	if len(positionals) > 0 {
		target = positionals[0]
	}

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
  --daemon              Tail the daemon log
  --steward             Tail the steward log
  --dolt                Tail the dolt server log
  --claude [label]      List claude subprocess logs for a wizard, or tail by label
  --claude-file <path>  Tail a specific claude log file by absolute path
  --lines N, -n N       Number of historical lines to show (default: 50)
  --no-follow           Print lines and exit (don't follow)
  --help                Show this help

Examples:
  spire logs                                  List available logs
  spire logs wizard-spi-abc                   Tail a specific wizard log
  spire logs spi-abc                          Tail log matching a bead ID
  spire logs --daemon                         Tail the daemon log
  spire logs --daemon -n 100                  Show last 100 lines of daemon log
  spire logs wizard-spi-abc --claude          List claude subprocess logs for the wizard
  spire logs wizard-spi-abc --claude epic-plan Tail newest claude log labelled "epic-plan"`)
}

// claudeLogFilenameRE extracts the label prefix (group 1) and the
// YYYYMMDD-HHMMSS timestamp suffix (group 2) from a claude log basename
// that has already had its .log extension stripped.
var claudeLogFilenameRE = regexp.MustCompile(`^(.+)-(\d{8}-\d{6})$`)

// claudeLogEntry describes one per-invocation claude log file.
type claudeLogEntry struct {
	Path      string
	Basename  string
	Label     string
	Timestamp string
	Size      int64
}

// claudeLogsDir returns the directory that holds per-invocation claude
// logs for the named wizard.
func claudeLogsDir(doltGlobal, wizardName string) string {
	return filepath.Join(doltGlobal, "wizards", wizardName, "claude")
}

// listClaudeLogEntries enumerates claude log files for a wizard, newest
// first. Returns (nil, nil) when the directory does not exist or is
// empty — missing logs is not an error.
func listClaudeLogEntries(doltGlobal, wizardName string) ([]claudeLogEntry, error) {
	dir := claudeLogsDir(doltGlobal, wizardName)
	matches, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		return nil, err
	}
	// Sort descending: lexicographic on the timestamp suffix is equivalent
	// to time-desc because the timestamp is zero-padded UTC.
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))

	var out []claudeLogEntry
	for _, p := range matches {
		base := filepath.Base(p)
		stem := strings.TrimSuffix(base, ".log")
		label, ts := stem, ""
		if m := claudeLogFilenameRE.FindStringSubmatch(stem); m != nil {
			label, ts = m[1], m[2]
		}
		var size int64
		if fi, err := os.Stat(p); err == nil {
			size = fi.Size()
		}
		out = append(out, claudeLogEntry{
			Path:      p,
			Basename:  base,
			Label:     label,
			Timestamp: ts,
			Size:      size,
		})
	}
	return out, nil
}

// listClaudeLogs writes a table of claude log entries for the named
// wizard to w. When the directory is missing or empty, it prints the
// "No claude invocations recorded" message and returns nil.
func listClaudeLogs(w io.Writer, doltGlobal, wizardName string) error {
	entries, err := listClaudeLogEntries(doltGlobal, wizardName)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintf(w, "No claude invocations recorded for %s\n", wizardName)
		return nil
	}

	fmt.Fprintf(w, "%sClaude invocations for %s%s\n\n", bold, wizardName, reset)
	fmt.Fprintf(w, "  %-24s %-17s %10s  %s\n", "LABEL", "TIMESTAMP", "SIZE", "PATH")
	for _, e := range entries {
		fmt.Fprintf(w, "  %-24s %-17s %10s  %s%s%s\n",
			e.Label, e.Timestamp, humanSize(e.Size), dim, e.Path, reset)
	}
	return nil
}

// resolveClaudeLog returns the newest claude log file whose parsed
// label exactly matches the requested label. The parsed label comes
// from `^(.+)-\d{8}-\d{6}$`, so `epic` matches `epic-20260417-173412.log`
// but NOT `epic-plan-20260417-120000.log`.
func resolveClaudeLog(doltGlobal, wizardName, label string) (string, error) {
	entries, err := listClaudeLogEntries(doltGlobal, wizardName)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Label == label {
			return e.Path, nil
		}
	}
	return "", fmt.Errorf("no claude log matching label %q for %s (looked in %s)",
		label, wizardName, claudeLogsDir(doltGlobal, wizardName))
}

// tailClaudeFile validates that absPath lives under <doltGlobal>/wizards/
// then tails it via observability.TailFile. This prevents arbitrary path
// reads from the --claude-file flag.
func tailClaudeFile(doltGlobal, absPath string, lines int, follow bool) error {
	if !filepath.IsAbs(absPath) {
		return fmt.Errorf("--claude-file requires an absolute path, got %q", absPath)
	}
	clean := filepath.Clean(absPath)
	wizardsRoot := filepath.Clean(filepath.Join(doltGlobal, "wizards")) + string(filepath.Separator)
	if !strings.HasPrefix(clean+string(filepath.Separator), wizardsRoot) {
		return fmt.Errorf("--claude-file must live under %s", wizardsRoot)
	}
	return observability.TailFile(clean, lines, follow)
}

// humanSize formats a file size as a short human-readable string.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), suffix)
}
