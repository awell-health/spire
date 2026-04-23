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
| `phase` | VARCHAR | Execution phase — see **`phase` values** below |
| `phase_bucket` | VARCHAR | High-level attribution bucket (`design`, `implement`, `review`, or empty) derived from `phase` |
| `role` | VARCHAR | Agent role (e.g. `apprentice`, `sage`, `wizard`, `arbiter`, `cleric`) |
| `model` | VARCHAR | LLM model used (e.g. `claude-opus-4-6`) |
| `tower` | VARCHAR | Tower name |
| `repo` | VARCHAR | Repository prefix |
| `branch` | VARCHAR | Git branch |

**`phase` values:**

`recordAgentRun` emits one row per phase transition. The full set of values
currently emitted by the executor:

| Value | Recorded by | Meaning |
|-------|-------------|---------|
| `plan` | `pkg/executor/executor_plan.go` | Wizard plan phase (apprentice dispatch for planning) |
| `implement` | `pkg/executor/graph_interpreter.go`, `action_dispatch.go` | Apprentice implementation run (any step whose action spawns an apprentice resolves to the step name — usually `implement`) |
| `review` | `pkg/executor/executor_review.go`, `graph_actions.go` | Wizard review-loop row (timing) + per-sage/arbiter dispatch rows |
| `merge` | `pkg/executor/graph_actions.go` (`actionMergeToMain`) | Staging branch landed on the base branch — `result='success'` or `result='error'`. Drives DORA deploy-count |
| `close` | `pkg/executor/graph_actions.go` (`actionBeadFinish`) | Bead closed via terminal-close path |
| `discard` | `pkg/executor/graph_actions.go` (`actionBeadFinish`) | Bead discarded (wontfix/discard terminal path) |
| `validate-design` | `pkg/executor/executor_design.go` | Wizard design-validation phase (wait/poll for linked design bead) |
| `enrich-subtasks` | `pkg/executor/executor_design.go` | Wizard epic subtask-enrichment phase (change-spec generation) |
| `auto-approve` | `pkg/executor/executor_review.go` | Wizard advanced past a gate without dispatch (e.g. sage approved on first round) |
| `skip` | `pkg/executor/executor_review.go` | Wizard skipped a formula phase (reason captured via `skip_reason`) |
| `waitForHuman` | `pkg/executor/executor_review.go` | Wizard parked waiting for human approval; `working_seconds` captures the block duration |
| `execute` | `pkg/executor/graph_interpreter.go` | Top-level wizard execute row (parent run for all child phases) |
| `triage` | `pkg/executor/recovery_phase.go` | Cleric recovery triage dispatch |

`phase_bucket` rolls these up into `design` / `implement` / `review` for
cost-attribution queries; `merge`, `close`, `discard`, `plan`, `execute`,
and `triage` currently bucket to empty (they are not cost-attributed into
the three high-level buckets). See `phaseToBucket` in
`pkg/executor/record.go` for the exact mapping.

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
| `duration_seconds` | DOUBLE | seconds | Total wall-clock time for the run (`completed_at − started_at`). **Per-attempt:** when a step retries (e.g. review-fix rounds in `wizardRunSpawn`), each attempt records its own `duration_seconds` rather than accumulating from the first attempt. |
| `startup_seconds` | DOUBLE | seconds | Time from agent spawn to the first `tool_event` emitted by the agent. Captures init cost: container/pod startup, repo clone, context focus, claim. |
| `working_seconds` | DOUBLE | seconds | Time from the first `tool_event` to the last `tool_event`. Captures active LLM work, excluding startup and teardown. |
| `queue_seconds` | DOUBLE | seconds | Time from the bead becoming ready (no open blockers) to the wizard being assigned and spawn beginning. READY-queue latency. |
| `review_seconds` | DOUBLE | seconds | Time from review-loop entry (first sage dispatch) to review-loop exit (final verdict: approve, reject, or arbiter decision). Persisted in graph-state `Vars` so a loop that spans re-summons still records end-to-end. |

**Current bucket coverage.** The bucket *columns* are always present on
`agent_runs`; population is per-phase and intentionally partial — we
record what we can measure reliably rather than synthesize values.

