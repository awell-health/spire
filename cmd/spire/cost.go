package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/observability"
	"github.com/spf13/cobra"
)

// costCmd surfaces the auth_profile observability added in spi-bkug1n:
// a cost summary split by credential slot (subscription vs api-key).
// Default section preserves the "Total runs / tokens / cost" shape the
// rest of the metrics CLI uses; the per-slot split is appended below.
var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Agent-run cost summary, split by auth profile (subscription/api-key)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdCost(args)
	},
}

// costStdoutWriter is the output sink for `spire cost`. Tests swap this
// to capture rendered output without touching os.Stdout.
var costStdoutWriter io.Writer = os.Stdout

// authCostAggregate is one bucket of the per-slot cost split. The
// starting auth_profile determines which bucket a run lands in; see
// authObservabilityReader for the 429-swap attribution convention.
type authCostAggregate struct {
	Slot             string // "subscription", "api-key", or "" for NULL rows
	Runs             int
	TotalTokens      int
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
	SwapCount        int // rows where auth_profile_final differs from auth_profile
}

// authRunDisplay is one row of the "Recent runs" block that `spire
// config auth show` appends below its slot summary.
type authRunDisplay struct {
	StartedAt   string // raw started_at string as bd sql --json returned it
	BeadID      string
	Phase       string
	TotalTokens int
	Swapped     bool // auth_profile_final set AND != auth_profile
}

// authObservabilityReader exposes the agent_runs queries that `spire
// cost` and `spire config auth show` share. Injected via a package-level
// variable so tests can feed in synthetic rows without a live Dolt.
type authObservabilityReader interface {
	// CostSplitByAuthProfile aggregates agent_runs grouped by the
	// starting auth_profile. Rows with NULL auth_profile bucket under
	// the empty-string key.
	//
	// Attribution convention for 429 swap rows (auth_profile_final is
	// non-null and differs from auth_profile): the run's full tokens
	// and cost contribute to the STARTING slot's totals, and that slot's
	// SwapCount is incremented. A token-level split between pre- and
	// post-swap segments isn't recoverable from the schema — Anthropic
	// bills per-call rather than per-run-segment — so this is
	// best-effort and flagged to the operator via the swap annotation.
	CostSplitByAuthProfile() (map[string]authCostAggregate, error)

	// RecentRunsByAuthProfile returns up to limit rows whose starting
	// auth_profile equals slot, ordered by started_at DESC.
	RecentRunsByAuthProfile(slot string, limit int) ([]authRunDisplay, error)
}

// authObsReader is the live reader. Tests override this variable (with
// cleanup) to inject a stub implementation.
var authObsReader authObservabilityReader = defaultAuthObsReader{}

// defaultAuthObsReader queries the tower's agent_runs table directly
// through `bd sql --json`. We target the Dolt source table rather than
// the DuckDB agent_runs_olap view because the auth_profile /
// auth_profile_final columns (spi-bkug1n) are not yet mirrored into the
// OLAP schema.
type defaultAuthObsReader struct{}

func (defaultAuthObsReader) CostSplitByAuthProfile() (map[string]authCostAggregate, error) {
	query := `SELECT
    COALESCE(auth_profile, '') AS slot,
    COUNT(*) AS runs,
    COALESCE(SUM(total_tokens), 0) AS total_tokens,
    COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
    COALESCE(SUM(cache_write_tokens), 0) AS cache_write_tokens,
    COALESCE(SUM(cost_usd), 0) AS cost_usd,
    COALESCE(SUM(CASE WHEN auth_profile_final IS NOT NULL AND auth_profile_final <> auth_profile THEN 1 ELSE 0 END), 0) AS swap_count
FROM agent_runs
GROUP BY COALESCE(auth_profile, '')`

	rows, err := observability.QueryJSON(query)
	if err != nil {
		return nil, err
	}
	out := make(map[string]authCostAggregate, len(rows)+2)
	for _, r := range rows {
		slot := observability.ToString(r["slot"])
		out[slot] = authCostAggregate{
			Slot:             slot,
			Runs:             observability.ToInt(r["runs"]),
			TotalTokens:      observability.ToInt(r["total_tokens"]),
			CacheReadTokens:  int64(observability.ToInt(r["cache_read_tokens"])),
			CacheWriteTokens: int64(observability.ToInt(r["cache_write_tokens"])),
			CostUSD:          observability.ToFloat(r["cost_usd"]),
			SwapCount:        observability.ToInt(r["swap_count"]),
		}
	}
	return out, nil
}

func (defaultAuthObsReader) RecentRunsByAuthProfile(slot string, limit int) ([]authRunDisplay, error) {
	if limit <= 0 {
		return nil, nil
	}
	// Escape slot as a defensive measure; call sites already constrain
	// it to the two known constants, but building SQL with %s is a
	// landmine waiting to happen if that ever loosens.
	esc := strings.ReplaceAll(slot, "'", "''")
	query := fmt.Sprintf(`SELECT
    COALESCE(started_at, '') AS started_at,
    COALESCE(bead_id, '') AS bead_id,
    COALESCE(phase, '') AS phase,
    COALESCE(total_tokens, 0) AS total_tokens,
    CASE WHEN auth_profile_final IS NOT NULL AND auth_profile_final <> auth_profile THEN 1 ELSE 0 END AS swapped
FROM agent_runs
WHERE auth_profile = '%s'
ORDER BY started_at DESC
LIMIT %d`, esc, limit)

	rows, err := observability.QueryJSON(query)
	if err != nil {
		return nil, err
	}
	out := make([]authRunDisplay, 0, len(rows))
	for _, r := range rows {
		out = append(out, authRunDisplay{
			StartedAt:   observability.ToString(r["started_at"]),
			BeadID:      observability.ToString(r["bead_id"]),
			Phase:       observability.ToString(r["phase"]),
			TotalTokens: observability.ToInt(r["total_tokens"]),
			Swapped:     observability.ToInt(r["swapped"]) > 0,
		})
	}
	return out, nil
}

