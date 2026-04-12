# Canonical Metric Set

This document defines every metric Spire captures, where the data lives,
and what each metric means. It is the contract for the CLI, board, MCP
server, web dashboard, and Slack bot.

---

## Data Flow

```
Agent run
  -> pkg/executor/record.go   (writes result.json + Dolt agent_runs row)
  -> Dolt agent_runs table     (source of truth, versioned)
  -> pkg/olap/etl.go           (incremental sync, cursor-based)
  -> DuckDB agent_runs_olap    (analytical copy)
  -> pkg/olap/views.go         (materialized view refresh after each ETL sync)
  -> Materialized views        (daily_formula_stats, weekly_merge_stats, etc.)
  -> pkg/olap/queries.go       (query functions return typed Go structs)
  -> Consumers                 (CLI, board, MCP server, web dashboard)
```

OTel-sourced events (tool calls, API calls, spans) flow through a
separate path:

```
Agent OTel SDK
  -> pkg/otel/receiver.go     (gRPC log/trace receiver)
  -> DuckDB tool_events, tool_spans, api_events   (written directly)
  -> pkg/olap/queries.go       (query functions)
  -> Consumers
```

All materialized views use a rolling 90-day window (`viewRetentionDays = 90`
in `pkg/olap/views.go`). Views are rebuilt via DELETE + re-aggregate after
each ETL sync.

---

## Per-Run Metrics

**Source table:** `agent_runs_olap` (DuckDB, defined in `pkg/olap/schema.go`)
**Populated by:** `pkg/olap/etl.go` syncing from Dolt `agent_runs`
**Query functions:** `QuerySummary`, `QueryModelBreakdown`, `QueryPhaseBreakdown`

### Dimensions

| Column | Type | Description |
|--------|------|-------------|
| `id` | VARCHAR (PK) | Unique run identifier |
| `bead_id` | VARCHAR | Bead this run belongs to |
| `epic_id` | VARCHAR | Parent epic bead ID |
| `parent_run_id` | VARCHAR | ID of the parent run (for nested agent spawns) |
| `formula_name` | VARCHAR | Formula used (e.g. `task-default`, `epic-wizard`) |
| `formula_version` | VARCHAR | Formula version string |
| `phase` | VARCHAR | Execution phase (e.g. `implement`, `review`, `seal`, `merge`) |
| `role` | VARCHAR | Agent role (e.g. `apprentice`, `sage`, `wizard`) |
| `model` | VARCHAR | LLM model used (e.g. `claude-opus-4-6`) |
| `tower` | VARCHAR | Tower name |
| `repo` | VARCHAR | Repository prefix |
| `branch` | VARCHAR | Git branch |

### Result

| Column | Type | Description |
|--------|------|-------------|
| `result` | VARCHAR | Outcome of the run |
| `failure_class` | VARCHAR | Failure categorization (set only on non-success results) |
| `attempt_number` | INTEGER | Which attempt this is for the same bead+phase |

**`result` values:**

| Value | Meaning |
|-------|---------|
| `success` | Run completed successfully |
| `test_failure` | Tests failed after implementation |
| `no_changes` | Agent produced no code changes |
| `timeout` | Run exceeded time limit |
| `review_rejected` | Sage rejected the implementation |
| `error` | Unrecoverable error during execution |
| `empty_diff` | Agent committed but diff was empty |

**`failure_class` values:**

| Value | Meaning |
|-------|---------|
| `timeout` | Time limit exceeded |
| `test_fail` | Test suite failed |
| `review_reject` | Review round rejected |
| `merge_conflict` | Git merge conflict |
| `build_fail` | Build/compilation failed |
| `unknown` | Unclassified failure |

Additional failure classes may be added by the failure classification
improvement task (spi-hj1kt).

### Timing

| Column | Type | Unit | Description |
|--------|------|------|-------------|
| `duration_seconds` | DOUBLE | seconds | Total wall-clock time for the run |
| `startup_seconds` | DOUBLE | seconds | Time from spawn to first agent action |
| `working_seconds` | DOUBLE | seconds | Time spent in active implementation |
| `queue_seconds` | DOUBLE | seconds | Time waiting in queue before execution |
| `review_seconds` | DOUBLE | seconds | Time spent in review rounds |

### Tokens and Cost

