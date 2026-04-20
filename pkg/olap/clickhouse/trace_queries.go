package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/olap"
)

// The TraceReader implementation translates the DuckDB query surface to
// ClickHouse idioms: MergeTree ORDER BY guarantees sort order without
// extra cost, groupArray / argMax aggregate per-group, and FINAL is
// avoided here because tool_spans is plain MergeTree (not Replacing).

// defaultTracesLimit caps QueryTraces results when filter.Limit is 0.
const defaultTracesLimit = 200

// QuerySpans returns every span for a single trace_id, ordered by
// start_time. Used by the waterfall view when the caller already has a
// trace_id (e.g. follow-up from QueryTraces).
func (s *Store) QuerySpans(ctx context.Context, traceID string) ([]olap.SpanRecord, error) {
	const q = `
		SELECT
			trace_id,
			span_id,
			parent_span_id,
			span_name,
			kind,
			duration_ms,
			success,
			start_time,
			end_time,
			attributes
		FROM tool_spans
		WHERE trace_id = ?
		ORDER BY start_time ASC
	`
	rows, err := s.db.QueryContext(ctx, q, traceID)
	if err != nil {
		return nil, fmt.Errorf("clickhouse QuerySpans: %w", err)
	}
	defer rows.Close()
	return scanSpans(rows)
}

