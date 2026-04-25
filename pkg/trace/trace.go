// Package trace assembles the pipeline + active-agent + log-tail view of a
// single bead's execution. It powers the /api/v1/beads/{id}/trace HTTP
// endpoint consumed by the desktop Trace tab and is the canonical location
// for the shape the CLI's `spire trace <bead>` display and the desktop UI
// share — raw status enums ("closed", "in_progress", "open") stay on the
// wire so the client maps to pills, not the collector.
package trace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/observability"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/wizardregistry"
)

// Data is the response shape returned by Collect. Field names and JSON
// tags match the /api/v1/beads/{id}/trace contract exactly.
type Data struct {
	Pipeline    []PipelineStep `json:"pipeline"`
	Totals      Totals         `json:"totals"`
	ActiveAgent *ActiveAgent   `json:"active_agent"`
	LogTail     []LogLine      `json:"log_tail"`
}

// PipelineStep is one entry in the pipeline array: formula step + metrics.
// Status values are the raw store enum ("closed", "in_progress", "open");
// the desktop maps these to pills (✅ / ▶ / ○) so the CLI renderer and the
// UI share a single source of truth.
type PipelineStep struct {
	Step       string  `json:"step"`
	Status     string  `json:"status"`
	DurationMs int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd"`
	Retries    int     `json:"retries"`
	Errors     int     `json:"errors"`
	Warns      int     `json:"warns"`
	Reads      int     `json:"reads"`
	Writes     int     `json:"writes"`
}

// Totals is the sum across every step in Pipeline.
type Totals struct {
	DurationMs int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd"`
	Retries    int     `json:"retries"`
	Errors     int     `json:"errors"`
	Warns      int     `json:"warns"`
	Reads      int     `json:"reads"`
	Writes     int     `json:"writes"`
}

