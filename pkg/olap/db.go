// Package olap provides a DuckDB-based OLAP layer for Spire analytics.
// Data is ETL'd from the Dolt operational database into a local DuckDB file
// for fast analytical queries (aggregations, trend lines, cost breakdowns).
package olap

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

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
func (d *DB) SqlDB() *sql.DB {
	return d.db
}
