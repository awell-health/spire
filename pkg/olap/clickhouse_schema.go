package olap

import (
	"database/sql"
	"fmt"
	"net/url"
)

// ClickHouse DDL statements. These mirror the DuckDB schema in schema.go but
// use ClickHouse-native types and MergeTree engines.

const chCreateToolEvents = `
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

const chCreateToolSpans = `
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

const chCreateAPIEvents = `
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
    tower              String,
    event_type         String DEFAULT 'api_request',
    retry_count        Int32
) ENGINE = MergeTree()
ORDER BY (tower, bead_id, timestamp)
`

// agent_runs_olap uses ReplacingMergeTree so ETL can do simple INSERTs
// without ON CONFLICT. ClickHouse deduplicates by keeping the row with
// the latest synced_at for each unique (id) during background merges.
const chCreateAgentRunsOLAP = `
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
    synced_at          DateTime64(3) DEFAULT now64(3),
    turns              Int32,
    max_turns          Int32,
    stop_reason        String,
    cache_read_tokens  Int64,
    cache_write_tokens Int64
) ENGINE = ReplacingMergeTree(synced_at)
ORDER BY (id)
`

// etl_cursor uses ReplacingMergeTree so cursor updates are simple INSERTs.
// ClickHouse keeps the row with the latest last_synced per table_name.
const chCreateETLCursor = `
CREATE TABLE IF NOT EXISTS etl_cursor (
    table_name  String,
    last_id     String DEFAULT '',
    last_synced DateTime64(3)
) ENGINE = ReplacingMergeTree(last_synced)
ORDER BY (table_name)
`

// bead_lifecycle_olap mirrors the Dolt bead_lifecycle sidecar (hmdwm feature).
// One row per bead_id; ReplacingMergeTree keeps the latest synced_at so later
// transitions (e.g. close after file) overwrite earlier state.
const chCreateBeadLifecycleOLAP = `
CREATE TABLE IF NOT EXISTS bead_lifecycle_olap (
    bead_id       String,
    bead_type     String,
    filed_at      Nullable(DateTime64(3)),
    ready_at      Nullable(DateTime64(3)),
    started_at    Nullable(DateTime64(3)),
    closed_at     Nullable(DateTime64(3)),
    updated_at    Nullable(DateTime64(3)),
    review_count  Int32,
    fix_count     Int32,
    arbiter_count Int32,
    synced_at     DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(synced_at)
ORDER BY (bead_id)
`

// Aggregate tables refreshed by RefreshMaterializedViews. Use
// ReplacingMergeTree keyed on synced_at so repeated refreshes deduplicate by
// primary key during background merges. Queries should use FINAL or GROUP BY
// to get the deduped row.
const chCreateDailyFormulaStats = `
CREATE TABLE IF NOT EXISTS daily_formula_stats (
    date              Date,
    formula_name      String,
    formula_version   String,
    tower             String,
    repo              String,
    run_count         Int32,
    success_count     Int32,
    total_cost_usd    Float64,
    avg_duration_s    Float64,
    avg_review_rounds Float64,
    synced_at         DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(synced_at)
ORDER BY (date, formula_name, formula_version, tower, repo)
`

const chCreateWeeklyMergeStats = `
CREATE TABLE IF NOT EXISTS weekly_merge_stats (
    week_start      Date,
    tower           String,
    repo            String,
    merge_count     Int32,
    failure_count   Int32,
    avg_lead_time_s Float64,
    synced_at       DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(synced_at)
ORDER BY (week_start, tower, repo)
`

const chCreatePhaseCostBreakdown = `
CREATE TABLE IF NOT EXISTS phase_cost_breakdown (
    date         Date,
    tower        String,
    formula_name String,
    phase        String,
    run_count    Int32,
    total_cost   Float64,
    synced_at    DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(synced_at)
ORDER BY (date, tower, formula_name, phase)
`

const chCreateToolUsageStats = `
CREATE TABLE IF NOT EXISTS tool_usage_stats (
    date         Date,
    tower        String,
    formula_name String,
    phase        String,
    total_runs   Int32,
    total_read   Int32,
    total_edit   Int32,
    total_tools  Int32,
    synced_at    DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(synced_at)
ORDER BY (date, tower, formula_name, phase)
`

const chCreateFailureHotspots = `
CREATE TABLE IF NOT EXISTS failure_hotspots (
    week_start      Date,
    tower           String,
    bead_id         String,
    failure_class   String,
    attempt_count   Int32,
    last_failure_at DateTime64(3),
    synced_at       DateTime64(3) DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(synced_at)
ORDER BY (week_start, tower, bead_id, failure_class)
`

// clickHouseSchemaStatements returns all ClickHouse DDL in creation order.
func clickHouseSchemaStatements() []string {
	return []string{
		chCreateToolEvents,
		chCreateToolSpans,
		chCreateAPIEvents,
		chCreateAgentRunsOLAP,
		chCreateETLCursor,
		chCreateBeadLifecycleOLAP,
		chCreateDailyFormulaStats,
		chCreateWeeklyMergeStats,
		chCreatePhaseCostBreakdown,
		chCreateToolUsageStats,
		chCreateFailureHotspots,
	}
}

// EnsureClickHouseDatabase connects to the ClickHouse default database and
// creates the target database if it doesn't exist. The dsn should be the
// full connection string including the target database
// (e.g. "clickhouse://host:9000/spire"). Idempotent.
func EnsureClickHouseDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse clickhouse dsn: %w", err)
	}
	dbName := ""
	if u.Path != "" {
		dbName = u.Path[1:] // strip leading "/"
	}
	if dbName == "" {
		return nil // no target database specified, nothing to create
	}

	// Connect to the "default" database to run the CREATE DATABASE statement.
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

// InitClickHouseSchema creates all OLAP tables in ClickHouse if they don't
// exist. Idempotent — safe to call on every startup.
func InitClickHouseSchema(db *sql.DB) error {
	for _, ddl := range clickHouseSchemaStatements() {
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("clickhouse schema: %w", err)
		}
	}
	return nil
}
