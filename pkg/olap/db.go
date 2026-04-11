// Package olap provides a DuckDB-based OLAP layer for Spire analytics.
// Data is ETL'd from the Dolt operational database into a local DuckDB file
// for fast analytical queries (aggregations, trend lines, cost breakdowns).
package olap

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "github.com/marcboeker/go-duckdb"
)

// DB wraps a DuckDB *sql.DB connection with a mutex for single-writer access.
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
// underlying *sql.DB. All write paths from outside the olap package
// must use this method to avoid races with ETL.
func (d *DB) WithWriteLock(fn func(*sql.DB) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fn(d.db)
}

// FormulaStats holds aggregated performance data for a formula name+version pair.
type FormulaStats struct {
	FormulaName     string
	FormulaVersion  string
	TotalRuns       int
	Successes       int
	SuccessRate     float64 // 0–100
	AvgCostUSD      float64
	AvgReviewRounds float64
	RunsLast30d     int
}

// QueryFormulaPerformance returns aggregated stats per formula name and version
// for all runs with started_at >= since.
func (d *DB) QueryFormulaPerformance(since time.Time) ([]FormulaStats, error) {
	const q = `
		SELECT
			formula_name,
			COALESCE(formula_version, 'unknown')                   AS formula_version,
			COUNT(*)                                                AS total_runs,
			SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END)    AS successes,
			ROUND(100.0 * SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END)
				/ NULLIF(COUNT(*), 0), 1)                          AS success_rate,
			ROUND(COALESCE(AVG(cost_usd), 0), 4)                   AS avg_cost_usd,
			ROUND(COALESCE(AVG(review_rounds), 0), 1)              AS avg_review_rounds,
			SUM(CASE WHEN started_at >= CURRENT_TIMESTAMP - INTERVAL 30 DAY THEN 1 ELSE 0 END) AS runs_last_30d
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
	var out []FormulaStats
	for rows.Next() {
		var s FormulaStats
		if err := rows.Scan(&s.FormulaName, &s.FormulaVersion, &s.TotalRuns,
			&s.Successes, &s.SuccessRate, &s.AvgCostUSD,
			&s.AvgReviewRounds, &s.RunsLast30d); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
