package olap

const createAgentRunsOLAP = `
CREATE TABLE IF NOT EXISTS agent_runs_olap (
    id               VARCHAR PRIMARY KEY,
    bead_id          VARCHAR,
    epic_id          VARCHAR,
    parent_run_id    VARCHAR,
    formula_name     VARCHAR,
    formula_version  VARCHAR,
    phase            VARCHAR,
    role             VARCHAR,
    model            VARCHAR,
    tower            VARCHAR,
    repo             VARCHAR,
    branch           VARCHAR,
    result           VARCHAR,
    review_rounds    INTEGER,
    prompt_tokens    BIGINT,
    completion_tokens BIGINT,
    total_tokens     BIGINT,
    cost_usd         DOUBLE,
    duration_seconds DOUBLE,
    startup_seconds  DOUBLE,
    working_seconds  DOUBLE,
    queue_seconds    DOUBLE,
    review_seconds   DOUBLE,
    files_changed    INTEGER,
    lines_added      INTEGER,
    lines_removed    INTEGER,
    read_calls       INTEGER,
    edit_calls       INTEGER,
    tool_calls_json  TEXT,
    failure_class    VARCHAR,
    attempt_number   INTEGER,
    started_at       TIMESTAMP,
    completed_at     TIMESTAMP,
    synced_at        TIMESTAMP DEFAULT now()
)`

// createETLCursor defines the cursor table. The last_id column stores the
// high-water-mark value for incremental sync. For agent_runs this is an
// RFC3339 started_at timestamp (not an id — the column name is historical).
const createETLCursor = `
CREATE TABLE IF NOT EXISTS etl_cursor (
    table_name   VARCHAR PRIMARY KEY,
    last_id      VARCHAR NOT NULL DEFAULT '',
    last_synced  TIMESTAMP
)`

const createDailyFormulaStats = `
CREATE TABLE IF NOT EXISTS daily_formula_stats (
    date             DATE,
    formula_name     VARCHAR,
    formula_version  VARCHAR,
    tower            VARCHAR,
    repo             VARCHAR,
    run_count        INTEGER,
    success_count    INTEGER,
    total_cost_usd   DOUBLE,
    avg_duration_s   DOUBLE,
    avg_review_rounds DOUBLE,
    PRIMARY KEY (date, formula_name, formula_version, tower, repo)
)`

const createWeeklyMergeStats = `
CREATE TABLE IF NOT EXISTS weekly_merge_stats (
    week_start       DATE,
    tower            VARCHAR,
    repo             VARCHAR,
    merge_count      INTEGER,
    failure_count    INTEGER,
    avg_lead_time_s  DOUBLE,
    PRIMARY KEY (week_start, tower, repo)
)`

const createPhaseCostBreakdown = `
CREATE TABLE IF NOT EXISTS phase_cost_breakdown (
    date         DATE,
    tower        VARCHAR,
    formula_name VARCHAR,
    phase        VARCHAR,
    run_count    INTEGER,
    total_cost   DOUBLE,
    PRIMARY KEY (date, tower, formula_name, phase)
)`

const createToolUsageStats = `
CREATE TABLE IF NOT EXISTS tool_usage_stats (
    date          DATE,
    tower         VARCHAR,
    formula_name  VARCHAR,
    phase         VARCHAR,
    total_runs    INTEGER,
    total_read    INTEGER,
    total_edit    INTEGER,
    total_tools   INTEGER,
    PRIMARY KEY (date, tower, formula_name, phase)
)`

const createFailureHotspots = `
CREATE TABLE IF NOT EXISTS failure_hotspots (
    week_start      DATE,
    tower           VARCHAR,
    bead_id         VARCHAR,
    failure_class   VARCHAR,
    attempt_count   INTEGER,
    last_failure_at TIMESTAMP,
    PRIMARY KEY (week_start, tower, bead_id, failure_class)
)`

const createToolEvents = `
CREATE TABLE IF NOT EXISTS tool_events (
    session_id    VARCHAR,
    bead_id       VARCHAR,
    agent_name    VARCHAR,
    step          VARCHAR,
    tool_name     VARCHAR,
    duration_ms   INTEGER,
    success       BOOLEAN,
    timestamp     TIMESTAMP DEFAULT current_timestamp,
    tower         VARCHAR,
    provider      VARCHAR,
    event_kind    VARCHAR
)`

const createToolSpans = `
CREATE TABLE IF NOT EXISTS tool_spans (
    trace_id       VARCHAR,
    span_id        VARCHAR,
    parent_span_id VARCHAR,
    session_id     VARCHAR,
    bead_id        VARCHAR,
    agent_name     VARCHAR,
    step           VARCHAR,
    span_name      VARCHAR,
    kind           VARCHAR,
    duration_ms    INTEGER,
    success        BOOLEAN,
    start_time     TIMESTAMP,
    end_time       TIMESTAMP,
    tower          VARCHAR,
    attributes     TEXT
)`

const createAPIEvents = `
CREATE TABLE IF NOT EXISTS api_events (
    session_id      VARCHAR,
    bead_id         VARCHAR,
    agent_name      VARCHAR,
    step            VARCHAR,
    provider        VARCHAR,
    model           VARCHAR,
    duration_ms     INTEGER,
    input_tokens    BIGINT,
    output_tokens   BIGINT,
    cache_read_tokens  BIGINT,
    cache_write_tokens BIGINT,
    cost_usd        DOUBLE,
    timestamp       TIMESTAMP DEFAULT current_timestamp,
    tower           VARCHAR
)`

// schemaMigrations returns ALTER TABLE statements for incremental schema
// evolution. Each is executed with errors ignored (column may already exist).
func schemaMigrations() []string {
	return []string{
		`ALTER TABLE tool_events ADD COLUMN IF NOT EXISTS provider VARCHAR`,
		`ALTER TABLE tool_events ADD COLUMN IF NOT EXISTS event_kind VARCHAR`,
	}
}

// allSchemaStatements returns the DDL statements in creation order.
func allSchemaStatements() []string {
	return []string{
		createAgentRunsOLAP,
		createETLCursor,
		createDailyFormulaStats,
		createWeeklyMergeStats,
		createPhaseCostBreakdown,
		createToolUsageStats,
		createFailureHotspots,
		createToolEvents,
		createToolSpans,
		createAPIEvents,
	}
}
