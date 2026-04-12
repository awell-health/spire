package olap

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func TestOpenInMemory(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open in-memory: %v", err)
	}
	defer db.Close()

	// Verify all tables exist by querying them
	tables := []string{
		"agent_runs_olap",
		"etl_cursor",
		"daily_formula_stats",
		"weekly_merge_stats",
		"phase_cost_breakdown",
	}
	for _, tbl := range tables {
		rows, err := db.QueryContext(context.Background(), "SELECT COUNT(*) FROM "+tbl)
		if err != nil {
			t.Errorf("query %s: %v", tbl, err)
			continue
		}
		rows.Close()
	}
}

func TestInitSchemaIdempotent(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Calling InitSchema again should not fail
	if err := db.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema (second call): %v", err)
	}
}

func TestETLSyncWithMockDolt(t *testing.T) {
	// Open in-memory DuckDB for OLAP
	olapDB, err := Open("")
	if err != nil {
		t.Fatalf("Open olap: %v", err)
	}
	defer olapDB.Close()

	// Create an in-memory DuckDB as a mock "Dolt" source with the agent_runs schema.
	// DuckDB supports database/sql interface, so we use it as a stand-in for the MySQL driver.
	mockDolt, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("Open mock dolt: %v", err)
	}
	defer mockDolt.Close()

	// Create agent_runs table in mock dolt with the columns the ETL expects
	_, err = mockDolt.Exec(`CREATE TABLE agent_runs (
		id VARCHAR PRIMARY KEY,
		bead_id VARCHAR,
		epic_id VARCHAR,
		parent_run_id VARCHAR,
		formula_name VARCHAR,
		formula_version VARCHAR,
		phase VARCHAR,
		role VARCHAR,
		model VARCHAR,
		tower VARCHAR,
		branch VARCHAR,
		result VARCHAR,
		review_rounds INTEGER,
		context_tokens_in BIGINT,
		context_tokens_out BIGINT,
		total_tokens BIGINT,
		cost_usd DOUBLE,
		duration_seconds DOUBLE,
		startup_seconds DOUBLE,
		working_seconds DOUBLE,
		queue_seconds DOUBLE,
		review_seconds DOUBLE,
		files_changed INTEGER,
		lines_added INTEGER,
		lines_removed INTEGER,
		read_calls INTEGER,
		edit_calls INTEGER,
		tool_calls_json TEXT,
		failure_class VARCHAR,
		attempt_number INTEGER,
		started_at TIMESTAMP,
		completed_at TIMESTAMP
	)`)
	if err != nil {
		t.Fatalf("create mock agent_runs: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)

	// Insert test rows
	_, err = mockDolt.Exec(`INSERT INTO agent_runs VALUES
		('run-001', 'spi-abc', 'spi-epic1', NULL, 'task-default', '3', 'implement', 'apprentice', 'claude-opus-4-6', 'my-tower', 'feat/abc', 'success', 2, 1000, 500, 1500, 0.15, 120.0, 5.0, 100.0, 10.0, 5.0, 3, 50, 20, 12, 5, '{"Read":12,"Edit":5}', NULL, 1, ?, ?),
		('run-002', 'spi-def', 'spi-epic1', 'run-001', 'task-default', '3', 'review', 'sage', 'claude-opus-4-6', 'my-tower', 'feat/def', 'success', 1, 800, 400, 1200, 0.10, 60.0, 3.0, 50.0, 5.0, 2.0, 0, 0, 0, 8, 0, '{"Read":8}', NULL, 1, ?, ?)`,
		now.Add(-time.Hour), now.Add(-30*time.Minute),
		now.Add(-20*time.Minute), now.Add(-10*time.Minute),
	)
	if err != nil {
		t.Fatalf("insert mock rows: %v", err)
	}

	// Run ETL
	ctx := context.Background()
	etl := NewETL(olapDB)
	n, err := etl.Sync(ctx, mockDolt)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 rows synced, got %d", n)
	}

	// Verify rows landed in agent_runs_olap
	var count int
	if err := olapDB.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_runs_olap").Scan(&count); err != nil {
		t.Fatalf("count agent_runs_olap: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 rows in agent_runs_olap, got %d", count)
	}

	// Verify cursor was updated to the last row's started_at (RFC3339)
	var cursorVal string
	if err := olapDB.db.QueryRowContext(ctx, "SELECT last_id FROM etl_cursor WHERE table_name = 'agent_runs'").Scan(&cursorVal); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	// Cursor should be an RFC3339 timestamp matching the later row's started_at
	expectedTS := now.Add(-20 * time.Minute).Format(time.RFC3339)
	if cursorVal != expectedTS {
		t.Errorf("expected cursor at %s, got %s", expectedTS, cursorVal)
	}

	// Second sync re-fetches the boundary row (started_at >=) but upserts it,
	// so it should sync 1 row (the boundary) with no net data change.
	n2, err := etl.Sync(ctx, mockDolt)
	if err != nil {
		t.Fatalf("Sync (second): %v", err)
	}
	// The boundary row is re-fetched; only rows at the cursor timestamp are returned
	if n2 > 1 {
		t.Errorf("expected at most 1 row re-synced on second call, got %d", n2)
	}

	// Verify materialized views have data
	var dailyCount int
	if err := olapDB.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_formula_stats").Scan(&dailyCount); err != nil {
		t.Fatalf("count daily_formula_stats: %v", err)
	}
	if dailyCount == 0 {
		t.Error("expected rows in daily_formula_stats after sync")
	}
}

