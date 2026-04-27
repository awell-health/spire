// attempt.go implements `spire attempt show`: render per-invocation
// tool calls captured during an attempt, with args / results lifted out
// of the OTLP attribute payload. Sage / cleric / archmage use this
// surface to audit "did the agent walk the right context" without
// having to query DuckDB by hand.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/observability"
	"github.com/awell-health/spire/pkg/olap"
	"github.com/spf13/cobra"
)

const (
	attemptShowDefaultArgWidth = 200
	attemptShowDefaultPageSize = 200
)

// attemptCmd is the parent for attempt-scoped subcommands. Today only
// `show` exists; a future `list` could surface attempt history per
// bead without going through `bd show` + label munging.
var attemptCmd = &cobra.Command{
	Use:   "attempt",
	Short: "Inspect agent attempts",
}

var attemptShowCmd = &cobra.Command{
	Use:   "show <attempt-id>",
	Short: "Render per-invocation tool calls captured during an attempt",
	Long: `Render per-invocation tool calls (Bash command text, Read file paths,
Grep patterns, etc.) for one attempt bead, ordered by timestamp. Used by
sage during review to verify the agent walked the relevant context.

Args / results are lifted from the OTLP attribute payload; rows show
"source: span" when the rich payload came from a tool span and
"source: log" when only a log-side row was captured (rare).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		page, _ := cmd.Flags().GetInt("page")
		pageSize, _ := cmd.Flags().GetInt("page-size")
		argWidth, _ := cmd.Flags().GetInt("arg-width")
		asJSON, _ := cmd.Flags().GetBool("json")
		return cmdAttemptShow(args[0], page, pageSize, argWidth, asJSON)
	},
}

func init() {
	attemptCmd.AddCommand(attemptShowCmd)
	attemptShowCmd.Flags().Int("page", 1, "Page number (one-based)")
	attemptShowCmd.Flags().Int("page-size", attemptShowDefaultPageSize, "Rows per page (max 1000)")
	attemptShowCmd.Flags().Int("arg-width", attemptShowDefaultArgWidth, "Max chars per attribute value before truncation")
	attemptShowCmd.Flags().Bool("json", false, "Emit raw rows as JSON instead of the human-readable table")
}

// attemptListFunc is the seam through which cmdAttemptShow queries the
// OLAP store. Tests can swap it out to render against fixtures.
var attemptListFunc = observability.ListAttemptToolCalls

// cmdAttemptShow drives `spire attempt show`. It opens the active
// tower's OLAP store, reads the requested page of tool calls, and
// renders them in either text-table or raw-JSON form.
func cmdAttemptShow(attemptID string, page, pageSize, argWidth int, asJSON bool) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	rows, err := attemptListFunc(attemptID, page, pageSize)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintf(os.Stdout, "No tool calls recorded for %s\n", attemptID)
		return nil
	}
	renderAttemptToolCalls(os.Stdout, attemptID, rows, argWidth)
	return nil
}

// renderAttemptToolCalls writes a compact, monospaced table of tool
// calls suitable for the terminal. Each row gets one header line
// (timestamp, tool, status, duration) and one or more attribute
// lines showing the lifted args/results. Long values are truncateAttemptValued
// to argWidth (default 200) — the full value is always available
// via --json.
func renderAttemptToolCalls(w *os.File, attemptID string, rows []olap.ToolCallRecord, argWidth int) {
	if argWidth <= 0 {
		argWidth = attemptShowDefaultArgWidth
	}
	fmt.Fprintf(w, "Attempt %s — %d tool call(s)\n\n", attemptID, len(rows))
	for i, r := range rows {
		status := "ok"
		if !r.Success {
			status = "FAIL"
		}
		fmt.Fprintf(w, "%d. %s  %s  %s  %dms  [src=%s]\n",
			i+1,
			r.Timestamp.UTC().Format("15:04:05"),
			r.ToolName,
			status,
			r.DurationMs,
			r.Source,
		)

		// Pretty-print the lifted attributes inline. Skip the
		// session/identity context — those are constant across
		// rows and just clutter the table.
		args := liftToolArgs(r.Attributes)
		for _, k := range orderedAttrKeys(args) {
			v := truncateAttemptValue(args[k], argWidth)
			fmt.Fprintf(w, "      %-14s  %s\n", k+":", v)
		}
		if r.Step != "" {
			fmt.Fprintf(w, "      step:           %s\n", r.Step)
		}
		fmt.Fprintln(w)
	}
}

// liftToolArgs decodes the JSON attribute blob and returns just the
// tool-input/output keys (Bash command, Read file_path, Grep pattern,
// etc.). Identity keys (session.id, user.email, organization.id) are
// dropped — they're useful for analytics joins, not for human review.
// Returns nil when the blob is empty or invalid.
func liftToolArgs(attributes string) map[string]string {
	if attributes == "" || attributes == "{}" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(attributes), &m); err != nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		// Skip the identity context — these are present on every
		// row and would crowd out the actual args.
		switch k {
		case "session.id", "user.email", "organization.id",
			"user.id", "user.account_uuid", "tower",
			"agent.name", "bead.id", "step", "service.name",
			"service.version", "service.instance.id":
			continue
		}
		switch tv := v.(type) {
		case string:
			if tv == "" {
				continue
			}
			out[k] = tv
		case []any:
			// Span events serialize as a slice; render a one-line
			// summary so reviewers see "events: 2 captured" and can
			// pull the JSON if they need detail.
			out[k] = fmt.Sprintf("(%d items, see --json)", len(tv))
		case map[string]any:
			out[k] = "(object, see --json)"
		default:
			out[k] = fmt.Sprintf("%v", tv)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// orderedAttrKeys returns args keys in a stable, human-friendly order:
// command/file_path/pattern first (the most useful for review), then
// the rest alphabetically. Stable ordering matters for terminal output
// — two consecutive runs against the same data should look identical.
func orderedAttrKeys(args map[string]string) []string {
	if len(args) == 0 {
		return nil
	}
	priority := []string{"command", "file_path", "pattern", "tool_input", "input_value",
		"old_string", "new_string", "tool_output", "output_value", "result", "error", "error_message"}
	seen := make(map[string]bool, len(args))
	out := make([]string, 0, len(args))
	for _, k := range priority {
		if _, ok := args[k]; ok {
			out = append(out, k)
			seen[k] = true
		}
	}
	// Append remaining keys in deterministic (sorted) order so each
	// row's lower section renders the same way every run.
	rest := make([]string, 0, len(args)-len(seen))
	for k := range args {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sortStrings(rest)
	return append(out, rest...)
}

// truncateAttemptValue clamps s to at most n runes and appends an ellipsis when
// trimming. Newlines are replaced with " | " so the table stays one
// line per attribute even for multi-line Bash commands.
func truncateAttemptValue(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " | ")
	if n <= 0 || len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// sortStrings is a tiny dependency-free sort to avoid pulling sort
// into this command file just for one call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
