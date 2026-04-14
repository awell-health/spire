package olap

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/ClickHouse/clickhouse-go/v2" // register "clickhouse" database/sql driver
)

// OLAPWriter is the abstraction over DuckDB and ClickHouse write paths.
// Both DuckWriter (serialized single-goroutine) and ClickHouseWriter
// (concurrent) satisfy this interface. The Submit method matches the
// writeFn signature used by the OTel receiver and ETL sync.
type OLAPWriter interface {
	Submit(fn func(*sql.Tx) error) error
	Close() error
}

// ClickHouseWriter provides the OLAPWriter interface backed by ClickHouse.
// Unlike DuckWriter, ClickHouse supports concurrent writes so Submit does
// not serialize through a goroutine — it begins a transaction directly.
// Note: ClickHouse transactions are not truly atomic (Commit is effectively
// a no-op; writes are auto-committed). This is fine for append-only event data.
type ClickHouseWriter struct {
	db  *sql.DB
	dsn string
}

// NewClickHouseWriter opens a ClickHouse connection and ensures the schema
// tables exist. The DSN format for clickhouse-go v2 is
// "clickhouse://host:port/database".
func NewClickHouseWriter(dsn string) (*ClickHouseWriter, error) {
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, fmt.Errorf("clickhouse open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}
	if err := InitClickHouseSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("clickhouse schema: %w", err)
	}
	return &ClickHouseWriter{db: db, dsn: dsn}, nil
}

// Submit executes fn inside a ClickHouse transaction. Concurrent calls are
// safe — no goroutine serialization is needed unlike DuckWriter.
func (cw *ClickHouseWriter) Submit(fn func(*sql.Tx) error) error {
	tx, err := cw.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("clickhouse begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Close closes the underlying ClickHouse connection pool.
func (cw *ClickHouseWriter) Close() error {
	return cw.db.Close()
}
