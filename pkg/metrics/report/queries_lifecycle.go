package report

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// lifecycleBeadTypes is the set of user-facing bead types for which
// we report lifecycle stats. Synthetic executor types (message,
// step, attempt, review) are excluded in the SQL filter.
var lifecycleBeadTypes = []string{"task", "epic", "bug", "feature", "chore", "design"}

// QueryLifecycleByType assembles the lifecycle panel: for each
// user-facing bead type, returns all six stages (F→R, F→S, F→C, R→S,
// R→C, S→C) with p50/p75/p95/p99 and the top-3 outliers above p95.
//
// The query runs against bead_lifecycle_olap filtered to beads that
// closed in the window. Stages with insufficient data return zeros
// (NULL percentiles are coalesced to 0 since the TS contract expects
// numbers, not null-check'able fields).
func (r *SQLReader) QueryLifecycleByType(ctx context.Context, scope Scope, since, until time.Time) ([]LifecycleByType, error) {
	if r.DB == nil {
		return nil, errNoDB
	}
	clause, scopeArgs := scope.beadIDClause("bead_id")

	// One query returns every (type, stage) aggregate row. The CASE
	// expression in each quantile gates the stage's duration; NULL
	// values drop out of quantile_cont naturally.
	q := fmt.Sprintf(`
		SELECT
			COALESCE(bead_type, 'unknown') AS bead_type,
			COUNT(*) AS cnt,
			-- F→R (filed → ready)
			quantile_cont(CASE WHEN ready_at IS NOT NULL AND filed_at IS NOT NULL
				THEN epoch(ready_at) - epoch(filed_at) END, 0.50) AS fr_p50,
			quantile_cont(CASE WHEN ready_at IS NOT NULL AND filed_at IS NOT NULL
				THEN epoch(ready_at) - epoch(filed_at) END, 0.75) AS fr_p75,
			quantile_cont(CASE WHEN ready_at IS NOT NULL AND filed_at IS NOT NULL
				THEN epoch(ready_at) - epoch(filed_at) END, 0.95) AS fr_p95,
			quantile_cont(CASE WHEN ready_at IS NOT NULL AND filed_at IS NOT NULL
				THEN epoch(ready_at) - epoch(filed_at) END, 0.99) AS fr_p99,
			-- F→S (filed → started)
			quantile_cont(CASE WHEN started_at IS NOT NULL AND filed_at IS NOT NULL
				THEN epoch(started_at) - epoch(filed_at) END, 0.50) AS fs_p50,
			quantile_cont(CASE WHEN started_at IS NOT NULL AND filed_at IS NOT NULL
				THEN epoch(started_at) - epoch(filed_at) END, 0.75) AS fs_p75,
			quantile_cont(CASE WHEN started_at IS NOT NULL AND filed_at IS NOT NULL
				THEN epoch(started_at) - epoch(filed_at) END, 0.95) AS fs_p95,
			quantile_cont(CASE WHEN started_at IS NOT NULL AND filed_at IS NOT NULL
				THEN epoch(started_at) - epoch(filed_at) END, 0.99) AS fs_p99,
			-- F→C (filed → closed)
			quantile_cont(epoch(closed_at) - epoch(filed_at), 0.50) AS fc_p50,
			quantile_cont(epoch(closed_at) - epoch(filed_at), 0.75) AS fc_p75,
			quantile_cont(epoch(closed_at) - epoch(filed_at), 0.95) AS fc_p95,
			quantile_cont(epoch(closed_at) - epoch(filed_at), 0.99) AS fc_p99,
			-- R→S (ready → started, queue)
			quantile_cont(CASE WHEN started_at IS NOT NULL AND ready_at IS NOT NULL
				THEN epoch(started_at) - epoch(ready_at) END, 0.50) AS rs_p50,
			quantile_cont(CASE WHEN started_at IS NOT NULL AND ready_at IS NOT NULL
				THEN epoch(started_at) - epoch(ready_at) END, 0.75) AS rs_p75,
			quantile_cont(CASE WHEN started_at IS NOT NULL AND ready_at IS NOT NULL
				THEN epoch(started_at) - epoch(ready_at) END, 0.95) AS rs_p95,
			quantile_cont(CASE WHEN started_at IS NOT NULL AND ready_at IS NOT NULL
				THEN epoch(started_at) - epoch(ready_at) END, 0.99) AS rs_p99,
			-- R→C (ready → closed)
			quantile_cont(CASE WHEN ready_at IS NOT NULL
				THEN epoch(closed_at) - epoch(ready_at) END, 0.50) AS rc_p50,
			quantile_cont(CASE WHEN ready_at IS NOT NULL
				THEN epoch(closed_at) - epoch(ready_at) END, 0.75) AS rc_p75,
			quantile_cont(CASE WHEN ready_at IS NOT NULL
				THEN epoch(closed_at) - epoch(ready_at) END, 0.95) AS rc_p95,
			quantile_cont(CASE WHEN ready_at IS NOT NULL
				THEN epoch(closed_at) - epoch(ready_at) END, 0.99) AS rc_p99,
			-- S→C (started → closed)
			quantile_cont(CASE WHEN started_at IS NOT NULL
				THEN epoch(closed_at) - epoch(started_at) END, 0.50) AS sc_p50,
			quantile_cont(CASE WHEN started_at IS NOT NULL
				THEN epoch(closed_at) - epoch(started_at) END, 0.75) AS sc_p75,
			quantile_cont(CASE WHEN started_at IS NOT NULL
				THEN epoch(closed_at) - epoch(started_at) END, 0.95) AS sc_p95,
			quantile_cont(CASE WHEN started_at IS NOT NULL
				THEN epoch(closed_at) - epoch(started_at) END, 0.99) AS sc_p99
		FROM bead_lifecycle_olap
		WHERE closed_at IS NOT NULL AND filed_at IS NOT NULL
		  AND closed_at >= ? AND closed_at <= ?
		  AND (bead_type IS NULL OR bead_type NOT IN ('message', 'step', 'attempt', 'review'))%s
		GROUP BY 1
	`, clause)

	args := append([]any{since, until}, scopeArgs...)
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("lifecycle by type: %w", err)
	}
	defer rows.Close()

	type stageQuantiles struct {
		p50, p75, p95, p99 sql.NullFloat64
	}
	type typeRow struct {
		beadType string
		count    int
		fr, fs, fc, rs, rc, sc stageQuantiles
	}

	var byType []typeRow
	for rows.Next() {
		var tr typeRow
		if err := rows.Scan(
			&tr.beadType, &tr.count,
			&tr.fr.p50, &tr.fr.p75, &tr.fr.p95, &tr.fr.p99,
			&tr.fs.p50, &tr.fs.p75, &tr.fs.p95, &tr.fs.p99,
			&tr.fc.p50, &tr.fc.p75, &tr.fc.p95, &tr.fc.p99,
			&tr.rs.p50, &tr.rs.p75, &tr.rs.p95, &tr.rs.p99,
			&tr.rc.p50, &tr.rc.p75, &tr.rc.p95, &tr.rc.p99,
			&tr.sc.p50, &tr.sc.p75, &tr.sc.p95, &tr.sc.p99,
		); err != nil {
			return nil, err
		}
		byType = append(byType, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Fetch outliers for each (type, stage) combination in one query —
	// TOP-3 above p95. LATERAL-free version: do a subquery per type
	// and filter in Go; cheap at the expected cardinality (<6 types).
	outliers, err := r.queryLifecycleOutliers(ctx, scope, since, until)
	if err != nil {
		return nil, err
	}

	// Translate typeRow -> LifecycleByType{stages: [...6 stages...]}
	// Order types by the canonical list so the frontend renders in a
	// stable order regardless of which types have data.
	rowByType := make(map[string]typeRow, len(byType))
	for _, tr := range byType {
		rowByType[tr.beadType] = tr
	}

	out := make([]LifecycleByType, 0, len(lifecycleBeadTypes))
	for _, bt := range lifecycleBeadTypes {
		tr, ok := rowByType[bt]
		if !ok {
			continue
		}
		stages := []LifecycleStageStats{
			mkStage("F→R", tr.fr, outliers[outlierKey{bt, "F→R"}]),
			mkStage("F→S", tr.fs, outliers[outlierKey{bt, "F→S"}]),
			mkStage("F→C", tr.fc, outliers[outlierKey{bt, "F→C"}]),
			mkStage("R→S", tr.rs, outliers[outlierKey{bt, "R→S"}]),
			mkStage("R→C", tr.rc, outliers[outlierKey{bt, "R→C"}]),
			mkStage("S→C", tr.sc, outliers[outlierKey{bt, "S→C"}]),
		}
		out = append(out, LifecycleByType{Type: bt, Stages: stages})
	}
	return out, nil
}

// mkStage converts a (stage label, quantiles, outliers) triple into
// the wire shape. NULL quantiles coalesce to 0 per the TS contract.
func mkStage(name string, q struct{ p50, p75, p95, p99 sql.NullFloat64 }, out []LifecycleOutlier) LifecycleStageStats {
	if out == nil {
		out = []LifecycleOutlier{}
	}
	return LifecycleStageStats{
		Stage:    name,
		P50:      nullFloat(q.p50),
		P75:      nullFloat(q.p75),
		P95:      nullFloat(q.p95),
		P99:      nullFloat(q.p99),
		Outliers: out,
	}
}

// outlierKey is the (bead_type, stage) composite key for the outlier
// lookup map built by queryLifecycleOutliers.
type outlierKey struct {
	beadType string
	stage    string
}

// queryLifecycleOutliers returns a map of (type, stage) → top-3
// outliers sorted by duration descending. It runs a single SQL
// statement that unrolls each stage via UNION ALL so we don't issue
// 6 queries per type.
func (r *SQLReader) queryLifecycleOutliers(ctx context.Context, scope Scope, since, until time.Time) (map[outlierKey][]LifecycleOutlier, error) {
	clause, scopeArgs := scope.beadIDClause("bead_id")
	// For each stage, emit (type, stage, bead_id, duration); the
	// report layer caps to top-3 per key after the fact. Doing the
	// cap in SQL with QUALIFY / ROW_NUMBER() is doable but adds
	// complexity for a tiny win at our row count.
	q := fmt.Sprintf(`
		WITH base AS (
			SELECT bead_id, COALESCE(bead_type, 'unknown') AS bead_type,
			       filed_at, ready_at, started_at, closed_at
			FROM bead_lifecycle_olap
			WHERE closed_at IS NOT NULL AND filed_at IS NOT NULL
			  AND closed_at >= ? AND closed_at <= ?
			  AND (bead_type IS NULL OR bead_type NOT IN ('message', 'step', 'attempt', 'review'))%s
		)
		SELECT bead_type, 'F→R' AS stage, bead_id,
		       (epoch(ready_at) - epoch(filed_at)) AS dur
		  FROM base WHERE ready_at IS NOT NULL
		UNION ALL
		SELECT bead_type, 'F→S' AS stage, bead_id,
		       (epoch(started_at) - epoch(filed_at)) AS dur
		  FROM base WHERE started_at IS NOT NULL
		UNION ALL
		SELECT bead_type, 'F→C' AS stage, bead_id,
		       (epoch(closed_at) - epoch(filed_at)) AS dur
		  FROM base
		UNION ALL
		SELECT bead_type, 'R→S' AS stage, bead_id,
		       (epoch(started_at) - epoch(ready_at)) AS dur
		  FROM base WHERE started_at IS NOT NULL AND ready_at IS NOT NULL
		UNION ALL
		SELECT bead_type, 'R→C' AS stage, bead_id,
		       (epoch(closed_at) - epoch(ready_at)) AS dur
		  FROM base WHERE ready_at IS NOT NULL
		UNION ALL
		SELECT bead_type, 'S→C' AS stage, bead_id,
		       (epoch(closed_at) - epoch(started_at)) AS dur
		  FROM base WHERE started_at IS NOT NULL
	`, clause)

	args := append([]any{since, until}, scopeArgs...)
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("lifecycle outliers: %w", err)
	}
	defer rows.Close()

	type rowRec struct {
		beadType, stage, beadID string
		duration                float64
	}
	all := make(map[outlierKey][]rowRec)
	for rows.Next() {
		var rr rowRec
		if err := rows.Scan(&rr.beadType, &rr.stage, &rr.beadID, &rr.duration); err != nil {
			return nil, err
		}
		k := outlierKey{rr.beadType, rr.stage}
		all[k] = append(all[k], rr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Sort and cap to top-3 per (type, stage). Skip entries whose
	// duration is <= the slice's 95th percentile so we only surface
	// true outliers, not every closed bead. Cheap approximation:
	// 95th percentile = 95th element of sorted list.
	result := make(map[outlierKey][]LifecycleOutlier, len(all))
	for k, slice := range all {
		sort.Slice(slice, func(i, j int) bool {
			return slice[i].duration > slice[j].duration
		})
		// p95 cutoff: the 5th-percentile-from-top index.
		var cutoff float64
		if n := len(slice); n > 20 {
			cutoff = slice[n/20].duration
		}
		var picks []LifecycleOutlier
		for _, rr := range slice {
			if rr.duration < cutoff {
				break
			}
			picks = append(picks, LifecycleOutlier{
				BeadID:          rr.beadID,
				DurationSeconds: rr.duration,
			})
			if len(picks) >= 3 {
				break
			}
		}
		result[k] = picks
	}
	return result, nil
}
