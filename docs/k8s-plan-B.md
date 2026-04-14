# Production Kubernetes: Plan B

> Making Spire production-ready for a team of 50 with 10 repos and 50 concurrent agents.
>
> Design bead: `spi-885rv`

---

## 1. Infrastructure Foundation

### 1.1 Pod Architecture (Final State)

Five pod types in the `spire` namespace. Nothing else.

| Pod | Kind | Replicas | Persistence |
|-----|------|----------|-------------|
| **steward** | Deployment (leader-elected) | 1 active + 1 standby | PVC for daemon state |
| **dolt** | StatefulSet | 1 | PVC for database |
| **clickhouse** | StatefulSet | 1 | PVC for analytics |
| **syncer** | CronJob | 1 (periodic) | None |
| **agent** | Bare Pod (one-shot) | 0-N (steward-controlled) | EmptyDir only |

The operator is not a separate deployment. It runs as a controller loop
inside the steward pod. One process manages: the daemon cycle, the
steward assignment loop, the OTel receiver, the k8s controller
reconciliation, and the DuckDB-to-ClickHouse writer. This eliminates
three race conditions that exist today between separate operator and
steward processes competing to update the same SpireWorkload CRs.

### 1.2 Steward Pod Internals

```
steward pod
+--------------------------------------------------+
| goroutines:                                      |
|   1. daemon cycle (sync, linear, webhooks)       |
|   2. steward cycle (assign, health, review route)|
|   3. OTel OTLP receiver (:4317)                 |
|   4. k8s controller-runtime manager             |
|      - BeadWatcher reconciler                    |
|      - WorkloadAssigner reconciler               |
|      - AgentMonitor reconciler                   |
|   5. ClickHouse writer (batched)                 |
|   6. health/metrics HTTP server (:8080)          |
+--------------------------------------------------+
| sidecar: steward-sidecar (LLM message processor)|
+--------------------------------------------------+
```

The steward exposes a single HTTP server for:
- `/healthz` and `/readyz` (k8s probes)
- `/metrics` (Prometheus-format, scraped by existing monitoring)
- `/api/v1/board` (JSON board state, consumed by web dashboard)
- `/api/v1/agents` (JSON agent status)
- `/api/v1/metrics/dora` (DORA metrics JSON)
- `/api/v1/events` (SSE stream of bead state changes)

These API endpoints are read-only. No mutations through HTTP. Mutations
flow through Dolt (bead graph) and k8s API (CRDs). The HTTP surface
exists purely for observation -- dashboards, Slack bots, and the CLI
`spire board --remote` mode.

### 1.3 ClickHouse Replaces DuckDB in Cluster

DuckDB is single-process and file-locked. In a cluster with N concurrent
agent pods each emitting OTel telemetry, a shared analytics store is
required. ClickHouse is the replacement because:

1. Same columnar storage model, same SQL dialect patterns
2. Multi-writer: agent pods send to the steward's OTLP receiver, which
   batches writes to ClickHouse
3. Server-based: survives pod restarts without lock recovery
4. MergeTree engine handles time-series data with automatic compaction

Schema migration path: the existing `pkg/olap/schema.go` DDL statements
translate directly. Table names stay identical. The `pkg/olap` package
gets a `Backend` interface with two implementations:

```go
type Backend interface {
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
```

- `DuckBackend` -- used locally, same as today
- `ClickHouseBackend` -- used in cluster, connects to ClickHouse StatefulSet

The ETL, view refresh, and query code call `Backend` methods. Zero query
changes. The switch is at startup: `spire up` locally opens DuckDB;
the steward pod reads `OLAP_BACKEND=clickhouse` and connects via
`clickhouse://spire-clickhouse.spire.svc:9000`.

### 1.4 Dolt StatefulSet

The current Deployment with PVC works but has a weakness: Recreate
strategy means downtime during rollouts. Switch to StatefulSet with a
proper VolumeClaimTemplate:

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: spire-dolt
  namespace: spire