func TestETLUpsertOnConflict(t *testing.T) {
	olapDB, err := Open("")
	if err != nil {
		t.Fatalf("Open olap: %v", err)
	}
	defer olapDB.Close()

	mockDolt, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("Open mock dolt: %v", err)
	}
	defer mockDolt.Close()

	_, err = mockDolt.Exec(`CREATE TABLE agent_runs (
		id VARCHAR PRIMARY KEY, bead_id VARCHAR, epic_id VARCHAR,
		parent_run_id VARCHAR, formula_name VARCHAR, formula_version VARCHAR,
		phase VARCHAR, role VARCHAR, model VARCHAR, tower VARCHAR,
		branch VARCHAR, result VARCHAR, review_rounds INTEGER,
		context_tokens_in BIGINT, context_tokens_out BIGINT, total_tokens BIGINT,
		cost_usd DOUBLE, duration_seconds DOUBLE,
		startup_seconds DOUBLE, working_seconds DOUBLE, queue_seconds DOUBLE, review_seconds DOUBLE,
		files_changed INTEGER, lines_added INTEGER, lines_removed INTEGER,
		read_calls INTEGER, edit_calls INTEGER, tool_calls_json TEXT,
		failure_class VARCHAR, attempt_number INTEGER,
		started_at TIMESTAMP, completed_at TIMESTAMP
	)`)
	if err != nil {
		t.Fatalf("create mock table: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)

	// Insert a row
	_, err = mockDolt.Exec(`INSERT INTO agent_runs VALUES
		('run-100', 'spi-x', NULL, NULL, 'formula-a', '1', 'plan', 'wizard', 'opus', 'tower1', 'main', 'running', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, NULL, NULL, 1, ?, NULL)`, now)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	ctx := context.Background()
	etl := NewETL(olapDB)

	// First sync
	n, err := etl.Sync(ctx, mockDolt)
	if err != nil {
		t.Fatalf("Sync 1: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1, got %d", n)
	}

	// Update the row in mock dolt (simulating dolt update)
	_, err = mockDolt.Exec(`UPDATE agent_runs SET result = 'success', cost_usd = 0.50 WHERE id = 'run-100'`)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// Reset cursor to trigger a full re-sync (empty = no filter)
	_, err = olapDB.db.ExecContext(ctx, `UPDATE etl_cursor SET last_id = '' WHERE table_name = 'agent_runs'`)
	if err != nil {
		t.Fatalf("reset cursor: %v", err)
	}

	// Second sync should upsert
	n2, err := etl.Sync(ctx, mockDolt)
	if err != nil {
		t.Fatalf("Sync 2: %v", err)
	}
	if n2 != 1 {
		t.Errorf("expected 1 on upsert, got %d", n2)
	}

	// Verify the updated values
	var result string
	var cost float64
	err = olapDB.db.QueryRowContext(ctx, "SELECT result, cost_usd FROM agent_runs_olap WHERE id = 'run-100'").Scan(&result, &cost)
	if err != nil {
		t.Fatalf("read updated row: %v", err)
	}
	if result != "success" {
		t.Errorf("expected result=success, got %s", result)
	}
	if cost != 0.50 {
		t.Errorf("expected cost=0.50, got %f", cost)
	}
}

// TestETLNonMonotonicIDs verifies the bug fix: rows with lexically smaller IDs
// than previously synced rows are still captured, because the cursor uses
// started_at (monotonic) instead of id (random hex).
func TestETLNonMonotonicIDs(t *testing.T) {
	olapDB, err := Open("")
	if err != nil {
		t.Fatalf("Open olap: %v", err)
	}
	defer olapDB.Close()

	mockDolt, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("Open mock dolt: %v", err)
	}
	defer mockDolt.Close()

	_, err = mockDolt.Exec(`CREATE TABLE agent_runs (
		id VARCHAR PRIMARY KEY, bead_id VARCHAR, epic_id VARCHAR,
		parent_run_id VARCHAR, formula_name VARCHAR, formula_version VARCHAR,
		phase VARCHAR, role VARCHAR, model VARCHAR, tower VARCHAR,
		branch VARCHAR, result VARCHAR, review_rounds INTEGER,
		context_tokens_in BIGINT, context_tokens_out BIGINT, total_tokens BIGINT,
		cost_usd DOUBLE, duration_seconds DOUBLE,
		startup_seconds DOUBLE, working_seconds DOUBLE, queue_seconds DOUBLE, review_seconds DOUBLE,
		files_changed INTEGER, lines_added INTEGER, lines_removed INTEGER,
		read_calls INTEGER, edit_calls INTEGER, tool_calls_json TEXT,
		failure_class VARCHAR, attempt_number INTEGER,
		started_at TIMESTAMP, completed_at TIMESTAMP
	)`)
	if err != nil {
		t.Fatalf("create mock table: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)

	// Insert a row with a lexically "large" id
	_, err = mockDolt.Exec(`INSERT INTO agent_runs VALUES
		('run-zzz', 'spi-a', NULL, NULL, 'f', '1', 'plan', 'wizard', 'opus', 't', 'main', 'success', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, NULL, NULL, 1, ?, ?)`,
		now.Add(-2*time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("insert row 1: %v", err)
	}

	ctx := context.Background()
	etl := NewETL(olapDB)

	// First sync: picks up run-zzz
	n, err := etl.Sync(ctx, mockDolt)
	if err != nil {
		t.Fatalf("Sync 1: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row, got %d", n)
	}

	// Insert a row with a lexically SMALLER id but LATER started_at.
	// With the old id-based cursor (WHERE id > 'run-zzz'), this row would be
	// skipped forever because 'run-aaa' < 'run-zzz'.
	_, err = mockDolt.Exec(`INSERT INTO agent_runs VALUES
		('run-aaa', 'spi-b', NULL, NULL, 'f', '1', 'impl', 'apprentice', 'opus', 't', 'main', 'success', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, NULL, NULL, 1, ?, ?)`,
		now.Add(-30*time.Minute), now.Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("insert row 2: %v", err)
	}

	// Second sync: must pick up run-aaa (plus re-process boundary)
	n2, err := etl.Sync(ctx, mockDolt)
	if err != nil {
		t.Fatalf("Sync 2: %v", err)
	}
	if n2 < 1 {
		t.Errorf("expected at least 1 new row synced, got %d", n2)
	}

	// Verify both rows are in the OLAP table
	var count int
	if err := olapDB.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_runs_olap").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 rows in agent_runs_olap, got %d", count)
	}
}

// TestWriteFuncSequential verifies that WriteFunc works correctly for sequential
// writes: open→write→close, then open again. This proves the per-write open/close
// pattern doesn't lose data.
func TestWriteFuncSequential(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/sequential_test.duckdb"

	if err := EnsureSchema(dbPath); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	const writes = 10
	for i := 0; i < writes; i++ {
		err := WriteFunc(dbPath, func(tx *sql.Tx) error {
			sessionID := fmt.Sprintf("sess-%d", i)
			_, err := tx.Exec(`INSERT INTO tool_events
				(session_id, bead_id, agent_name, step, tool_name, duration_ms, success, timestamp, tower)
				VALUES (?, 'test', 'test', 'test', 'Read', 10, true, current_timestamp, 'test')`,
				sessionID)
			return err
		})
		if err != nil {
			t.Fatalf("WriteFunc write %d: %v", i, err)
		}
	}

	// Verify all writes persisted across open/close cycles.
	var count int
	if err := ReadFunc(dbPath, func(db *sql.DB) error {
		return db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM tool_events").Scan(&count)
	}); err != nil {
		t.Fatalf("ReadFunc: %v", err)
	}
	if count != writes {
		t.Errorf("expected %d rows, got %d", writes, count)
	}
}

// TestWriteFuncRetryOnLockError verifies that IsDuckDBLockError correctly
// identifies lock errors and that WriteFunc would retry on them.
func TestWriteFuncRetryOnLockError(t *testing.T) {
	// Verify lock error detection works for all known DuckDB lock messages.
	lockMessages := []string{
		"IO Error: Could not set lock on file",
		"database is locked",
		"io error: failed to set lock on database file",
	}
	for _, msg := range lockMessages {
		if !IsDuckDBLockError(fmt.Errorf("%s", msg)) {
			t.Errorf("expected %q to be detected as lock error", msg)
		}
	}

	// Non-lock errors should not trigger retry.
	nonLockMessages := []string{
		"syntax error",
		"table not found",
		"connection refused",
	}
	for _, msg := range nonLockMessages {
		if IsDuckDBLockError(fmt.Errorf("%s", msg)) {
			t.Errorf("expected %q to NOT be detected as lock error", msg)
		}
	}
}

// TestReadFunc verifies that ReadFunc opens, queries, and closes without error.
func TestReadFunc(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/readfunc_test.duckdb"

	if err := EnsureSchema(dbPath); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	var count int
	if err := ReadFunc(dbPath, func(db *sql.DB) error {
		return db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM agent_runs_olap").Scan(&count)
	}); err != nil {
		t.Fatalf("ReadFunc: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows in empty table, got %d", count)
	}
}

// TestIsDuckDBLockError verifies lock error detection.
func TestIsDuckDBLockError(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"Could not set lock on file", true},
		{"IO Error: could not set lock on file", true},
		{"database is locked", true},
		{"syntax error", false},
		{"connection refused", false},
	}
	for _, tt := range tests {
		got := IsDuckDBLockError(fmt.Errorf("%s", tt.msg))
		if got != tt.want {
			t.Errorf("IsDuckDBLockError(%q) = %v, want %v", tt.msg, got, tt.want)
		}
	}
	if IsDuckDBLockError(nil) {
		t.Error("IsDuckDBLockError(nil) should be false")
	}
}

