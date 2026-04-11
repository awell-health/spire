package olap

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ETL handles incremental sync from Dolt agent_runs to DuckDB agent_runs_olap.
type ETL struct {
	db *DB
}

// NewETL creates a new ETL instance backed by the given DuckDB database.
func NewETL(db *DB) *ETL {
	return &ETL{db: db}
}

// Sync performs an incremental ETL from the Dolt agent_runs table (via doltConn)
// into the DuckDB agent_runs_olap table. It uses a cursor stored in DuckDB's
// etl_cursor table to track the high-water mark (an RFC3339 started_at timestamp).
// Returns the number of rows synced and any error.
func (e *ETL) Sync(ctx context.Context, doltConn *sql.DB) (int, error) {
	e.db.mu.Lock()
	defer e.db.mu.Unlock()

	// 1. Read cursor (started_at timestamp) from DuckDB
	lastTS, err := e.readCursor(ctx)
	if err != nil {
		return 0, fmt.Errorf("olap etl read cursor: %w", err)
	}

	// 2. Query Dolt for rows at or after the cursor timestamp
	rows, err := e.queryDolt(ctx, doltConn, lastTS)
	if err != nil {
		return 0, fmt.Errorf("olap etl query dolt: %w", err)
	}

	if len(rows) == 0 {
		return 0, nil
	}

	// 3. Bulk insert into DuckDB (upsert handles re-processed boundary rows)
	newHighWater, err := e.insertRows(ctx, rows)
	if err != nil {
		return 0, fmt.Errorf("olap etl insert: %w", err)
	}

	// 4. Update cursor
	if err := e.updateCursor(ctx, newHighWater); err != nil {
		return 0, fmt.Errorf("olap etl update cursor: %w", err)
	}

	// 5. Refresh materialized views
	if err := RefreshMaterializedViews(ctx, e.db); err != nil {
		return len(rows), fmt.Errorf("olap etl refresh views: %w", err)
	}

	return len(rows), nil
}

// agentRunRow holds one row from the Dolt agent_runs table.
type agentRunRow struct {
	ID               string
	BeadID           sql.NullString
	EpicID           sql.NullString
	ParentRunID      sql.NullString
	FormulaName      sql.NullString
	FormulaVersion   sql.NullString
	Phase            sql.NullString
	Role             sql.NullString
	Model            sql.NullString
	Tower            sql.NullString
	Branch           sql.NullString
	Result           sql.NullString
	ReviewRounds     sql.NullInt64
	PromptTokens     sql.NullInt64
	CompletionTokens sql.NullInt64
	TotalTokens      sql.NullInt64
	CostUSD          sql.NullFloat64
	DurationSeconds  sql.NullFloat64
	StartupSeconds   sql.NullFloat64
	WorkingSeconds   sql.NullFloat64
	QueueSeconds     sql.NullFloat64
	ReviewSeconds    sql.NullFloat64
	FilesChanged     sql.NullInt64
	LinesAdded       sql.NullInt64
	LinesRemoved     sql.NullInt64
	ReadCalls        sql.NullInt64
	EditCalls        sql.NullInt64
	ToolCallsJSON    sql.NullString
	FailureClass     sql.NullString
	AttemptNumber    sql.NullInt64
	StartedAt        sql.NullTime
	CompletedAt      sql.NullTime
}

// readCursor returns the high-water-mark started_at timestamp (RFC3339) from
// the etl_cursor table. Returns "" on first sync or if the stored value is a
// stale id-based cursor (pre-fix data starting with "run-").
func (e *ETL) readCursor(ctx context.Context) (string, error) {
	var val string
	err := e.db.db.QueryRowContext(ctx,
		"SELECT last_id FROM etl_cursor WHERE table_name = 'agent_runs'",
	).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Detect stale id-based cursor from before the started_at migration.
	// Trigger a full re-sync (safe because insertRows uses upsert).
	if strings.HasPrefix(val, "run-") {
		return "", nil
	}
	return val, nil
}

// queryDolt fetches up to 500 rows from agent_runs at or after lastTS.
// Uses started_at as a monotonic cursor (not id, which is random hex).
// The >= boundary means rows at the exact cursor timestamp are re-fetched;
// the upsert in insertRows makes this harmless.
func (e *ETL) queryDolt(ctx context.Context, doltConn *sql.DB, lastTS string) ([]agentRunRow, error) {
	baseCols := `SELECT
		id, bead_id, epic_id, parent_run_id,
		formula_name, CAST(formula_version AS CHAR) AS formula_version,
		phase, role, model, tower, branch, result,
		review_rounds,
		context_tokens_in, context_tokens_out, total_tokens,
		cost_usd, duration_seconds,
		startup_seconds, working_seconds, queue_seconds, review_seconds,
		files_changed, lines_added, lines_removed,
		read_calls, edit_calls, tool_calls_json, failure_class, attempt_number,
		started_at, completed_at
	FROM agent_runs`

	var query string
	var args []any
	if lastTS == "" {
		// First sync — fetch everything
		query = baseCols + ` ORDER BY started_at, id LIMIT 500`
	} else {
		// Incremental — fetch rows at or after the cursor timestamp.
		// >= re-processes boundary rows; the upsert handles duplicates.
		// Parse the RFC3339 string into time.Time so the SQL driver sends a
		// proper DATETIME parameter (required by DuckDB; works fine with MySQL too).
		ts, err := time.Parse(time.RFC3339, lastTS)
		if err != nil {
			return nil, fmt.Errorf("parse cursor timestamp %q: %w", lastTS, err)
		}
		query = baseCols + ` WHERE started_at >= ? ORDER BY started_at, id LIMIT 500`
		args = append(args, ts)
	}

	sqlRows, err := doltConn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()

	var rows []agentRunRow
	for sqlRows.Next() {
		var r agentRunRow
		if err := sqlRows.Scan(
			&r.ID, &r.BeadID, &r.EpicID, &r.ParentRunID,
			&r.FormulaName, &r.FormulaVersion,
			&r.Phase, &r.Role, &r.Model, &r.Tower, &r.Branch, &r.Result,
			&r.ReviewRounds,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
			&r.CostUSD, &r.DurationSeconds,
			&r.StartupSeconds, &r.WorkingSeconds, &r.QueueSeconds, &r.ReviewSeconds,
			&r.FilesChanged, &r.LinesAdded, &r.LinesRemoved,
			&r.ReadCalls, &r.EditCalls, &r.ToolCallsJSON, &r.FailureClass, &r.AttemptNumber,
			&r.StartedAt, &r.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan agent_runs row: %w", err)
		}
		rows = append(rows, r)
	}
	return rows, sqlRows.Err()
}

