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
	"text/tabwriter"
	"time"

	"github.com/awell-health/spire/pkg/board/logstream"
	"github.com/awell-health/spire/pkg/observability"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
		if p, _ := cmd.Flags().GetString("provider"); p != "" {
			fullArgs = append(fullArgs, "--provider", p)
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

var logsPrettyCmd = &cobra.Command{
	Use:   "pretty <bead-id>",
	Short: "Pretty-print the latest transcript for a bead to stdout",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		providerFilter, _ := cmd.Flags().GetString("provider")
		expand, _ := cmd.Flags().GetBool("expand")
		return runLogsPretty(args[0], providerFilter, expand)
	},
}

func init() {
	logsCmd.Flags().Bool("daemon", false, "Tail the daemon log")
	logsCmd.Flags().Bool("dolt", false, "Tail the dolt server log")
	logsCmd.Flags().Bool("steward", false, "Tail the steward log")
	logsCmd.Flags().Bool("claude", false, "List/tail per-invocation subprocess transcripts for a wizard")
	logsCmd.Flags().String("claude-file", "", "Tail a specific claude log file by absolute path (must live under <dolt-global>/wizards/)")
	logsCmd.Flags().String("provider", "", "Limit transcript listing to a single provider (claude, codex)")
	logsCmd.Flags().IntP("lines", "n", 50, "Number of historical lines to show")
	logsCmd.Flags().Bool("no-follow", false, "Print lines and exit (don't follow)")

	logsPrettyCmd.Flags().String("provider", "", "Limit to a single provider (claude, codex)")
	logsPrettyCmd.Flags().Bool("expand", false, "Render long bodies in full instead of truncating")
	logsCmd.AddCommand(logsPrettyCmd)
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
	providerFilter := ""

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
		case "--provider":
			if i+1 >= len(args) {
				return fmt.Errorf("--provider requires a name")
			}
			i++
			providerFilter = args[i]
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
			if strings.HasPrefix(args[i], "--provider=") {
				providerFilter = strings.TrimPrefix(args[i], "--provider=")
			} else if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag: %s\n\nRun 'spire logs --help' for usage.", args[i])
			} else {
				positionals = append(positionals, args[i])
			}
		}
		i++
	}

	gd := doltGlobalDir()

	// --claude-file: tail a specific file after validating it lives under wizards/.
	if flagClaudeFile != "" {
		return tailClaudeFile(gd, flagClaudeFile, lines, follow)
	}

	// --claude: list transcripts for a wizard (all providers by default), or
	// tail by label (claude-specific label convention).
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
			return listTranscripts(os.Stdout, gd, wizardName, providerFilter)
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
       spire logs pretty <bead-id> [--provider=name] [--expand]

Tail agent and system logs.

Arguments:
  <name>           Agent name or bead ID to tail (substring match)