// ActiveAgent describes the currently-running wizard for the bead, if any.
// Nil in Data when no attempt is in-progress — the contract requires
// `active_agent: null` (not an empty object) on the wire.
type ActiveAgent struct {
	Name      string `json:"name"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Model     string `json:"model,omitempty"`
	Branch    string `json:"branch,omitempty"`
}

// LogLine is one entry from the wizard log tail. TS is an RFC3339-ish
// timestamp prefix when the log emits one; otherwise "". Line is always
// the raw line so a client that ignores TS loses nothing.
type LogLine struct {
	TS   string `json:"ts"`
	Line string `json:"line"`
}

// NotFoundError is returned by Collect when the bead ID does not resolve.
// The gateway uses errors.As to surface 404 vs 500 correctly.
type NotFoundError struct {
	ID  string
	Err error
}

func (e *NotFoundError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("bead not found: %s: %v", e.ID, e.Err)
	}
	return "bead not found: " + e.ID
}

func (e *NotFoundError) Unwrap() error { return e.Err }

// Options configures Collect behavior.
type Options struct {
	// Tail is the max number of log lines to return. 0 skips log tailing.
	Tail int

	// Registry resolves the active wizard's start time so the trace can
	// report ElapsedMs. Wired by callers in local mode (wizardregistry/local)
	// and cluster mode (wizardregistry/cluster). Nil leaves ElapsedMs at 0.
	Registry wizardregistry.Registry
}

// DefaultTailLines is the default log-tail cap callers should use when
// no explicit ?tail= query param is supplied.
const DefaultTailLines = 200

// MaxTailLines caps requests so a pathological ?tail=1000000 can't DOS
// the log reader.
const MaxTailLines = 2000

// Collect assembles the trace Data for beadID.
//   - Returns *NotFoundError when the bead does not exist (gateway → 404).
//   - For beads that exist but have never run, returns an empty-shape Data
//     (empty Pipeline, zero Totals, nil ActiveAgent, empty LogTail).
//   - Missing wizard-<bead>.log yields empty LogTail, not an error —
//     the bead existing but not having emitted logs is normal.
func Collect(beadID string, opts Options) (*Data, error) {
	target, err := store.GetBead(beadID)
	if err != nil {
		return nil, &NotFoundError{ID: beadID, Err: err}
	}

	data := &Data{
		Pipeline: []PipelineStep{},
		LogTail:  []LogLine{},
	}

	// Pipeline: step beads, sorted by formula topology.
	steps, _ := store.GetStepBeads(beadID)
	stepNames := make(map[string]bool, len(steps))
	for _, s := range steps {
		name := store.StepBeadPhaseName(s)
		if name == "" {
			continue
		}
		data.Pipeline = append(data.Pipeline, PipelineStep{
			Step:   name,
			Status: s.Status,
		})
		stepNames[name] = true
	}
	info := formula.BeadInfo{ID: target.ID, Type: target.Type, Labels: target.Labels}
	if g, ferr := formula.ResolveV3(info); ferr == nil {
		order := formula.StepOrderMap(g)
		sort.SliceStable(data.Pipeline, func(i, j int) bool {
			return stepPos(data.Pipeline[i].Step, order) < stepPos(data.Pipeline[j].Step, order)
		})
	}

	// Per-step metrics from agent_runs. Best-effort: older towers / beads
	// with no runs produce zero metrics, matching the empty-shape contract.
	if runs, mErr := observability.StepMetricsForBead(beadID); mErr == nil && len(runs) > 0 {
		applyStepMetrics(data.Pipeline, runs, stepNames)
	}

	// Sum totals.
	for _, p := range data.Pipeline {
		data.Totals.DurationMs += p.DurationMs
		data.Totals.CostUSD += p.CostUSD
		data.Totals.Retries += p.Retries
		data.Totals.Errors += p.Errors
		data.Totals.Warns += p.Warns
		data.Totals.Reads += p.Reads
		data.Totals.Writes += p.Writes
	}

	// Active agent.
	agentName := ""
	if attempt, _ := store.GetActiveAttempt(beadID); attempt != nil {
		aa := &ActiveAgent{Name: extractAgentName(*attempt)}
		for _, l := range attempt.Labels {
			switch {
			case strings.HasPrefix(l, "branch:"):
				aa.Branch = l[len("branch:"):]
			case strings.HasPrefix(l, "model:"):
				aa.Model = l[len("model:"):]
			}
		}
		agentName = aa.Name
		if aa.Name != "" && opts.Registry != nil {
			ctx := context.Background()
			if w, gerr := opts.Registry.Get(ctx, aa.Name); gerr == nil {
				if !w.StartedAt.IsZero() {
					aa.ElapsedMs = time.Since(w.StartedAt).Milliseconds()
				}
			} else if entries, lerr := opts.Registry.List(ctx); lerr == nil {
				for _, w := range entries {
					if w.BeadID == beadID && !w.StartedAt.IsZero() {
						aa.ElapsedMs = time.Since(w.StartedAt).Milliseconds()
						break
					}
				}
			}
		}
		data.ActiveAgent = aa
	}

	// Log tail. Prefer the active attempt's agent name; fall back to the
	// wizard-<beadID> convention so a never-run bead still resolves the
	// canonical log path (if one happens to exist from a prior attempt).
	if opts.Tail > 0 {
		tailN := opts.Tail
		if tailN > MaxTailLines {
			tailN = MaxTailLines
		}
		lookup := agentName
		if lookup == "" {
			lookup = "wizard-" + beadID
		}
		if path := resolveLogPath(lookup); path != "" {
			if lines, lErr := ReadLastLines(path, tailN); lErr == nil {
				data.LogTail = make([]LogLine, 0, len(lines))
				for _, ln := range lines {
					ts, rest := parseLogTS(ln)
					data.LogTail = append(data.LogTail, LogLine{TS: ts, Line: rest})
				}
			}
		}
	}

	return data, nil
}

// resolveLogPath returns the on-disk path of the first log file matching
// pkg/agent's naming conventions (<name>.log, <name>-fix.log,
// wizard-<name>.log). Returns "" when no log exists.
func resolveLogPath(name string) string {
	if name == "" {
		return ""
	}
	rc, err := agent.ResolveBackend("").Logs(name)
	if err != nil {
		return ""
	}
	defer rc.Close()
	if f, ok := rc.(*os.File); ok {
		return f.Name()
	}
	return ""
}

// extractAgentName returns the attempt bead's wizard name. Prefers the
// agent:<name> label; falls back to "attempt: <name>" title parsing for
// older beads that pre-date the label.
func extractAgentName(b store.Bead) string {
	if name := store.HasLabel(b, "agent:"); name != "" {
		return name
	}
	if strings.HasPrefix(b.Title, "attempt:") {
		return strings.TrimSpace(strings.TrimPrefix(b.Title, "attempt:"))
	}
	return ""
}

// stepPos returns a step's display position from the formula order map.
// Unknown names sort to 999 so they land at the end of the pipeline.
func stepPos(name string, order map[string]int) int {
	if order == nil {
		return 0
	}
	if p, ok := order[name]; ok {
		return p
	}
	return 999
}

// applyStepMetrics aggregates agent_runs rows by step name and fills in
// DurationMs / CostUSD / Reads / Writes / Retries on matching pipeline
// entries. Retries counts the runs-per-step beyond the first — an
// honest approximation until the executor stamps per-step retry counters.
func applyStepMetrics(pipeline []PipelineStep, runs []observability.StepRunRow, stepNames map[string]bool) {
	type agg struct {
		durationSec int
		cost        float64
		reads       int
		writes      int
		runs        int
	}
	byStep := make(map[string]*agg)
	for _, run := range runs {
		stepName := mapPhaseToStep(run.Phase, run.PhaseBucket, stepNames)
		if stepName == "" {
			continue
		}
		a, ok := byStep[stepName]
		if !ok {
			a = &agg{}
			byStep[stepName] = a
		}
		dur := run.Duration
		// For an active run (no completed_at) use a running clock so the
		// desktop's 5s poll shows elapsed time ticking up between restarts.
		if run.CompletedAt == "" && run.StartedAt != "" {
			if t, err := time.Parse(time.RFC3339, run.StartedAt); err == nil {
				dur = int(time.Since(t).Seconds())
			}
		}
		a.durationSec += dur
		a.cost += run.CostUSD
		a.runs++
		if run.ToolCallsJSON != "" {
			var tools map[string]int
			if json.Unmarshal([]byte(run.ToolCallsJSON), &tools) == nil {
				a.reads += tools["Read"]
				a.writes += tools["Write"]
			}
		}
	}
	for i := range pipeline {
		if a, ok := byStep[pipeline[i].Step]; ok {
			pipeline[i].DurationMs = int64(a.durationSec) * 1000
			pipeline[i].CostUSD = a.cost
			pipeline[i].Reads = a.reads
			pipeline[i].Writes = a.writes
			if a.runs > 1 {
				pipeline[i].Retries = a.runs - 1
			}
		}
	}
}

// mapPhaseToStep maps an agent_runs phase/bucket to a formula step name.
// Direct match first (phase == step), then bucket fallback for compound
// phases like "build-fix" → implement. Mirrors the CLI's trace logic so
// the two renderers can't drift.
func mapPhaseToStep(phase, phaseBucket string, stepNames map[string]bool) string {
	if stepNames[phase] {
		return phase
	}
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

// parseLogTS pulls an RFC3339-ish timestamp off the front of a log line.
// Returns ("", line) unchanged when the line has no recognizable prefix —
// the desktop receives the raw line via LogLine.Line either way, so the
// parse is a pure UI assist.
func parseLogTS(line string) (ts, rest string) {
	if len(line) >= 20 {
		candidate := line[:20]
		if _, err := time.Parse(time.RFC3339, candidate); err == nil {
			return candidate, strings.TrimSpace(line[20:])
		}
	}
	if len(line) >= 19 {
		candidate := line[:19]
		if _, err := time.Parse("2006-01-02 15:04:05", candidate); err == nil {
			return candidate, strings.TrimSpace(line[19:])
		}
		if _, err := time.Parse("2006-01-02T15:04:05", candidate); err == nil {
			return candidate, strings.TrimSpace(line[19:])
		}
	}
	return "", line
}

// ReadLastLines returns the last n lines of path with bounded memory.
// Uses a 256 KB scanner buffer (long log lines don't trip bufio.Scanner)
// and a rolling window of n*2 to avoid loading huge files into memory.
func ReadLastLines(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 256*1024)
	scanner.Buffer(buf, 256*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n*2 {
			lines = lines[len(lines)-n:]
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