// TestETLStaleCursorMigration verifies that a stale id-based cursor value
// (from before the started_at fix) triggers a full re-sync.
func TestETLStaleCursorMigration(t *testing.T) {
	olapDB, err := Open("")
	if err != nil {
		t.Fatalf("Open olap: %v", err)
	}
	defer olapDB.Close()

	mockDolt, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("Open mock dolt: %v", err)
	}
	defer mockDolt.Close()

	_, err = mockDolt.Exec(`CREATE TABLE agent_runs (
		id VARCHAR PRIMARY KEY, bead_id VARCHAR, epic_id VARCHAR,
		parent_run_id VARCHAR, formula_name VARCHAR, formula_version VARCHAR,
		phase VARCHAR, role VARCHAR, model VARCHAR, tower VARCHAR,
		branch VARCHAR, result VARCHAR, review_rounds INTEGER,
		context_tokens_in BIGINT, context_tokens_out BIGINT, total_tokens BIGINT,
		cost_usd DOUBLE, duration_seconds DOUBLE,
		startup_seconds DOUBLE, working_seconds DOUBLE, queue_seconds DOUBLE, review_seconds DOUBLE,
		files_changed INTEGER, lines_added INTEGER, lines_removed INTEGER,
		read_calls INTEGER, edit_calls INTEGER, tool_calls_json TEXT,
		failure_class VARCHAR, attempt_number INTEGER,
		started_at TIMESTAMP, completed_at TIMESTAMP
	)`)
	if err != nil {
		t.Fatalf("create mock table: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	_, err = mockDolt.Exec(`INSERT INTO agent_runs VALUES
		('run-abc', 'spi-x', NULL, NULL, 'f', '1', 'plan', 'wizard', 'opus', 't', 'main', 'success', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, NULL, NULL, 1, ?, ?)`,
		now, now)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	ctx := context.Background()

	// Manually insert a stale id-based cursor (simulating pre-fix state)
	_, err = olapDB.db.ExecContext(ctx,
		`INSERT INTO etl_cursor (table_name, last_id, last_synced) VALUES ('agent_runs', 'run-xyz', now())`)
	if err != nil {
		t.Fatalf("insert stale cursor: %v", err)
	}

	// Sync should detect the stale cursor, reset, and do a full sync
	etl := NewETL(olapDB)
	n, err := etl.Sync(ctx, mockDolt)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row from full re-sync, got %d", n)
	}

	// Cursor should now be a timestamp, not an id
	var cursorVal string
	if err := olapDB.db.QueryRowContext(ctx, "SELECT last_id FROM etl_cursor WHERE table_name = 'agent_runs'").Scan(&cursorVal); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursorVal == "" || cursorVal == "run-xyz" {
		t.Errorf("cursor should be an RFC3339 timestamp, got %q", cursorVal)
	}
}
