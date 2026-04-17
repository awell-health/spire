//go:build cgo

package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ETL handles incremental sync from Dolt agent_runs to DuckDB agent_runs_olap.
type ETL struct {
	// Persistent DB mode (backward compat — used by tests and legacy callers).
	db *DB

	// Path-based mode: open→use→close per operation.
	dbPath  string
	writeFn func(fn func(*sql.Tx) error) error
}

// NewETL creates a new ETL instance backed by a persistent *DB.
// Used by tests and backward-compatible callers.
func NewETL(db *DB) *ETL {
	return &ETL{db: db}
}

// NewETLPath creates an ETL that opens/closes DuckDB per operation using
// WriteFunc (with retry-on-lock). Used by spire up for standalone ETL seed.
func NewETLPath(dbPath string) *ETL {
	return &ETL{
		dbPath: dbPath,
		writeFn: func(fn func(*sql.Tx) error) error {
			return WriteFunc(dbPath, fn)
		},
	}
}

// NewETLWithWriter creates an ETL that submits writes through a custom write
// function (e.g. DuckWriter.Submit for serialized daemon writes).
func NewETLWithWriter(dbPath string, wf func(fn func(*sql.Tx) error) error) *ETL {
	return &ETL{
		dbPath:  dbPath,
		writeFn: wf,
	}
}

// Sync performs an incremental ETL from the Dolt agent_runs table (via doltConn)
// into the DuckDB agent_runs_olap table. It uses a cursor stored in DuckDB's
// etl_cursor table to track the high-water mark (an RFC3339 started_at timestamp).
// Returns the number of rows synced and any error.
func (e *ETL) Sync(ctx context.Context, doltConn *sql.DB) (int, error) {
	// Legacy path: persistent *DB with mutex.
	if e.db != nil {
		return e.syncWithDB(ctx, doltConn)
	}
	return e.syncWithPath(ctx, doltConn)
}

// syncWithDB is the original sync path using a persistent *DB connection.
func (e *ETL) syncWithDB(ctx context.Context, doltConn *sql.DB) (int, error) {
	e.db.mu.Lock()
	defer e.db.mu.Unlock()

	lastTS, err := readCursor(ctx, e.db.db)
	if err != nil {
		return 0, fmt.Errorf("olap etl read cursor: %w", err)
	}

	rows, err := queryDolt(ctx, doltConn, lastTS)
	if err != nil {
		return 0, fmt.Errorf("olap etl query dolt: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	newHighWater, err := insertRows(ctx, e.db.db, rows)
	if err != nil {
		return 0, fmt.Errorf("olap etl insert: %w", err)
	}

	if err := updateCursor(ctx, e.db.db, newHighWater); err != nil {
		return 0, fmt.Errorf("olap etl update cursor: %w", err)
	}

	if err := RefreshMaterializedViews(ctx, e.db); err != nil {
		return len(rows), fmt.Errorf("olap etl refresh views: %w", err)
	}

	return len(rows), nil
}

// syncWithPath uses the open→write→close pattern for lock-safe DuckDB access.
func (e *ETL) syncWithPath(ctx context.Context, doltConn *sql.DB) (int, error) {
	// 1. Read cursor via ReadFunc (read-only, no lock contention).
	var lastTS string
	if err := ReadFunc(e.dbPath, func(db *sql.DB) error {
		var err error
		lastTS, err = readCursor(ctx, db)
		return err
	}); err != nil {
		return 0, fmt.Errorf("olap etl read cursor: %w", err)
	}

	// 2. Query Dolt for rows at or after the cursor (no DuckDB access needed).
	rows, err := queryDolt(ctx, doltConn, lastTS)
	if err != nil {
		return 0, fmt.Errorf("olap etl query dolt: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	// 3. Insert rows + update cursor in a single write transaction.
	var newHighWater string
	if err := e.writeFn(func(tx *sql.Tx) error {
		var txErr error
		newHighWater, txErr = insertRowsTx(ctx, tx, rows)
		if txErr != nil {
			return fmt.Errorf("olap etl insert: %w", txErr)
		}
		return updateCursorTx(ctx, tx, newHighWater)
	}); err != nil {
		return 0, err
	}

	// 4. Refresh materialized views in a separate write.
	if err := e.writeFn(func(tx *sql.Tx) error {
		return RefreshMaterializedViewsTx(ctx, tx)
	}); err != nil {
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
func readCursor(ctx context.Context, db *sql.DB) (string, error) {
	var val string
	err := db.QueryRowContext(ctx,
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
func queryDolt(ctx context.Context, doltConn *sql.DB, lastTS string) ([]agentRunRow, error) {
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

// insertRows inserts rows using the raw *sql.DB (legacy persistent-DB path).
func insertRows(ctx context.Context, db *sql.DB, rows []agentRunRow) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}
	q, args := buildInsertSQL(rows)
	if _, err := db.ExecContext(ctx, q, args...); err != nil {
		return "", err
	}
	return lastHighWater(rows), nil
}

// insertRowsTx inserts rows inside a transaction (path-based pattern).
func insertRowsTx(ctx context.Context, tx *sql.Tx, rows []agentRunRow) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}
	q, args := buildInsertSQL(rows)
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return "", err
	}
	return lastHighWater(rows), nil
}

// repoFromBeadID extracts the repo prefix from a bead ID.
// Examples: "web-a3f8" → "web", "spi-b7d0.1" → "spi", "api-8a01.2.3" → "api".
// Returns empty string if the bead ID has no dash (no prefix).
func repoFromBeadID(beadID string) string {
	if beadID == "" {
		return ""
	}
	idx := strings.Index(beadID, "-")
	if idx <= 0 {
		return ""
	}
	return beadID[:idx]
}

// normalizeFormulaName returns the formula name, defaulting to "adhoc" when
// the value is NULL or empty so it isn't filtered out of aggregation queries.
func normalizeFormulaName(fn sql.NullString) string {
	if !fn.Valid || fn.String == "" {
		return "adhoc"
	}
	return fn.String
}

// buildInsertSQL constructs the upsert SQL and args for a batch of rows.
func buildInsertSQL(rows []agentRunRow) (string, []any) {
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
			normalizeFormulaName(r.FormulaName), r.FormulaVersion,
			r.Phase, r.Role, r.Model, r.Tower,
			repoFromBeadID(r.BeadID.String), // repo — derived from bead_id prefix
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
		repo = EXCLUDED.repo,
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

	return b.String(), args
}

// lastHighWater returns the RFC3339 high-water mark from the last row.
func lastHighWater(rows []agentRunRow) string {
	last := rows[len(rows)-1]
	if last.StartedAt.Valid {
		return last.StartedAt.Time.UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// updateCursor updates the ETL cursor using a raw *sql.DB (legacy path).
func updateCursor(ctx context.Context, db *sql.DB, lastID string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO etl_cursor (table_name, last_id, last_synced)
		 VALUES ('agent_runs', ?, now())
		 ON CONFLICT (table_name) DO UPDATE SET last_id = EXCLUDED.last_id, last_synced = now()`,
		lastID,
	)
	return err
}

// updateCursorTx updates the ETL cursor inside a transaction (path-based pattern).
func updateCursorTx(ctx context.Context, tx *sql.Tx, lastID string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO etl_cursor (table_name, last_id, last_synced)
		 VALUES ('agent_runs', ?, now())
		 ON CONFLICT (table_name) DO UPDATE SET last_id = EXCLUDED.last_id, last_synced = now()`,
		lastID,
	)
	return err
}