| Phase | `startup_seconds` | `working_seconds` | `queue_seconds` | `review_seconds` |
|-------|-------------------|-------------------|-----------------|------------------|
| `merge` | populated | populated | — | — |
| `validate-design` | — | populated | — | — |
| `enrich-subtasks` | — | populated | — | — |
| `waitForHuman` | — | populated (= block duration) | — | — |
| `review` (wizard loop row) | — | — | — | populated |
| `implement`, `review` (sage), `plan`, `close`, `discard`, `auto-approve`, `skip` | — | — | — | — |

`queue_seconds` has a defined option (`withQueueSeconds` in
`pkg/executor/record.go`) but no caller populates it yet; the column
exists for forward-compatibility with a READY-queue tracker.

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
**Query functions:** `QueryAPIEventsByBead(beadID) -> []APIEventStats`, `QueryRateLimitEvents(window) -> []RateLimitBucket`
**Go types:** `olap.APIEventStats`, `olap.RateLimitBucket`

Per-model LLM API call data plus rate-limit observations, populated via
the OTel pipeline and direct writes from the Anthropic-call wrapper:

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
| `event_type` | VARCHAR | `api_request` (default, for normal LLM calls) or `rate_limit` (429/throttle observation) |
| `retry_count` | INTEGER | For `rate_limit` rows: number of retries the SDK attempted before giving up or succeeding. `NULL` for `api_request` rows. |

**Aggregated query results (`APIEventStats`):**

| Field | Type | Description |
|-------|------|-------------|
| `Model` | string | Model identifier |
| `Count` | int | Total API calls |
| `AvgDurationMs` | float64 | Average call duration in milliseconds |
| `TotalCostUSD` | float64 | Total cost across all calls |
| `TotalInputTokens` | int64 | Total tokens sent |
| `TotalOutputTokens` | int64 | Total tokens received |

### Rate-Limit Events

Every Anthropic API rate-limit observation (HTTP 429, throttle, or
SDK-surfaced retry-after event) writes one `api_events` row with
`event_type='rate_limit'`. The row records `provider`, `model`,
`timestamp`, and `retry_count` (when the SDK reports it); `input_tokens`
/ `output_tokens` / `cost_usd` are typically zero for these rows since
the request did not complete.

`QueryRateLimitEvents(window time.Duration) -> []RateLimitBucket`
aggregates per day over the window. Each bucket:

| Field | Type | Description |
|-------|------|-------------|
| `Day` | time.Time | Calendar day (truncated to day) |
| `Count` | int | Rate-limit rows observed that day |

The CLI surface is `spire metrics` (default flag), which prints a single
line under the summary block:

```
Rate limits: N in last 24h
```

computed by summing the 24-hour window's buckets.

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

### Attribution model (phase-level, not turn-level)

Token and cost attribution stops at the **phase** granularity recorded
on `agent_runs` — `prompt_tokens`, `completion_tokens`, `cost_usd`, and
`cache_*_tokens` are summed over the whole run and aggregated into
`phase_cost_breakdown` per `(date, tower, formula_name, phase)`. There
is deliberately no per-turn cost table.

**Why not per-turn:** every consumer we ship (DORA, formula performance,
cost trend, per-bead trace, CLI `--phase`) answers questions at the
phase or run level. A turn-level table would multiply row volume by
~10–100× per run and would not change any dashboard we serve today, so
we pay the extra storage only when a consumer asks for it.

**If this decision ever flips**, the shape would be:

> `turn_events` table (one row per model turn) with `run_id` FK to
> `agent_runs`, `turn_index`, `input_tokens`, `output_tokens`,
> `cache_read_tokens`, `cache_write_tokens`, `cost_usd`, `tool_name`
> (if the turn ended in a tool_use), and `timestamp`. Populated from
> the OTel `api_events` stream, which already carries per-call tokens
> and cost. Aggregations stay in `phase_cost_breakdown`; the new table
> is additive.

Until a consumer asks for per-turn visibility, do not add the table.

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
| _(default)_ | `QuerySummary` + `QueryRateLimitEvents(24h)` + `QueryFormulaPerformance` | Overall stats + rate-limit line + formula comparison |
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
