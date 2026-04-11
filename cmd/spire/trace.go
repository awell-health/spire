package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/board"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/olap"
	"github.com/spf13/cobra"
)

var traceCmd = &cobra.Command{
	Use:   "trace [bead-id]",
	Short: "Execution DAG timeline (--json, --follow)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			fullArgs = append(fullArgs, "--json")
		}
		if follow, _ := cmd.Flags().GetBool("follow"); follow {
			fullArgs = append(fullArgs, "--follow")
		}
		if v, _ := cmd.Flags().GetString("interval"); v != "" {
			fullArgs = append(fullArgs, "--interval", v)
		}
		if cmd.Flags().Changed("log-lines") {
			n, _ := cmd.Flags().GetInt("log-lines")
			fullArgs = append(fullArgs, "--log-lines", strconv.Itoa(n))
		}
		if noLog, _ := cmd.Flags().GetBool("no-log"); noLog {
			fullArgs = append(fullArgs, "--no-log")
		}
		fullArgs = append(fullArgs, args...)
		return cmdTrace(fullArgs)
	},
}

func init() {
	traceCmd.Flags().Bool("json", false, "Output as JSON")
	traceCmd.Flags().BoolP("follow", "f", false, "Follow mode")
	traceCmd.Flags().String("interval", "", "Refresh interval for follow mode")
	traceCmd.Flags().Int("log-lines", 15, "Number of log lines to show")
	traceCmd.Flags().Bool("no-log", false, "Suppress log output")
}

// traceData holds the assembled trace for a bead, used for both rendering and JSON output.
type traceData struct {
	ID            string                  `json:"id"`
	Title         string                  `json:"title"`
	Status        string                  `json:"status"`
	Priority      int                     `json:"priority"`
	Type          string                  `json:"type"`
	Phase         string                  `json:"phase"`
	Steps         []traceStep             `json:"steps"`
	Attempt       *traceAgent             `json:"active_attempt,omitempty"`
	Reviews       []traceReview           `json:"reviews,omitempty"`
	Subtasks      []traceData             `json:"subtasks,omitempty"`
	ToolBreakdown []olap.StepToolBreakdown `json:"tool_breakdown,omitempty"`
}

type traceStep struct {
	Name   string `json:"name"`
	Status string `json:"status"` // closed, in_progress, open
}

type traceAgent struct {
	Name     string `json:"name"`
	Elapsed  string `json:"elapsed"`
	Worktree string `json:"worktree,omitempty"`
	Model    string `json:"model,omitempty"`
	Branch   string `json:"branch,omitempty"`
	LogFile  string `json:"log_file,omitempty"`
}

type traceReview struct {
	Round   int    `json:"round"`
	Verdict string `json:"verdict"` // approve, request_changes, or "" if pending
	Status  string `json:"status"`
}

func cmdTrace(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if err := requireDolt(); err != nil {
		return err
	}

	var beadID string
	flagJSON := false
	flagFollow := false
	logLines := 15
	interval := 3 * time.Second

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			flagJSON = true
		case "--follow", "-f":
			flagFollow = true
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("--interval: invalid duration %q", args[i])
			}
			interval = d
		case "--log-lines":
			if i+1 >= len(args) {
				return fmt.Errorf("--log-lines requires a number")
			}
			i++
			n, err := fmt.Sscanf(args[i], "%d", &logLines)
			if n != 1 || err != nil {
				return fmt.Errorf("--log-lines: invalid number %q", args[i])
			}
		case "--no-log":
			logLines = 0
		case "--help", "-h":
			printTraceUsage()
			return nil
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag: %s\n\nRun 'spire trace --help' for usage.", args[i])
			}
			beadID = args[i]
		}
	}

	if beadID == "" {
		return fmt.Errorf("usage: spire trace <bead-id> [--json] [--follow] [--interval 3s]")
	}

	if flagJSON && !flagFollow {
		td, err := buildTrace(beadID)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(td)
	}

	if !flagFollow {
		td, err := buildTrace(beadID)
		if err != nil {
			return err
		}
		renderTrace(td)
		renderWizardLog(td, logLines)
		return nil
	}

	// Live follow mode.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	render := func() {
		td, err := buildTrace(beadID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "trace: %v\n", err)
			return
		}
		if flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(td) //nolint:errcheck
		} else {
			board.ClearScreen()
			renderTrace(td)
			renderWizardLog(td, logLines)
			fmt.Printf("\n%sUpdated: %s  (Ctrl-C to exit)%s\n", dim, time.Now().Format("15:04:05"), reset)
		}
	}

	render()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			fmt.Println()
			return nil
		case <-ticker.C:
			render()
		}
	}
}

