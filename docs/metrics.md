# Spire Metrics

Spire treats agent performance the way engineering teams treat service reliability: measure everything, surface what matters, act on the signals.

When you summon wizards to work on an epic, you're managing a team. Like any team, you need to know: are they fast? Are they reliable? Where are the bottlenecks? Are they getting better over time?

## What gets measured

Every wizard run records a full lifecycle trace:

```
filed → queued → startup → working → review → merged
```

| Phase | What's happening | What it tells you |
|-------|-----------------|-------------------|
| **Queue** | Bead sitting in READY, waiting for a wizard | Do you need more capacity? (`spire summon`) |
| **Startup** | Clone repo, install deps, claim bead, build context | Is your image slow? Are deps heavy? |
| **Working** | Claude is reading, writing, testing | Are your beads right-sized? |
| **Review** | Sage reads diff + spec, calls Opus | Is review a bottleneck? |
| **Total** | End to end | The bill. |

These map directly to the columns on `spire board`: Ready → Working → Review → Merged.

## DORA metrics

The four DORA metrics — the industry standard for engineering team performance — fall out naturally:

| DORA metric | Spire equivalent | What to watch |
|-------------|-----------------|---------------|
| **Deployment frequency** | Beads merged per day | Are wizards shipping? |
| **Lead time for changes** | Filed → merged (queue + startup + working + review) | How fast does an idea become code? |
| **Change failure rate** | Failed runs / total runs | Are specs clear? Are beads well-scoped? |
| **Time to restore** | Failed → next success on same bead | How quickly does the tower recover? |

## Stale and shutdown

Two thresholds in [`spire.yaml`](../spire.yaml), one config, two behaviors:

```yaml
agent:
  stale: 10m      # warning — wizard exceeded guidelines
  timeout: 15m    # fatal — tower kills the pod
```

Track compliance to understand if your beads are right-sized. Too many shutdowns means beads are too large. The steward enforces these thresholds: stale triggers a warning, timeout kills the agent.

## Where the data lives

- **Schema**: [`migrations/agent_runs.sql`](../migrations/agent_runs.sql) — the `agent_runs` table definition
- **Recorder**: [`pkg/metrics/recorder.go`](../pkg/metrics/recorder.go) — the `AgentRun` struct and `Record()` function
- **Wizard timestamps**: [`agent-entrypoint.sh`](../agent-entrypoint.sh) — captures `STARTED_AT`, `CLAUDE_STARTED_AT`, writes `result.json`
- **Review timestamps**: wizard review phase — `recordRun()` with review verdict timing
- **Config**: [`spire.yaml`](../spire.yaml) — `agent.stale`, `agent.timeout`, `agent.max-turns`
- **Defaults**: [`pkg/repoconfig/repoconfig.go`](../pkg/repoconfig/repoconfig.go) — `applyDefaults()`

## Querying

```bash
spire metrics              # quick summary
spire metrics --bead <id>  # per-bead breakdown
spire metrics --model      # cost by model
```

Or query directly:

```bash
bd sql "SELECT avg(startup_seconds), avg(working_seconds), avg(review_seconds) FROM agent_runs WHERE result='success'"
```

## Context-enriched queries

Each agent run now records the formula, branch, bead type, tower, and wave it belonged to. This unlocks slicing by any of those dimensions.

### Formula tracking — success rate by formula

```sql
SELECT formula_name, formula_version,
       COUNT(*) AS runs,
       SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) AS ok,
       ROUND(100.0 * SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) / COUNT(*), 1) AS success_pct
FROM agent_runs
WHERE formula_name IS NOT NULL
GROUP BY formula_name, formula_version
ORDER BY success_pct ASC;
```

### Bead type reliability — failure rate by issue type

```sql
SELECT bead_type,
       COUNT(*) AS runs,
       ROUND(100.0 * SUM(CASE WHEN result != 'success' THEN 1 ELSE 0 END) / COUNT(*), 1) AS fail_pct,
       ROUND(AVG(duration_seconds), 0) AS avg_duration_s
FROM agent_runs
WHERE bead_type IS NOT NULL
GROUP BY bead_type
ORDER BY fail_pct DESC;
```

### Tower cost allocation

```sql
SELECT tower,
       COUNT(*) AS runs,
       ROUND(SUM(cost_usd), 2) AS total_cost,
       ROUND(AVG(cost_usd), 4) AS avg_cost
FROM agent_runs
WHERE tower IS NOT NULL
GROUP BY tower
ORDER BY total_cost DESC;
```

### Audit trail — full context for a specific bead

```sql
SELECT id, phase, formula_name, formula_version, branch, commit_sha,
       bead_type, tower, wave_index, result, cost_usd, duration_seconds,
       started_at
FROM agent_runs
WHERE bead_id = 'spi-xxxx'
ORDER BY started_at;
```

## What to watch

| Signal | Meaning | Action |
|--------|---------|--------|
| Startup > 60s | Image or deps are slow | Pre-bake deps in Docker image |
| Queue > 5min | Not enough wizards | `spire summon` more capacity |
| Review > 2min | Sage is bottlenecked | Check Opus context size |
| Failure rate > 30% | Specs or beads are unclear | Improve specs, decompose beads |
| Shutdowns > 10% | Beads too large for timeout | Smaller beads, or increase timeout |
| Review rounds > 2 | Wizard and spec are misaligned | Better specs, or review the spec itself |

## The bigger picture

Engineering is changing. The engineer's role is shifting from implementer to technical director — someone who writes specs, reviews output, and steers a team of agents. Metrics are how you know if your team is performing.

Spire gives you the same visibility into your agent team that platform engineering gives you into your services. Not because metrics are inherently valuable, but because you can't improve what you can't see.
