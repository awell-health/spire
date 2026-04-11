package olap

import (
	"context"
	"database/sql"
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

	// Verify cursor was updated
	var lastID string
	if err := olapDB.db.QueryRowContext(ctx, "SELECT last_id FROM etl_cursor WHERE table_name = 'agent_runs'").Scan(&lastID); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if lastID != "run-002" {
		t.Errorf("expected cursor at run-002, got %s", lastID)
	}

	// Second sync should be a no-op (cursor is at run-002, no new rows)
	n2, err := etl.Sync(ctx, mockDolt)
	if err != nil {
		t.Fatalf("Sync (second): %v", err)
	}
	if n2 != 0 {
		t.Errorf("expected 0 rows synced on second call, got %d", n2)
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

	// Reset cursor to re-sync the same row
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
