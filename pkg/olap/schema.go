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

// allSchemaStatements returns the DDL statements in creation order.
func allSchemaStatements() []string {
	return []string{
		createAgentRunsOLAP,
		createETLCursor,
		createDailyFormulaStats,
		createWeeklyMergeStats,
		createPhaseCostBreakdown,
	}
}