| Column | Type | Description |
|--------|------|-------------|
| `prompt_tokens` | BIGINT | Total input tokens sent to the LLM |
| `completion_tokens` | BIGINT | Total output tokens received from the LLM |
| `total_tokens` | BIGINT | `prompt_tokens + completion_tokens` |
| `cost_usd` | DOUBLE | Estimated cost in USD |

### Code Changes

| Column | Type | Description |
|--------|------|-------------|
| `files_changed` | INTEGER | Number of files modified |
| `lines_added` | INTEGER | Lines added across all files |
| `lines_removed` | INTEGER | Lines removed across all files |

### Tool Usage (per run)

| Column | Type | Description |
|--------|------|-------------|
| `read_calls` | INTEGER | Number of file read tool invocations |
| `edit_calls` | INTEGER | Number of file edit tool invocations |
| `tool_calls_json` | TEXT | JSON map of tool_name -> invocation count |

### Review

| Column | Type | Description |
|--------|------|-------------|
| `review_rounds` | INTEGER | Number of review iterations before acceptance |

### Timestamps

| Column | Type | Description |
|--------|------|-------------|
| `started_at` | TIMESTAMP | When the run began |
| `completed_at` | TIMESTAMP | When the run finished |
| `synced_at` | TIMESTAMP | When this row was synced to DuckDB (default: now()) |

---

## DORA Metrics

**Source tables:** `weekly_merge_stats` (materialized view), `agent_runs_olap`
**Query function:** `QueryDORA(since time.Time) -> *DORAMetrics`
**Go type:** `olap.DORAMetrics`

| Metric | Field | Unit | How it's computed |
|--------|-------|------|-------------------|
| Deploy Frequency | `DeployFrequency` | deploys/week | `AVG(merge_count)` from `weekly_merge_stats` |
| Lead Time | `LeadTimeSeconds` | seconds | `AVG(avg_lead_time_s)` from `weekly_merge_stats` — time from first run to successful completion per bead |
| Change Failure Rate | `ChangeFailureRate` | ratio 0.0–1.0 | `SUM(failure_count) / (SUM(merge_count) + SUM(failure_count))` |
| MTTR | `MTTRSeconds` | seconds | From `agent_runs_olap`: avg time from first failure to next success per bead |

### weekly_merge_stats (materialized view)

Aggregated per `(week_start, tower, repo)` from a per-bead subquery:

| Column | Type | Description |
|--------|------|-------------|
| `week_start` | DATE (PK) | Monday of the week |
| `tower` | VARCHAR (PK) | Tower name |
| `repo` | VARCHAR (PK) | Repository prefix |
| `merge_count` | INTEGER | Distinct beads with successful seal/merge phase |
| `failure_count` | INTEGER | Distinct beads with failures in key phases (seal, merge, implement, review) |
| `avg_lead_time_s` | DOUBLE | Avg seconds from first run to successful completion per bead |

---

## Formula Performance

**Source table:** `daily_formula_stats` (materialized view), `agent_runs_olap` (direct query)
**Query function:** `QueryFormulaPerformance(since time.Time) -> []FormulaStats`
**Go type:** `olap.FormulaStats`

| Field | Type | Description |
|-------|------|-------------|
| `FormulaName` | string | Formula identifier (e.g. `task-default`) |
| `FormulaVersion` | string | Formula version |
| `TotalRuns` | int | Total runs across the time window |
| `Successes` | int | Runs with `result = 'success'` |
| `SuccessRate` | float64 | Percentage (0–100) |
| `AvgCostUSD` | float64 | Average cost per run in USD |
| `AvgReviewRounds` | float64 | Average review rounds (only counts runs with reviews > 0) |
| `RunsLast30d` | int | Runs in the most recent 30-day window |

### daily_formula_stats (materialized view)

Aggregated per `(date, formula_name, formula_version, tower, repo)`:

| Column | Type | Description |
|--------|------|-------------|
| `date` | DATE (PK) | Calendar date |
| `formula_name` | VARCHAR (PK) | Formula identifier |
| `formula_version` | VARCHAR (PK) | Formula version |
| `tower` | VARCHAR (PK) | Tower name |
| `repo` | VARCHAR (PK) | Repository prefix |
| `run_count` | INTEGER | Total runs that day |
| `success_count` | INTEGER | Successful runs |
| `total_cost_usd` | DOUBLE | Total cost |
| `avg_duration_s` | DOUBLE | Average run duration in seconds |
| `avg_review_rounds` | DOUBLE | Average review rounds |

---

## Tool Usage

Two data sources provide tool usage metrics at different granularity.

### OTel Tool Events (preferred)