// QueryTraces returns trace summaries matching the filter. One row per
// trace_id aggregated from tool_spans via argMax for the root span
// name, MIN/MAX for the time window, COUNT for span_count, and an
// AND-of-success reduction over per-span success flags.
func (s *Store) QueryTraces(ctx context.Context, filter olap.TraceFilter) ([]olap.TraceSummary, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultTracesLimit
	}

	// Build WHERE dynamically. ClickHouse's query planner prefers the
	// filters inside the subquery so the primary key can be used.
	args := []any{}
	where := "1 = 1"
	if filter.BeadID != "" {
		where += " AND bead_id = ?"
		args = append(args, filter.BeadID)
	}
	if !filter.Since.IsZero() {
		where += " AND start_time >= ?"
		args = append(args, filter.Since)
	}
	if !filter.Until.IsZero() {
		where += " AND start_time < ?"
		args = append(args, filter.Until)
	}

	// argMax(span_name, -parent_span_id empty bias) picks the root row —
	// the one with an empty parent_span_id. We approximate by argMax
	// against start_time ascending (root usually starts first), with a
	// tie break on empty parent_span_id via argMax(... parent_span_id='').
	q := fmt.Sprintf(`
		SELECT
			trace_id,
			any(bead_id)                                               AS bead_id,
			argMax(span_name, parent_span_id = '')                     AS root_span,
			count()                                                    AS span_count,
			toInt32(dateDiff('millisecond', min(start_time), max(end_time))) AS duration_ms,
			min(start_time)                                            AS trace_start,
			max(end_time)                                              AS trace_end,
			min(success)                                               AS success
		FROM tool_spans
		WHERE %s
		GROUP BY trace_id
		ORDER BY trace_start DESC
		LIMIT ?
	`, where)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse QueryTraces: %w", err)
	}
	defer rows.Close()

	var out []olap.TraceSummary
	for rows.Next() {
		var t olap.TraceSummary
		if err := rows.Scan(
			&t.TraceID, &t.BeadID, &t.RootSpan, &t.SpanCount,
			&t.DurationMs, &t.StartTime, &t.EndTime, &t.Success,
		); err != nil {
			return nil, fmt.Errorf("clickhouse QueryTraces scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// QueryToolSpansByBead returns every span recorded for a bead,
// ordered by start_time. Powers the per-bead waterfall in
// `spire trace`.
func (s *Store) QueryToolSpansByBead(beadID string) ([]olap.SpanRecord, error) {
	const q = `
		SELECT
			trace_id,
			span_id,
			parent_span_id,
			span_name,
			kind,
			duration_ms,
			success,
			start_time,
			end_time,
			attributes
		FROM tool_spans
		WHERE bead_id = ?
		ORDER BY start_time ASC
	`
	rows, err := s.db.QueryContext(context.Background(), q, beadID)
	if err != nil {
		return nil, fmt.Errorf("clickhouse QueryToolSpansByBead: %w", err)
	}
	defer rows.Close()
	return scanSpans(rows)
}

// QueryToolEventsByBead returns aggregated tool-call counts for a bead,
// grouped by tool_name and step (empty string when step is unset).
func (s *Store) QueryToolEventsByBead(beadID string) ([]olap.ToolEventStats, error) {
	const q = `
		SELECT
			tool_name,
			step,
			count()                              AS cnt,
			avg(duration_ms)                     AS avg_duration_ms,
			countIf(success = false)             AS failure_count
		FROM tool_events
		WHERE bead_id = ?
		GROUP BY tool_name, step
		ORDER BY cnt DESC
	`
	rows, err := s.db.QueryContext(context.Background(), q, beadID)
	if err != nil {
		return nil, fmt.Errorf("clickhouse QueryToolEventsByBead: %w", err)
	}
	defer rows.Close()

	var out []olap.ToolEventStats
	for rows.Next() {
		var e olap.ToolEventStats
		if err := rows.Scan(&e.ToolName, &e.Step, &e.Count, &e.AvgDurationMs, &e.FailureCount); err != nil {
			return nil, fmt.Errorf("clickhouse QueryToolEventsByBead scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// QueryToolEventsByStep returns per-step tool breakdowns for the bead,
// with steps preserved in first-seen order from ClickHouse's GROUP BY
// output (deterministically stabilised by the step, cnt DESC ORDER BY).
func (s *Store) QueryToolEventsByStep(beadID string) ([]olap.StepToolBreakdown, error) {
	const q = `
		SELECT
			if(step = '', 'unknown', step)      AS step_name,
			tool_name,
			count()                             AS cnt,
			avg(duration_ms)                    AS avg_duration_ms,
			countIf(success = false)            AS failure_count
		FROM tool_events
		WHERE bead_id = ?
		GROUP BY step_name, tool_name
		ORDER BY step_name ASC, cnt DESC
	`
	rows, err := s.db.QueryContext(context.Background(), q, beadID)
	if err != nil {
		return nil, fmt.Errorf("clickhouse QueryToolEventsByStep: %w", err)
	}
	defer rows.Close()

	stepMap := make(map[string][]olap.ToolEventStats)
	var stepOrder []string
	for rows.Next() {
		var step string
		var e olap.ToolEventStats
		if err := rows.Scan(&step, &e.ToolName, &e.Count, &e.AvgDurationMs, &e.FailureCount); err != nil {
			return nil, fmt.Errorf("clickhouse QueryToolEventsByStep scan: %w", err)
		}
		if _, ok := stepMap[step]; !ok {
			stepOrder = append(stepOrder, step)
		}
		stepMap[step] = append(stepMap[step], e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []olap.StepToolBreakdown
	for _, step := range stepOrder {
		out = append(out, olap.StepToolBreakdown{Step: step, Tools: stepMap[step]})
	}
	return out, nil
}

// QueryAPIEventsByBead returns LLM API call aggregates for a bead.
func (s *Store) QueryAPIEventsByBead(beadID string) ([]olap.APIEventStats, error) {
	const q = `
		SELECT
			if(model = '', 'unknown', model)    AS model_name,
			count()                             AS cnt,
			avg(duration_ms)                    AS avg_duration_ms,
			sum(cost_usd)                       AS total_cost,
			sum(input_tokens)                   AS total_input,
			sum(output_tokens)                  AS total_output
		FROM api_events
		WHERE bead_id = ?
		GROUP BY model_name
		ORDER BY cnt DESC
	`
	rows, err := s.db.QueryContext(context.Background(), q, beadID)
	if err != nil {
		return nil, fmt.Errorf("clickhouse QueryAPIEventsByBead: %w", err)
	}
	defer rows.Close()

	var out []olap.APIEventStats
	for rows.Next() {
		var e olap.APIEventStats
		if err := rows.Scan(&e.Model, &e.Count, &e.AvgDurationMs, &e.TotalCostUSD, &e.TotalInputTokens, &e.TotalOutputTokens); err != nil {
			return nil, fmt.Errorf("clickhouse QueryAPIEventsByBead scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// scanSpans decodes tool_spans rows into SpanRecord values. ClickHouse
// returns DateTime64(3) columns as time.Time so no conversion is needed.
func scanSpans(rows *sql.Rows) ([]olap.SpanRecord, error) {
	var out []olap.SpanRecord
	for rows.Next() {
		var s olap.SpanRecord
		var start, end time.Time
		if err := rows.Scan(
			&s.TraceID, &s.SpanID, &s.ParentSpanID, &s.SpanName,
			&s.Kind, &s.DurationMs, &s.Success, &start, &end, &s.Attributes,
		); err != nil {
			return nil, fmt.Errorf("clickhouse span scan: %w", err)
		}
		s.StartTime = start
		s.EndTime = end
		out = append(out, s)
	}
	return out, rows.Err()
}