spec:
  replicas: 1
  serviceName: spire-dolt
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: 20Gi
```

20Gi initial size handles ~100k beads (Dolt is compact). Monitor with
a `PersistentVolumeClaimResize` alert at 80%.

Connection pooling: agent pods connect to `spire-dolt.spire.svc:3306`
via MySQL protocol. At 50 concurrent agents, each holding 1-2
connections, the Dolt server needs `max_connections: 150`. The current
config has 100; bump to 200 with a note in the Helm values.

### 1.5 Syncer

Change from CronJob to a sidecar inside the steward pod. Rationale:
the syncer needs the same Dolt credentials, the same .beads config,
and runs on the same cadence as the daemon cycle. Making it a sidecar
eliminates a separate pod, a separate RBAC config, and a separate
failure domain.

The syncer goroutine inside the steward already runs `dolt pull` then
`dolt push` on each daemon cycle. In cluster mode, this goroutine
talks to the Dolt StatefulSet's PVC via a network connection (not local
filesystem), so the push/pull commands run from the steward pod using
`dolt sql` commands over the MySQL protocol to trigger server-side
remotes.

Alternative: keep the syncer as a separate CronJob if the team wants
sync decoupled from steward health. Document both modes in the Helm chart
as `syncer.mode: sidecar | cronjob`.

### 1.6 Agent Pod Lifecycle (Refined)

Agent pods are ephemeral. One pod per bead execution. The steward
creates the pod; the pod runs the formula to completion and exits.

**What changes from the current implementation:**

1. **No more beads-seed ConfigMap.** The agent pod connects to the Dolt
   server directly via MySQL protocol. `pkg/store` already supports
   server mode with `DOLT_HOST`/`DOLT_PORT`. The initContainer that
   copies metadata.json is eliminated. This removes a bootstrapping
   fragility (stale ConfigMap) and a manual maintenance burden.

2. **Git clone caching via PVC.** Agent pods clone the target repo on
   every run. For a 50-agent cluster, that is 50 clones of the same
   repo. Solution: a shared ReadOnlyMany PVC per repo that holds a bare
   clone, updated by the syncer. Agent pods mount this as a base for
   `git clone --reference`. Clone time drops from minutes to seconds.

   ```yaml
   volumes:
     - name: repo-cache
       persistentVolumeClaim:
         claimName: repo-cache-web  # one per prefix
         readOnly: true
     - name: workspace
       emptyDir: {}
   ```

   The syncer maintains these caches: `git fetch --prune` on each cycle.

3. **OTel resource attributes injected at pod creation.** The steward
   sets `OTEL_RESOURCE_ATTRIBUTES` on each agent pod:

   ```
   bead.id=web-abc,agent.name=wizard-3,tower=my-team,step=implement
   ```

   This means every OTel span and log from the agent pod carries bead
   context. The existing `pkg/otel/receiver.go` `extractResourceAttrs`
   function already reads these. No receiver changes needed.

4. **Pod priority classes.** P0 beads get `system-cluster-critical`
   priority class. P1-P2 get default. P3-P4 get `low-priority`. This
   lets the scheduler preempt P4 agent pods when P0 work arrives.

### 1.7 Secrets Management

Production secrets architecture:

| Secret | Contents | Consumed by |
|--------|----------|-------------|
| `spire-anthropic-default` | `ANTHROPIC_API_KEY` (Sonnet-tier) | agent pods (tasks, bugs, chores) |
| `spire-anthropic-heavy` | `ANTHROPIC_API_KEY` (Opus-tier) | agent pods (epics, reviews, arbiters) |
| `spire-github` | `GITHUB_TOKEN` or SSH key | agent pods (clone/push) |
| `spire-dolthub` | DoltHub credentials + JWK | steward pod, syncer |
| `spire-clickhouse` | ClickHouse password | steward pod |

The SpireConfig CR maps token names to secrets:

```yaml
spec:
  tokens:
    default:
      secret: spire-anthropic-default
      key: ANTHROPIC_API_KEY
    heavy:
      secret: spire-anthropic-heavy
      key: ANTHROPIC_API_KEY
  routing:
    - match: { type: epic }
      token: heavy
    - match: { type: review }
      token: heavy
    - match: { phase: arbiter }
      token: heavy
