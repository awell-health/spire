package report

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// errNoDB is returned by every SQLReader method when DB is nil. The
// gateway maps this to a 503 so the frontend falls back to its
// fixture without leaking DuckDB internals to the browser.
var errNoDB = errors.New("report: reader has no database connection")

// QueryThroughputWeekly returns one row per ISO week in the window.
// RunsSuccess / RunsFailure count agent runs (not merges); the
// LeadTimeP50Seconds is derived from bead_lifecycle_olap closures in
// that week so it reflects end-to-end time, not per-phase time.
//
// The query always returns at least 12 rows of history (backfilled
// zero-valued rows) so the frontend sparkline has a consistent length
// regardless of how quiet the window was.
func (r *SQLReader) QueryThroughputWeekly(ctx context.Context, scope Scope, since, until time.Time) ([]ThroughputWeek, error) {
	if r.DB == nil {
		return nil, errNoDB
	}
	// Always widen to at least 12 weeks so sparkline / WoW deltas have
	// enough history even for a 24h or 7d window.
	wideSince := since
	if t := until.AddDate(0, 0, -84); t.Before(wideSince) {
		wideSince = t
	}

	runsClause, runsArgs := scope.beadIDClause("bead_id")
	lifeClause, lifeArgs := scope.beadIDClause("bead_id")

	runsSQL := fmt.Sprintf(`
		SELECT
			date_trunc('week', started_at)::DATE AS week_start,
			SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) AS runs_success,
			SUM(CASE WHEN result NOT IN ('success', 'skipped', 'approve', 'no_changes', '') THEN 1 ELSE 0 END) AS runs_failure
		FROM agent_runs_olap
		WHERE started_at >= ? AND started_at <= ?%s
		GROUP BY 1
	`, runsClause)
	lifeSQL := fmt.Sprintf(`
		SELECT
			date_trunc('week', closed_at)::DATE AS week_start,
			quantile_cont(epoch(closed_at) - epoch(filed_at), 0.50) AS lead_p50
		FROM bead_lifecycle_olap
		WHERE closed_at IS NOT NULL AND filed_at IS NOT NULL
		  AND closed_at >= ? AND closed_at <= ?
		  AND (bead_type IS NULL OR bead_type NOT IN ('message', 'step', 'attempt', 'review'))%s
		GROUP BY 1
	`, lifeClause)

	runsArgs = append([]any{wideSince, until}, runsArgs...)
	lifeArgs = append([]any{wideSince, until}, lifeArgs...)

	type wkRow struct {
		week             time.Time
		success, failure int
		leadP50          float64
	}
	byWeek := make(map[time.Time]*wkRow)

	rows, err := r.DB.QueryContext(ctx, runsSQL, runsArgs...)
	if err != nil {
		return nil, fmt.Errorf("throughput runs: %w", err)
	}
	for rows.Next() {
		var (
			week        time.Time
			success, f  int
		)
		if err := rows.Scan(&week, &success, &f); err != nil {
			rows.Close()
			return nil, err
		}
		week = week.UTC()
		r := byWeek[week]
		if r == nil {
			r = &wkRow{week: week}
			byWeek[week] = r
		}
		r.success = success
		r.failure = f
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	lrows, err := r.DB.QueryContext(ctx, lifeSQL, lifeArgs...)
	if err != nil {
		return nil, fmt.Errorf("throughput lead-time: %w", err)
	}
	for lrows.Next() {
		var (
			week    time.Time
			leadP50 sql.NullFloat64
		)
		if err := lrows.Scan(&week, &leadP50); err != nil {
			lrows.Close()
			return nil, err
		}
		week = week.UTC()
		r := byWeek[week]
		if r == nil {
			r = &wkRow{week: week}
			byWeek[week] = r
		}
		r.leadP50 = nullFloat(leadP50)
	}
	lrows.Close()
	if err := lrows.Err(); err != nil {
		return nil, err
	}

	// Generate a continuous 12-week spine ending at the most recent
	// Monday so sparkline position is predictable.
	end := startOfWeek(until)
	out := make([]ThroughputWeek, 0, 12)
	for i := 11; i >= 0; i-- {
		week := end.AddDate(0, 0, -7*i)
		r := byWeek[week]
		tw := ThroughputWeek{WeekStart: week.Format("2006-01-02")}
		if r != nil {
			tw.RunsSuccess = r.success
			tw.RunsFailure = r.failure
			tw.LeadTimeP50Seconds = r.leadP50
		}
		out = append(out, tw)
	}
	return out, nil
}

// startOfWeek rounds t down to the preceding Monday (UTC) —
// aligns with DuckDB's date_trunc('week', ...).
func startOfWeek(t time.Time) time.Time {
	t = t.UTC()
	// Monday = 1, Sunday = 0 — Go's Weekday has Sun=0 so shift.
	offset := (int(t.Weekday()) + 6) % 7
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -offset)
}
