package clickhouse

import (
	"database/sql"
	"fmt"
	"net/url"
)

// ClickHouse DDL statements. These mirror the DuckDB schema in
// pkg/olap/schema.go but use ClickHouse-native types and MergeTree
// engines. Translated in preparation for the cluster path that writes
// trace spans and runs-analytics data to an external ClickHouse server.

const createToolEvents = `
CREATE TABLE IF NOT EXISTS tool_events (
    session_id    String,
    bead_id       String,
    agent_name    String,
    step          String,
    tool_name     String,
    duration_ms   Int32,
    success       Bool,
    timestamp     DateTime64(3) DEFAULT now64(3),
    tower         String,
    provider      String,
    event_kind    String
) ENGINE = MergeTree()
ORDER BY (tower, bead_id, timestamp)
`

const createToolSpans = `
CREATE TABLE IF NOT EXISTS tool_spans (
    trace_id       String,
    span_id        String,
    parent_span_id String,
    session_id     String,
    bead_id        String,
    agent_name     String,
    step           String,
    span_name      String,
    kind           String,
    duration_ms    Int32,
    success        Bool,
    start_time     DateTime64(3),
    end_time       DateTime64(3),
    tower          String,
    attributes     String
) ENGINE = MergeTree()
ORDER BY (tower, bead_id, start_time)
`

const createAPIEvents = `
CREATE TABLE IF NOT EXISTS api_events (
    session_id         String,
    bead_id            String,
    agent_name         String,
    step               String,
    provider           String,
    model              String,
    duration_ms        Int32,
    input_tokens       Int64,
    output_tokens      Int64,
    cache_read_tokens  Int64,
    cache_write_tokens Int64,
    cost_usd           Float64,
    timestamp          DateTime64(3) DEFAULT now64(3),
    tower              String
) ENGINE = MergeTree()
ORDER BY (tower, bead_id, timestamp)
`

// agent_runs_olap uses ReplacingMergeTree so ETL can do simple INSERTs
// without ON CONFLICT. ClickHouse deduplicates by keeping the row with
// the latest synced_at for each unique (id) during background merges.
const createAgentRunsOLAP = `
CREATE TABLE IF NOT EXISTS agent_runs_olap (
    id                 String,
    bead_id            String,
    epic_id            String,
    parent_run_id      String,
    formula_name       String,
    formula_version    String,
    phase              String,
    role               String,
    model              String,
    tower              String,
    repo               String,
    branch             String,
    result             String,
    review_rounds      Int32,
    prompt_tokens      Int64,
    completion_tokens  Int64,
    total_tokens       Int64,
    cost_usd           Float64,
    duration_seconds   Float64,
    startup_seconds    Float64,
    working_seconds    Float64,
    queue_seconds      Float64,
    review_seconds     Float64,
    files_changed      Int32,
    lines_added        Int32,
    lines_removed      Int32,
    read_calls         Int32,
    edit_calls         Int32,
    tool_calls_json    String,
    failure_class      String,
    attempt_number     Int32,
    started_at         DateTime64(3),
    completed_at       DateTime64(3),
    synced_at          DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(synced_at)
ORDER BY (id)
`

// etl_cursor uses ReplacingMergeTree so cursor updates are simple INSERTs.
// ClickHouse keeps the row with the latest last_synced per table_name.
const createETLCursor = `
CREATE TABLE IF NOT EXISTS etl_cursor (
    table_name  String,
    last_id     String DEFAULT '',
    last_synced DateTime64(3)
) ENGINE = ReplacingMergeTree(last_synced)
ORDER BY (table_name)
`

// schemaStatements returns all ClickHouse DDL in creation order.
func schemaStatements() []string {
	return []string{
		createToolEvents,
		createToolSpans,
		createAPIEvents,
		createAgentRunsOLAP,
		createETLCursor,
	}
}

// ensureDatabase connects to the ClickHouse default database and creates
// the target database if it doesn't exist. The dsn should be the full
// connection string including the target database (e.g.
// "clickhouse://host:9000/spire"). Idempotent.
func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse clickhouse dsn: %w", err)
	}
	dbName := ""
	if u.Path != "" {
		dbName = u.Path[1:]
	}
	if dbName == "" {
		return nil
	}

	bootstrap := *u
	bootstrap.Path = "/default"
	db, err := sql.Open("clickhouse", bootstrap.String())
	if err != nil {
		return fmt.Errorf("clickhouse bootstrap open: %w", err)
	}
	defer db.Close()

	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS " + dbName); err != nil {
		return fmt.Errorf("clickhouse create database %s: %w", dbName, err)
	}
	return nil
}

// initSchema creates all OLAP tables in ClickHouse if they don't exist.
// Idempotent — safe to call on every startup.
func initSchema(db *sql.DB) error {
	for _, ddl := range schemaStatements() {
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("clickhouse schema: %w", err)
		}
	}
	return nil
}