Flags:
  --daemon              Tail the daemon log
  --steward             Tail the steward log
  --dolt                Tail the dolt server log
  --claude [label]      List subprocess transcripts for a wizard (all providers),
                        or tail by claude label
  --claude-file <path>  Tail a specific claude log file by absolute path
  --provider <name>     Limit transcript listing to a single provider (claude, codex)
  --lines N, -n N       Number of historical lines to show (default: 50)
  --no-follow           Print lines and exit (don't follow)
  --help                Show this help

Examples:
  spire logs                                       List available logs
  spire logs wizard-spi-abc                        Tail a specific wizard log
  spire logs spi-abc                               Tail log matching a bead ID
  spire logs --daemon                              Tail the daemon log
  spire logs --daemon -n 100                       Show last 100 lines of daemon log
  spire logs wizard-spi-abc --claude               List all transcripts for the wizard
  spire logs wizard-spi-abc --claude --provider=codex   List only codex transcripts
  spire logs wizard-spi-abc --claude epic-plan     Tail newest claude log labelled "epic-plan"
  spire logs pretty spi-abc                        Pretty-print the latest transcript for a bead
  spire logs pretty spi-abc --provider=codex       Pretty-print the latest codex transcript
  spire logs pretty spi-abc --expand               Do not truncate long bodies`)
}

// transcriptFile describes one per-invocation transcript file under a
// wizard's provider subdirectory.
type transcriptFile struct {
	Provider string
	Path     string
	ModTime  time.Time
	Size     int64
}

// providerExtensions returns the file extensions that count as
// transcripts for a given provider. The legacy ".log" for claude
// preserves backward compat with transcripts captured before the
// ".jsonl" convention landed (see spi-7mgv9).
func providerExtensions(provider string) []string {
	switch provider {
	case "claude":
		return []string{".jsonl", ".log"}
	case "codex":
		return []string{".jsonl"}
	default:
		return []string{".jsonl"}
	}
}

// discoverTranscripts enumerates transcript files across every
// registered provider under wizardDir. When providerFilter is non-empty,
// only that provider's subdirectory is scanned. Files whose basename
// ends in ".stderr.log" are excluded — they are sidecar diagnostics,
// not transcripts. Results are sorted by ModTime ascending so the last
// element is the most recently modified.
func discoverTranscripts(wizardDir, providerFilter string) ([]transcriptFile, error) {
	var out []transcriptFile
	seen := map[string]bool{}
	for _, p := range logstream.Registered() {
		if providerFilter != "" && p != providerFilter {
			continue
		}
		dir := filepath.Join(wizardDir, p)
		for _, ext := range providerExtensions(p) {
			matches, _ := filepath.Glob(filepath.Join(dir, "*"+ext))
			for _, m := range matches {
				if strings.HasSuffix(m, ".stderr.log") {
					continue
				}
				if seen[m] {
					continue
				}
				seen[m] = true
				st, err := os.Stat(m)
				if err != nil {
					continue
				}
				out = append(out, transcriptFile{
					Provider: p,
					Path:     m,
					ModTime:  st.ModTime(),
					Size:     st.Size(),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.Before(out[j].ModTime) })
	return out, nil
}

// wizardDirForName returns <doltGlobal>/wizards/<wizardName>.
func wizardDirForName(doltGlobal, wizardName string) string {
	return filepath.Join(doltGlobal, "wizards", wizardName)
}

// wizardDirForBead maps a bead ID (with or without the "wizard-" prefix)
// to its wizard directory under <doltGlobal>/wizards/.
func wizardDirForBead(doltGlobal, beadID string) string {
	name := beadID
	if !strings.HasPrefix(name, "wizard-") {
		name = "wizard-" + name
	}
	return wizardDirForName(doltGlobal, name)
}

// listTranscripts writes a provider-agnostic table of transcripts for a
// wizard to w. Entries are rendered newest-first. If providerFilter is
// non-empty, only that provider is listed. When no transcripts are
// found, a friendly message is printed and nil is returned.
func listTranscripts(w io.Writer, doltGlobal, wizardName, providerFilter string) error {
	wizardDir := wizardDirForName(doltGlobal, wizardName)
	ts, err := discoverTranscripts(wizardDir, providerFilter)
	if err != nil {
		return err
	}
	if len(ts) == 0 {
		if providerFilter != "" {
			fmt.Fprintf(w, "No %s transcripts recorded for %s\n", providerFilter, wizardName)
		} else {
			fmt.Fprintf(w, "No transcripts recorded for %s\n", wizardName)
		}
		return nil
	}

	// Display newest-first; discoverTranscripts returns ascending order.
	sort.Slice(ts, func(i, j int) bool { return ts[i].ModTime.After(ts[j].ModTime) })

	fmt.Fprintf(w, "%sTranscripts for %s%s\n\n", bold, wizardName, reset)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  PROVIDER\tNAME\tSIZE\tMODIFIED\tPATH")
	now := time.Now()
	for _, t := range ts {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s ago\t%s%s%s\n",
			t.Provider,
			filepath.Base(t.Path),
			humanSize(t.Size),
			humanDuration(now.Sub(t.ModTime)),
			dim, t.Path, reset,
		)
	}
	return tw.Flush()
}

// runLogsPretty discovers the latest transcript for a bead and renders
// it through the per-provider logstream adapter to stdout.
func runLogsPretty(beadID, providerFilter string, expand bool) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	gd := doltGlobalDir()
	wizardDir := wizardDirForBead(gd, beadID)
	ts, err := discoverTranscripts(wizardDir, providerFilter)
	if err != nil {
		return err
	}
	if len(ts) == 0 {
		return fmt.Errorf("no transcripts found for %s (looked in %s)", beadID, wizardDir)
	}
	latest := ts[len(ts)-1]

	content, err := os.ReadFile(latest.Path)
	if err != nil {
		return err
	}
	adapter := logstream.Get(latest.Provider)
	events, ok := adapter.Parse(string(content))
	if !ok || len(events) == 0 {
		// Adapter didn't recognize the content; print raw bytes so the
		// user still sees something rather than silent empty output.
		_, err := os.Stdout.Write(content)
		return err
	}
	width := terminalWidth()
	for _, ev := range events {
		for _, line := range adapter.Render(ev, width, expand) {
			fmt.Println(line)
		}
	}
	return nil
}

// terminalWidth returns the current stdout column width, or 100 when
// stdout is not a TTY or the query fails.
func terminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 100
}

// humanDuration formats a non-negative duration as a short string like
// "42s", "3m", "2h", or "5d".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// claudeLogFilenameRE extracts the label prefix (group 1) and the
// YYYYMMDD-HHMMSS timestamp suffix (group 2) from a claude log basename
// that has already had its .log extension stripped.
var claudeLogFilenameRE = regexp.MustCompile(`^(.+)-(\d{8}-\d{6})$`)

// claudeLogEntry describes one per-invocation claude log file, used by
// the label-based tail workflow (`--claude <wizard> <label>`).
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
// empty — missing logs is not an error. Only ".log" files are returned,
// since label-based resolution is keyed off the legacy filename shape.
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