**Source table:** `tool_events` (DuckDB, defined in `pkg/olap/schema.go`)
**Query functions:** `QueryToolEvents(since)`, `QueryToolEventsByBead(beadID)`, `QueryToolEventsByStep(beadID)`
**Go types:** `olap.ToolEventStats`, `olap.StepToolBreakdown`

Per-tool invocation data from the OTel pipeline:

| Column | Type | Description |
|--------|------|-------------|
| `session_id` | VARCHAR | Agent session identifier |
| `bead_id` | VARCHAR | Associated bead |
| `agent_name` | VARCHAR | Agent that made the call |
| `step` | VARCHAR | Formula step (e.g. `implement`, `review`) |
| `tool_name` | VARCHAR | Tool invoked (e.g. `Read`, `Edit`, `Bash`) |
| `duration_ms` | INTEGER | Call duration in milliseconds |
| `success` | BOOLEAN | Whether the call succeeded |
| `timestamp` | TIMESTAMP | When the call occurred |
| `tower` | VARCHAR | Tower name |
| `provider` | VARCHAR | Tool provider |
| `event_kind` | VARCHAR | Event classification |

**Aggregated query results (`ToolEventStats`):**

| Field | Type | Description |
|-------|------|-------------|
| `ToolName` | string | Tool identifier |
| `Count` | int | Total invocations |
| `AvgDurationMs` | float64 | Average call duration in milliseconds |
| `FailureCount` | int | Number of failed invocations |
| `Step` | string | Formula step (populated in per-bead queries) |

### Legacy Tool Usage Stats (fallback)

**Source table:** `tool_usage_stats` (materialized view from `agent_runs_olap`)
**Query function:** `QueryToolUsage(since time.Time) -> []ToolUsageStats`
**Go type:** `olap.ToolUsageStats`

Per-formula/phase aggregate of read and edit calls from agent run records:

| Field | Type | Description |
|-------|------|-------------|
| `FormulaName` | string | Formula identifier |
| `Phase` | string | Execution phase |
| `TotalRead` | int | Total file read calls |
| `TotalEdit` | int | Total file edit calls |
| `TotalTools` | int | `TotalRead + TotalEdit` |
| `ReadRatio` | float64 | `TotalRead / TotalTools` (0.0–1.0) |

### tool_usage_stats (materialized view)

Aggregated per `(date, tower, formula_name, phase)`:

| Column | Type | Description |
|--------|------|-------------|
| `date` | DATE (PK) | Calendar date |
| `tower` | VARCHAR (PK) | Tower name |
| `formula_name` | VARCHAR (PK) | Formula identifier |
| `phase` | VARCHAR (PK) | Execution phase |
| `total_runs` | INTEGER | Number of runs |
| `total_read` | INTEGER | Sum of read_calls |
| `total_edit` | INTEGER | Sum of edit_calls |
| `total_tools` | INTEGER | Sum of read + edit calls |

---

## Tool Spans

**Source table:** `tool_spans` (DuckDB, defined in `pkg/olap/schema.go`)
**Query function:** `QueryToolSpansByBead(beadID) -> []SpanRecord`
**Go type:** `olap.SpanRecord`

OpenTelemetry trace spans for waterfall/timeline visualization:

| Column | Type | Description |
|--------|------|-------------|
| `trace_id` | VARCHAR | OTel trace identifier |
| `span_id` | VARCHAR | OTel span identifier |
| `parent_span_id` | VARCHAR | Parent span (for nesting) |
| `session_id` | VARCHAR | Agent session |
| `bead_id` | VARCHAR | Associated bead |
| `agent_name` | VARCHAR | Agent name |
| `step` | VARCHAR | Formula step |
| `span_name` | VARCHAR | Span operation name |
| `kind` | VARCHAR | Span kind |
| `duration_ms` | INTEGER | Duration in milliseconds |
| `success` | BOOLEAN | Whether the operation succeeded |
| `start_time` | TIMESTAMP | Span start |
| `end_time` | TIMESTAMP | Span end |
| `tower` | VARCHAR | Tower name |
| `attributes` | TEXT | JSON-encoded span attributes |

---

## API/Cost Metrics

**Source table:** `api_events` (DuckDB, defined in `pkg/olap/schema.go`)
**Query function:** `QueryAPIEventsByBead(beadID) -> []APIEventStats`
**Go type:** `olap.APIEventStats`

Per-model LLM API call data, populated via the OTel pipeline:

