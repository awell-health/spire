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
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/observability"
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
	Spans         []olap.SpanRecord        `json:"spans,omitempty"`
	APIStats      []olap.APIEventStats     `json:"api_stats,omitempty"`
}

type traceStep struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"` // closed, in_progress, open
	Duration   int     `json:"duration_seconds,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	TokensIn   int     `json:"tokens_in,omitempty"`
	TokensOut  int     `json:"tokens_out,omitempty"`
	ReadCalls  int     `json:"read_calls,omitempty"`
	EditCalls  int     `json:"edit_calls,omitempty"`
	WriteCalls int     `json:"write_calls,omitempty"`
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
	// Sort steps by formula topology (not a hardcoded map).
	info := formula.BeadInfo{ID: target.ID, Type: string(target.Type), Labels: target.Labels}
	if g, err := formula.ResolveV3(info); err == nil {
		order := formula.StepOrderMap(g)
		sort.Slice(td.Steps, func(i, j int) bool {
			return traceStepPos(td.Steps[i].Name, order) < traceStepPos(td.Steps[j].Name, order)
		})
	}

	// Per-step metrics from agent_runs.
	populateStepMetrics(td)

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

	// OTel data: spans (waterfall), tool breakdown, API stats.
	if tc, err := config.ActiveTowerConfig(); err == nil {
		store, err := olap.OpenBackend(olap.Config{
			Backend: os.Getenv("SPIRE_OLAP_BACKEND"),
			Path:    tc.OLAPPath(),
			DSN:     os.Getenv("SPIRE_CLICKHOUSE_DSN"),
		})
		if err == nil {
			defer store.Close()
			var reader olap.TraceReader = store
			if spans, err := reader.QueryToolSpansByBead(beadID); err == nil && len(spans) > 0 {
				td.Spans = spans
			}
			if steps, err := reader.QueryToolEventsByStep(beadID); err == nil && len(steps) > 0 {
				td.ToolBreakdown = steps
			}
			if apiStats, err := reader.QueryAPIEventsByBead(beadID); err == nil && len(apiStats) > 0 {
				td.APIStats = apiStats
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

		// Per-step metrics table (only if any step has data).
		if hasStepMetrics(td.Steps) {
			renderStepMetrics(&s, td.Steps)
		}
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

	// Span waterfall (from traces pipeline — Claude beta).
	if len(td.Spans) > 0 {
		fmt.Fprintf(&s, "%sSpan waterfall:%s (%d spans)\n", bold, reset, len(td.Spans))
		renderSpanWaterfall(&s, td.Spans)
		s.WriteString("\n")
	}

	// Tool breakdown (from logs pipeline).
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

	// API stats (from logs pipeline).
	if len(td.APIStats) > 0 {
		fmt.Fprintf(&s, "%sAPI calls:%s\n", bold, reset)
		for _, a := range td.APIStats {
			fmt.Fprintf(&s, "  %s: %d calls, avg %dms, $%.4f total, %dk in / %dk out\n",
				a.Model, a.Count, int(a.AvgDurationMs), a.TotalCostUSD,
				a.TotalInputTokens/1000, a.TotalOutputTokens/1000)
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

// renderSpanWaterfall renders an indented tree of spans using parent-child relationships.
func renderSpanWaterfall(s *strings.Builder, spans []olap.SpanRecord) {
	// Build parent→children map.
	childMap := make(map[string][]int)
	var roots []int
	for i, sp := range spans {
		if sp.ParentSpanID == "" || sp.ParentSpanID == strings.Repeat("0", len(sp.ParentSpanID)) {
			roots = append(roots, i)
		} else {
			childMap[sp.ParentSpanID] = append(childMap[sp.ParentSpanID], i)
		}
	}

	// If no clear roots found (all spans have parents not in this set),
	// fall back to flat list.
	if len(roots) == 0 {
		for i := range spans {
			roots = append(roots, i)
		}
	}

	// Render tree with depth limit.
	var walk func(idx int, depth int)
	walk = func(idx int, depth int) {
		if depth > 8 {
			return // prevent runaway nesting
		}
		sp := spans[idx]
		indent := strings.Repeat("  ", depth+1)
		durStr := fmt.Sprintf("%dms", sp.DurationMs)
		statusIcon := green + "✓" + reset
		if !sp.Success {
			statusIcon = red + "✗" + reset
		}
		kindTag := ""
		if sp.Kind != "" && sp.Kind != "other" {
			kindTag = dim + " [" + sp.Kind + "]" + reset
		}
		fmt.Fprintf(s, "%s%s %s %s%s%s\n", indent, statusIcon, durStr, sp.SpanName, kindTag, reset)
		for _, ci := range childMap[sp.SpanID] {
			walk(ci, depth+1)
		}
	}

	for _, ri := range roots {
		walk(ri, 0)
	}
}

// --- Rendering helpers ---

// hasStepMetrics returns true if any step has metrics data.
func hasStepMetrics(steps []traceStep) bool {
	for _, s := range steps {
		if s.Duration > 0 || s.CostUSD > 0 || s.TokensIn > 0 || s.TokensOut > 0 {
			return true
		}
	}
	return false
}

// renderStepMetrics writes the per-step metrics table to a builder.
func renderStepMetrics(s *strings.Builder, steps []traceStep) {
	fmt.Fprintf(s, "%sSteps:%s\n", bold, reset)

	var totalDur int
	var totalCost float64
	var totalIn, totalOut int
	var totalR, totalE, totalW int

	for _, step := range steps {
		icon := stepStatusIcon(step.Status)
		if step.Duration > 0 || step.CostUSD > 0 || step.TokensIn > 0 {
			// Step with metrics.
			totalDur += step.Duration
			totalCost += step.CostUSD
			totalIn += step.TokensIn
			totalOut += step.TokensOut
			totalR += step.ReadCalls
			totalE += step.EditCalls
			totalW += step.WriteCalls

			fmt.Fprintf(s, "  %s %-12s %7s  $%-5s  R:%-3d E:%-3d W:%-3d  %s in / %s out\n",
				icon, step.Name,
				formatElapsed(time.Duration(step.Duration)*time.Second),
				formatCost(step.CostUSD),
				step.ReadCalls, step.EditCalls, step.WriteCalls,
				formatTokensK(step.TokensIn), formatTokensK(step.TokensOut))
		} else {
			// Pending step — no data.
			fmt.Fprintf(s, "  %s %-12s %s—%s\n", icon, step.Name, dim, reset)
		}
	}

	// Separator and total row.
	fmt.Fprintf(s, "  %s────────────────────────────────────────────────────────%s\n", dim, reset)
	fmt.Fprintf(s, "  %-14s %7s  $%-5s  R:%-3d E:%-3d W:%-3d  %s in / %s out\n",
		"Total",
		formatElapsed(time.Duration(totalDur)*time.Second),
		formatCost(totalCost),
		totalR, totalE, totalW,
		formatTokensK(totalIn), formatTokensK(totalOut))
	s.WriteString("\n")
}

// stepStatusIcon returns a compact status icon for the metrics table.
func stepStatusIcon(status string) string {
	switch status {
	case "closed":
		return green + "✅" + reset
	case "in_progress":
		return cyan + "▶" + reset
	default:
		return dim + "○" + reset
	}
}

// formatCost formats a USD cost for display.
func formatCost(cost float64) string {
	if cost < 0.01 {
		return fmt.Sprintf("%.4f", cost)
	}
	return fmt.Sprintf("%.2f", cost)
}

// formatTokensK formats token counts in K (thousands) notation.
func formatTokensK(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%dK", n/1000)
}

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

// traceStepPos returns the display position for a step name given a formula
// order map. If order is nil, returns 0 (preserves insertion order).
// Unknown steps sort to 999 (end).
func traceStepPos(name string, order map[string]int) int {
	if order == nil {
		return 0
	}
	if pos, ok := order[name]; ok {
		return pos
	}
	return 999
}

// populateStepMetrics queries agent_runs for the bead and populates metrics
// fields on each traceStep. Gracefully no-ops if no data is available.
func populateStepMetrics(td *traceData) {
	runs, err := observability.StepMetricsForBead(td.ID)
	if err != nil || len(runs) == 0 {
		return
	}
	populateStepMetricsFromRuns(td, runs)
}

// populateStepMetricsFromRuns is the pure aggregation logic extracted for testability.
// It maps agent_runs rows to formula step names and populates metrics on traceStep entries.
func populateStepMetricsFromRuns(td *traceData, runs []observability.StepRunRow) {
	if len(runs) == 0 {
		return
	}

	// Build a set of known step names for matching.
	stepNames := make(map[string]bool)
	for _, s := range td.Steps {
		stepNames[s.Name] = true
	}

	// Aggregate runs into per-step metrics.
	type stepAgg struct {
		duration   int
		costUSD    float64
		tokensIn   int
		tokensOut  int
		readCalls  int
		editCalls  int
		writeCalls int
	}
	agg := make(map[string]*stepAgg)

	for _, run := range runs {
		stepName := mapPhaseToStep(run.Phase, run.PhaseBucket, stepNames)
		if stepName == "" {
			continue
		}

		a, ok := agg[stepName]
		if !ok {
			a = &stepAgg{}
			agg[stepName] = a
		}

		dur := run.Duration
		// For active runs (no completed_at), compute running clock.
		if run.CompletedAt == "" && run.StartedAt != "" {
			if t, err := time.Parse(time.RFC3339, run.StartedAt); err == nil {
				dur = int(time.Since(t).Seconds())
			}
		}

		a.duration += dur
		a.costUSD += run.CostUSD
		a.tokensIn += run.TokensIn
		a.tokensOut += run.TokensOut

		// Parse tool_calls_json for R/E/W breakdown.
		if run.ToolCallsJSON != "" {
			var tools map[string]int
			if json.Unmarshal([]byte(run.ToolCallsJSON), &tools) == nil {
				a.readCalls += tools["Read"]
				a.editCalls += tools["Edit"]
				a.writeCalls += tools["Write"]
			}
		}
	}

	// Apply aggregated metrics to traceStep entries.
	for i := range td.Steps {
		if a, ok := agg[td.Steps[i].Name]; ok {
			td.Steps[i].Duration = a.duration
			td.Steps[i].CostUSD = a.costUSD
			td.Steps[i].TokensIn = a.tokensIn
			td.Steps[i].TokensOut = a.tokensOut
			td.Steps[i].ReadCalls = a.readCalls
			td.Steps[i].EditCalls = a.editCalls
			td.Steps[i].WriteCalls = a.writeCalls
		}
	}
}

// mapPhaseToStep maps an agent_runs phase to a formula step name.
// Direct match is tried first, then phase_bucket mapping.
func mapPhaseToStep(phase, phaseBucket string, stepNames map[string]bool) string {
	// Direct match: phase == step name (e.g., "implement" → "implement").
	if stepNames[phase] {
		return phase
	}

	// Phase-to-bucket mapping (mirrors executor/record.go phaseToBucket).
	bucket := phaseBucket
	if bucket == "" {
		switch phase {
		case "implement", "build-fix":
			bucket = "implement"
		case "review", "review-fix", "sage-review":
			bucket = "review"
		case "validate-design", "enrich-subtasks", "auto-approve", "skip", "waitForHuman":
			bucket = "design"
		}
	}

	// Map bucket to step name.
	switch bucket {
	case "implement":
		if stepNames["implement"] {
			return "implement"
		}
	case "review":
		if stepNames["review"] {
			return "review"
		}
	case "design":
		if stepNames["plan"] {
			return "plan"
		}
	}

	return ""
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
