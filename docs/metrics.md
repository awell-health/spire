# Spire Metrics

Every wizard and artificer run is recorded in the `agent_runs` Dolt table. This data powers `spire metrics` and enables DORA-style engineering analytics across your agent fleet.

## The wizard lifecycle — what gets measured

```
filed ──→ assigned ──→ startup ──→ working ──→ review ──→ merged
  │          │           │           │           │          │
  │          │           │           │           │          └ merge time (artificer)
  │          │           │           │           └ review_seconds
  │          │           │           └ working_seconds
  │          │           └ startup_seconds
  │          └ queue_seconds
  └ bead created_at
```

| Metric | What it measures | Source |
|--------|-----------------|--------|
| `queue_seconds` | Time a bead sat in READY waiting for a wizard | `bead.created_at` → `agent_run.started_at` |
| `startup_seconds` | Pod start → Claude start (clone, install, claim, focus) | `agent-entrypoint.sh` timestamps |
| `working_seconds` | Claude start → Claude done (the actual LLM work) | `agent-entrypoint.sh` timestamps |
| `review_seconds` | Branch pushed → artificer verdict | Artificer cycle timestamps |
| `duration_seconds` | Pod start → pod exit (total wall time) | `started_at` → `completed_at` |

## Time splits

The wizard entrypoint captures three timestamps:
- `STARTED_AT` — when the pod starts (set at script start)
- `CLAUDE_STARTED_AT` — right before the agent command runs (after all setup)
- `completed_at` — when the pod exits

From these:
- `startup_seconds = CLAUDE_STARTED_AT - STARTED_AT`
- `working_seconds = completed_at - CLAUDE_STARTED_AT`
- `total_seconds = completed_at - STARTED_AT`

The artificer captures:
- `review_seconds` — time from detecting new commits to delivering a verdict

## Querying metrics

```bash
# Quick summary
spire metrics

# Per-bead breakdown
spire metrics --bead spi-7v2

# Model costs
spire metrics --model
```

### Raw SQL queries via `bd sql`

```sql
-- Average time breakdown for successful wizard runs
SELECT
  avg(startup_seconds) as avg_startup,
  avg(working_seconds) as avg_working,
  avg(review_seconds) as avg_review,
  avg(duration_seconds) as avg_total
FROM agent_runs
WHERE result = 'success' AND role = 'wizard';

-- Startup overhead as percentage of total time
SELECT
  avg(startup_seconds * 100.0 / duration_seconds) as startup_pct
FROM agent_runs
WHERE duration_seconds > 0 AND role = 'wizard';

-- Queue time: how long beads wait before a wizard picks them up
SELECT
  avg(queue_seconds) as avg_queue,
  max(queue_seconds) as max_queue
FROM agent_runs
WHERE queue_seconds > 0;

-- Review time: how long the artificer takes per review
SELECT
  avg(review_seconds) as avg_review,
  avg(review_rounds) as avg_rounds
FROM agent_runs
WHERE role = 'artificer' AND review_seconds > 0;
```

## DORA metrics

The `agent_runs` table maps directly to DORA:

### Deployment frequency

How many beads merge per day?

```sql
SELECT
  DATE(completed_at) as day,
  count(*) as merged
FROM agent_runs
WHERE result = 'success'
GROUP BY DATE(completed_at)
ORDER BY day DESC;
```

### Lead time for changes

How long from bead filed to bead merged?

```sql
-- Requires joining with beads data
-- queue_seconds + duration_seconds approximates this
SELECT
  avg(queue_seconds + duration_seconds) as avg_lead_time_seconds
FROM agent_runs
WHERE result = 'success' AND queue_seconds > 0;
```

### Change failure rate

What percentage of wizard runs fail?

```sql
SELECT
  count(*) as total,
  sum(CASE WHEN result = 'success' THEN 1 ELSE 0 END) as succeeded,
  sum(CASE WHEN result != 'success' THEN 1 ELSE 0 END) as failed,
  round(sum(CASE WHEN result != 'success' THEN 1 ELSE 0 END) * 100.0 / count(*), 1) as failure_rate_pct
FROM agent_runs
WHERE role = 'wizard';
```

### Time to restore

How quickly does a failed bead get re-attempted and succeed?

```sql
SELECT
  a.bead_id,
  a.completed_at as failed_at,
  b.completed_at as restored_at,
  b.duration_seconds as restore_duration
FROM agent_runs a
JOIN agent_runs b ON a.bead_id = b.bead_id
WHERE a.result != 'success'
  AND b.result = 'success'
  AND b.started_at > a.completed_at
ORDER BY a.completed_at DESC;
```

## Stale and shutdown tracking

Two thresholds from `spire.yaml`:

```yaml
agent:
  stale: 10m      # warning — wizard exceeded guidelines
  timeout: 15m    # fatal — tower kills the pod
```

The steward checks in_progress beads each cycle:
- **Stale** (elapsed > `agent.stale`): logs a WARNING, creates an alert bead
- **Shutdown** (elapsed > `agent.timeout`): kills the pod, logs SHUTDOWN

Track compliance:

```sql
-- How many runs stayed within guidelines vs needed shutdown?
SELECT
  sum(CASE WHEN duration_seconds <= 600 THEN 1 ELSE 0 END) as within_guideline,
  sum(CASE WHEN duration_seconds > 600 AND duration_seconds <= 900 THEN 1 ELSE 0 END) as overtime_warning,
  sum(CASE WHEN duration_seconds > 900 THEN 1 ELSE 0 END) as killed,
  count(*) as total
FROM agent_runs
WHERE role = 'wizard';
```

## agent_runs table schema

```sql
CREATE TABLE agent_runs (
    id VARCHAR(32) PRIMARY KEY,
    bead_id VARCHAR(64) NOT NULL,
    epic_id VARCHAR(64),
    agent_name VARCHAR(128),
    model VARCHAR(64) NOT NULL,
    role VARCHAR(16) NOT NULL,         -- 'wizard' or 'artificer'

    -- Time splits
    duration_seconds INT,              -- total wall time
    startup_seconds INT,               -- pod start → claude start
    working_seconds INT,               -- claude start → claude done
    queue_seconds INT,                 -- bead filed → wizard assigned
    review_seconds INT,                -- branch pushed → artificer verdict

    -- Token usage
    context_tokens_in INT,
    context_tokens_out INT,
    total_tokens INT,
    turns INT,

    -- Result
    result VARCHAR(32) NOT NULL,       -- success, test_failure, timeout, error, stopped

    -- Review (artificer)
    review_rounds INT DEFAULT 0,
    artificer_verdict VARCHAR(32),     -- approve, request_changes, reject

    -- Code
    files_changed INT,
    lines_added INT,
    lines_removed INT,
    tests_added INT,
    tests_passed BOOLEAN,

    -- Timestamps
    started_at DATETIME NOT NULL,
    completed_at DATETIME,

    -- Indexes
    INDEX idx_bead (bead_id),
    INDEX idx_result (result),
    INDEX idx_model (model)
);
```

## What to watch

| Signal | Query | Action |
|--------|-------|--------|
| High startup time | `avg(startup_seconds) > 60` | Optimize image, pre-bake deps |
| High queue time | `avg(queue_seconds) > 300` | Summon more wizards |
| High review time | `avg(review_seconds) > 120` | Check artificer model/context |
| Low success rate | `failure_rate > 30%` | Improve specs, decompose beads |
| Many shutdowns | `killed > 10%` | Beads too large, reduce scope |
| High review rounds | `avg(review_rounds) > 2` | Spec quality issue |
