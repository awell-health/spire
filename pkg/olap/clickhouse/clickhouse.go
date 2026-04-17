// Package clickhouse implements the pure-Go ClickHouse backend for the
// Spire OLAP layer. It satisfies olap.Writer and olap.TraceReader but
// deliberately does NOT implement olap.MetricsReader — that surface is
// DuckDB-only and the omission is enforced by a compile-time assertion
// elsewhere in this package.
//
// Concurrency. Unlike DuckDB, ClickHouse supports concurrent inserts
// through MergeTree's append-only model, so Submit does not serialize
// through a goroutine. Callers may invoke Submit from many goroutines
// simultaneously.
//
// Transactions. ClickHouse does not implement ACID transactions the
// way a traditional OLTP engine does — BeginTx / Commit are accepted
// by the driver but writes are effectively auto-committed. The
// Submit / Rollback contract is retained for parity with DuckDB, and
// the contract test harness documents which engine has strict rollback
// and which has auto-commit semantics.
package clickhouse

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/ClickHouse/clickhouse-go/v2" // register the "clickhouse" database/sql driver

	"github.com/awell-health/spire/pkg/olap"
)

// Store is the ClickHouse-backed OLAP implementation. It satisfies
// olap.Writer and olap.TraceReader (see compile-time assertions
// below). Intentionally does NOT satisfy olap.MetricsReader — metrics
// queries use DuckDB-only SQL idioms and live only on the DuckDB Store.
type Store struct {
	db  *sql.DB
	dsn string
}

// Compile-time assertions:
//   - Store satisfies olap.Writer and olap.TraceReader, so it can be
//     handed out as olap.ReadWrite by the factory.
//   - Store does NOT satisfy olap.MetricsReader. This is enforced by a
//     negative assertion in an `_test.go` file — Go's type system has
//     no direct way to express "must not implement X", so the proof is
//     that the compile-time assertion `var _ olap.MetricsReader =
//     (*Store)(nil)` would fail at build time.
var (
	_ olap.Writer      = (*Store)(nil)
	_ olap.TraceReader = (*Store)(nil)
	_ olap.ReadWrite   = (*Store)(nil)
)

// Open opens a ClickHouse connection, verifies reachability with a
// Ping, and ensures the schema exists. The DSN format for
// clickhouse-go/v2 is "clickhouse://host:port/database".
func Open(cfg olap.Config) (*Store, error) {
	dsn := cfg.DSN
	if dsn == "" {
		return nil, fmt.Errorf("clickhouse: DSN is empty")
	}
	if err := ensureDatabase(dsn); err != nil {
		return nil, fmt.Errorf("clickhouse ensure db: %w", err)
	}
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, fmt.Errorf("clickhouse open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("clickhouse schema: %w", err)
	}
	return &Store{db: db, dsn: dsn}, nil
}

// Submit executes fn inside a ClickHouse transaction. Concurrent calls
// are safe — no goroutine serialization is needed unlike the DuckDB
// backend. Note that ClickHouse's transaction semantics are weaker than
// DuckDB's: rollback may not reverse inserts. See the package doc.
func (s *Store) Submit(fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("clickhouse begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Close closes the underlying ClickHouse connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}