| Column | Type | Description |
|--------|------|-------------|
| `session_id` | VARCHAR | Agent session |
| `bead_id` | VARCHAR | Associated bead |
| `agent_name` | VARCHAR | Agent name |
| `step` | VARCHAR | Formula step |
| `provider` | VARCHAR | API provider (e.g. `anthropic`) |
| `model` | VARCHAR | Model identifier (e.g. `claude-opus-4-6`) |
| `duration_ms` | INTEGER | API call duration in milliseconds |
| `input_tokens` | BIGINT | Tokens sent |
| `output_tokens` | BIGINT | Tokens received |
| `cache_read_tokens` | BIGINT | Tokens served from prompt cache |
| `cache_write_tokens` | BIGINT | Tokens written to prompt cache |
| `cost_usd` | DOUBLE | Cost for this API call in USD |
| `timestamp` | TIMESTAMP | When the call occurred |
| `tower` | VARCHAR | Tower name |

**Aggregated query results (`APIEventStats`):**

| Field | Type | Description |
|-------|------|-------------|
| `Model` | string | Model identifier |
| `Count` | int | Total API calls |
| `AvgDurationMs` | float64 | Average call duration in milliseconds |
| `TotalCostUSD` | float64 | Total cost across all calls |
| `TotalInputTokens` | int64 | Total tokens sent |
| `TotalOutputTokens` | int64 | Total tokens received |

---

## Cost Trend

**Source table:** `agent_runs_olap` (direct query)
**Query function:** `QueryCostTrend(days int) -> []CostTrendPoint`
**Go type:** `olap.CostTrendPoint`

Daily cost and token aggregates:

| Field | Type | Description |
|-------|------|-------------|
| `Date` | time.Time | Calendar date |
| `TotalCost` | float64 | Total cost in USD for the day |
| `RunCount` | int | Number of runs |
| `PromptTokens` | int64 | Total input tokens |
| `CompletionTokens` | int64 | Total output tokens |

---

## Failure Analysis

**Source table:** `failure_hotspots` (materialized view)
**Query function:** `QueryBugCausality(limit int) -> []BugCausality`
**Go type:** `olap.BugCausality`

Identifies beads with repeated failures, ordered by attempt count:

| Field | Type | Description |
|-------|------|-------------|
| `BeadID` | string | Bead that keeps failing |
| `FailureClass` | string | Type of failure |
| `AttemptCount` | int | Number of failed attempts |
| `LastFailure` | time.Time | Most recent failure timestamp |

### failure_hotspots (materialized view)

Aggregated per `(week_start, tower, bead_id, failure_class)`:

| Column | Type | Description |
|--------|------|-------------|
| `week_start` | DATE (PK) | Monday of the week |
| `tower` | VARCHAR (PK) | Tower name |
| `bead_id` | VARCHAR (PK) | Failing bead |
| `failure_class` | VARCHAR (PK) | Failure category |
| `attempt_count` | INTEGER | Failed attempts in this window |
| `last_failure_at` | TIMESTAMP | Most recent failure |

---

## Phase Cost Breakdown

**Source table:** `phase_cost_breakdown` (materialized view)
**No dedicated query function** — used for cost attribution analysis.

Aggregated per `(date, tower, formula_name, phase)`:

| Column | Type | Description |
|--------|------|-------------|
| `date` | DATE (PK) | Calendar date |
| `tower` | VARCHAR (PK) | Tower name |
| `formula_name` | VARCHAR (PK) | Formula identifier |
| `phase` | VARCHAR (PK) | Execution phase |
| `run_count` | INTEGER | Number of runs |
| `total_cost` | DOUBLE | Total cost in USD for this phase |

---

## Summary Stats

**Source table:** `agent_runs_olap` (direct query)
**Query function:** `QuerySummary(since time.Time) -> *SummaryStats`
**Go type:** `olap.SummaryStats`

| Field | Type | Description |
|-------|------|-------------|
| `TotalRuns` | int | Total runs in the window |
| `Successes` | int | Successful runs |
| `Failures` | int | Failed runs (excludes `skipped`) |
| `SuccessRate` | float64 | Percentage (0–100) |
| `AvgCostUSD` | float64 | Average cost per run |
| `AvgDurationS` | float64 | Average duration in seconds |
| `TotalCostUSD` | float64 | Sum of all costs |

---

## Model Breakdown

**Source table:** `agent_runs_olap` (direct query)
**Query function:** `QueryModelBreakdown(since time.Time) -> []ModelStats`
**Go type:** `olap.ModelStats`