// buildTrace assembles the full trace DAG for a bead.
func buildTrace(beadID string) (*traceData, error) {
	target, err := storeGetBead(beadID)
	if err != nil {
		return nil, fmt.Errorf("trace %s: %w", beadID, err)
	}

	td := &traceData{
		ID:       target.ID,
		Title:    target.Title,
		Status:   string(target.Status),
		Priority: target.Priority,
		Type:     string(target.Type),
		Phase:    getPhase(target),
	}

	// Step beads (pipeline phases).
	steps, _ := storeGetStepBeads(beadID)
	for _, s := range steps {
		name := stepBeadPhaseName(s)
		if name == "" {
			continue
		}
		td.Steps = append(td.Steps, traceStep{
			Name:   name,
			Status: string(s.Status),
		})
	}
	sort.Slice(td.Steps, func(i, j int) bool {
		return board.PhaseIndex(td.Steps[i].Name) < board.PhaseIndex(td.Steps[j].Name)
	})

	// Active attempt.
	attempt, _ := storeGetActiveAttemptFunc(beadID)
	if attempt != nil {
		ag := &traceAgent{
			Name: extractAgentName(*attempt),
		}
		// Extract model, branch from labels.
		for _, l := range attempt.Labels {
			switch {
			case strings.HasPrefix(l, "branch:"):
				ag.Branch = l[len("branch:"):]
			case strings.HasPrefix(l, "model:"):
				ag.Model = l[len("model:"):]
			}
		}
		// Check wizard registry for worktree path and elapsed time.
		if ag.Name != "" {
			reg := loadWizardRegistry()
			for _, w := range reg.Wizards {
				if w.Name == ag.Name || w.BeadID == beadID {
					if w.Worktree != "" {
						ag.Worktree = w.Worktree
					}
					if w.StartedAt != "" {
						if t, ok := board.ParseBoardTime(w.StartedAt); ok {
							ag.Elapsed = formatElapsed(time.Since(t))
						}
					}
					break
				}
			}
			// Resolve log file path via agent backend.
			backend := ResolveBackend("")
			if rc, err := backend.Logs(ag.Name); err == nil {
				if f, ok := rc.(*os.File); ok {
					ag.LogFile = f.Name()
				}
				rc.Close()
			}
		}
		td.Attempt = ag
	}

	// Review history.
	reviews, _ := storeGetReviewBeads(beadID)
	for _, r := range reviews {
		rn := reviewRoundNumber(r)
		verdict := reviewBeadVerdict(r)
		td.Reviews = append(td.Reviews, traceReview{
			Round:   rn,
			Verdict: verdict,
			Status:  string(r.Status),
		})
	}

	// Tool breakdown from OTel-sourced tool_events.
	if tc, err := config.ActiveTowerConfig(); err == nil {
		if adb, err := olap.Open(tc.OLAPPath()); err == nil {
			defer adb.Close()
			if steps, err := adb.QueryToolEventsByStep(beadID); err == nil && len(steps) > 0 {
				td.ToolBreakdown = steps
			}
		}
	}

	// For epics: subtask tree.
	if target.Type == "epic" {
		children, _ := storeGetChildren(beadID)
		for _, child := range children {
			// Skip step, attempt, and review beads — they're internal.
			if isStepBead(child) || isAttemptBead(child) || isReviewRoundBead(child) {
				continue
			}
			subtask, _ := buildTrace(child.ID)
			if subtask != nil {
				td.Subtasks = append(td.Subtasks, *subtask)
			}
		}
	}

	return td, nil
}

// renderTrace prints the trace to stdout with ANSI formatting.
func renderTrace(td *traceData) {
	fmt.Print(renderTraceToString(td))
}