// cmdCost is the entry point for `spire cost`. Reads agent_runs, renders
// a top-line total summary (maintaining the shape used elsewhere), then
// appends a per-auth-profile split.
func cmdCost(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if len(args) > 0 {
		return errors.New("usage: spire cost")
	}

	split, err := authObsReader.CostSplitByAuthProfile()
	if err != nil {
		return fmt.Errorf("read cost split: %w", err)
	}
	renderCost(costStdoutWriter, split)
	return nil
}

// renderCost writes the total-then-split cost view. The first block
// mirrors the "Total runs / tokens / cost" shape used by `spire
// metrics`; the second block adds the subscription/api-key breakdown
// and surfaces 429 swap attribution.
func renderCost(w io.Writer, split map[string]authCostAggregate) {
	var total authCostAggregate
	for _, v := range split {
		total.Runs += v.Runs
		total.TotalTokens += v.TotalTokens
		total.CacheReadTokens += v.CacheReadTokens
		total.CacheWriteTokens += v.CacheWriteTokens
		total.CostUSD += v.CostUSD
		total.SwapCount += v.SwapCount
	}

	fmt.Fprintln(w, "Cost (all recorded runs)")
	fmt.Fprintf(w, "  Total runs: %d   Total tokens: %s   Total cost: $%.2f\n",
		total.Runs, humanTokens(total.TotalTokens), total.CostUSD)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "By auth profile:")
	sub := split[config.AuthSlotSubscription]
	api := split[config.AuthSlotAPIKey]
	// subscription row: dollar value is a placeholder because Anthropic
	// doesn't expose per-token subscription spend. Tokens reflect real
	// usage; the `metered` annotation tells operators not to read the
	// zero as "free."
	fmt.Fprintf(w, "  %-14s %5d runs  %9s tokens   $0 metered\n",
		config.AuthSlotSubscription+":", sub.Runs, humanTokens(sub.TotalTokens))
	fmt.Fprintf(w, "  %-14s %5d runs  %9s tokens   $%.2f actual\n",
		config.AuthSlotAPIKey+":", api.Runs, humanTokens(api.TotalTokens), api.CostUSD)

	// Swap annotation: attribution is to the starting slot, so a swap
	// inflates the subscription bucket's run count while the actual
	// spend ends up in the api-key dollars column. Calling that out
	// explicitly here so operators can reconcile.
	if total.SwapCount > 0 {
		fmt.Fprintf(w, "  (%d run%s promoted subscription → api-key after 429; attributed to subscription)\n",
			total.SwapCount, plural(total.SwapCount))
	}
	// Surface unattributed rows (auth_profile NULL — historical rows
	// pre-dating the spawn-point plumbing) so their tokens/cost aren't
	// invisible in the split totals.
	if unrec, ok := split[""]; ok && unrec.Runs > 0 {
		fmt.Fprintf(w, "  %-14s %5d runs  %9s tokens   $%.2f (no auth_profile recorded)\n",
			"(unrecorded)", unrec.Runs, humanTokens(unrec.TotalTokens), unrec.CostUSD)
	}
}

// renderRecentRunsPerSlot writes the "Recent runs" block `spire config
// auth show` appends below its slot summary. Each slot gets up to
// `limit` of its most-recent runs; empty slots render "(no runs yet)".
func renderRecentRunsPerSlot(w io.Writer, reader authObservabilityReader, limit int) error {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Recent runs (last %d per slot)\n", limit)
	for _, slot := range []string{config.AuthSlotSubscription, config.AuthSlotAPIKey} {
		fmt.Fprintf(w, "  %s:\n", slot)
		runs, err := reader.RecentRunsByAuthProfile(slot, limit)
		if err != nil {
			return fmt.Errorf("recent runs for %s: %w", slot, err)
		}
		if len(runs) == 0 {
			fmt.Fprintln(w, "    (no runs yet)")
			continue
		}
		for _, r := range runs {
			swap := ""
			if r.Swapped {
				swap = "  (swap→api-key)"
			}
			fmt.Fprintf(w, "    %s  %-18s %-16s %7s tokens%s\n",
				formatStartedAt(r.StartedAt),
				truncate(r.BeadID, 18),
				truncate(r.Phase, 16),
				humanTokens(r.TotalTokens), swap)
		}
	}
	return nil
}

// humanTokens renders a token count using the most-significant unit so
// the split table lines up even at 7-digit totals. Matches the k/M
// convention the metrics tables already use.
func humanTokens(n int) string {
	switch {
	case n < 1_000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// formatStartedAt normalizes an agent_runs.started_at string into a
// compact UTC "YYYY-MM-DD HH:MM" display. bd sql --json returns Dolt
// DATETIME values as "2026-04-24 17:34:43" by default; RFC3339 is
// accepted as a fallback for callers that format timestamps at insert.
// Unparseable values fall through unchanged so no data is lost.
func formatStartedAt(raw string) string {
	if raw == "" {
		return "—"
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format("2006-01-02 15:04")
		}
	}
	return raw
}