func (e *ETL) insertRows(ctx context.Context, rows []agentRunRow) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString(`INSERT INTO agent_runs_olap (
		id, bead_id, epic_id, parent_run_id,
		formula_name, formula_version,
		phase, role, model, tower, repo, branch, result,
		review_rounds,
		prompt_tokens, completion_tokens, total_tokens,
		cost_usd, duration_seconds,
		startup_seconds, working_seconds, queue_seconds, review_seconds,
		files_changed, lines_added, lines_removed,
		read_calls, edit_calls, tool_calls_json, failure_class, attempt_number,
		started_at, completed_at, synced_at
	) VALUES `)

	args := make([]any, 0, len(rows)*34)
	for i, r := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			r.ID, r.BeadID, r.EpicID, r.ParentRunID,
			r.FormulaName, r.FormulaVersion,
			r.Phase, r.Role, r.Model, r.Tower,
			nil, // repo — not in Dolt agent_runs, populated as NULL for now
			r.Branch, r.Result,
			r.ReviewRounds,
			r.PromptTokens, r.CompletionTokens, r.TotalTokens,
			r.CostUSD, r.DurationSeconds,
			r.StartupSeconds, r.WorkingSeconds, r.QueueSeconds, r.ReviewSeconds,
			r.FilesChanged, r.LinesAdded, r.LinesRemoved,
			r.ReadCalls, r.EditCalls, r.ToolCallsJSON, r.FailureClass, r.AttemptNumber,
			r.StartedAt, r.CompletedAt, time.Now().UTC(),
		)
	}

	b.WriteString(` ON CONFLICT (id) DO UPDATE SET
		bead_id = EXCLUDED.bead_id,
		epic_id = EXCLUDED.epic_id,
		parent_run_id = EXCLUDED.parent_run_id,
		formula_name = EXCLUDED.formula_name,
		formula_version = EXCLUDED.formula_version,
		phase = EXCLUDED.phase,
		role = EXCLUDED.role,
		model = EXCLUDED.model,
		tower = EXCLUDED.tower,
		branch = EXCLUDED.branch,
		result = EXCLUDED.result,
		review_rounds = EXCLUDED.review_rounds,
		prompt_tokens = EXCLUDED.prompt_tokens,
		completion_tokens = EXCLUDED.completion_tokens,
		total_tokens = EXCLUDED.total_tokens,
		cost_usd = EXCLUDED.cost_usd,
		duration_seconds = EXCLUDED.duration_seconds,
		startup_seconds = EXCLUDED.startup_seconds,
		working_seconds = EXCLUDED.working_seconds,
		queue_seconds = EXCLUDED.queue_seconds,
		review_seconds = EXCLUDED.review_seconds,
		files_changed = EXCLUDED.files_changed,
		lines_added = EXCLUDED.lines_added,
		lines_removed = EXCLUDED.lines_removed,
		read_calls = EXCLUDED.read_calls,
		edit_calls = EXCLUDED.edit_calls,
		tool_calls_json = EXCLUDED.tool_calls_json,
		failure_class = EXCLUDED.failure_class,
		attempt_number = EXCLUDED.attempt_number,
		started_at = EXCLUDED.started_at,
		completed_at = EXCLUDED.completed_at,
		synced_at = EXCLUDED.synced_at`)

	if _, err := e.db.db.ExecContext(ctx, b.String(), args...); err != nil {
		return "", err
	}

	// Return the last row's started_at as the new high-water mark.
	last := rows[len(rows)-1]
	if last.StartedAt.Valid {
		return last.StartedAt.Time.UTC().Format(time.RFC3339), nil
	}
	// Fallback: should not happen (started_at is NOT NULL in Dolt), but
	// use current time rather than stalling the cursor.
	return time.Now().UTC().Format(time.RFC3339), nil
}

func (e *ETL) updateCursor(ctx context.Context, lastID string) error {
	_, err := e.db.db.ExecContext(ctx,
		`INSERT INTO etl_cursor (table_name, last_id, last_synced)
		 VALUES ('agent_runs', ?, now())
		 ON CONFLICT (table_name) DO UPDATE SET last_id = EXCLUDED.last_id, last_synced = now()`,
		lastID,
	)
	return err
}
