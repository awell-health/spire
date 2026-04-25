package olap

import (
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// TestInitClickHouseSchema verifies that all CREATE TABLE statements execute
// against a mock database without error.
func TestInitClickHouseSchema(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	stmts := clickHouseSchemaStatements()
	for range stmts {
		mock.ExpectExec("CREATE TABLE IF NOT EXISTS").WillReturnResult(sqlmock.NewResult(0, 0))
	}

	if err := InitClickHouseSchema(db); err != nil {
		t.Fatalf("InitClickHouseSchema: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestInitClickHouseSchema_Error verifies that a DDL error propagates.
func TestInitClickHouseSchema_Error(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("CREATE TABLE IF NOT EXISTS").WillReturnError(errors.New("connection refused"))

	if err := InitClickHouseSchema(db); err == nil {
		t.Fatal("expected error from InitClickHouseSchema, got nil")
	}
}

// TestClickHouseWriter_Submit verifies that Submit calls fn with a *sql.Tx
// and commits the transaction.
func TestClickHouseWriter_Submit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// Construct ClickHouseWriter directly (bypassing NewClickHouseWriter which
	// would try to connect to a real ClickHouse server).
	cw := &ClickHouseWriter{db: db, dsn: "clickhouse://localhost:9000/test"}

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO tool_events").
		WithArgs("sess-1", "spi-abc", "agent", "impl", "Read", 10, true, "tower1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = cw.Submit(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO tool_events (session_id, bead_id, agent_name, step, tool_name, duration_ms, success, tower) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			"sess-1", "spi-abc", "agent", "impl", "Read", 10, true, "tower1")
		return err
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestClickHouseWriter_SubmitError verifies that Submit rolls back on fn error.
func TestClickHouseWriter_SubmitError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cw := &ClickHouseWriter{db: db, dsn: "clickhouse://localhost:9000/test"}

	mock.ExpectBegin()
	mock.ExpectRollback()

	fnErr := errors.New("insert failed")
	err = cw.Submit(func(tx *sql.Tx) error {
		return fnErr
	})
	if !errors.Is(err, fnErr) {
		t.Fatalf("expected fn error, got: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestClickHouseWriter_ConcurrentSubmit verifies that concurrent Submit calls
// don't block each other (unlike DuckWriter which serializes through a channel).
func TestClickHouseWriter_ConcurrentSubmit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	cw := &ClickHouseWriter{db: db, dsn: "clickhouse://localhost:9000/test"}

	const workers = 5
	for i := 0; i < workers; i++ {
		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO tool_events").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
	}

	var wg sync.WaitGroup
	var errCount atomic.Int32

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := cw.Submit(func(tx *sql.Tx) error {
				_, err := tx.Exec("INSERT INTO tool_events (session_id) VALUES (?)", "concurrent")
				return err
			})
			if err != nil {
				errCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if c := errCount.Load(); c != 0 {
		t.Errorf("expected 0 errors from concurrent Submit, got %d", c)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestClickHouseWriter_Close verifies that Close closes the underlying db.
func TestClickHouseWriter_Close(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}

	cw := &ClickHouseWriter{db: db, dsn: "clickhouse://localhost:9000/test"}
	mock.ExpectClose()

	if err := cw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestClickHouseSchemaStatements verifies the expected number of DDL statements.
func TestClickHouseSchemaStatements(t *testing.T) {
	stmts := clickHouseSchemaStatements()
	// 11 tables: tool_events, tool_spans, api_events, agent_runs_olap,
	// etl_cursor, bead_lifecycle_olap, daily_formula_stats,
	// weekly_merge_stats, phase_cost_breakdown, tool_usage_stats,
	// failure_hotspots.
	if len(stmts) != 11 {
		t.Errorf("expected 11 schema statements, got %d", len(stmts))
	}
}
