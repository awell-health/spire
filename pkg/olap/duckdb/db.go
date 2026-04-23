//go:build cgo

// Package duckdb implements the local CGO-only OLAP backend. It satisfies
// olap.Store (Writer + TraceReader + MetricsReader). The pure-Go cluster
// path lives in pkg/olap/clickhouse and intentionally implements only
// Writer + TraceReader.
package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/marcboeker/go-duckdb"

	"github.com/awell-health/spire/pkg/olap"
)

// WriteFunc opens a DuckDB database at path, begins a transaction, calls fn,
// commits, and closes the database — all in one call. If DuckDB returns a lock
// error, it retries up to 3 times with exponential backoff (100ms, 200ms).
// This is the foundational write primitive for the open→write→close pattern
// that prevents long-held file locks.
func WriteFunc(path string, fn func(tx *sql.Tx) error) error {
	const maxRetries = 3
	const baseDelay = 100 * time.Millisecond

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(baseDelay * time.Duration(1<<uint(attempt-1)))
		}
		lastErr = writeOnce(path, fn)
		if lastErr == nil {
			return nil
		}
		if !IsDuckDBLockError(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("olap write: lock not released after %d retries: %w", maxRetries, lastErr)
}

// writeOnce opens the DB, runs fn in a transaction, and closes. Single attempt.
func writeOnce(path string, fn func(tx *sql.Tx) error) error {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return fmt.Errorf("olap write open: %w", err)
	}
	// Single connection ensures db.Close() releases the file lock immediately.
	db.SetMaxOpenConns(1)
	defer db.Close()

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ReadFunc opens a DuckDB database at path, calls fn with the raw *sql.DB for
// read-only queries, and closes the database. Reads don't need retry since
// DuckDB allows concurrent readers.
func ReadFunc(path string, fn func(db *sql.DB) error) error {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return fmt.Errorf("olap read open: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	return fn(db)
}

// EnsureSchema opens the DuckDB file at path, creates all tables if they don't
// exist, and closes. Used at startup to initialize without holding the DB open.
func EnsureSchema(path string) error {
	db, err := Open(path)
	if err != nil {
		return err
	}
	return db.Close()
}

// IsDuckDBLockError returns true if the error is a DuckDB file lock conflict.
// DuckDB's lock error isn't a typed error — we match on substrings defensively
// since the exact message varies across DuckDB versions.
func IsDuckDBLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "could not set lock") ||
		strings.Contains(msg, "database is locked") ||
		(strings.Contains(msg, "io error") && strings.Contains(msg, "lock"))
}

// DB wraps a DuckDB *sql.DB connection with a mutex for single-writer access.
// Satisfies olap.Store (Writer + TraceReader + MetricsReader).
type DB struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

// Open opens (or creates) a DuckDB database at path and initializes the schema.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("olap open %s: %w", path, err)
	}
	d := &DB{db: sqlDB, path: path}
	if err := d.InitSchema(context.Background()); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the underlying DuckDB connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// InitSchema creates all OLAP tables if they don't exist. Idempotent.
func (d *DB) InitSchema(ctx context.Context) error {
	for _, ddl := range allSchemaStatements() {
		if _, err := d.db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("olap schema: %w", err)
		}
	}
	// Migrate existing tool_events table: add columns introduced in dual-signal pipeline.
	for _, alt := range schemaMigrations() {
		d.db.ExecContext(ctx, alt) // ignore errors — column may already exist
	}
	return nil
}

// QueryContext executes a read-only query against the DuckDB database.
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, query, args...)
}

// SqlDB returns the raw *sql.DB for direct read queries (e.g. spire metrics).
// Callers MUST NOT use the returned handle for writes — all write paths must go
// through WithWriteLock to respect DuckDB's single-writer constraint.
func (d *DB) SqlDB() *sql.DB {
	return d.db
}

// WithWriteLock acquires the single-writer mutex and calls fn with the
// underlying *sql.DB. All write paths from outside the duckdb package
// must use this method to avoid races with ETL.
func (d *DB) WithWriteLock(fn func(*sql.DB) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fn(d.db)
}

// Submit implements olap.Writer. It runs fn inside a transaction under
// the single-writer mutex. Suitable for callers that hold a persistent
// *DB; the daemon's path-based DuckWriter wraps WriteFunc instead.
func (d *DB) Submit(fn func(*sql.Tx) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// QueryFormulaPerformance returns aggregated stats per formula name and version
// for all runs with started_at >= since.
//
// Success counting mirrors weekly_merge_stats (views.go): result='approve' is a
// sage verdict for successful review and must count alongside result='success'.
// avg_review_rounds mirrors daily_formula_stats (views.go): runs with
// review_rounds=0 (apprentice/wizard) are excluded so the average reflects only
// the phases that actually accumulate review rounds (sage-review).
func (d *DB) QueryFormulaPerformance(since time.Time) ([]olap.FormulaStats, error) {
	const q = `
		SELECT
			formula_name,
			COALESCE(formula_version, 'unknown')                            AS formula_version,
			COUNT(*)                                                         AS total_runs,
			SUM(CASE WHEN result IN ('success', 'approve') THEN 1 ELSE 0 END) AS successes,
			ROUND(100.0 * SUM(CASE WHEN result IN ('success', 'approve') THEN 1 ELSE 0 END)
				/ NULLIF(COUNT(*), 0), 1)                                    AS success_rate,
			ROUND(COALESCE(AVG(cost_usd), 0), 4)                             AS avg_cost_usd,
			ROUND(COALESCE(AVG(CASE WHEN review_rounds > 0 THEN review_rounds END), 0), 1) AS avg_review_rounds,
			SUM(CASE WHEN started_at >= current_timestamp::TIMESTAMP - INTERVAL 30 DAY THEN 1 ELSE 0 END) AS runs_last_30d
		FROM agent_runs_olap
		WHERE formula_name IS NOT NULL
		  AND formula_name != ''
		  AND started_at >= ?
		GROUP BY formula_name, formula_version
		ORDER BY total_runs DESC
	`
	rows, err := d.db.QueryContext(context.Background(), q, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []olap.FormulaStats
	for rows.Next() {
		var s olap.FormulaStats
		if err := rows.Scan(&s.FormulaName, &s.FormulaVersion, &s.TotalRuns,
			&s.Successes, &s.SuccessRate, &s.AvgCostUSD,
			&s.AvgReviewRounds, &s.RunsLast30d); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
