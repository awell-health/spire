package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/board"
)

// traceData holds the assembled trace for a bead, used for both rendering and JSON output.
type traceData struct {
	ID       string       `json:"id"`
	Title    string       `json:"title"`
	Status   string       `json:"status"`
	Priority int          `json:"priority"`
	Type     string       `json:"type"`
	Phase    string       `json:"phase"`
	Steps    []traceStep  `json:"steps"`
	Attempt  *traceAgent  `json:"active_attempt,omitempty"`
	Reviews  []traceReview `json:"reviews,omitempty"`
	Subtasks []traceData  `json:"subtasks,omitempty"`
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
	// Header.
	fmt.Printf("%sTRACE: %s%s — %s\n", bold, td.ID, reset, td.Title)
	fmt.Printf("  Status: %s  Priority: P%d  Type: %s\n", colorStatus(td.Status), td.Priority, td.Type)
	fmt.Println()

	// Pipeline.
	if len(td.Steps) > 0 {
		fmt.Printf("%sPipeline:%s ", bold, reset)
		for i, s := range td.Steps {
			if i > 0 {
				fmt.Print(" → ")
			}
			fmt.Print(renderStepBadge(s))
		}
		fmt.Println()
		fmt.Println()
	}

	// Active attempt.
	if td.Attempt != nil {
		a := td.Attempt
		fmt.Printf("%sActive agent:%s %s%s%s", bold, reset, cyan, a.Name, reset)
		if a.Elapsed != "" {
			fmt.Printf("  %s%s%s", dim, a.Elapsed, reset)
		}
		fmt.Println()
		if a.Model != "" {
			fmt.Printf("  Model: %s\n", a.Model)
		}
		if a.Branch != "" {
			fmt.Printf("  Branch: %s\n", a.Branch)
		}
		if a.Worktree != "" {
			fmt.Printf("  Worktree: %s\n", a.Worktree)
		}
		fmt.Println()
	}

	// Review history.
	if len(td.Reviews) > 0 {
		fmt.Printf("%sReview history:%s\n", bold, reset)
		for _, r := range td.Reviews {
			verdict := r.Verdict
			if verdict == "" {
				verdict = "pending"
			}
			icon := reviewVerdictIcon(verdict)
			fmt.Printf("  Round %d: %s %s\n", r.Round, icon, verdict)
		}
		fmt.Println()
	}

	// Subtasks (for epics).
	if len(td.Subtasks) > 0 {
		fmt.Printf("%sSubtasks:%s\n", bold, reset)
		for _, sub := range td.Subtasks {
			renderSubtask(sub, "  ")
		}
	}
}

// renderSubtask prints a compact subtask trace line with its pipeline.
func renderSubtask(td traceData, indent string) {
	statusIcon := subtaskStatusIcon(td.Status)
	fmt.Printf("%s%s %s %-12s %s", indent, statusIcon, board.PriorityStr(td.Priority), td.ID, board.Truncate(td.Title, 35))

	// Show inline pipeline if steps exist.
	if len(td.Steps) > 0 {
		fmt.Print("  ")
		for i, s := range td.Steps {
			if i > 0 {
				fmt.Print(" ")
			}
			fmt.Print(renderStepBadgeCompact(s))
		}
	}

	// Show active agent if any.
	if td.Attempt != nil {
		fmt.Printf("  %s%s%s", cyan, td.Attempt.Name, reset)
		if td.Attempt.Elapsed != "" {
			fmt.Printf(" %s%s%s", dim, td.Attempt.Elapsed, reset)
		}
	}

	fmt.Println()

	// Show review status inline for subtasks.
	if len(td.Reviews) > 0 {
		last := td.Reviews[len(td.Reviews)-1]
		verdict := last.Verdict
		if verdict == "" {
			verdict = "pending"
		}
		fmt.Printf("%s  review r%d: %s\n", indent, last.Round, verdict)
	}
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

func printTraceUsage() {
	fmt.Println(`Usage: spire trace <bead-id> [flags]

Show the full execution DAG for a bead: pipeline steps, active agent,
review history, and (for epics) subtask tree.

Flags:
  --json             Output as JSON
  --follow, -f       Live-updating mode (clear and re-render)
  --interval <dur>   Refresh interval in follow mode (default: 3s)
  --help             Show this help

Examples:
  spire trace spi-abc          One-shot trace
  spire trace spi-abc -f       Live follow mode
  spire trace spi-abc --json   JSON output`)
}