// renderTraceToString renders the full trace to a string with ANSI formatting.
// Used by both the CLI (via renderTrace) and the board terminal pane.
func renderTraceToString(td *traceData) string {
	var s strings.Builder

	// Header.
	fmt.Fprintf(&s, "%sTRACE: %s%s — %s\n", bold, td.ID, reset, td.Title)
	fmt.Fprintf(&s, "  Status: %s  Priority: P%d  Type: %s\n", colorStatus(td.Status), td.Priority, td.Type)
	s.WriteString("\n")

	// Pipeline.
	if len(td.Steps) > 0 {
		fmt.Fprintf(&s, "%sPipeline:%s ", bold, reset)
		for i, step := range td.Steps {
			if i > 0 {
				s.WriteString(" → ")
			}
			s.WriteString(renderStepBadge(step))
		}
		s.WriteString("\n\n")
	}

	// Active attempt.
	if td.Attempt != nil {
		a := td.Attempt
		fmt.Fprintf(&s, "%sActive agent:%s %s%s%s", bold, reset, cyan, a.Name, reset)
		if a.Elapsed != "" {
			fmt.Fprintf(&s, "  %s%s%s", dim, a.Elapsed, reset)
		}
		s.WriteString("\n")
		if a.Model != "" {
			fmt.Fprintf(&s, "  Model: %s\n", a.Model)
		}
		if a.Branch != "" {
			fmt.Fprintf(&s, "  Branch: %s\n", a.Branch)
		}
		if a.Worktree != "" {
			fmt.Fprintf(&s, "  Worktree: %s\n", a.Worktree)
		}
		s.WriteString("\n")
	}

	// Review history.
	if len(td.Reviews) > 0 {
		fmt.Fprintf(&s, "%sReview history:%s\n", bold, reset)
		for _, r := range td.Reviews {
			verdict := r.Verdict
			if verdict == "" {
				verdict = "pending"
			}
			icon := reviewVerdictIcon(verdict)
			fmt.Fprintf(&s, "  Round %d: %s %s\n", r.Round, icon, verdict)
		}
		s.WriteString("\n")
	}

	// Tool breakdown (from OTel pipeline).
	if len(td.ToolBreakdown) > 0 {
		fmt.Fprintf(&s, "%sTool usage:%s\n", bold, reset)
		for _, step := range td.ToolBreakdown {
			fmt.Fprintf(&s, "  %s: ", step.Step)
			for i, t := range step.Tools {
				if i > 0 {
					s.WriteString("  ")
				}
				fmt.Fprintf(&s, "%s: %d", t.ToolName, t.Count)
			}
			s.WriteString("\n")
		}
		s.WriteString("\n")
	}

	// Subtasks (for epics).
	if len(td.Subtasks) > 0 {
		fmt.Fprintf(&s, "%sSubtasks:%s\n", bold, reset)
		for _, sub := range td.Subtasks {
			renderSubtaskToString(&s, sub, "  ")
		}
	}

	return s.String()
}

// renderSubtask prints a compact subtask trace line with its pipeline.
func renderSubtask(td traceData, indent string) {
	var s strings.Builder
	renderSubtaskToString(&s, td, indent)
	fmt.Print(s.String())
}

// renderSubtaskToString writes a compact subtask trace line to a builder.
func renderSubtaskToString(s *strings.Builder, td traceData, indent string) {
	statusIcon := subtaskStatusIcon(td.Status)
	fmt.Fprintf(s, "%s%s %s %-12s %s", indent, statusIcon, board.PriorityStr(td.Priority), td.ID, board.Truncate(td.Title, 35))

	// Show inline pipeline if steps exist.
	if len(td.Steps) > 0 {
		s.WriteString("  ")
		for i, step := range td.Steps {
			if i > 0 {
				s.WriteString(" ")
			}
			s.WriteString(renderStepBadgeCompact(step))
		}
	}

	// Show active agent if any.
	if td.Attempt != nil {
		fmt.Fprintf(s, "  %s%s%s", cyan, td.Attempt.Name, reset)
		if td.Attempt.Elapsed != "" {
			fmt.Fprintf(s, " %s%s%s", dim, td.Attempt.Elapsed, reset)
		}
	}

	s.WriteString("\n")

	// Show review status inline for subtasks.
	if len(td.Reviews) > 0 {
		last := td.Reviews[len(td.Reviews)-1]
		verdict := last.Verdict
		if verdict == "" {
			verdict = "pending"
		}
		fmt.Fprintf(s, "%s  review r%d: %s\n", indent, last.Round, verdict)
	}
}