```

The steward reads SpireConfig at pod creation time and injects the
appropriate `envFrom.secretKeyRef` into each agent pod spec. This
already works in `agent_monitor.go` -- the routing rules add
type-based selection.

For teams using external secret managers (Vault, AWS Secrets Manager),
the Helm chart supports `ExternalSecret` CRDs from the
external-secrets-operator. The chart renders ExternalSecret objects
that sync into the k8s Secret objects above.

### 1.8 Disaster Recovery

**Dolt database:** The syncer pushes to DoltHub on every cycle. DoltHub
is the off-cluster backup. Recovery: delete the PVC, restart the
StatefulSet, the init script clones from DoltHub. RPO = syncer interval
(default 2 minutes). RTO = clone time + pod restart (~3 minutes).

**ClickHouse analytics:** ClickHouse data is reconstructable. The
source of truth is Dolt's `agent_runs` table plus the OTel events
stream. If ClickHouse PVC is lost:
1. Delete and recreate the StatefulSet
2. ETL re-syncs from Dolt `agent_runs` (full backfill)
3. OTel events from before the loss are gone -- they are operational
   telemetry, not business-critical state. Accept this.

**Agent pod failure:** Pods are ephemeral and one-shot. If a pod dies
mid-execution, the bead stays in `in_progress`. The steward's stale
detection (configurable, default 15m) marks it as stale. The health
check creates a recovery bead. The recovery formula collects context,
decides whether to reset and retry or escalate to human. This already
works -- `recovery-default.formula.toml` handles it.

**Steward pod failure:** The standby pod (leader election via
controller-runtime's `LeaderElection: true`) takes over. In-flight
agents are unaffected -- they are independent processes. The new steward
reads Dolt state and picks up where the old one left off. Assignment
state is in Dolt (bead status), not in steward memory.

---

## 2. Observability and the Human Experience

### 2.1 Three Audiences, Three Interfaces

**The VP of Engineering** needs: weekly throughput trends, cost
attribution, reliability metrics, and confidence that agents are not
producing regressions. They never look at individual beads.

**The tech lead** needs: real-time board of what agents are doing right
now, the ability to steer or stop specific agents, review approval
queues, and formula performance data to tune the pipeline.

**The PM** needs: a way to file work in plain English, track which
features shipped, see estimated completion timelines, and never think
about git, pods, or formulas.

### 2.2 Web Dashboard (New Component)

The steward's HTTP API endpoints (`/api/v1/*`) power a static web
dashboard. This is a separate repo (`spire-dashboard`) deploying as a
static site (Vercel, Cloudflare Pages, or nginx pod in the cluster).

**Dashboard views:**

1. **Board** -- Live bead board grouped by status (Ready, Working,
   Review, Merged, Failed). Filterable by repo prefix, epic, priority.
   Real-time updates via SSE from steward `/api/v1/events`.

2. **Agent Fleet** -- Grid of active agent pods. Each card shows: bead
   being worked, current formula step, elapsed time, token spend so far,
   last tool call. Color-coded: green (healthy), yellow (stale warning),
   red (timed out). Click to see the agent's OTel trace waterfall.

3. **DORA Dashboard** -- Four DORA metrics, weekly trends, filterable
   by repo. Data from ClickHouse `weekly_merge_stats`. Thresholds
   configurable (e.g., "lead time > 30min is yellow, > 60min is red").

4. **Cost Center** -- Daily/weekly cost by repo, by formula, by model.
   Breakdown: plan vs implement vs review spend. From ClickHouse
   `phase_cost_breakdown` and `api_events`.

5. **Epic Timeline** -- Gantt-style view for epics. Each subtask is a
   bar: filed -> started -> completed. Critical path highlighted.
   Dependencies shown as arrows. Data from Dolt bead graph.

6. **Formula Lab** -- Performance comparison across formula versions.
   Success rate, cost, duration, review rounds. Side-by-side when a
   team publishes a new version. From ClickHouse `daily_formula_stats`.

7. **Filing Queue** -- Where PMs live. Simple form: title, description,
   priority, type. Shows filed work and its status. No technical
   details -- just "Filed -> Agent Working -> Under Review -> Shipped"
   pipeline view.

**Authentication:** The dashboard reads from the steward API, which is
cluster-internal by default. For external access:
- Option A: Ingress with OAuth2-proxy (GitHub org membership)
- Option B: Tailscale/Cloudflare Tunnel for zero-trust access
- Option C: `kubectl port-forward` for development

### 2.3 Slack Integration

A Slack bot that posts to a channel per tower. Three notification types:

1. **Completion notifications:** "Agent merged `web-x2mk.3` (Add OAuth2 
   provider) into main. 247 lines across 8 files. Cost: $0.42. 
   Review: 1 round, approved."

2. **Attention-required alerts:** "Bead `api-b7d0` needs human approval
   (hooked step: `human.approve`). [Approve] [Reject] [View]" -- with
   Slack interactive buttons that call the steward's action endpoint.

3. **Daily digest:** Posted at 9am team-local. "Yesterday: 12 beads
   merged, 2 failed (1 recovered automatically), 3 waiting for review.
   Top cost: epic spi-abc ($14.20). Today's queue: 8 ready beads."

The Slack bot is a separate process (`cmd/spire-slack/`) that consumes
the steward SSE stream and the ClickHouse query APIs. It is not part of
the steward pod -- it deploys as its own Deployment or runs as a
serverless function.

### 2.4 OTel Pipeline at Scale

At 50 concurrent agents, each emitting ~100 tool events per minute
and ~500 spans per minute during active work:
- Tool events: ~5,000/min peak
- Spans: ~25,000/min peak
- API events: ~500/min (one per LLM call)

The steward's OTLP receiver handles this by batching writes to
ClickHouse. The existing `writeFn` pattern in `pkg/otel/receiver.go`
already supports batched writes. The ClickHouse writer accumulates
events in a buffer and flushes every 5 seconds or when the buffer
reaches 10,000 events, whichever comes first.

```go
type ClickHouseWriter struct {
    mu      sync.Mutex
    buf     []ToolEvent
    spanBuf []ToolSpan
    apiBuf  []APIEvent
    ticker  *time.Ticker
    ch      *sql.DB
}
```

ClickHouse's `Buffer` table engine can also absorb bursts -- configure
the target tables with `Buffer(target_table, 16, 10, 100, 10000, ...)`.

### 2.5 Trace Waterfall View

The existing `tool_spans` table in ClickHouse stores full hierarchical
spans. The web dashboard renders these as a waterfall:

```
[wizard-3: web-x2mk.3]  ─────────────────────────── 8m 22s
  [plan]                 ──────── 1m 04s
    LLM: plan.generate   ────── 58s  ($0.08)
  [implement]            ──────────────── 4m 12s
    Bash: pnpm install   ── 12s
    Read: src/auth.ts    ─ 2s
    Edit: src/auth.ts    ── 8s
    Read: src/auth.test  ─ 1s
    Write: src/auth.test ── 4s
    Bash: pnpm test      ──── 34s
    LLM: implement       ──────────── 3m 11s  ($0.18)
  [review]               ───────── 2m 48s
    LLM: sage-review     ────────── 2m 44s  ($0.12)
  [merge]                ── 18s
    Bash: git merge      ─ 3s
    Bash: git push       ── 8s
```

This view answers: "Why did this bead take 8 minutes? Where did the
time go? How much was LLM vs build vs test?"

The `spire trace` CLI command already renders a simplified version of
this. The web dashboard renders the full interactive waterfall with
zoom, span detail panel, and cost annotations.

---

## 3. Agent Intelligence and Learning

### 3.1 The Recovery Knowledge Base

After 6 months of operation with thousands of completed beads, the
`recovery-default` formula has been triggered hundreds of times. Each
recovery bead records structured metadata:

- `failure_class`: empty-implement, build-failure, test-failure, merge-conflict, timeout, ...
- `parent_bead`: the bead that failed
- `chosen_action`: reset-hard, retry-with-context, escalate, fix-specific-file, ...
- `outcome`: resolved, escalated, recurring
- Prior learning references (which old recovery beads informed this decision)

This data is a goldmine. Build a **recovery recommender**:

```sql
-- For a given failure class and repo, what action has the highest
-- success rate?
SELECT
    chosen_action,
    COUNT(*) AS attempts,
    SUM(CASE WHEN outcome = 'resolved' THEN 1 ELSE 0 END) AS successes,
    successes::FLOAT / attempts AS success_rate
FROM recovery_metadata
WHERE failure_class = 'build-failure'
  AND repo = 'web'
  AND created_at >= now() - INTERVAL 90 DAY
GROUP BY chosen_action
ORDER BY success_rate DESC
```

The recovery formula's `decide` step already receives prior learnings
from `store.GetPriorLearnings()`. Enhance this with ranked historical
success rates per failure class. The model receives: "For build
failures in the web repo, `retry-with-context` resolved 78% of cases,
`reset-hard` resolved 45%, `escalate` resolved 12%."

This is ZFC-compliant: the system gathers context (success rates) and
presents it to the model. The model decides. No local heuristic ranking.

### 3.2 Formula Evolution Tracking

The `daily_formula_stats` table tracks success rate, cost, and review
rounds per formula version. When a team publishes a new formula version
via `spire formula publish`, the system can compare:

```
bug-default v3.1: success 82%, avg cost $0.31, avg 1.2 review rounds
bug-default v3.2: success 89%, avg cost $0.28, avg 1.0 review rounds
```

Build a **formula health check** that runs weekly (steward daemon duty):

1. Query `daily_formula_stats` for the last 7 days vs previous 7 days
2. If success rate dropped > 10%, or cost increased > 25%, or review
   rounds increased > 0.5 avg: create an alert bead with type
   `formula-regression`
3. The alert bead includes the statistics comparison and the diff
   between formula versions

This gives teams a feedback loop: publish formula changes, measure
impact, roll back if performance degrades.

### 3.3 Cross-Repo Context

At 10 repos in a tower, agents working on `web-` beads don't know what
recently changed in `api-`. But they should -- a breaking API change
in `api-` directly affects `web-`.

**Recent-changes context injection.** When the steward assigns a bead,
it queries Dolt for beads merged in the last 24 hours across all repos
in the tower. This "recent tower context" is written to the bead as a
comment:

```
[tower-context] Recent changes across the tower (last 24h):
- api-x3f2: "Changed auth endpoint response format" (merged 3h ago)
- api-x3f3: "Added rate limiting to /users endpoint" (merged 1h ago)
- shared-a1b2: "Updated TypeScript types for auth module" (merged 5h ago)
```

The wizard reads this context during the plan step. If the bead is
"Add OAuth2 to the web app" and the api repo just changed the auth
endpoint format, the wizard plans accordingly.

This is a steward responsibility (tower-level coordination), not an
executor responsibility (per-bead). The steward generates the context;
the executor reads it mechanically.

### 3.4 Trust Gradients

Not all formulas should auto-merge to main. Trust is earned through
track record. Implement a three-tier trust model:

**Tier 1: Supervised (default for new repos)**
- `human.approve` gate before merge
- All agent PRs require human review
- Recovery always escalates (never auto-resolves)

**Tier 2: Semi-autonomous (earned after 50 successful merges)**
- P3/P4 beads auto-merge if sage approves
- P0-P2 beads still require `human.approve`
- Recovery can auto-resolve known failure classes

**Tier 3: Autonomous (earned after 200 successful merges with <5% failure rate)**
- All beads auto-merge if sage approves
- `human.approve` gates only for security-sensitive paths (configurable)
- Recovery can auto-resolve and auto-retry

Trust level is stored per-repo in the Dolt `repos` table:

```sql
ALTER TABLE repos ADD COLUMN trust_tier INT DEFAULT 1;
ALTER TABLE repos ADD COLUMN successful_merges INT DEFAULT 0;
ALTER TABLE repos ADD COLUMN total_merges INT DEFAULT 0;
```

The steward evaluates trust tier on each cycle. When a repo crosses
a threshold, it creates a notification bead: "Repo `web` promoted to
Tier 2 (52 successful merges, 96% success rate). P3/P4 beads will now
auto-merge."

The formula doesn't change. The `human.approve` step's condition
evaluates `vars.trust_tier`:

```toml
[steps.human-gate]
kind = "op"
action = "human.approve"
needs = ["review"]
[steps.human-gate.when]
all = [
  { left = "vars.trust_tier", op = "lt", right = "3" },
  { left = "vars.priority", op = "le", right = "2" },
]
```

### 3.5 Codebase Familiarity Index

Track which files agents have successfully modified and which they
struggle with. After thousands of beads, build a per-repo familiarity
map:

```sql
-- From agent_runs_olap + tool_events
SELECT
    file_path,
    COUNT(*) FILTER (WHERE result = 'success') AS successful_touches,
    COUNT(*) FILTER (WHERE result != 'success') AS failed_touches,
    successful_touches::FLOAT / NULLIF(successful_touches + failed_touches, 0) AS success_rate
FROM file_changes  -- new table, populated from OTel Edit events
WHERE repo = 'web'
GROUP BY file_path
ORDER BY failed_touches DESC
```

This answers: "Which files are hard for agents?" If `src/core/auth.ts`
has a 40% failure rate, the wizard's plan step gets extra context:
"WARNING: auth.ts has a 40% agent failure rate. Consider providing
detailed implementation hints or breaking the task into smaller pieces."

This requires a new ClickHouse table `file_changes` populated from
`Edit` tool events in the OTel stream. The receiver already captures
Edit events with file paths. Add a post-processor that extracts the
file path from the span attributes and writes it to `file_changes`.

### 3.6 Review Pattern Learning

After thousands of review cycles, extract patterns from sage feedback:

```sql
SELECT
    verdict,
    finding_category,  -- from structured review metadata
    COUNT(*) AS occurrences
FROM review_findings  -- new table
WHERE repo = 'web'
  AND created_at >= now() - INTERVAL 90 DAY
GROUP BY verdict, finding_category
ORDER BY occurrences DESC
```

Common findings like "missing error handling", "no test coverage for
edge case", "import not cleaned up" become context for the implement
step. Before an apprentice starts coding, the wizard injects:

"The top 3 review findings in this repo are:
1. Missing error handling for async operations (found in 34% of reviews)
2. Incomplete test coverage for error paths (28%)
3. Unused imports left after refactoring (22%)

Address these proactively in your implementation."

This is gathered from the review bead metadata (already structured
via `spi-w00yn`: "store verdict and findings as structured metadata").
The steward queries ClickHouse and appends the context to the bead.

---

## 4. The Design-to-Execution Bridge

### 4.1 The PM Filing Experience

Today: PMs need to run `spire file` from a terminal. This is a
non-starter for non-engineers.

**Slack filing.** The Slack bot accepts natural language:

```
/spire Add dark mode support to the patient dashboard.
Priority: medium. This should respect the user's OS theme preference.
```

The bot (using Claude) parses this into a structured bead:
- Title: "Add dark mode support to patient dashboard"
- Description: "Implement dark mode... (expanded from PM's input)"
- Type: feature
- Priority: 2
- Prefix: `web-` (inferred from "patient dashboard" + repo descriptions)

The bot replies: "Filed as `web-a3f8` (priority P2, feature). It will
be picked up by the next available agent. [View in dashboard]"

**Web filing.** The dashboard's Filing Queue view has a form that
accepts plain English. Same Claude-backed parsing.

**Email filing (stretch).** Forward an email to `spire@team.domain`.
The email body becomes the bead description. Subject becomes title.
Useful for external stakeholder requests.

### 4.2 Design Bead Workflow at Scale

The `design -> plan -> implement -> review -> merge` flow is the
backbone. At scale, the design phase needs more structure.

**Design bead template.** When a PM files a design bead, they get a
structured form (Slack or web):

1. **Problem statement**: What is broken or missing?
2. **Desired outcome**: What does success look like?
3. **Constraints**: Performance budget? Backward compatibility?
   Security requirements?
4. **Affected areas**: Which repos? Which modules?
5. **Acceptance criteria**: Bulleted list of testable conditions.

This template is stored in the bead description. When the wizard's
`design-check` step validates the design, it checks for these sections.
If "Acceptance criteria" is empty, the wizard creates a `needs-human`
alert: "Design bead spi-abc is missing acceptance criteria."

**Design review routing.** Design beads can have a `reviewer` label
that routes them to a specific human. The steward creates a
notification when a design bead is filed. The reviewer approves or
requests changes through the dashboard/Slack. Only after approval does
the design bead close, unblocking the epic's plan step.

### 4.3 Spec Quality Scoring

Not all specs are equal. A one-line spec ("add dark mode") produces
worse agent outcomes than a detailed spec. Track this:

```sql
SELECT
    LENGTH(description) AS spec_length,
    result,
    review_rounds,
    cost_usd
FROM agent_runs_olap a
JOIN issues i ON a.bead_id = i.id
WHERE a.formula_name = 'task-default'
  AND a.started_at >= now() - INTERVAL 90 DAY
```

Correlation analysis (run monthly by the steward, reported in the
dashboard): "Beads with specs under 100 characters have a 45% success
rate. Beads with specs over 500 characters have an 82% success rate.
Beads with acceptance criteria have 91% success rate."

Surface this as a nudge when filing: "Your spec is 47 characters.
Beads with longer specs succeed 2x more often. Consider adding
acceptance criteria."

### 4.4 Epic Estimation

After enough epic completions, the system can estimate how long a new
epic will take:

```sql
SELECT
    subtask_count,
    AVG(total_duration_hours) AS avg_duration,
    PERCENTILE_CONT(0.9) WITHIN GROUP (ORDER BY total_duration_hours) AS p90_duration
FROM epic_completions  -- materialized view
GROUP BY subtask_count
```

When a wizard plans an epic and creates N subtasks, the steward annotates
the epic: "Based on 47 completed epics with similar scope (~8 subtasks),
estimated completion: 4-6 hours wall clock (P50-P90). Estimated cost:
$8-$14."

This gives PMs a rough timeline without requiring manual estimation.
The estimates improve as the corpus grows.

### 4.5 Dependency-Aware Scheduling

The steward already reads the dependency graph (`bd ready --json` finds
beads with no open blockers). Enhance this for cross-repo awareness:

When the steward assigns work, it considers:
1. **Critical path priority boost.** If bead X is on the critical path
   of a P0 epic, and X has 3 downstream dependents waiting, X gets a
   scheduling boost regardless of its individual priority.

2. **Same-repo batching.** If 5 beads from the `web-` prefix are all
   ready, assign them to agents in a batch. This amortizes the git
   clone cost (agents share the repo cache PVC) and lets the staging
   merge handle integration.

3. **Cross-repo dependency notification.** If `web-abc.3` depends on
   `api-def.1`, and `api-def.1` just merged, the steward immediately
   marks `web-abc.3` as ready and prioritizes it.

This is steward-level policy, not formula logic. Implemented in
`pkg/steward/steward.go`'s `TowerCycle`.

---

## 5. What Breaks at Scale (and How to Fix It)

### 5.1 Dolt Connection Exhaustion

**Problem:** 50 concurrent agent pods + steward + syncer = 52+ Dolt
connections. Each agent may hold 2-3 connections (bd, store API, direct
queries).

**Fix:** Connection pooling at the agent level. The `pkg/store` package
opens one connection per process. Agent pods should use a connection
pool with max 3 connections and idle timeout of 30s. Additionally, the
Dolt config should set `max_connections: 200` with `read_timeout_millis`
and `write_timeout_millis` set to prevent hung connections.

**Monitoring:** Expose `dolt_connections_active` as a Prometheus metric
from the steward (queried via `SHOW PROCESSLIST`). Alert at 80% of max.

### 5.2 Git Push Contention

**Problem:** Multiple agents merging to `main` simultaneously. Even
with squash merges, git push is a serial operation.

**Fix:** The wizard's `git.merge_to_main` action already uses
fast-forward-only merges. But at 50 agents, push contention is real.
Implement a **merge queue** in the steward:

1. When a wizard's review approves, the wizard sends a merge request
   to the steward (via bead status change to `merge-pending`)
2. The steward's merge worker processes requests serially per repo:
   fetch, rebase, test, push
3. If rebase fails (conflict), create a recovery bead
4. If test fails post-rebase, reject the merge and re-open the bead

This is better than each agent pod trying to push independently because:
- Conflicts are detected before push, not after
- Test-after-rebase catches integration failures
- The merge worker can batch multiple non-conflicting merges

The merge queue runs as a goroutine in the steward. State is tracked
in a new `merge_queue` table in Dolt:

```sql
CREATE TABLE merge_queue (
    bead_id    VARCHAR PRIMARY KEY,
    repo       VARCHAR NOT NULL,
    branch     VARCHAR NOT NULL,
    status     VARCHAR NOT NULL,  -- pending, rebasing, testing, pushing, done, failed
    queued_at  TIMESTAMP,
    started_at TIMESTAMP,
    completed_at TIMESTAMP
);
```

### 5.3 ClickHouse Write Amplification

**Problem:** At peak, 25,000 spans/min arriving at the OTLP receiver.
Writing each individually would overwhelm ClickHouse.

**Fix:** Already addressed by the batched writer (section 2.4). But
add back-pressure: if the buffer exceeds 100,000 events (20 seconds
at peak), the OTLP receiver starts returning `RESOURCE_EXHAUSTED` to
agent pods. Agent pods buffer locally and retry. Claude Code and Codex
both have OTLP retry logic built in.

### 5.4 Stale Bead Accumulation

**Problem:** After 6 months, the bead graph has 5,000+ closed beads.
Queries slow down. The board becomes unwieldy.

**Fix:** Dolt's git-like branching helps here. Monthly, the syncer
creates a "archive" branch, moves closed beads older than 90 days to
a separate `archived_issues` table, and commits. The main branch stays
lean. Archived beads are still queryable via `SELECT * FROM
dolt_history` or by checking out the archive branch.

The Dolt database size with 5,000 beads is still small (~50MB). This
is not an urgent problem. But the board query performance matters: add
a `WHERE status IN ('open', 'in_progress', 'deferred')` filter to the
default board query and index on status.

### 5.5 Formula Deadlocks

**Problem:** An epic's subtask depends on another subtask that depends
on the first (circular dependency). The steward never marks either
as ready.

**Fix:** The steward's `GetReadyWork` already uses topological sort,
which detects cycles. Enhance it to: when a cycle is detected, create
a `needs-human` alert bead listing the cycle. Don't silently drop the
work.

### 5.6 Multi-Repo Staging Conflicts

**Problem:** Epic `spi-abc` has subtasks in `web-` and `api-`. The
`web-` subtask modifies a shared TypeScript types file. The `api-`
subtask modifies the same file. Both merge to their respective repos'
main branches. But the shared types file is in a monorepo -- or worse,
in a separate `shared-` repo that both depend on.

**Fix:** This is the hardest problem. Three approaches, in order of
complexity:

1. **Dependency ordering.** File `shared-` subtasks first. Make `web-`
   and `api-` subtasks depend on `shared-` completion. The dependency
   graph enforces serialization.

2. **Cross-repo staging.** For monorepos, the epic wizard creates a
   single staging branch that spans all subtasks. This already works
   for single-repo epics. Extend `subgraph-implement` to handle
   multiple repos by creating one staging branch per repo and a
   coordination step that verifies cross-repo compatibility.

3. **Integration test step.** Add a `verify.cross-repo` action that
   clones all affected repos at their staged state and runs an
   integration test suite. This is a new formula step, not a formula
   change.

Start with approach 1 (dependency ordering). It is the simplest and
covers 90% of cases. File it as a steward enhancement.

---

## 6. Helm Chart Design

### 6.1 Values Structure

```yaml
# values.yaml
tower:
  name: my-team

dolt:
  image: dolthub/dolt-sql-server:latest
  storage: 20Gi
  maxConnections: 200
  resources:
    requests: { cpu: 200m, memory: 512Mi }
    limits: { cpu: "1", memory: 1Gi }
  dolthub:
    remote: org/spire-db  # empty = no DoltHub sync
    credentialsSecret: spire-dolthub

clickhouse:
  enabled: true
  image: clickhouse/clickhouse-server:24.3
  storage: 50Gi
  resources:
    requests: { cpu: 200m, memory: 512Mi }
    limits: { cpu: "1", memory: 2Gi }
  password:
    secret: spire-clickhouse
    key: password

steward:
  image: ghcr.io/awell-health/spire-steward:latest
  interval: 2m
  maxConcurrent: 20   # tower-wide agent cap
  sidecar:
    enabled: true
    model: claude-sonnet-4-6
  resources:
    requests: { cpu: 100m, memory: 256Mi }
    limits: { cpu: 500m, memory: 512Mi }

agent:
  image: ghcr.io/awell-health/spire-agent:latest
  defaultResources:
    requests: { cpu: 200m, memory: 512Mi }
    limits: { cpu: "1", memory: 2Gi }
  staleTimeout: 10m
  killTimeout: 15m
  priorityClasses:
    p0: system-cluster-critical
    default: ""
    low: low-priority

syncer:
  mode: sidecar  # sidecar | cronjob
  interval: 2m

tokens:
  default:
    secret: spire-anthropic-default
    key: ANTHROPIC_API_KEY
  heavy:
    secret: spire-anthropic-heavy
    key: ANTHROPIC_API_KEY

github:
  secret: spire-github
  key: GITHUB_TOKEN

repoCache:
  enabled: true
  storage: 10Gi  # per repo

ingress:
  enabled: false
  host: spire.internal.example.com
  tls: true

slack:
  enabled: false
  botToken:
    secret: spire-slack
    key: BOT_TOKEN
  channel: "#spire-notifications"
```

### 6.2 Helm Hooks

1. **pre-install:** Validate that required secrets exist. Fail fast
   if `spire-anthropic-default` is missing.

2. **post-install:** Bootstrap the Dolt database. If `dolthub.remote`
   is set, clone from DoltHub. If not, initialize a fresh database.
   This replaces the manual `spire tower attach` step.

3. **pre-upgrade:** Backup the Dolt PVC snapshot (if cloud provider
   supports it) via a VolumeSnapshot CRD.

4. **post-upgrade:** Run schema migrations on both Dolt and ClickHouse.
   The steward binary already handles this at startup, but the hook
   ensures it runs before the steward pod starts serving.

### 6.3 What the Install Looks Like

```bash
# Create secrets first
kubectl create namespace spire
kubectl -n spire create secret generic spire-anthropic-default \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-...
kubectl -n spire create secret generic spire-anthropic-heavy \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-...
kubectl -n spire create secret generic spire-github \
  --from-literal=GITHUB_TOKEN=ghp_...
kubectl -n spire create secret generic spire-dolthub \
  --from-file=pub_key=~/.dolt/creds/xxx.jwk \
  --from-literal=key_id=xxx

# Install
helm install spire awell/spire \
  --namespace spire \
  --values my-team-values.yaml

# Verify
kubectl -n spire get pods
# NAME                              READY   STATUS    AGE
# spire-steward-0                   2/2     Running   30s
# spire-dolt-0                      1/1     Running   45s
# spire-clickhouse-0                1/1     Running   40s

# Register repos (from local machine)
spire tower attach --name my-team --dolthub org/spire-db
cd ~/code/web-app && spire repo add
cd ~/code/api-server && spire repo add
spire push

# File work and agents will pick it up
spire file "Add dark mode" -t feature -p 2
spire push
# ... cluster syncer pulls, steward assigns, agent pod executes ...
```

---

## 7. Implementation Phases

### Phase 1: Core Infrastructure (Weeks 1-3)

The minimum to run agent pods in k8s with observability.

1. **Merge operator into steward.** Move controller-runtime loops
   into the steward binary. Single process, single Deployment.
   Delete `operator/main.go` as a standalone binary.

2. **ClickHouse StatefulSet.** Helm template, schema init job,
   `pkg/olap.ClickHouseBackend` implementation.

3. **Steward HTTP API.** `/healthz`, `/readyz`, `/metrics`,
   `/api/v1/board`, `/api/v1/agents`. JSON only.

4. **Eliminate beads-seed ConfigMap.** Agent pods connect to Dolt
   directly. Remove `beadsSeedInitContainer` from agent_monitor.go.

5. **Dolt StatefulSet migration.** Deployment -> StatefulSet with
   VolumeClaimTemplate.

6. **End-to-end smoke test.** `helm install` -> file bead -> agent
   pod runs -> bead closes. Automated in CI.

**Exit criteria:** A bead filed locally, synced to cluster, executed
by an agent pod, merged to main, status synced back.

### Phase 2: Observability (Weeks 3-5)

1. **ClickHouse writer in steward.** Batched writes from OTLP
   receiver to ClickHouse. Replace DuckDB writes in cluster mode.

2. **ETL from Dolt to ClickHouse.** Agent_runs sync. View refresh.

3. **Prometheus metrics endpoint.** Connection counts, queue depth,
   agent pod counts, bead throughput.

4. **Web dashboard MVP.** Board view + Agent Fleet view. Static site
   consuming steward HTTP API.

5. **Fill recording gaps.** Fix the observability items from PLAN.md
   section 7: populate timing buckets, parent-child run linkage,
   per-phase token breakdown.

**Exit criteria:** A team of 5 can see what agents are doing in
real-time via the web dashboard. Cost tracking works.

### Phase 3: Scale and Reliability (Weeks 5-8)

1. **Merge queue.** Steward-managed serial merge per repo.

2. **Git clone cache PVC.** Shared repo cache, managed by syncer.

3. **Trust tiers.** Repo-level trust in Dolt, conditional
   `human.approve` gates in formulas.

4. **Concurrency limits.** Per-tower and per-repo max_concurrent
   enforcement in steward.

5. **Pod priority classes.** P0 beads preempt P4 beads.

6. **Load test at 50 agents.** Synthetic beads, 10 repos, measure
   Dolt connection usage, ClickHouse write throughput, merge queue
   latency.

**Exit criteria:** 50 concurrent agents sustained for 24 hours
without connection exhaustion, stale beads, or data loss.

### Phase 4: Intelligence (Weeks 8-12)

1. **Recovery recommender.** Success rate ranking per failure class
   injected into recovery formula context.

2. **Formula health check.** Weekly automated comparison of formula
   versions.

3. **Cross-repo context injection.** Recent tower changes as bead
   comment.

4. **Spec quality nudge.** Length and structure correlation surfaced
   at filing time.

5. **Review pattern learning.** Top findings per repo injected into
   implement context.

**Exit criteria:** The system demonstrably makes better decisions on
month 6 than month 1, measured by success rate trend.

### Phase 5: Human Experience (Weeks 10-14)

1. **Slack bot.** Filing, notifications, daily digest, interactive
   approval buttons.

2. **Web dashboard full.** DORA dashboard, Cost Center, Epic Timeline,
   Formula Lab, Filing Queue.

3. **PM filing flow.** Natural language filing via Slack and web.

4. **Epic estimation.** Historical-based duration and cost estimates.

**Exit criteria:** A PM can file a feature request, track its progress,
and see it ship -- without touching a terminal.

---

## 8. What Emerges

After 6 months of continuous operation:

**The bead graph becomes institutional memory.** Every decision, every
recovery, every review finding is recorded. New team members can trace
why a system works the way it does: follow the bead graph from epic to
subtasks to review comments to merge. This is better than git blame
because it includes the reasoning, not just the diff.

**Formula tuning becomes data-driven.** Instead of guessing whether a
new formula is better, you measure it. The Formula Lab shows that the
team's custom `task-careful` formula (with an extra validation step)
costs 20% more but catches 35% more review issues. Worth it for
security-sensitive repos, not for internal tools.

**The recovery system gets smarter.** Early on, every failure escalates
to human. After 200 recovery beads, the system knows that `build-failure`
in the web repo is almost always a stale lockfile (resolution:
`pnpm install && retry`). The recovery recommender surfaces this. The
model picks the right action without human involvement.

**Cost attribution changes behavior.** When the PM sees that their
vague one-liner specs cost 3x more than detailed specs (because agents
need more review rounds), they start writing better specs. The cost
feedback loop drives spec quality without any mandate.

**Trust accumulates.** Repos that start supervised gradually earn
autonomy. After 3 months, the web repo is at Tier 3 -- P3/P4 tasks
auto-merge. The team trusts the pipeline because they watched it earn
that trust through 200 successful merges. The VP trusts it because the
DORA metrics show improvement.

**Cross-repo context prevents regressions.** When an API change lands,
web-facing tasks automatically get context about it. The agent adapts
its implementation. Integration failures drop because agents work with
current knowledge, not stale assumptions.

**The merge queue becomes a flow metric.** Queue depth and wait time
tell you when to scale up agents (queue growing) or scale down (agents
idle). Combined with cost data, this gives the team an optimal agent
count per time of day.

---

## 9. Non-Goals

Explicitly out of scope for this plan:

- **Multi-cluster.** One cluster per tower. Cross-cluster coordination
  is a post-v2 problem.
- **Custom runtimes.** Agents run Claude Code or Codex CLI. Custom
  agent binaries are not supported yet.
- **GPU workloads.** Agents are I/O bound (API calls, git ops), not
  compute bound. No GPU scheduling.
- **Real-time collaboration.** Agents don't pair-program with humans.
  Humans steer via specs and reviews, not live editing.
- **Self-hosting DoltHub.** Use DoltHub cloud. Self-hosted Dolt remotes
  are a separate infrastructure concern.