| Field | Type | Description |
|-------|------|-------------|
| `Model` | string | Model identifier |
| `RunCount` | int | Total runs |
| `SuccessRate` | float64 | Percentage (0–100) |
| `AvgCostUSD` | float64 | Average cost per run |
| `AvgDurationS` | float64 | Average duration in seconds |
| `TotalTokens` | int64 | Total tokens across all runs |

---

## Phase Breakdown

**Source table:** `agent_runs_olap` (direct query)
**Query function:** `QueryPhaseBreakdown(since time.Time) -> []PhaseStats`
**Go type:** `olap.PhaseStats`

| Field | Type | Description |
|-------|------|-------------|
| `Phase` | string | Execution phase |
| `RunCount` | int | Total runs |
| `SuccessRate` | float64 | Percentage (0–100) |
| `AvgCostUSD` | float64 | Average cost per run |
| `AvgDurationS` | float64 | Average duration in seconds |

---

## Weekly Trends

**Source table:** `agent_runs_olap` (direct query)
**Query function:** `QueryTrends(since time.Time) -> []WeeklyTrend`
**Go type:** `olap.WeeklyTrend`

| Field | Type | Description |
|-------|------|-------------|
| `WeekStart` | time.Time | Monday of the week |
| `RunCount` | int | Total runs that week |
| `SuccessRate` | float64 | Percentage (0–100) |
| `TotalCostUSD` | float64 | Total cost for the week |
| `MergeCount` | int | Distinct beads with successful seal/merge |

---

## Failure Breakdown

**Source table:** `agent_runs_olap` (direct query)
**Query function:** `QueryFailures(since time.Time) -> []FailureStats`
**Go type:** `olap.FailureStats`

| Field | Type | Description |
|-------|------|-------------|
| `FailureClass` | string | Failure category |
| `Count` | int | Number of failures |
| `Percentage` | float64 | Percentage of total failures |

---

## Metric Consumers

### CLI (`spire metrics`)

The CLI exposes all metrics via flags. Default time window is 90 days.

| Flag | Query Function | What it shows |
|------|---------------|---------------|
| _(default)_ | `QuerySummary` + `QueryFormulaPerformance` | Overall stats + formula comparison |
| `--dora` | `QueryDORA` | DORA four key metrics |
| `--model` | `QueryModelBreakdown` | Per-model stats (runs, success rate, cost, tokens) |
| `--phase` | `QueryPhaseBreakdown` | Per-phase stats (runs, success rate, cost, duration) |
| `--trends` | `QueryTrends` | Week-over-week trend lines |
| `--failures` | `QueryFailures` | Failure breakdown by class |
| `--tools` | `QueryToolEvents` / `QueryToolUsage` | Tool call stats (prefers OTel data, falls back to legacy) |
| `--bugs` | `QueryBugCausality` | Top 5 failure hotspots |
| `--bead <id>` | direct query on `agent_runs_olap` | Per-bead run stats |
| `--json` | _(combinable with any flag)_ | JSON output for programmatic use |

All flags fall back to the `pkg/observability` Dolt-based queries when
DuckDB is unavailable.

### Board Metrics Mode (`pkg/board/metrics_mode.go`)

The TUI board displays a DORA header + 2x2 grid. Auto-refreshes every
30 seconds.

| Section | Position | Query Functions | Data |
|---------|----------|----------------|------|
| DORA header | top bar | `QueryDORA` | Deploy freq, lead time, failure rate, MTTR (color-coded) |
| Formula Performance | top-left (1) | `QueryFormulaPerformance` | Per-formula runs, success %, cost, review rounds |
| Cost Trend (30d) | top-right (2) | `QueryCostTrend` | Daily cost, token count, run count |
| Failure Hotspots | bottom-left (3) | `QueryBugCausality` | Beads with repeated failures |
| Tool Usage | bottom-right (4) | `QueryToolEvents` / `QueryToolUsage` | Per-tool call count, avg duration, failures |

### MCP Server (future)

Planned MCP resources:

| Resource | Metrics |
|----------|---------|
| `spire_metrics` | Summary stats, DORA, formula performance, tool usage |
| `spire_status` | Active runs, queue depth, agent health |
| `spire_trace` | Per-bead tool spans, API events, step breakdowns |

### Web Dashboard (future)

Will consume all metrics via the same `pkg/olap` query functions,
rendered as interactive charts and tables.
