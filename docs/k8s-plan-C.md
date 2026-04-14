# Spire Production Kubernetes Plan

> Comprehensive plan for making Spire production-ready on Kubernetes.
> Covers infrastructure, observability, intelligence, and the human experience.
>
> Design bead: `spi-885rv`
> Date: 2026-04-12

---

## Table of Contents

1. [Current State and Gaps](#1-current-state-and-gaps)
2. [Cluster Architecture](#2-cluster-architecture)
3. [Infrastructure and Operations](#3-infrastructure-and-operations)
4. [Observability and the Human Experience](#4-observability-and-the-human-experience)
5. [Agent Intelligence and Learning](#5-agent-intelligence-and-learning)
6. [The Design-to-Execution Bridge](#6-the-design-to-execution-bridge)
7. [Scale: 50 Agents Across 10 Repos](#7-scale-50-agents-across-10-repos)
8. [Six Months In: Emergent Capabilities](#8-six-months-in-emergent-capabilities)
9. [Security Model](#9-security-model)
10. [Migration Path](#10-migration-path)
11. [Implementation Phases](#11-implementation-phases)

---

## 1. Current State and Gaps

### What exists

- Helm chart with CRDs (SpireAgent, SpireWorkload, SpireConfig), steward/dolt/syncer/operator deployments
- Operator with three control loops: BeadWatcher, WorkloadAssigner, AgentMonitor
- Process and Docker agent backends; no k8s backend
- OTel pipeline: OTLP gRPC receiver (traces + logs), DuckDB sink for tool_events/tool_spans/api_events
- Steward: work assignment, review routing, health checks, hooked-step sweep
- DoltHub sync via syncer pod (pull/push on interval)

### Critical gaps for production

| Gap | Impact |
|-----|--------|
| No k8s Backend implementation | Steward cannot spawn agent pods via k8s API |
| DuckDB is single-process | Cannot handle concurrent OTel writes from 50 agent pods |
| No ClickHouse deployment | No analytics backend for cluster mode |
| No leader election for steward | Multiple steward pods would double-assign work |
| No PodDisruptionBudget | Upgrades could kill running wizards |
| No NetworkPolicy | Agent pods have unrestricted cluster network access |
| No resource quotas | A runaway agent pod could starve the cluster |
| No backup/DR for Dolt PV | Single point of failure for the entire work graph |
| No web dashboard | Team visibility requires CLI access to the cluster |
| No Slack/webhook notifications | PMs and managers have no passive visibility |
| Graph state files on local filesystem | Steward hooked-step sweep doesn't work across pod restarts |
| beads-seed ConfigMap required | Every agent pod needs manual ConfigMap setup |

---

## 2. Cluster Architecture

### Pod topology

```
                                +-----------+
                                | Ingress   |
                                | (webhook  |
                                | + web UI) |
                                +-----+-----+
                                      |
          +---------------------------+---------------------------+
          |                           |                           |
+---------v--------+    +------------v-----------+    +----------v-------+
| steward          |    | web-dashboard          |    | webhook-receiver |
| (Deployment, 1)  |    | (Deployment, 2)        |    | (Deployment, 2)  |
| - steward loop   |    | - Next.js/Go SSR       |    | - Linear/GitHub  |
| - OTLP receiver  |    | - WebSocket for live   |    | - queues to Dolt |
| - ETL writer     |    | - reads Dolt + CH      |    +------------------+
| - sidecar (LLM)  |    +------------------------+
+--------+---------+
         |
         |  spawns via k8s API
         |
+--------v-----------+--+--+--+--+
| agent pods (0..N)  |  |  |  |  |   ephemeral, one per wizard
| - wizard container |  |  |  |  |
| - familiar sidecar |  |  |  |  |
+----+---------------+--+--+--+--+
     |
     | MySQL (3306)           OTLP (4317)
     v                        v
+----+--------+    +----------+---------+
| dolt        |    | clickhouse         |
| StatefulSet |    | StatefulSet        |
| (1 replica) |    | (1 replica, scale  |
|             |    |  to 3 for HA)      |
+------+------+    +--------------------+
       |
+------v------+
| syncer      |
| CronJob     |
| (push/pull) |
+-------------+
```

### New components vs existing

| Component | Status | What changes |
|-----------|--------|-------------|
| Steward pod | Exists | Add leader election, OTLP→ClickHouse writer, k8s backend |
| Dolt StatefulSet | Exists | Add backup CronJob, monitoring, connection pooling |
| Agent pods | Exists (template) | Switch from operator-created to steward-created via k8s Backend |
| Syncer | Exists | Add health checks, exponential backoff, metrics |
| ClickHouse | **New** | StatefulSet, schema auto-migration, retention policies |
| Web dashboard | **New** | Read-only views over Dolt + ClickHouse |
| Webhook receiver | Exists (in daemon) | Extract to standalone Deployment for scaling |

---

## 3. Infrastructure and Operations

### 3.1 The k8s Backend

The missing piece. `pkg/agent/backend.go` defines the `Backend` interface (Spawn, List, Logs, Kill). Add `backend_k8s.go`:

```go
// backend_k8s.go
type K8sBackend struct {
    client    kubernetes.Interface
    namespace string
    config    K8sBackendConfig
}

type K8sBackendConfig struct {
    AgentImage       string        // ghcr.io/awell-health/spire-agent:v0.35.0
    SidecarImage     string        // ghcr.io/awell-health/spire-sidecar:v0.35.0
    ServiceAccount   string        // spire-agent
    ResourceRequests corev1.ResourceList
    ResourceLimits   corev1.ResourceList
    NodeSelector     map[string]string
    Tolerations      []corev1.Toleration
    TTLAfterFinished *int32        // auto-cleanup completed pods
}
```

**Spawn flow:**

1. Steward calls `backend.Spawn(SpawnConfig{...})`
2. K8sBackend builds a Pod spec:
   - Init container: seed `.beads/` from ConfigMap (or from Dolt directly via `bd init --stealth`)
   - Main container: `spire execute <bead-id>` with env vars for Dolt DSN, tower name, OTel endpoint
   - Sidecar: familiar container with `/comms` emptyDir shared volume
   - Secrets: mounted from k8s Secrets (Anthropic key, GitHub token)
   - Labels: `spire.awell.io/bead-id`, `spire.awell.io/agent-name`, `spire.awell.io/tower`
3. K8sBackend creates the Pod via client-go
4. Returns a `K8sHandle` that wraps pod watch for Wait/Alive/Signal

**List flow:** `kubectl get pods -l app.kubernetes.io/part-of=spire-agent` -- parse into `[]Info`.

**Kill flow:** `kubectl delete pod <name> --grace-period=30`.

**Logs flow:** `kubectl logs <pod-name> -c wizard --follow`.

**Registration with ResolveBackend:**

```go
func ResolveBackend(name string) Backend {
    switch name {
    case "process", "":
        return newProcessBackend()
    case "docker":
        return newDockerBackend()
    case "kubernetes", "k8s":
        return newK8sBackend()
    }
}
```

The steward pod runs with `agent.backend: kubernetes` in its config. The k8s backend reads in-cluster config automatically.

### 3.2 Steward as a production service

**Leader election.** Use `k8s.io/client-go/tools/leaderelection` with a Lease object. Only the leader runs the steward cycle. Standby replicas stay warm (connected to Dolt, OTLP receiver running) and take over within 15 seconds.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: spire-steward
spec:
  replicas: 2  # active-standby
  template:
    spec:
      containers:
        - name: steward
          args: ["steward", "--leader-elect", "--lease-name=spire-steward"]
```

**Health endpoints.** The steward already has a metrics server (`pkg/steward/metrics_server.go`). Extend with:

- `/healthz` -- liveness: goroutine count, memory, last cycle completion
- `/readyz` -- readiness: Dolt connection verified, ClickHouse writable, leader status
- `/metrics` -- Prometheus-format metrics (cycle duration, beads assigned, agents active, queue depth)

**Prometheus metrics to export:**

```
spire_steward_cycle_duration_seconds{tower}
spire_steward_beads_ready{tower}
spire_steward_beads_assigned_total{tower}
spire_steward_agents_active{tower}
spire_steward_agents_idle{tower}
spire_steward_stale_beads{tower}
spire_steward_shutdown_beads{tower}
spire_agent_pod_duration_seconds{tower,bead_type,formula,result}
spire_agent_cost_usd_total{tower,repo,formula}
spire_otel_events_received_total{tower,signal}
spire_dolt_sync_duration_seconds{tower,direction}
```

**Graph state persistence.** Currently, `graph_state.json` lives at `~/.config/spire/runtime/<agent>/graph_state.json`. In k8s, this file dies with the pod. Two options:

Option A (recommended): **Store graph state in Dolt.** Add a `graph_states` table:

```sql
CREATE TABLE graph_states (
    bead_id    VARCHAR PRIMARY KEY,
    agent_name VARCHAR,
    state_json JSON,
    updated_at TIMESTAMP
);
```

The executor reads/writes graph state via store API instead of filesystem. This makes hooked-step sweep work across pod restarts, enables the steward to inspect graph state without filesystem access, and makes crash recovery automatic.

Option B: **ConfigMap per agent.** Too much k8s API churn; not recommended.

### 3.3 ClickHouse deployment

ClickHouse replaces DuckDB as the OLAP backend in cluster mode. Same columnar engine philosophy, but multi-writer and server-based.

**StatefulSet:**

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: spire-clickhouse
  namespace: spire
spec:
  replicas: 1
  serviceName: spire-clickhouse
  template:
    spec:
      containers:
        - name: clickhouse
          image: clickhouse/clickhouse-server:24.3
          ports:
            - containerPort: 8123  # HTTP
            - containerPort: 9000  # native
          volumeMounts:
            - name: data
              mountPath: /var/lib/clickhouse
          resources:
            requests: { cpu: 200m, memory: 512Mi }
            limits:   { cpu: 1000m, memory: 2Gi }
  volumeClaimTemplates:
    - metadata: { name: data }
      spec:
        accessModes: ["ReadWriteOnce"]
        resources: { requests: { storage: 50Gi } }
```

**Schema migration.** The existing `pkg/olap/schema.go` DDL statements translate almost 1:1 to ClickHouse. Create a `pkg/olap/clickhouse.go` that:

1. Implements the same `WriteFunc` interface as DuckDB
2. Uses `clickhouse-go` driver
3. Maps DuckDB types to ClickHouse (DOUBLE → Float64, TEXT → String, etc.)
4. Uses MergeTree engine with `ORDER BY (tower, started_at)` for time-series queries

**Backend abstraction.** The OTel receiver's `writeFn func(fn func(*sql.Tx) error) error` already abstracts the storage backend. In cluster mode, the steward passes a ClickHouse-backed writeFn instead of the DuckWriter:

```go
if cfg.Backend == "kubernetes" {
    chConn := clickhouse.Open(...)
    writeFn = func(fn func(*sql.Tx) error) error {
        return chConn.Tx(fn)
    }
} else {
    writeFn = duckWriter.Submit
}
```

**Retention policies.** ClickHouse TTL on tables:

| Table | Retention | Rationale |
|-------|-----------|-----------|
| tool_events | 90 days | High volume, low historical value per event |
| tool_spans | 90 days | Same as tool_events |
| api_events | 180 days | Cost analysis needs longer history |
| agent_runs_olap | 1 year | DORA metrics, formula performance trends |
| daily_formula_stats | 2 years | Trend analysis |
| failure_hotspots | 1 year | Failure pattern mining |

### 3.4 Dolt operations

**Backup CronJob.** Dolt's git-like nature makes backup straightforward:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: spire-dolt-backup
spec:
  schedule: "0 */6 * * *"  # every 6 hours
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: backup
              command: ["sh", "-c"]
              args:
                - |
                  cd /var/lib/dolt/${DB_NAME}
                  dolt backup sync s3://spire-backups/${TOWER_NAME}/$(date +%Y%m%d-%H%M%S)
```

DoltHub sync is already a backup, but S3 backups provide an independent recovery path.

**Connection pooling.** With 50 agent pods connecting to a single Dolt instance, connection limits matter. Dolt's `max_connections: 100` in the current config supports this, but add a ProxySQL or connection pool sidecar if connection churn becomes an issue:

```
agent pods (50) → ProxySQL (connection pool) → Dolt (max_connections: 100)
```

**Monitoring.** Dolt exposes MySQL-compatible `SHOW STATUS` and `SHOW PROCESSLIST`. Add a metrics sidecar that exports:

```
spire_dolt_connections_active
spire_dolt_connections_total
spire_dolt_queries_per_second
spire_dolt_merge_conflicts_total
spire_dolt_database_size_bytes
```

### 3.5 Secrets management

**Hierarchy:**

```
                    +-------------------+
                    | External Secrets  |
                    | Operator (ESO)    |
                    +--------+----------+
                             |
                    syncs from Vault/AWS SM/GCP SM
                             |
                    +--------v----------+
                    | k8s Secrets       |
                    | (spire namespace) |
                    +--------+----------+
                             |
          +------------------+------------------+
          |                  |                  |
    +-----v------+    +-----v------+    +------v-----+
    | steward    |    | agent pod  |    | syncer pod |
    | - Anthropic|    | - Anthropic|    | - DoltHub  |
    | - GitHub   |    | - GitHub   |    |   creds    |
    | - Linear   |    +------------+    +------------+
    +------------+
```

**Per-repo token isolation.** Different repos may need different GitHub tokens (different orgs, different permission scopes). The SpireConfig CR already supports multiple token refs:

```yaml
tokens:
  default:
    secret: anthropic-api-key
    key: ANTHROPIC_API_KEY
  heavy:
    secret: anthropic-opus-key
    key: ANTHROPIC_API_KEY
  github-frontend:
    secret: github-frontend-token
    key: GITHUB_TOKEN
  github-backend:
    secret: github-backend-token
    key: GITHUB_TOKEN
```

The k8s Backend reads the repo's prefix from the bead, looks up the token mapping, and injects the correct Secret reference into the agent pod's env.

**Secret rotation.** With ESO, rotation is automatic. Agent pods are ephemeral (one per wizard invocation), so they pick up new secrets on every spawn. The steward pod needs a restart on secret rotation -- handle with a Reloader sidecar or ESO's `refreshInterval`.

### 3.6 Disaster recovery

**RPO/RTO targets:**

| Component | RPO | RTO | Strategy |
|-----------|-----|-----|----------|
| Dolt (work graph) | 0 (DoltHub sync) | 5 min (clone from DoltHub) | DoltHub is the DR copy |
| ClickHouse (analytics) | 6 hours (backup interval) | 30 min (restore from S3) | S3 backups + TTL means most data is re-derivable from Dolt |
| Graph state | 0 (stored in Dolt) | 0 (automatic) | Survives pod restarts |
| Agent pods | N/A (ephemeral) | 0 | Steward re-spawns from last graph state |
| Secrets | N/A | Depends on ESO source | External source is the backup |

**Full cluster loss recovery:**

1. `helm install spire` on new cluster
2. Dolt init container clones from DoltHub (all work graph state restored)
3. ClickHouse starts empty; ETL repopulates from Dolt `agent_runs` + agents re-export OTel
4. Steward starts, reads graph_states table, re-spawns wizards for in-progress beads
5. In-flight wizard work is lost (ephemeral pods), but graph state tracks last completed step -- wizards resume from that step, not from scratch

---

## 4. Observability and the Human Experience

This is where production Spire must fundamentally differ from local Spire. On a laptop, one person watches the board TUI. In production, five personas need five different views:

### 4.1 The five personas

| Persona | Needs | How they interact |
|---------|-------|-------------------|
| **VP of Engineering** | "Are agents shipping features? What's the ROI?" | Weekly email digest, executive dashboard |
| **Engineering Manager** | "What's in flight? Any stuck work? Cost per epic?" | Web dashboard, Slack alerts for stuck/failed beads |
| **Tech Lead** | "What's the agent doing to my repo? Is the review quality good?" | Web dashboard with repo filter, PR-level detail, formula tuning |
| **Product Manager** | "Is my feature being built? When will it land?" | Slack notifications on status changes, epic progress view |
| **Agent Operator** | "How many agents are running? Any pod failures? Cost burn rate?" | Grafana dashboards, PagerDuty alerts |

### 4.2 Web dashboard

A read-only web application that queries Dolt (work graph) and ClickHouse (metrics). Not a full SPA -- a server-rendered Go application with WebSocket for live updates.

**Views:**

**Tower Overview (default).** Shows what matters right now:
- Active agents: count, list with bead ID and current step, time elapsed
- Queue: ready beads sorted by priority, estimated wait time
- Recent completions: last 20 merged beads with lead time
- Alerts: stuck agents, failed runs, corrupted beads
- Cost: today's spend, burn rate, projected monthly

**Epic Progress.** For PMs and managers:
- Gantt-style view of epic subtasks (derived from bead hierarchy + dep graph)
- Each subtask shows: status, assigned agent, current formula step, time in step
- Color coding: green (progressing), yellow (stale), red (failed/stuck), blue (waiting for human)
- Dependency lines between subtasks
- "Estimated completion" based on historical formula duration for this bead type

**Agent Detail.** For tech leads and operators:
- Formula step waterfall (like `spire trace` but rendered in the browser)
- Real-time log streaming via WebSocket (proxied from `kubectl logs`)
- Token/cost breakdown per step
- Review verdicts and findings inline
- "Steer" button: sends a message to the wizard via the familiar

**Repository View.** For tech leads:
- All beads scoped to one repo prefix
- Recent merges with diff stats (files changed, lines added/removed)
- Review quality: approval rate, average review rounds, common rejection reasons
- Formula performance: which formula runs best for this repo

**Metrics Dashboard.** For operators and managers:
- DORA metrics computed from ClickHouse: deployment frequency, lead time, change failure rate, time to restore
- Formula comparison: success rate, cost, duration by formula name and version
- Cost breakdown: by tower, repo, formula, phase, model
- Trend lines: week-over-week for all key metrics

**Implementation approach:**

```
web-dashboard/
  cmd/dashboard/main.go      # Go HTTP server
  internal/
    handlers/                 # page handlers
    queries/                  # Dolt + ClickHouse query layer
    realtime/                 # WebSocket hub for live updates
  templates/                  # Go html/template
  static/                     # CSS, JS (htmx for interactivity)
```

Use htmx for interactivity (no React/Vue build step, keeps it simple). WebSocket for live agent status updates -- the steward publishes events to a Redis pub/sub or in-process channel that the dashboard subscribes to.

**Deployment:** Separate Deployment in the Helm chart, 2 replicas behind the Ingress. Read-only access to Dolt and ClickHouse.

### 4.3 Slack integration

Slack is the passive observability channel. People who don't want to open a dashboard still need to know what's happening.

**Events that trigger Slack messages:**

| Event | Channel | Persona |
|-------|---------|---------|
| Bead merged successfully | #spire-activity | Everyone |
| Bead failed (agent crashed, build failed, review discarded) | #spire-alerts | Tech lead, operator |
| Human approval needed (hooked step) | #spire-approvals | Tech lead (mentioned) |
| Epic completed (all subtasks merged) | #spire-activity | PM, manager |
| Agent stale > threshold | #spire-alerts | Operator |
| Cost threshold exceeded (daily/weekly) | #spire-alerts | Manager |
| Design bead filed | #spire-designs | PM, tech lead |

**Message format (example: bead merged):**

```
:white_check_mark: *spi-abc.2* merged to main
*Add OAuth2 support* (task, P2)
Repo: web- | Formula: task-default | Duration: 8m 12s
Cost: $0.43 | Review: 1 round (approved)
<https://dashboard.spire/beads/spi-abc.2|View details>
```

**Implementation:** A Slack bot that subscribes to Dolt commit hooks (or polling). When a bead transitions to a terminal status (closed, discard, escalate), the bot formats and sends the message. Use Slack's Block Kit for rich formatting.

Add to the steward cycle as a post-assignment hook -- after each cycle, emit events for any state transitions that occurred. The webhook receiver already processes Linear events; extend it to emit Slack notifications.

### 4.4 What makes a PM actually excited

A PM does not care about pods, agents, or formulas. A PM cares about:

1. **"I described what I wanted, and it got built."** The design bead → epic → subtasks → merged code pipeline must feel like magic. The PM writes a design bead (plain English spec), an engineer approves it, and hours later the code is merged and the PM gets a Slack notification.

2. **"I can see progress without asking anyone."** The Epic Progress view in the web dashboard shows exactly where each piece of work is, what's blocking, and what's next. No standups needed for status.

3. **"I know when it will be done."** Estimated completion based on historical formula performance. "Tasks of this type in this repo take a median of 12 minutes. Your epic has 6 remaining subtasks with 3 parallelizable. ETA: 45 minutes."

4. **"When something goes wrong, I know immediately."** Slack notification when a subtask fails or needs human intervention. The PM doesn't need to diagnose -- they just need to know "this one needs an engineer to look at it."

5. **"I can file more work without bothering anyone."** `spire design "Add dark mode support"` from a Slack slash command. The PM never touches a terminal.

**Slack slash commands for PMs:**

```
/spire design "Add password reset flow"     → creates a design bead
/spire status spi-abc                       → shows epic progress
/spire status                               → shows tower overview
/spire approve spi-abc.1                    → approves a hooked step
```

### 4.5 What makes an engineering manager trust this

Trust comes from three things: visibility, control, and a track record.

**Visibility:**
- Every agent action is traced (OTel spans). The manager can drill into any bead and see exactly what the agent did, what files it changed, what the reviewer said.
- Code review quality is measurable: review rounds, rejection reasons, common findings. If review quality drops, it shows up in metrics before it shows up in bugs.
- Cost is transparent: per-bead, per-repo, per-day. No surprise bills.

**Control:**
- `human.approve` gates on any formula step. The manager can require human approval before merges land on production repos.
- Per-repo concurrency limits. "Only 2 agents working on the API repo at a time."
- Model selection per repo: use Opus for the critical payment service, Sonnet for the docs site.
- Kill switch: `spire dismiss --all` or scale agent pods to 0.

**Track record:**
- After 100 successful merges with zero reverts, the manager starts trusting.
- After 500 merges, the manager starts wondering why they're still reviewing everything manually.
- The system builds its own track record through metrics. "Last 30 days: 247 beads merged, 3 reverted (1.2% failure rate), average lead time 14 minutes, total cost $312."

---

## 5. Agent Intelligence and Learning

### 5.1 Recovery learning database

The recovery system already captures structured learnings (recovery-default formula: collect_context → decide → execute → verify → learn → finish). In cluster mode, these learnings become a shared knowledge base.

**Schema for recovery learnings (already partially exists in recovery bead metadata):**

```sql
CREATE TABLE recovery_learnings (
    id               VARCHAR PRIMARY KEY,
    failure_class    VARCHAR NOT NULL,    -- e.g. "build-failure", "empty-implement", "merge-conflict"
    repo_prefix      VARCHAR,
    formula_name     VARCHAR,
    step_name        VARCHAR,             -- which formula step failed
    root_cause       TEXT,                -- Claude-generated root cause analysis
    resolution       TEXT,                -- what fixed it
    prevention       TEXT,                -- how to prevent it next time
    confidence       FLOAT,              -- 0-1, how confident the learning is
    applied_count    INT DEFAULT 0,       -- how many times this learning was used
    success_count    INT DEFAULT 0,       -- how many times it led to successful recovery
    created_at       TIMESTAMP,
    last_applied_at  TIMESTAMP
);
```

**Prior-learning lookup at scale.** When a wizard encounters a failure, the recovery formula's `collect_context` step queries this table:

```sql
SELECT * FROM recovery_learnings
WHERE failure_class = ?
  AND (repo_prefix = ? OR repo_prefix IS NULL)
  AND confidence > 0.5
ORDER BY (success_count::FLOAT / GREATEST(applied_count, 1)) DESC, created_at DESC
LIMIT 5;
```

The wizard sees "5 similar failures were resolved before, here's what worked." This is not magic -- it is structured recall. The model decides whether the prior learning applies; the system just surfaces it.

**Confidence decay.** Learnings that are applied but don't lead to successful recovery have their confidence reduced:

```
On recovery success: confidence = min(1.0, confidence + 0.1)
On recovery failure: confidence = max(0.0, confidence - 0.2)
```

After enough failures, bad learnings drop below the 0.5 threshold and stop being surfaced.

### 5.2 Formula evolution

Formulas are the DNA of agent behavior. Today they are hand-crafted TOML files. After 6 months of operation, the system has enough data to suggest formula improvements.

**Formula performance tracking (already in OLAP schema):**

```sql
-- daily_formula_stats already captures:
-- run_count, success_count, total_cost_usd, avg_duration_s, avg_review_rounds
-- grouped by date, formula_name, formula_version, tower, repo
```

**Formula A/B testing.** When a team publishes a new formula version, the steward can split traffic:

```toml
# Tower-level formula config
[formula-routing]
bug-default.v3 = 0.8   # 80% of bugs use v3
bug-default.v4 = 0.2   # 20% of bugs use v4 (experimental)
```

After N runs (configurable, default 20), compare success rate, cost, and duration. If v4 is statistically better, auto-promote. If worse, auto-rollback. The steward does the mechanical split and comparison; the decision to promote/rollback is surfaced to a human unless auto-promote is enabled.

**Formula suggestion engine.** After analyzing hundreds of runs:

"For repo `web-`, bug-default with `max_review_rounds=2` has a 94% success rate but 12% of those are arbiter-decided. Increasing to `max_review_rounds=3` would reduce arbiter escalations and improve review quality."

This is a ClickHouse query + model interpretation, surfaced as a steward-sidecar suggestion to the archmage:

```
spire collect  →
  "Steward suggests: increase max_review_rounds to 3 for web- repo bug fixes.
   Data: 47 runs, 6 arbiter escalations, 0 arbiter-discards (all would have
   converged with one more round). Estimated impact: -12% arbiter rate, +$0.08/run."
```

### 5.3 Cross-repo context

When an agent works on a frontend task that depends on a backend API change, it needs context from both repos. Today, each wizard has context from one repo only.

**Cross-repo context assembly.** The executor's context resolution already reads `spire.yaml`'s `context:` list. Extend this with dependency-based context:

1. When a bead has a `depends-on` dep pointing to a bead in another repo, the executor fetches that bead's description, comments, and (if merged) the diff.
2. This context is injected into the wizard's prompt alongside the repo's `context:` files.
3. The wizard can see "the API endpoint you depend on was implemented in spi-abc.1, here's the interface."

This is mechanical context assembly (ZFC-compliant) -- no local reasoning about what's relevant, just structured retrieval based on the dependency graph.

**Implementation:**

```go
// In pkg/executor, during context assembly:
func (e *Executor) assembleCrossRepoContext(beadID string) (string, error) {
    deps, _ := store.GetDependencies(beadID)
    var context strings.Builder
    for _, dep := range deps {
        if dep.Type != "depends-on" { continue }
        depBead, _ := store.GetBead(dep.BlockerID)
        if depBead.Status == "closed" {
            // Fetch the merged diff for closed deps
            context.WriteString(fmt.Sprintf("## Dependency: %s\n%s\n", dep.BlockerID, depBead.Description))
            // If in a different repo, fetch the diff via git
        }
    }
    return context.String(), nil
}
```

### 5.4 Trust gradients

Not all repos and bead types deserve the same level of autonomy. A docs update should auto-merge; a payment service change should require human approval.

**Trust levels per repo:**

| Level | What | Gate |
|-------|------|------|
| 0 - Sandbox | Agent can run, but all merges require human approval | `human.approve` on merge step |
| 1 - Supervised | Agent merges automatically, but failures trigger human review | Default for new repos |
| 2 - Trusted | Agent merges, auto-recovers from failures, files follow-up work | After N successful merges |
| 3 - Autonomous | Agent can file its own beads based on codebase analysis | Explicitly opted in |

**Promotion criteria (mechanically tracked, human-approved):**

```
Level 0 → 1: 10 successful merges, 0 reverts, human approves promotion
Level 1 → 2: 50 successful merges, <2% failure rate, human approves
Level 2 → 3: 200 successful merges, <1% failure rate, recovery success >80%, human approves
```

The steward tracks these metrics per repo. When promotion criteria are met, it sends a message: "Repo `web-` has met Level 2 trust criteria (52 merges, 0 reverts). Promote? [approve/reject]"

**Implementation:** Add a `trust_level` column to the `repos` table. The executor reads it and adjusts formula behavior:

- Level 0: Inject `human.approve` step before merge
- Level 1: Normal formula execution
- Level 2: Enable auto-recovery (recovery formula runs automatically on failure)
- Level 3: Enable `exploration.suggest` action (agent can propose new beads)

### 5.5 Review pattern mining

After thousands of sage reviews, the system has a corpus of review findings. Mine this for patterns:

**Common review findings table:**

```sql
CREATE TABLE review_findings (
    id               VARCHAR PRIMARY KEY,
    bead_id          VARCHAR,
    repo_prefix      VARCHAR,
    finding_category VARCHAR,    -- "error-handling", "test-coverage", "naming", "security"
    finding_text     TEXT,
    severity         VARCHAR,    -- "blocker", "major", "minor", "nit"
    was_fixed        BOOLEAN,
    created_at       TIMESTAMP
);
```

Populated by parsing sage review bead metadata (already stored as structured metadata since `spi-w00yn`).

**What you learn from this data:**

1. "80% of rejections in the `api-` repo are about error handling. Add error-handling requirements to the repo's CLAUDE.md."
2. "The bug-default formula's implement step consistently produces code that fails the `naming` review category. Add a naming convention document to the formula's context."
3. "Sage reviews take 3x longer for TypeScript repos than Go repos. The sage prompt may need TypeScript-specific review criteria."

These insights are surfaced as steward suggestions (via the sidecar LLM) or as weekly digest emails. They drive formula evolution and CLAUDE.md improvements -- a feedback loop where agent work quality improves the system that produces agent work.

---

## 6. The Design-to-Execution Bridge

This is the pipeline that makes non-engineers productive. A PM writes a spec; code gets shipped.

### 6.1 The pipeline

```
PM writes design bead
    ↓
Tech lead reviews design bead (comment + close)
    ↓
Engineer files epic with --ref to design bead
    ↓
    epic-default formula:
      1. design-check (validates design bead is closed with content)
      2. plan (Opus generates subtask breakdown)
      3. materialize (creates child beads)
      4. implement (dispatches apprentices in waves)
      5. review (sage reviews)
      6. merge (squash to main)
      7. close
    ↓
PM gets Slack notification: "Your feature shipped"
```

### 6.2 Making design beads accessible to non-engineers

Currently, `spire design "Auth system"` requires CLI access. For PMs:

**Option A: Slack slash command.**

```
/spire design "Password reset flow"
```

The webhook receiver creates a design bead, posts a thread with the bead ID, and the PM adds detail as Slack thread replies. A bot syncs thread content to bead comments.

**Option B: Web form.**

The web dashboard includes a "File Design" form:
- Title (required)
- Description (rich text, markdown)
- Priority selector
- Related epics (autocomplete from existing beads)

Submits via the store API. The PM gets a link to track the design bead's progress.

**Option C: Email-to-bead.**

`design@spire.your-company.com` creates a design bead from the email subject and body. Reply-to adds comments. Simple, works for people who live in email.

### 6.3 Design bead quality

The design-check step in epic-default validates that a design bead is closed and has content. But "has content" is a low bar. For production, add a design quality signal:

**Design review formula.** A new formula `design-review` that runs when a design bead is closed:

```toml
name = "design-review"
entry = "assess"

[steps.assess]
kind = "op"
action = "wizard.run"
flow = "design-assess"
title = "Assess design completeness"
model = "claude-opus-4-6"
[steps.assess.with]
prompt = """
Review this design bead for completeness. Check:
1. Is the problem clearly stated?
2. Are acceptance criteria defined?
3. Are edge cases considered?
4. Are dependencies on other systems identified?
5. Is scope bounded (not too vague, not too prescriptive)?

Output a quality score (1-5) and specific gaps.
If score < 3, add a needs-human label and list what's missing.
"""
```

This runs automatically when the tech lead closes a design bead. If quality is low, the PM gets a Slack message: "Your design for 'Password reset flow' needs more detail on edge cases and acceptance criteria. [Edit in dashboard]"

### 6.4 Spec-to-subtask tracing

After an epic completes, the PM should be able to trace from their original spec to the merged code:

```
Design bead (PM's spec)
  → Epic (engineer's filing)
    → Subtask 1 (agent's implementation)
      → Merged commit (SHA, diff, files)
    → Subtask 2
      → Merged commit
```

This tracing is already possible through the bead hierarchy + git labels. The web dashboard renders it as a tree with expandable diff views. The PM sees "here's what you asked for, here's what got built, here's the code."

---

## 7. Scale: 50 Agents Across 10 Repos

### 7.1 What breaks

| Problem | At what scale | Mitigation |
|---------|---------------|------------|
| Dolt connection exhaustion | 50+ concurrent pods | Connection pooling (ProxySQL), connection reuse in store API |
| OTel write contention | 50 agents × ~100 events/min | ClickHouse handles this natively; batch inserts every 5s |
| Steward cycle time | 50 agents to health-check, 100+ beads to assess | Parallelize health checks; cache roster between cycles |
| DoltHub sync conflicts | Multiple repos pushing simultaneously | Field-level ownership already handles this; syncer runs sequentially |
| Git clone time in agent pods | Large repos (>1GB) | Shallow clones (`--depth=1 --single-branch`), or pre-warmed PV with repo cache |
| API rate limits | 50 agents hitting Anthropic concurrently | Token bucket rate limiter in k8s Backend, per-tower budget |
| Node resource pressure | 50 agent pods × (2 CPU, 4Gi each) = 100 CPU, 200Gi | Node autoscaling, or dedicated agent node pool with spot instances |
| Pod scheduling latency | Cold start: pull image + clone repo | Pre-pulled images on agent nodes, repo cache volume |

### 7.2 Concurrency control

The steward already has a `max_concurrent` concept (PLAN.md item 2). In k8s, this becomes:

```yaml
# SpireConfig CR
spec:
  concurrency:
    global: 50          # max agent pods cluster-wide
    perRepo:
      web-: 5           # max 5 agents on frontend repo
      api-: 3           # max 3 agents on backend repo (more critical)
      docs-: 10         # docs repo can handle more parallelism
    perType:
      epic: 5           # max 5 epic wizards (they spawn sub-agents)
      task: 40          # tasks are cheap
      recovery: 5       # recovery shouldn't dominate capacity
```

The steward enforces these limits before calling `backend.Spawn()`. If at capacity, beads stay in the ready queue.

### 7.3 Resource management

**Agent pod resource tiers:**

| Bead type | CPU request | Memory request | Timeout |
|-----------|-------------|----------------|---------|
| task/bug | 500m | 1Gi | 15m |
| epic | 1000m | 2Gi | 60m |
| recovery | 250m | 512Mi | 10m |

**Node pools:**

```
Pool: spire-agents
  - Spot instances (70% savings)
  - Taint: spire.awell.io/agent=true:NoSchedule
  - Labels: spire.awell.io/role=agent
  - Autoscaler: min 0, max 20 nodes

Pool: spire-control
  - On-demand instances
  - Labels: spire.awell.io/role=control
  - Steward, Dolt, ClickHouse, dashboard run here
  - Autoscaler: min 2, max 4 nodes
```

Agent pods get a toleration for the agent taint. This isolates agent workloads from the control plane and allows aggressive scaling.

### 7.4 Cost control

**Per-tower budget.** The steward tracks cumulative LLM cost (from api_events in ClickHouse) and stops spawning agents when the budget is exhausted:

```yaml
spec:
  budget:
    daily: 100.00       # USD
    weekly: 500.00
    monthly: 1500.00
    alertAt: 0.8         # alert at 80% of budget
```

**Per-bead cost caps.** If a single bead exceeds $5 in LLM cost (configurable), the steward kills it and files a recovery bead. This prevents runaway agents from burning through the budget.

**Cost attribution.** Every API event in ClickHouse has `bead_id`, `tower`, `repo_prefix`, `formula_name`, `step`. Roll up cost at any dimension:

```sql
SELECT repo, SUM(cost_usd) as total_cost, COUNT(DISTINCT bead_id) as beads
FROM api_events
WHERE tower = 'my-team' AND timestamp > now() - INTERVAL 7 DAY
GROUP BY repo
ORDER BY total_cost DESC;
```

### 7.5 Repo cache volume

Git cloning is the biggest cold-start cost for agent pods. A shared PV with pre-cloned repos eliminates this:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: spire-repo-cache
spec:
  accessModes: ["ReadOnlyMany"]  # RO for agent pods
  resources:
    requests:
      storage: 100Gi
```

A CronJob runs `git fetch` on all registered repos every 5 minutes, keeping the cache warm. Agent pods mount this as a read-only volume and use `git clone --reference /cache/<repo> --dissociate` for instant clones.

---

## 8. Six Months In: Emergent Capabilities

After 6 months of continuous operation with thousands of completed beads, new capabilities emerge from the data.

### 8.1 Predictive queue management

With historical data on formula duration by bead type and repo, the steward can predict:

- **Queue wait time:** "5 beads ahead of yours, estimated wait: 23 minutes"
- **Completion time:** "This epic has 8 subtasks, 3 parallelizable. Based on historical task-default runs for this repo, ETA: 2h 15m"
- **Capacity planning:** "At current burn rate, you'll need 10 more agent slots to clear the backlog by Friday"

These predictions are ClickHouse queries against `agent_runs_olap`:

```sql
SELECT
    formula_name,
    repo,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_seconds) as median_duration,
    percentile_cont(0.9) WITHIN GROUP (ORDER BY duration_seconds) as p90_duration
FROM agent_runs_olap
WHERE result = 'success' AND started_at > now() - INTERVAL 30 DAY
GROUP BY formula_name, repo;
```

### 8.2 Failure prediction

After seeing enough failures, the system can predict which beads are likely to fail:

- Beads with descriptions matching past failure patterns (similar keywords, similar scope)
- Beads in repos with high failure rates
- Beads filed during periods of rapid change (many concurrent merges)

The steward-sidecar LLM reviews the ready queue and flags high-risk beads: "spi-xyz looks similar to spi-abc (which failed 3 times before succeeding). Consider adding more detail to the spec or assigning a human reviewer."

### 8.3 Automatic CLAUDE.md evolution

The system's review findings (Section 5.5) accumulate patterns. After enough data:

"The top 3 review rejection categories for `api-` repo are:
1. Missing error handling (34 occurrences)
2. Missing test cases (28 occurrences)
3. Inconsistent naming (15 occurrences)

Suggested CLAUDE.md additions:
- 'All new functions must include error handling for nil inputs and network failures.'
- 'All new functions must include at least one unit test.'
- 'Use camelCase for local variables, PascalCase for exported functions.'"

The steward-sidecar can draft a PR to the repo's CLAUDE.md with these additions. A human reviews and merges it. Future agents benefit immediately.

### 8.4 Formula marketplace

Teams using Spire across different repos develop formulas optimized for their workflows. A formula marketplace (tower-level or public) enables sharing:

```bash
spire formula search "security review"
# → security-review v2.1 by @alice (4.8 stars, 200 runs, 96% success)
# → security-audit v1.0 by @bob (4.2 stars, 45 runs, 88% success)

spire formula install security-review
```

The marketplace is backed by a public DoltHub database of formula metadata. Formulas themselves are TOML files stored in the tower's Dolt. Ratings and run statistics come from aggregated (anonymized) ClickHouse data that teams opt into sharing.

### 8.5 Cross-team knowledge transfer

When a new team starts using Spire, they benefit from the recovery learnings and formula optimizations of existing teams (if shared). The recovery_learnings table with `repo_prefix IS NULL` entries are universal learnings that apply everywhere.

Example: Team A discovers that TypeScript repos fail 40% of the time when the sage prompt doesn't include `tsconfig.json` path information. They record this as a recovery learning. Team B, starting with TypeScript, automatically benefits -- the recovery formula surfaces Team A's learning when their first TypeScript failure occurs.

---

## 9. Security Model

### 9.1 Network policies

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: agent-pod-policy
  namespace: spire
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/component: agent
  policyTypes: ["Ingress", "Egress"]
  egress:
    # Allow: Dolt, ClickHouse (via steward OTel), GitHub, Anthropic API
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: spire-dolt
      ports: [{ port: 3306, protocol: TCP }]
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: spire-steward
      ports: [{ port: 4317, protocol: TCP }]  # OTLP
    - to: [{ ipBlock: { cidr: 0.0.0.0/0 } }]  # GitHub + Anthropic (external)
      ports:
        - { port: 443, protocol: TCP }
        - { port: 22, protocol: TCP }   # git SSH
  ingress: []  # no inbound traffic to agent pods
```

### 9.2 RBAC

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: spire-agent
  namespace: spire
rules:
  # Agents can read ConfigMaps and Secrets in spire namespace only
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]
    resourceNames: ["spire-credentials"]  # only their own credentials
```

### 9.3 Pod security

```yaml
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000      # wizard user
    fsGroup: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: wizard
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
        readOnlyRootFilesystem: false  # agents need to write to /workspace
```

### 9.4 Audit trail

Every agent action is already traced via OTel. For security-critical operations:

- All git pushes are logged with bead ID, agent name, commit SHA, files changed
- All LLM prompts are logged (opt-in, off by default for cost/privacy)
- All bead state transitions are in Dolt's commit history (built-in audit trail)
- Pod creation/deletion events are in k8s audit logs

The Dolt commit history is the immutable audit trail: "Who filed this bead? Who approved it? What agent executed it? What code was merged? When?"

---

## 10. Migration Path

### 10.1 From local to cluster

A team using Spire locally transitions to k8s without losing any state:

1. **Day 0:** Local Spire works. Beads in local Dolt, agents as processes.
2. **Day 1:** `helm install spire --set tower.dolthub.remote=<existing-remote>`. The cluster's Dolt init container clones from DoltHub. All beads, history, and learnings are preserved.
3. **Day 2:** Team switches `agent.backend: kubernetes` in their `spire.yaml`. `spire summon` now creates pods instead of processes. Everything else is unchanged.
4. **Day 3:** Enable the web dashboard. PMs and managers start watching.
5. **Week 1:** Add Slack integration. Passive visibility for everyone.
6. **Week 2:** Start filing work from Slack. PMs are onboarded.

### 10.2 Gradual adoption within a team

Not everyone switches at once:

- Developer A keeps using local mode (`agent.backend: process`). Their beads sync via DoltHub.
- Developer B uses the cluster (`agent.backend: kubernetes`). Their beads also sync via DoltHub.
- Both see each other's work on the board. The steward assigns work to whichever agents are available.

This is the hybrid deployment mode from ARCHITECTURE.md, now made concrete.

---

## 11. Implementation Phases

### Phase 1: Core k8s backend (2 weeks)

**Goal:** Steward can spawn and manage agent pods via the k8s API.

| Task | Effort | Dependencies |
|------|--------|-------------|
| `pkg/agent/backend_k8s.go` -- Spawn, List, Logs, Kill | 3d | None |
| `pkg/agent/backend_k8s_test.go` -- integration tests with fake client | 2d | backend_k8s.go |
| Register "kubernetes" in ResolveBackend | 1h | backend_k8s.go |
| Graph state persistence in Dolt (`graph_states` table) | 2d | None |
| Update executor to read/write graph state from store API | 2d | graph_states table |
| Update steward hooked-step sweep to query Dolt instead of filesystem | 1d | graph_states table |
| Helm chart: add backend config to SpireConfig | 1d | backend_k8s.go |

**Exit criteria:** `spire summon 1` on a k8s steward creates an agent pod that completes a task-default formula and merges to main.

### Phase 2: ClickHouse + production OLAP (2 weeks)

**Goal:** OTel events flow to ClickHouse; metrics queries work against ClickHouse.

| Task | Effort | Dependencies |
|------|--------|-------------|
| `pkg/olap/clickhouse.go` -- WriteFunc implementation | 2d | None |
| ClickHouse schema (translate DuckDB DDL) | 1d | clickhouse.go |
| ClickHouse StatefulSet in Helm chart | 1d | None |
| Backend-switch in steward daemon (DuckDB vs ClickHouse based on config) | 1d | clickhouse.go |
| Update `spire metrics` to query ClickHouse when available | 2d | clickhouse.go |
| Retention policies (TTL on ClickHouse tables) | 1d | Schema |
| ETL from Dolt → ClickHouse (reuse existing ETL with ClickHouse writer) | 2d | clickhouse.go |

**Exit criteria:** `spire metrics` on a cluster deployment shows DORA metrics sourced from ClickHouse.

### Phase 3: Steward hardening (1 week)

**Goal:** Steward is production-ready: leader election, health endpoints, Prometheus metrics.

| Task | Effort | Dependencies |
|------|--------|-------------|
| Leader election via k8s Lease | 2d | None |
| Health endpoints (/healthz, /readyz) | 1d | None |
| Prometheus metrics exporter | 2d | None |
| Concurrency limits (global, per-repo, per-type) | 1d | Phase 1 |
| Cost budget enforcement | 1d | Phase 2 |

**Exit criteria:** Steward runs as 2 replicas with automatic failover; Prometheus scrapes steward metrics.

### Phase 4: Web dashboard (3 weeks)

**Goal:** Read-only web UI for tower overview, epic progress, agent detail, metrics.

| Task | Effort | Dependencies |
|------|--------|-------------|
| Go HTTP server skeleton with htmx | 2d | None |
| Tower overview page (active agents, queue, recent completions, alerts) | 3d | Phase 2 |
| Epic progress page (Gantt-style subtask view) | 3d | Phase 2 |
| Agent detail page (formula waterfall, log streaming) | 3d | Phase 1, 2 |
| Metrics dashboard page (DORA, formula comparison, cost) | 3d | Phase 2 |
| WebSocket live updates for agent status | 2d | Phase 1 |
| Helm chart: dashboard Deployment + Ingress | 1d | All above |

**Exit criteria:** PM can open a browser, see epic progress, and understand what agents are doing without CLI access.

### Phase 5: Slack integration (1 week)

**Goal:** Passive notifications and slash commands.

| Task | Effort | Dependencies |
|------|--------|-------------|
| Slack bot with event notifications (merged, failed, stuck, approval needed) | 2d | Phase 1, 2 |
| Slash commands (/spire design, /spire status, /spire approve) | 2d | Phase 4 |
| Channel routing config in SpireConfig | 1d | Slack bot |

**Exit criteria:** PM gets Slack notification when their epic ships; can file design beads from Slack.

### Phase 6: Intelligence (ongoing, starts after Phase 2)

| Task | Effort | Dependencies |
|------|--------|-------------|
| Recovery learnings table in Dolt + ClickHouse | 2d | Phase 2 |
| Prior-learning lookup in recovery formula | 2d | Recovery learnings |
| Review findings mining (parse sage metadata) | 3d | Phase 2 |
| Formula A/B testing in steward | 3d | Phase 2, 3 |
| Trust gradient per repo (trust_level column, promotion criteria) | 2d | Phase 2 |
| Cross-repo context assembly in executor | 2d | None |

### Phase 7: Scale hardening (after Phase 3)

| Task | Effort | Dependencies |
|------|--------|-------------|
| Repo cache PV + CronJob | 2d | Phase 1 |
| Node pool configuration (agents on spot instances) | 1d | Phase 1 |
| Network policies | 1d | Phase 1 |
| Pod security standards | 1d | Phase 1 |
| Dolt backup CronJob | 1d | None |
| Connection pooling (ProxySQL if needed) | 2d | Phase 1 at scale |

---

## Summary

Production Spire on Kubernetes is not just "put the CLI in pods." It is a system where:

1. **The steward is a real service** -- leader-elected, health-checked, budget-enforcing, Prometheus-emitting.
2. **Agents are ephemeral pods** -- spawned by the steward via a k8s Backend, isolated by NetworkPolicy, bounded by resource limits.
3. **Analytics live in ClickHouse** -- replacing single-process DuckDB with a multi-writer columnar database.
4. **Five personas get five views** -- web dashboard for visual thinkers, Slack for passive observers, CLI for operators, Grafana for infra.
5. **The system gets smarter** -- recovery learnings, formula evolution, review pattern mining, trust gradients.
6. **Non-engineers file work through Slack** -- design beads, status checks, approval gates, all without touching a terminal.
7. **Scale is handled by architecture, not prayer** -- concurrency limits, cost budgets, spot instances, repo caches, connection pools.

The exit state after Phase 5 (approximately 9 weeks): a team of 10 engineers and 5 PMs can file work, watch agents execute it, review what lands, and track ROI -- all without anyone SSHing into a cluster or reading pod logs.