// renderTraceForBoard builds and renders a full trace (including log tail)
// as a single string for display in the board terminal pane.
func renderTraceForBoard(beadID string) (string, error) {
	td, err := buildTrace(beadID)
	if err != nil {
		return "", err
	}
	result := renderTraceToString(td)
	result += renderWizardLogToString(td, 15)
	return result, nil
}

// --- Rendering helpers ---

func renderStepBadge(s traceStep) string {
	switch s.Status {
	case "closed":
		return green + "[✅ " + s.Name + "]" + reset
	case "in_progress":
		return cyan + "[▶ " + s.Name + "]" + reset
	default: // open
		return dim + "[○ " + s.Name + "]" + reset
	}
}

func renderStepBadgeCompact(s traceStep) string {
	switch s.Status {
	case "closed":
		return green + "✅" + reset
	case "in_progress":
		return cyan + "▶" + reset
	default:
		return dim + "○" + reset
	}
}

func colorStatus(status string) string {
	switch status {
	case "in_progress":
		return cyan + status + reset
	case "closed":
		return green + status + reset
	case "open":
		return yellow + status + reset
	default:
		return status
	}
}

func subtaskStatusIcon(status string) string {
	switch status {
	case "closed":
		return green + "✓" + reset
	case "in_progress":
		return cyan + "◐" + reset
	default:
		return dim + "○" + reset
	}
}

func reviewVerdictIcon(verdict string) string {
	switch verdict {
	case "approve":
		return green + "✓" + reset
	case "request_changes":
		return yellow + "↻" + reset
	case "reject":
		return red + "✗" + reset
	default:
		return dim + "…" + reset
	}
}

func extractAgentName(b Bead) string {
	// Try agent: label first.
	if name := hasLabel(b, "agent:"); name != "" {
		return name
	}
	// Fallback: parse from title "attempt: <name>".
	if strings.HasPrefix(b.Title, "attempt:") {
		name := strings.TrimSpace(strings.TrimPrefix(b.Title, "attempt:"))
		return name
	}
	return ""
}

func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// renderWizardLog shows the last N lines of the active wizard's log file.
func renderWizardLog(td *traceData, lines int) {
	fmt.Print(renderWizardLogToString(td, lines))
}

// renderWizardLogToString renders the wizard log tail to a string.
func renderWizardLogToString(td *traceData, lines int) string {
	if lines <= 0 || td.Attempt == nil || td.Attempt.LogFile == "" {
		return ""
	}
	tail, err := readLastLines(td.Attempt.LogFile, lines)
	if err != nil || len(tail) == 0 {
		return ""
	}
	var s strings.Builder
	fmt.Fprintf(&s, "%sWizard log%s (%s):\n", bold, reset, td.Attempt.LogFile)
	s.WriteString(dim)
	for _, line := range tail {
		fmt.Fprintf(&s, "  %s\n", line)
	}
	fmt.Fprintf(&s, "%s\n", reset)
	return s.String()
}

// readLastLines returns the last n lines from a file.
func readLastLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	// Use a 256KB buffer to handle long log lines.
	buf := make([]byte, 0, 256*1024)
	scanner.Buffer(buf, 256*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		// Keep a rolling window to avoid unbounded memory on large logs.
		if len(lines) > n*2 {
			lines = lines[len(lines)-n:]
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, err
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func printTraceUsage() {
	fmt.Println(`Usage: spire trace <bead-id> [flags]

Show the full execution DAG for a bead: pipeline steps, active agent,
review history, wizard log tail, and (for epics) subtask tree.

Flags:
  --json               Output as JSON
  --follow, -f         Live-updating mode (clear and re-render)
  --interval <dur>     Refresh interval in follow mode (default: 3s)
  --log-lines <n>      Number of wizard log lines to show (default: 15)
  --no-log             Suppress wizard log output
  --help               Show this help

Examples:
  spire trace spi-abc              One-shot trace with log tail
  spire trace spi-abc -f           Live follow mode (log updates each cycle)
  spire trace spi-abc --log-lines 30  Show more log lines
  spire trace spi-abc --no-log     Trace without log output
  spire trace spi-abc --json       JSON output (includes log_file path)`)
}
