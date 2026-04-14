# Spire Production Kubernetes: Plan A

> Design bead: spi-885rv
> Status: comprehensive plan for making Spire production-ready on Kubernetes
> Scope: infrastructure, observability, intelligence, the human experience, and scaling to 50 agents across 10 repos

---

## Table of contents

1. [System architecture](#1-system-architecture)
2. [Infrastructure and operations](#2-infrastructure-and-operations)
3. [Observability and the human experience](#3-observability-and-the-human-experience)
4. [Agent intelligence and learning](#4-agent-intelligence-and-learning)
5. [The design-to-execution bridge](#5-the-design-to-execution-bridge)
6. [What breaks at scale](#6-what-breaks-at-scale)
7. [Implementation phases](#7-implementation-phases)
8. [Six months in: the emergent system](#8-six-months-in-the-emergent-system)

---

## 1. System architecture

### 1.1 Pod topology

```
                        +-----------+
                        |  Ingress  |
                        | (webhook  |
                        |  + API)   |
                        +-----+-----+
                              |
           +------------------+------------------+
           |                  |                  |
    +------+------+   +------+------+   +-------+------+
    |   steward   |   |    dolt     |   |  clickhouse  |
    | Deployment  |   | StatefulSet |   | StatefulSet  |
    | replicas: 1 |   | replicas: 1 |   | replicas: 1  |
    | (leader-    |   | (PVC: 10Gi) |   | (PVC: 50Gi)  |
    |  elected)   |   +------+------+   +-------+------+
    +------+------+          |                  |
           |          mysql://3306         tcp://9000
           |                 |                  |
           |    +------------+------------+     |
           |    |            |            |     |
      +----+---+--+  +------+-----+  +---+----+--+
      | agent pod  |  | agent pod  |  | agent pod |
      | wizard +   |  | wizard +   |  | wizard +  |
      | familiar   |  | familiar   |  | familiar  |
      +------------+  +------------+  +-----------+
           |
    +------+------+
    |    syncer    |
    | Deployment  |
    | (ephemeral) |
    +-------------+
         |
    DoltHub remote
```

### 1.2 What changes from local, what stays the same

The core insight: Spire's architecture is already k8s-native in spirit. `pkg/store`, `pkg/executor`, `pkg/wizard`, `pkg/formula`, and `pkg/otel` are all transport-agnostic. The only things that change are process spawning (fork to pod), storage (local file to PVC/ClickHouse), and secrets (keychain to k8s Secrets).

| Layer | Local | Cluster | Code changes needed |
|-------|-------|---------|---------------------|
| Store API | localhost:3307 | spire-dolt.spire.svc:3306 | DSN config only |
| Agent spawn | `os/exec` fork | k8s pod create via API | New `agent.K8sBackend` |
| OTel sink | DuckDB file | ClickHouse server | New `olap.ClickHouseWriter` |
| Secrets | `~/.config/spire/credentials` | k8s Secrets | Already handled in operator |
| Formula engine | Unchanged | Unchanged | None |
| Wizard subprocess | Unchanged | Unchanged (runs in pod) | None |
| Git worktrees | Local filesystem | Pod emptyDir (ephemeral) | None |
| Board TUI | Local terminal | Local terminal, remote dolt | DSN config only |
| OTel receiver | In daemon process | In steward pod | None |
| DoltHub sync | In daemon process | Syncer pod | None |

### 1.3 The steward as the cluster control plane

The steward pod is the brain. It consolidates five functions that are currently spread across multiple processes locally:

1. **Work assignment** (from `pkg/steward/steward.go` `TowerCycle`)
2. **OTel OTLP receiver** (from `pkg/otel/receiver.go`, port 4317)
3. **Hooked-step sweep** (from `pkg/steward/steward.go` `SweepHookedSteps`)
4. **Prometheus metrics endpoint** (from `pkg/steward/metrics_server.go`, port 9090)
5. **Health and readiness** (new, ports 8080)

The steward does NOT run the daemon sync loop or Linear integration in cluster mode. Those are handled by the syncer pod and webhook ingress respectively.

```yaml
# steward Deployment (simplified)
spec:
  replicas: 1
  template:
    metadata:
      labels:
        app.kubernetes.io/name: spire-steward
    spec:
      containers:
        - name: steward
          image: ghcr.io/awell-health/spire-steward:v1.0
          command: ["spire", "steward", "--cluster"]
          ports:
            - containerPort: 4317   # OTLP gRPC
              name: otlp
            - containerPort: 9090   # Prometheus metrics
              name: metrics
            - containerPort: 8080   # health + readiness
              name: health
          env:
            - name: DOLT_HOST
              value: spire-dolt.spire.svc
            - name: CLICKHOUSE_DSN
              value: tcp://spire-clickhouse.spire.svc:9000
            - name: STEWARD_MODE
              value: cluster
```

**Leader election.** Single-replica Deployment is sufficient for v1. The steward is stateless (all state is in Dolt and ClickHouse). If it dies, k8s restarts it. Agents in flight continue independently. For HA in v2: use k8s leader election via `coordination.k8s.io/v1` Lease objects.

---

## 2. Infrastructure and operations

### 2.1 Dolt: the operational database

**StatefulSet with PVC.** The Dolt SQL server runs as a StatefulSet (not Deployment) because it needs stable network identity and persistent storage. Single replica.

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
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 10Gi
```

**Connection pooling.** At 50 concurrent agents, each agent pod holds one MySQL connection to Dolt for store operations. The steward holds another. Total: ~52 connections. Dolt's `max_connections: 100` in the existing config handles this. For 100+ agents, add a connection pooler (ProxySQL or similar) between agent pods and Dolt.

**Backup and DR.** Three layers:
1. **DoltHub sync** via syncer pod (default: every 5 minutes). This is the primary DR mechanism. Full database history is on DoltHub. Recovery: `dolt clone` from DoltHub.
2. **PVC snapshots** via VolumeSnapshot CRDs (if the storage class supports it). Hourly snapshots, 24-hour retention. This handles the "DoltHub is down" scenario.
3. **Dolt branch-based point-in-time recovery.** Every steward cycle commits. Every merge commits. The dolt log IS the audit trail. `dolt checkout` to any historical commit restores the full state.

**Schema migrations.** Dolt supports standard SQL ALTER TABLE. Migrations run as part of the steward startup (same pattern as `olap.InitSchema`). Backward-compatible: new columns with defaults, never drop columns in the same release.

### 2.2 ClickHouse: the analytics database

**Why not DuckDB in cluster mode.** DuckDB is single-process, file-locked. The design bead (spi-885rv) already calls this out. In a cluster with 50 agents exporting OTel telemetry concurrently, plus the steward querying analytics, plus the metrics endpoint serving Prometheus scrapes, DuckDB's single-writer lock becomes a bottleneck.

ClickHouse is the production replacement. Same columnar query model, server-based, multi-writer, multi-reader.

**Schema mapping.** Every table in `pkg/olap/schema.go` maps 1:1 to a ClickHouse table. The DDL changes are mechanical:

| DuckDB type | ClickHouse type |
|-------------|-----------------|
| VARCHAR | String |
| BIGINT | Int64 |
| DOUBLE | Float64 |
| INTEGER | Int32 |
| BOOLEAN | UInt8 |
| TIMESTAMP | DateTime64(3) |
| DATE | Date |
| TEXT | String |

**Implementation: `olap.ClickHouseWriter`.** A new backend that implements the same write interface as `olap.DB` but targets a ClickHouse server. The OTel receiver already calls `writeFn(func(*sql.Tx) error)`. The ClickHouse writer wraps `clickhouse-go/v2` and provides the same transactional write interface.

```go
// pkg/olap/clickhouse.go (new file)
type ClickHouseDB struct {
    conn clickhouse.Conn
}

func (c *ClickHouseDB) WriteBatch(fn func(batch driver.Batch) error) error { ... }
```

**Data flow:**
```
agent pod → OTLP gRPC → steward OTel receiver → ClickHouseWriter → ClickHouse
steward ETL cycle → Dolt agent_runs → ClickHouse agent_runs_olap
spire metrics → ClickHouse → Prometheus text format
```

**Retention policy.** ClickHouse TTL on tables:
- `tool_events`: 90 days (high cardinality, per-tool-call granularity)
- `tool_spans`: 90 days
- `api_events`: 180 days (lower cardinality, higher value for cost analysis)
- `agent_runs_olap`: indefinite (low cardinality, the core analytical asset)
- Aggregation tables (`daily_formula_stats`, `weekly_merge_stats`): indefinite

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
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 50Gi
```

### 2.3 Agent pods: the ephemeral workforce

**One pod per wizard invocation.** Pods are ephemeral. The steward spawns a pod when a bead is assigned. The pod runs the formula lifecycle, writes result.json, exits. The operator reaps the pod.

**The `agent.K8sBackend`.** A new implementation of the existing `agent.Backend` interface:

```go
// pkg/agent/k8s_backend.go (new file)
type K8sBackend struct {
    clientset kubernetes.Interface
    namespace string
    image     string
    config    *SpireConfig
}

func (k *K8sBackend) Spawn(cfg SpawnConfig) (Handle, error) {
    // Build pod spec (reuse logic from agent_monitor.go buildWorkloadPod)
    // Create pod via k8s API
    // Return K8sHandle wrapping pod name
}

func (k *K8sBackend) List() ([]Info, error) {
    // List pods with label spire.awell.io/managed=true
    // Map pod status to Info{Name, Alive, BeadID}
}

func (k *K8sBackend) Kill(name string) error {
    // Delete pod by name
}
```

This replaces the current operator's `AgentMonitor.reconcileManagedAgent` with a direct steward-driven model. The steward is the scheduler. No intermediate CRDs needed for work assignment. The SpireWorkload CRD becomes optional (for pinning work to specific node pools).

**OTel export from agent pods.** Every agent pod exports to the steward's OTLP receiver. The `OTEL_EXPORTER_OTLP_ENDPOINT` environment variable is set on pod creation:

```yaml
env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://spire-steward.spire.svc:4317
  - name: OTEL_RESOURCE_ATTRIBUTES
    value: "bead.id=$(SPIRE_BEAD_ID),agent.name=$(SPIRE_AGENT_NAME),tower=$(TOWER_NAME)"
```

**Resource profiles.** Three tiers:

| Profile | CPU req/limit | Memory req/limit | Use case |
|---------|--------------|------------------|----------|
| standard | 200m / 1000m | 512Mi / 2Gi | task-default, bug-default, chore-default |
| heavy | 500m / 2000m | 1Gi / 4Gi | epic-default (Opus planning + child dispatch) |
| review | 100m / 500m | 256Mi / 1Gi | subgraph-review (Opus review, short-lived) |

Profiles are configured in SpireConfig and selected by the steward based on the bead's formula.

**Pod security.** Agent pods run as non-root (user `wizard`, UID 1000). No host access. No privileged containers. Network policy restricts egress to: Dolt service, steward OTLP, GitHub API (for git operations), Anthropic API (for LLM calls). Everything else is blocked.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: agent-egress
  namespace: spire
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: spire-agent
  policyTypes: [Egress]
  egress:
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: spire-dolt
      ports:
        - port: 3306
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: spire-steward
      ports:
        - port: 4317  # OTLP
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
      ports:
        - port: 443  # GitHub API, Anthropic API
```

### 2.4 Secrets management

**Current state.** The existing operator injects secrets from SpireConfig token refs. This works but is flat (one Anthropic key for all agents).

**Production model: per-repo secret scoping.**

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: spire-secrets
  namespace: spire
type: Opaque
data:
  ANTHROPIC_API_KEY_DEFAULT: <base64>     # Sonnet — standard agents
  ANTHROPIC_API_KEY_HEAVY: <base64>       # Opus — wizards, sages, arbiters
  GITHUB_TOKEN_ORG_A: <base64>            # GitHub PAT for org A repos
  GITHUB_TOKEN_ORG_B: <base64>            # GitHub PAT for org B repos
  DOLT_REMOTE_USER: <base64>
  DOLT_REMOTE_PASSWORD: <base64>
```

The steward maps secrets to agent pods based on the repo's configuration in the `repos` table. Each repo row includes a `github_token_ref` field that points to a specific key in the secret. This allows different repos to use different GitHub tokens (critical for multi-org setups).

**External Secrets Operator integration.** For production environments, support ESO annotations on the secret template so teams can pull from Vault, AWS Secrets Manager, or GCP Secret Manager:

```yaml
# values.yaml
secrets:
  external: true
  provider: vault
  path: secret/data/spire
```

### 2.5 Syncer pod

The syncer runs `spire pull` and `spire push` on a configurable interval. It is the bridge between the cluster's Dolt instance and DoltHub.

**Current implementation** (in `k8s/syncer.yaml`) is a shell loop. For production:

1. Replace the shell loop with a Go binary that handles graceful shutdown, exponential backoff on failures, and conflict resolution logging.
2. Add a `/healthz` endpoint so k8s can detect a stuck syncer.
3. Add Prometheus metrics: `spire_sync_last_success_timestamp`, `spire_sync_duration_seconds`, `spire_sync_conflicts_total`.

**Conflict resolution.** The field-level ownership model (cluster owns status, user owns content, append-only for comments/messages) is already defined in ARCHITECTURE.md. The syncer enforces this programmatically during merge. If a conflict cannot be resolved by ownership rules, it is logged and an alert bead is created.

### 2.6 Helm chart evolution

The existing Helm chart (`helm/spire/`) needs these additions:

```yaml
# values.yaml additions

clickhouse:
  enabled: true    # false to use DuckDB mode (dev/testing)
  resources:
    requests:
      cpu: 200m
      memory: 512Mi
    limits:
      cpu: 1000m
      memory: 2Gi
  storage:
    size: 50Gi
  retention:
    toolEvents: 90   # days
    apiEvents: 180
    agentRuns: 0      # indefinite

steward:
  leaderElection: false  # true for HA (v2)
  otlpPort: 4317
  metricsPort: 9090
  healthPort: 8080
  maxConcurrent: 10  # tower-wide agent concurrency limit

ingress:
  enabled: false
  host: spire.example.com
  tls: true
  paths:
    - path: /api/webhooks
      service: spire-steward
      port: 8080
    - path: /api/metrics
      service: spire-steward
      port: 9090

networkPolicies:
  enabled: true

podDisruptionBudgets:
  dolt:
    minAvailable: 1
  steward:
    minAvailable: 1

monitoring:
  serviceMonitor:
    enabled: false  # true if Prometheus Operator is installed
    interval: 30s
```

### 2.7 Bootstrap sequence

```bash
# 1. Create namespace and install CRDs
helm install spire awell/spire \
  --namespace spire --create-namespace \
  --set dolthub.remote=myorg/my-tower \
  --set-file secrets.anthropicKey=./anthropic.key \
  --set-file secrets.githubToken=./github.token \
  --set-file secrets.dolthubUser=./dolthub.user \
  --set-file secrets.dolthubPassword=./dolthub.pass

# 2. The chart creates:
#    - spire-dolt StatefulSet (clones from DoltHub on first boot)
#    - spire-clickhouse StatefulSet (empty, schema auto-created)
#    - spire-steward Deployment (starts assignment cycle + OTLP receiver)
#    - spire-syncer Deployment (starts DoltHub sync loop)
#    - RBAC, NetworkPolicies, ServiceMonitor (if enabled)
#    - SpireConfig singleton (tower config)

# 3. Register repos (from developer laptop)
spire repo add --prefix=web https://github.com/myorg/web-app
spire push   # syncer pulls, steward picks up the new repo

# 4. File work and summon agents
spire file "Add dark mode" -t feature -p 2
spire push   # steward assigns to an idle agent, creates pod
```

---

## 3. Observability and the human experience

This is where the plan goes beyond infrastructure. A production Spire cluster serves three audiences with fundamentally different needs:

| Audience | Primary question | Cadence |
|----------|-----------------|---------|
| **Tech lead** | "Is my epic progressing? Which agents are stuck? Should I intervene?" | Real-time, multiple times per day |
| **Engineering manager** | "How fast are we shipping? What's the cost? Are agents getting better over time?" | Daily/weekly |
| **PM / non-engineer** | "Is my spec being worked on? When will it land? Can I see what changed?" | Event-driven (notifications) |

### 3.1 The web dashboard

A TUI is great for engineers. It is useless for PMs. The production system needs a web dashboard.

**Architecture.** The dashboard is a read-only web app that queries Dolt (for work graph state) and ClickHouse (for analytics). It runs as a separate Deployment in the cluster, exposed via ingress.

```
Browser → Ingress → spire-dashboard → Dolt (work graph)
                                     → ClickHouse (analytics)
```

**Technology.** Server-rendered Go + htmx. No SPA framework. The dashboard is a thin query layer over Dolt and ClickHouse SQL. Server-rendered means:
- No build step for frontend
- Ships in the same Go binary
- Works behind corporate proxies
- Fast first paint (no JS bundle)

**Views:**

#### 3.1.1 Tower overview (the "war room" view)

What the VP of Engineering sees when they open the dashboard.

```
+------------------------------------------------------------------+
| Tower: my-team                                    Last sync: 2m ago |
+------------------------------------------------------------------+
| AGENTS           | QUEUE              | THIS WEEK               |
| 12 active        | 8 beads queued     | 47 beads merged         |
| 3 idle           | P0: 1  P1: 3       | 94% success rate        |
| 2 in review      | P2: 4              | $142.30 total cost      |
| 0 stuck          |                    | 3.2h avg lead time      |
+------------------------------------------------------------------+
| ACTIVE WORK                                                       |
| spi-x2mk   "Auth overhaul"        epic    wizard-3   implement   |
|   spi-x2mk.1  "OAuth2"            task    wizard-5   review      |
|   spi-x2mk.2  "MFA support"       task    wizard-7   implement   |
| web-b7d0   "Dark mode"            feature wizard-1   plan        |
| api-8a01   "Rate limiting"        bug     wizard-2   merge       |
+------------------------------------------------------------------+
| RECENT COMPLETIONS                                                |
| spi-g1q3y  "OTel dual-signal"     feat    12m ago    $3.20       |
| web-c4f2   "Fix login redirect"   bug     34m ago    $0.80       |
+------------------------------------------------------------------+
```

Key design decisions:
- **No pagination on the active work view.** At 50 concurrent agents, the list fits on one screen. If it doesn't, the tower has too many concurrent agents and the steward concurrency limit should be lower.
- **Cost is always visible.** Every bead shows its accumulated cost. The weekly rollup shows total spend. This is how you build trust: transparency.
- **"Stuck" is a first-class status.** A bead is stuck when the wizard has exceeded the stale threshold. The dashboard shows it in red. One click to see the logs.

#### 3.1.2 Epic drill-down

What the tech lead and PM see when they click an epic.

```
+------------------------------------------------------------------+
| Epic: spi-x2mk "Auth system overhaul"                            |
| Status: in_progress  |  5/8 subtasks done  |  $47.20  |  2d 4h  |
+------------------------------------------------------------------+
| TIMELINE                                                          |
| Apr 10 09:00  Filed (JB)                                         |
| Apr 10 09:05  Design bead spi-x2mk-d1 linked                    |
| Apr 10 14:00  Design approved (JB)                               |
| Apr 10 14:02  Plan generated: 8 subtasks in 3 waves              |
| Apr 10 14:10  Wave 1 started (OAuth2, Session mgmt)              |
| Apr 10 16:30  Wave 1 complete (2/2 merged)                       |
| Apr 10 16:32  Wave 2 started (MFA, Token refresh, Audit log)     |
| Apr 11 09:00  MFA merged. Token refresh in review. Audit log     |
|               implementing.                                       |
| Apr 11 09:15  Token refresh: sage requested changes (round 1/3)  |
| Apr 11 09:45  Token refresh: fix applied, sage re-reviewing      |
+------------------------------------------------------------------+
| SUBTASK TABLE                                                     |
| ID           | Title              | Status    | Cost  | Duration |
| spi-x2mk.1  | OAuth2             | merged    | $3.20 | 1h 20m   |
| spi-x2mk.2  | Session management | merged    | $2.80 | 55m      |
| spi-x2mk.3  | MFA support        | merged    | $4.10 | 2h 05m   |
| spi-x2mk.4  | Token refresh      | review    | $5.60 | 1h 40m   |
| spi-x2mk.5  | Audit logging      | implement | $2.10 | 35m      |
| spi-x2mk.6  | Rate limiting      | queued    | -     | -        |
| spi-x2mk.7  | Error handling     | queued    | -     | -        |
| spi-x2mk.8  | Integration tests  | queued    | -     | -        |
+------------------------------------------------------------------+
| DESIGN CONTEXT                                                    |
| Design bead: spi-x2mk-d1 "Auth system design"                   |
| Key decisions: REST API (not GraphQL), JWT with refresh tokens,   |
| bcrypt for password hashing, 15-minute access token TTL.          |
+------------------------------------------------------------------+
```

**The timeline is the killer feature.** It tells the PM a story: "your spec was filed, a design was linked, the plan broke it into 8 tasks, and here's exactly where things stand." No Jira. No standup. No "can you check on this?" Slack messages. The timeline IS the status update.

The timeline is built from:
- Bead status transitions (dolt commit history)
- Comment history (the `comments` table)
- Agent run records (the `agent_runs_olap` table in ClickHouse)
- Review verdicts (the `review_verdict` metadata on review-round beads)

#### 3.1.3 Agent activity view

What the tech lead sees when they want to understand agent behavior.

```
+------------------------------------------------------------------+
| AGENTS (12 active / 15 registered)                               |
+------------------------------------------------------------------+
| Name      | Status  | Bead       | Phase     | Duration | Model  |
| wizard-1  | working | web-b7d0   | plan      | 2m 30s   | sonnet |
| wizard-2  | working | api-8a01   | merge     | 0m 45s   | sonnet |
| wizard-3  | working | spi-x2mk   | implement | 14m 20s  | opus   |
| wizard-5  | review  | spi-x2mk.1 | sage      | 3m 10s   | opus   |
| wizard-7  | working | spi-x2mk.2 | implement | 8m 05s   | sonnet |
| wizard-8  | idle    | -          | -         | -        | -      |
| ...                                                               |
+------------------------------------------------------------------+
| wizard-3 DETAIL                                                   |
| Bead: spi-x2mk "Auth system overhaul"                           |
| Formula: epic-default v3                                          |
| Step: implement (subgraph-implement → dispatch-children)         |
| Children dispatched: 3/8 (wave 2 of 3)                          |
| Last tool call: Read /pkg/auth/oauth.go (2s ago)                 |
| Tokens used: 45,230 prompt + 12,800 completion                  |
| Cost so far: $2.10                                                |
| OTel trace: [view waterfall]                                     |
+------------------------------------------------------------------+
```

The "last tool call" line is powered by the OTel `tool_events` table. It answers the question every tech lead has: "what is this agent actually doing right now?"

The "view waterfall" link opens the OTel trace view for the current session, showing the full hierarchy of LLM calls and tool invocations. This is the same data `spire trace` renders in the TUI, but in a browser with clickable spans.

#### 3.1.4 Analytics dashboard

What the engineering manager reviews weekly.

**DORA metrics panel:**
- Deployment frequency: beads merged per day (line chart, 30-day window)
- Lead time: filed-to-merged distribution (histogram)
- Change failure rate: failed runs / total runs (line chart)
- Time to restore: time from failure to next successful merge (line chart)

**Cost panel:**
- Total spend by day (bar chart)
- Cost per bead by type: task avg $2.50, bug avg $1.80, epic avg $35.00
- Cost per formula version (are newer formulas more expensive?)
- Cost per repo (which repos are most expensive?)

**Formula performance panel:**
- Success rate by formula (table, sortable)
- Average review rounds by formula
- Average duration by formula
- Formula version comparison: "task-default v3.1 vs v3.0" side-by-side

**Agent efficiency panel:**
- Beads completed per agent per day
- Average time per phase (plan, implement, review, merge)
- Review round distribution (1 round: 70%, 2 rounds: 20%, 3+ rounds: 10%)
- Arbiter escalation rate

All queries run against ClickHouse. The dashboard re-queries on page load (no caching layer needed at this scale).

### 3.2 Notification system

The dashboard is pull-based. Notifications are push-based. Both are needed.

#### 3.2.1 Slack integration

**Events that trigger Slack notifications:**

| Event | Channel | Audience |
|-------|---------|----------|
| Epic completed | #spire-activity | Everyone |
| Bead stuck (stale threshold exceeded) | #spire-alerts | Tech lead |
| Review needs human decision (arbiter exhausted) | #spire-review | Tech lead |
| Human approval gate hit (`human.approve`) | #spire-review | Archmage |
| Design bead needs content | #spire-design | PM |
| Daily cost exceeds budget threshold | #spire-alerts | Eng manager |
| Formula failure rate exceeds threshold | #spire-alerts | Eng manager |

**Implementation.** The steward runs a notification loop alongside its work assignment cycle. Events are detected by comparing the current state to the previous cycle's state. Notifications are sent via Slack Incoming Webhooks (simplest) or Slack API (for threading and rich blocks).

```yaml
# values.yaml
notifications:
  slack:
    enabled: true
    webhookUrl: https://hooks.slack.com/services/T.../B.../...
    channels:
      activity: "#spire-activity"
      alerts: "#spire-alerts"
      review: "#spire-review"
      design: "#spire-design"
    budgetThreshold: 500  # USD/day
    failureRateThreshold: 0.3  # 30%
```

#### 3.2.2 Email digest

Weekly email digest for engineering managers. Contains:
- Beads merged this week (count, list)
- Total cost
- Success rate trend (vs last week)
- Top 3 most expensive beads
- Top 3 longest-running beads
- Formula performance changes

Built from ClickHouse queries. Sent via SMTP or SendGrid.

### 3.3 The TUI in cluster mode

The existing board TUI (`spire board`) works locally. In cluster mode, it connects to the cluster's Dolt instance:

```bash
# Set DOLT_HOST to the cluster's dolt service (port-forwarded or via ingress)
kubectl port-forward svc/spire-dolt 3307:3306 -n spire &
DOLT_HOST=127.0.0.1 DOLT_PORT=3307 spire board
```

For ClickHouse-backed views (metrics mode):
```bash
kubectl port-forward svc/spire-clickhouse 9000:9000 -n spire &
CLICKHOUSE_DSN=tcp://127.0.0.1:9000 spire board --mode metrics
```

The TUI is always available for engineers who prefer terminals. The web dashboard is for everyone else.

### 3.4 Audit trail

Every action in Spire is recorded in Dolt's commit history. This gives you a complete, tamper-evident audit trail for free.

```sql
-- Who merged what and when?
SELECT commit_hash, committer, date, message
FROM dolt_log
WHERE message LIKE '%merge%'
ORDER BY date DESC
LIMIT 20;

-- What changed in a specific bead?
SELECT *
FROM dolt_diff_issues
WHERE to_id = 'spi-x2mk'
ORDER BY to_commit_date DESC;
```

For compliance-sensitive environments, the audit trail can be exported to an external system (CloudTrail, Datadog, Splunk) via a sidecar that tails `dolt_log`.

---

## 4. Agent intelligence and learning

### 4.1 The recovery knowledge base

Spire already has a recovery system: `recovery-default` formula, prior-learning lookup, structured metadata. In production, this becomes a compound learning system.

**How it works today:**
1. A wizard fails (build error, empty implement, review deadlock)
2. The steward creates a recovery bead with failure class metadata
3. The recovery formula runs: `collect_context` gathers prior learnings, `decide` chooses a recovery action, `execute` applies it, `learn` extracts a durable learning

**What changes at scale:**

The `learn` step writes a structured learning record to a new `learnings` table in Dolt:

```sql
CREATE TABLE learnings (
    id              VARCHAR PRIMARY KEY,
    failure_class   VARCHAR NOT NULL,      -- e.g. "build-failure", "empty-implement"
    repo            VARCHAR,
    formula_name    VARCHAR,
    trigger_bead_id VARCHAR,               -- the bead that failed
    recovery_bead_id VARCHAR,              -- the recovery bead that fixed it
    root_cause      TEXT,                  -- LLM-generated root cause analysis
    fix_pattern     TEXT,                  -- what worked
    prevention      TEXT,                  -- how to avoid this in the future
    confidence      FLOAT,                 -- LLM's self-assessed confidence (0-1)
    created_at      TIMESTAMP,
    applied_count   INTEGER DEFAULT 0,     -- how many times this learning was used
    success_count   INTEGER DEFAULT 0      -- how many times it worked when applied
);
```

**Prior-learning lookup gets smarter.** The `collect_context` step in `recovery-default` already does prior-learning lookup. In production, it queries the `learnings` table:

```sql
SELECT * FROM learnings
WHERE failure_class = ?
  AND (repo = ? OR repo IS NULL)
ORDER BY (success_count::FLOAT / NULLIF(applied_count, 0)) DESC,
         created_at DESC
LIMIT 5;
```

This returns the most effective learnings for this failure class, weighted by success rate. The `decide` step receives these as context and chooses a recovery strategy informed by what has worked before.

**Learning effectiveness tracking.** When a learning is applied, `applied_count` increments. When the recovery succeeds, `success_count` increments. Over time, ineffective learnings sink to the bottom. Effective ones rise. This is mechanical feedback, not LLM judgment (ZFC-compliant).

### 4.2 Formula evolution

After 6 months of operation, you have thousands of completed beads. The `agent_runs_olap` table and ClickHouse analytics contain a rich history of formula performance.

**Automated formula performance reports.** A weekly job queries ClickHouse and generates a report:

```
Formula Performance Report (week of Apr 6, 2026)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

task-default v3.2 (current)
  Runs: 142  |  Success: 91%  |  Avg cost: $2.30  |  Avg review rounds: 1.4
  vs v3.1: +3% success, -$0.40 cost, -0.2 review rounds

bug-default v3.1 (current)
  Runs: 67   |  Success: 88%  |  Avg cost: $1.90  |  Avg review rounds: 1.1
  vs v3.0: -2% success, +$0.10 cost  ← REGRESSION

epic-default v3.0 (current)
  Runs: 12   |  Success: 75%  |  Avg cost: $38.50 |  Avg review rounds: 2.1
  Wave efficiency: 78% parallel (avg 2.3 waves)
```

When a formula version regresses, the report flags it. The archmage can then either revert to the previous version (`spire formula publish bug-default --version v3.0`) or iterate in the workshop.

**Formula A/B testing.** The tower can run two formula versions simultaneously:

```bash
# Publish v3.2 as a canary
spire formula publish task-default --version v3.2 --canary 20

# 20% of new task beads get v3.2, 80% get v3.1
# After 50 runs, compare in the analytics dashboard
```

Implementation: the steward assigns a formula version label to each bead based on the canary percentage. The executor reads the label and loads the specified version. ClickHouse analytics segment by formula version automatically (the `formula_version` column already exists in `agent_runs_olap`).

### 4.3 Cross-repo context

At 10 repos, agents in one repo don't know about patterns established in another. This is a solved problem once you have the shared Dolt database.

**Cross-repo learning lookup.** When a wizard plans a task in repo A, it can query the learnings table for patterns from repo B:

```sql
-- "Has any repo solved a similar problem?"
SELECT * FROM learnings
WHERE failure_class = 'build-failure'
  AND fix_pattern LIKE '%dependency%'
ORDER BY success_count DESC
LIMIT 3;
```

**Cross-repo formula sharing.** Tower-level formulas are already shared across all repos in a tower (stored in the `formulas` table in Dolt). This means a formula improvement published from repo A is immediately available to repo B.

**Cross-repo dependency awareness.** The steward already handles cross-repo dependencies via the bead graph. If `web-b7d0` depends on `api-8a01`, the steward won't assign `web-b7d0` until `api-8a01` is closed. This works unchanged in k8s because both repos' beads are in the same Dolt database.

### 4.4 Trust gradients

Trust is built over time through demonstrated competence. The system should track and enforce trust levels.

**Per-repo trust levels:**

| Level | Name | Behavior |
|-------|------|----------|
| 0 | **Supervised** | Human must approve every merge (`human.approve` gate on merge step). All reviews require human sign-off. |
| 1 | **Assisted** | Sage review is sufficient for tasks/bugs. Epics require human approval. |
| 2 | **Autonomous** | Full auto-merge for tasks/bugs. Epics need human approval on the plan step only. |
| 3 | **Trusted** | Full auto-merge for everything. Human is notified but not gated. |

**How trust escalates.** The trust level for a repo starts at 0 (supervised). After N consecutive successful merges with no reverts and no human overrides, the archmage can promote:

```bash
spire trust promote web --level 1  # "I've reviewed 20 merges, they're good"
```

The trust level is stored in the `repos` table and read by the formula engine. The formula adjusts its behavior based on trust level:

```toml
# task-default.formula.toml
[steps.merge]
kind = "op"
action = "git.merge_to_main"
needs = ["review"]

# At trust level 0, this step requires human approval
[steps.merge.when]
all = [
  { left = "steps.review.outputs.outcome", op = "eq", right = "merge" },
]

# The executor checks trust level and injects a human.approve
# gate if trust < 2. This is a runtime policy, not formula logic.
```

The trust check is a runtime policy enforcement in the executor (ZFC-compliant: it's a policy gate, not a judgment). The formula doesn't change; the executor adds or removes the gate.

**Trust metrics.** ClickHouse tracks trust-relevant signals:
- Consecutive successful merges per repo
- Revert rate per repo (how often does a human revert an agent merge?)
- Override rate per repo (how often does a human override an agent decision?)
- Review quality score (what percentage of sage reviews match human reviews when both are available?)

### 4.5 Spec quality scoring

The biggest failure mode at scale: bad specs produce bad work. The system should detect this early.

**Spec quality signals (all mechanical, ZFC-compliant):**
- Word count < 50: likely underspecified
- No acceptance criteria: likely ambiguous
- References non-existent files or functions: likely outdated
- Has open questions or TODOs: likely not ready
- Linked design bead is empty or has no comments: design not done

**Implementation.** A `spec.lint` action (new opcode) runs before the plan step in every formula. It checks structural properties of the bead description and linked design bead. If the spec fails lint, the wizard sets the bead to `needs-human` with a structured explanation:

```
Spec lint failed for spi-x2mk.6:
  - Description is 23 words (minimum: 50)
  - No acceptance criteria found (expected "## Acceptance criteria" section)
  - Linked design bead spi-x2mk-d1 has no comments
```

This saves agent compute by catching bad specs before they burn $3 on a planning step that will produce garbage.

---

## 5. The design-to-execution bridge

This is the most important section for making non-engineers excited about Spire. The design-to-execution bridge is the flow from "PM writes a spec" to "code lands on main."

### 5.1 The PM's interface

A PM should never need to touch the terminal. Their interface is:

1. **Write a design bead** via the web dashboard (rich text editor, markdown preview)
2. **See agent progress** via the epic drill-down view
3. **Receive notifications** via Slack when milestones hit
4. **Review outcomes** via the diff viewer (see what code changed, in plain English)

**Design bead authoring in the web dashboard:**

```
+------------------------------------------------------------------+
| New Design Bead                                                   |
+------------------------------------------------------------------+
| Title: [ Auth system overhaul                                  ] |
|                                                                   |
| Description:                                                     |
| +--------------------------------------------------------------+ |
| | ## Problem                                                    | |
| | Users can't log in with Google OAuth. Currently we only       | |
| | support email/password.                                       | |
| |                                                               | |
| | ## Desired outcome                                            | |
| | - Google OAuth login on /login page                          | |
| | - JWT token refresh (15-minute access, 7-day refresh)        | |
| | - MFA support (TOTP)                                         | |
| |                                                               | |
| | ## Constraints                                                | |
| | - Must use the existing session store (Redis)                | |
| | - Must work with our Cloudflare WAF rules                    | |
| | - No breaking changes to the existing /api/auth endpoints    | |
| |                                                               | |
| | ## Acceptance criteria                                        | |
| | - [ ] Google OAuth login works end-to-end                    | |
| | - [ ] Token refresh works without user interaction           | |
| | - [ ] MFA enrollment and verification works                  | |
| +--------------------------------------------------------------+ |
|                                                                   |
| Type: [epic ▼]    Priority: [P1 ▼]    Repo: [web-app ▼]        |
|                                                                   |
| [Create Design Bead]                                             |
+------------------------------------------------------------------+
```

This creates a design bead via the Dolt store API. The PM fills in the description, the system files the bead.

**Closing the design bead (approval gate).** Once the PM is satisfied with the design, they click "Approve Design" in the web dashboard. This:
1. Closes the design bead
2. Creates an epic bead linked to it via `discovered-from` dependency
3. The steward picks up the epic and assigns a wizard
4. The wizard's `design-check` step validates the linkage
5. The wizard's `plan` step generates subtasks from the design

The PM sees the epic appear on their dashboard within minutes, with a plan and progress tracking.

### 5.2 The diff explanation

When an agent merges code, the PM can't read a diff. They need a plain-English explanation of what changed.

**Implementation.** The `bead.finish` action (the close step in every formula) generates a merge summary as a comment on the bead:

```
## Merge Summary

**What changed:**
- Added Google OAuth login flow to /login page (new: /pkg/auth/oauth.go, modified: /pages/login.tsx)
- Added JWT token refresh middleware (new: /pkg/auth/refresh.go, modified: /middleware/auth.go)
- Added MFA TOTP enrollment and verification (new: /pkg/auth/mfa.go, /pages/settings/mfa.tsx)

**Tests added:**
- 12 new unit tests in /pkg/auth/*_test.go
- 3 new integration tests in /test/auth/

**Files changed:** 14 files, +1,240 lines, -83 lines

**Review:** Approved by sage (round 1/1). No arbiter escalation.
**Cost:** $38.50 (plan: $4.20, implement: $28.30, review: $6.00)
**Duration:** 4h 20m (filed to merged)
```

This summary is visible in the web dashboard's epic drill-down view and in the Slack notification.

### 5.3 The feedback loop

When a PM is unhappy with the result, they need a way to say so without touching code.

**Mechanism: the `spire send` command from the web dashboard.**

The PM clicks "Request Changes" on a completed bead and types natural language:

```
"The OAuth login button should be on the right side of the login form,
not the left. Also, the token TTL should be 30 minutes, not 15."
```

This creates a message bead routed to the wizard (via `ref:<bead-id>` label). The steward detects the feedback and re-opens the bead. A new wizard is summoned with the feedback as context. The cycle repeats.

**Trust signal.** Every PM-initiated reopen is tracked. High reopen rates on a specific type of work indicate either bad specs or bad formula behavior. The analytics dashboard surfaces this:

```
Reopen Rate by Epic (last 30 days)
  spi-x2mk "Auth overhaul"     0 reopens (clean execution)
  web-c3d1  "Dashboard redesign" 3 reopens (spec clarity issue)
  api-f7g2  "API pagination"    1 reopen (review missed edge case)
```

---

## 6. What breaks at scale

### 6.1 Dolt connection saturation

**Problem.** At 50 concurrent agents, each holding a MySQL connection, plus the steward, syncer, and dashboard, you have ~55 connections. Dolt's default `max_connections: 100` handles this. At 100+ agents, connections become the bottleneck.

**Solution.** Connection pooling via ProxySQL as a sidecar to the Dolt StatefulSet:

```yaml
# Dolt pod with ProxySQL sidecar
containers:
  - name: dolt
    ports:
      - containerPort: 3306
  - name: proxysql
    image: proxysql/proxysql:latest
    ports:
      - containerPort: 6033  # client-facing port
    volumeMounts:
      - name: proxysql-config
        mountPath: /etc/proxysql.cnf
```

Agent pods connect to port 6033 (ProxySQL). ProxySQL multiplexes to Dolt's port 3306 with connection reuse. This scales to 500+ agent pods with 50 backend connections.

### 6.2 Steward scheduling throughput

**Problem.** The steward runs one cycle every N minutes. At 50 agents with 100 queued beads, each cycle needs to: query ready beads, load roster, assign work, detect review-ready beads, check health. If the cycle takes longer than the interval, cycles overlap.

**Solution.** Split the steward cycle into independent goroutines:

```go
// In cluster mode, run these concurrently
go assignmentLoop(ctx, interval)     // ready beads → idle agents
go reviewLoop(ctx, interval)         // detect review-ready → spawn sage
go healthLoop(ctx, interval)         // stale/timeout detection
go hookedStepLoop(ctx, interval)     // hooked-step sweep
go notificationLoop(ctx, interval)   // Slack/email notifications
```

Each loop has its own interval and error isolation. The assignment loop runs more frequently (30s) than the health loop (2m).

### 6.3 Git clone latency

**Problem.** Every agent pod clones the target repo at startup. For large repos (1GB+), this adds 2-5 minutes to every bead. At 50 concurrent agents starting simultaneously (wave dispatch), this creates a thundering herd on GitHub's API.

**Solution: shared repo cache via PVC.**

A persistent volume holds a bare clone of each registered repo. Agent pods start by copying from the cache (local filesystem copy, not network clone), then fetch only the delta.

```yaml
# repo-cache PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: spire-repo-cache
  namespace: spire
spec:
  accessModes: [ReadOnlyMany]  # ROMnay — multiple pods read concurrently
  resources:
    requests:
      storage: 20Gi
```

A cache-refresher CronJob runs `git fetch` on each cached repo every 5 minutes. Agent pods mount the cache read-only and do a local `git clone --reference /cache/<repo>` which reuses objects from the cache.

**Impact.** Clone time drops from 2-5 minutes to 5-15 seconds for most repos.

### 6.4 ClickHouse write amplification

**Problem.** At 50 concurrent agents, each emitting OTel events every few seconds, the steward's OTel receiver processes hundreds of events per second. Writing each event individually to ClickHouse is inefficient.

**Solution.** The `ClickHouseWriter` batches writes. Events are accumulated in memory and flushed every 5 seconds or when the batch reaches 1000 events, whichever comes first.

```go
type ClickHouseWriter struct {
    conn      clickhouse.Conn
    batch     []ToolEvent
    mu        sync.Mutex
    flushTick *time.Ticker
}

func (w *ClickHouseWriter) Submit(fn func(*sql.Tx) error) error {
    // Extract events from the transaction function
    // Buffer them
    // Flush when batch is full or ticker fires
}
```

### 6.5 DoltHub sync conflicts at scale

**Problem.** With multiple developers pushing to DoltHub and the cluster syncer also pushing, merge conflicts become more frequent. The field-level ownership model handles most cases, but edge cases emerge: two developers edit the same bead's description simultaneously.

**Solution.** The syncer detects conflicts and creates a `conflict` bead:

```
Sync conflict on bead spi-x2mk:
  Local: description = "Auth system overhaul (updated spec)"
  Remote: description = "Auth system overhaul v2"
  Resolution: user wins (content fields are user-authoritative)
```

The conflict bead is informational (type=chore, auto-closed). It ensures the archmage knows when their edits were overridden. For truly irreconcilable conflicts (rare), the syncer creates a `needs-human` bead and halts sync for that table until resolved.

### 6.6 Agent pod resource contention

**Problem.** 50 agent pods running Claude Code simultaneously consume significant cluster resources. Each pod runs a Claude Code subprocess that can spike to 2GB memory and 2 CPU cores during code generation.

**Solution: resource quotas and priority classes.**

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: spire-agents
  namespace: spire
spec:
  hard:
    pods: "60"
    requests.cpu: "40"
    requests.memory: "80Gi"
    limits.cpu: "80"
    limits.memory: "160Gi"
```

```yaml
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: spire-agent-p0
value: 1000
description: "P0 agent work (critical bugs, production issues)"
---
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: spire-agent-p1
value: 500
description: "P1 agent work (normal features and tasks)"
---
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: spire-agent-p2
value: 100
description: "P2+ agent work (low priority, nice-to-have)"
```

The steward sets the priority class on each agent pod based on the bead's priority. P0 beads preempt P2 beads when the cluster is full.

### 6.7 Steward single point of failure

**Problem.** The steward is a single-replica Deployment. If it dies, no new work is assigned until k8s restarts it (typically 30-60 seconds). Agents in flight continue independently.

**Mitigation (v1).** This is acceptable for v1. The steward is stateless (all state in Dolt). k8s restarts it quickly. In-flight agents are unaffected.

**Solution (v2): leader-elected steward.**

```go
// Use controller-runtime's leader election
mgr, _ := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
    LeaderElection:   true,
    LeaderElectionID: "spire-steward-leader",
})
```

Two steward replicas, one active (leader), one standby. Failover in <5 seconds.

---

## 7. Implementation phases

### Phase 1: Core cluster (weeks 1-4)

**Goal:** A working cluster that executes beads. No dashboard, no notifications. The TUI + `kubectl` are the only interfaces.

| Task | Priority | Depends on |
|------|----------|------------|
| `agent.K8sBackend` implementing `agent.Backend` interface | P0 | - |
| `olap.ClickHouseWriter` implementing same write interface as DuckDB | P0 | - |
| Steward `--cluster` flag: start OTLP receiver, connect to ClickHouse, use K8sBackend | P0 | K8sBackend, ClickHouseWriter |
| ClickHouse StatefulSet in Helm chart | P0 | - |
| Repo cache PVC + cache-refresher CronJob | P1 | - |
| Network policies for agent pods | P1 | - |
| Resource profiles (standard/heavy/review) in SpireConfig | P1 | - |
| Pod priority classes | P2 | - |
| End-to-end smoke test: file bead locally, push, agent executes in cluster, bead closes | P0 | All P0 items |

**Acceptance criteria:**
- `helm install` brings up dolt + clickhouse + steward + syncer
- `spire file` + `spire push` from laptop results in an agent pod running in the cluster
- Agent pod executes formula, merges code, bead closes
- `spire pull` from laptop shows the closed bead
- OTel events visible in ClickHouse

### Phase 2: Observability (weeks 5-8)

**Goal:** Humans can see what agents are doing without kubectl.

| Task | Priority | Depends on |
|------|----------|------------|
| Web dashboard: tower overview view | P0 | Phase 1 |
| Web dashboard: epic drill-down view | P0 | Phase 1 |
| Web dashboard: agent activity view | P1 | Phase 1 |
| Web dashboard: analytics dashboard | P1 | Phase 1 |
| Prometheus ServiceMonitor for steward metrics | P1 | Phase 1 |
| Grafana dashboard templates (importable JSON) | P2 | ServiceMonitor |
| Slack notification integration | P1 | Phase 1 |
| Merge summary generation on bead.finish | P1 | Phase 1 |
| Diff explanation for PMs | P2 | Merge summary |
| Ingress for dashboard + API | P1 | Dashboard |

**Acceptance criteria:**
- Web dashboard shows all active work, agent status, and analytics
- Slack notifications fire for stuck agents and completed epics
- Merge summaries appear as comments on completed beads
- Prometheus can scrape steward metrics

### Phase 3: Intelligence (weeks 9-12)

**Goal:** The system learns from its history and gets measurably better over time.

| Task | Priority | Depends on |
|------|----------|------------|
| `learnings` table in Dolt schema | P0 | Phase 1 |
| Recovery formula writes to `learnings` table | P0 | learnings table |
| Prior-learning lookup queries `learnings` table | P0 | learnings table |
| Learning effectiveness tracking (applied_count, success_count) | P0 | learnings table |
| Formula performance reports (weekly, from ClickHouse) | P1 | Phase 2 |
| Formula A/B testing (canary percentages) | P2 | Performance reports |
| `spec.lint` action for spec quality checking | P1 | - |
| Trust levels in repos table | P1 | - |
| Trust-based merge gating in executor | P1 | Trust levels |
| Cross-repo learning lookup | P2 | learnings table |

**Acceptance criteria:**
- Recovery beads write structured learnings to the `learnings` table
- Subsequent recovery beads for the same failure class use prior learnings
- Formula performance report shows success rate trends
- Spec lint catches underspecified beads before they burn compute
- Trust levels gate auto-merge on new repos

### Phase 4: PM experience (weeks 13-16)

**Goal:** A non-engineer can file work and track it to completion without any help.

| Task | Priority | Depends on |
|------|----------|------------|
| Design bead authoring in web dashboard | P0 | Phase 2 dashboard |
| Design approval flow (close design bead, create epic) | P0 | Design authoring |
| PM feedback mechanism (request changes from dashboard) | P1 | Phase 2 dashboard |
| Email digest for engineering managers | P2 | Phase 2 analytics |
| Mobile-responsive dashboard | P2 | Phase 2 dashboard |
| Onboarding wizard for new tower setup | P2 | Dashboard |

**Acceptance criteria:**
- PM can write a design bead in the web dashboard
- PM can approve the design and see the epic created
- PM can track progress via the epic drill-down view
- PM can request changes on completed work via the dashboard
- Weekly email digest arrives with metrics

---

## 8. Six months in: the emergent system

After 6 months of continuous operation with ~5,000 completed beads across 10 repos, the system has emergent properties that no individual component was designed for:

### 8.1 The institutional memory

The `learnings` table contains 800+ structured recovery learnings. The system has encountered and recovered from every common failure mode: build failures, empty implements, review deadlocks, merge conflicts, flaky tests, API rate limits, timeout cascades.

New agents don't start from zero. They start from the accumulated wisdom of 5,000 prior executions. A build failure in repo A triggers a recovery that finds "this exact failure class was solved 47 times in repos B and C by adding a missing dependency to the install step." The fix is applied in seconds, not minutes.

### 8.2 The formula tuning flywheel

Formula v3.0 had a 78% success rate. After 4 months of weekly performance reports and iterative tuning, formula v3.7 has a 94% success rate and costs 30% less per bead.

The improvements are specific and data-driven:
- v3.1: Added spec lint to catch underspecified beads (+5% success rate)
- v3.2: Reduced max_review_rounds from 3 to 2 for bugs (-15% cost per bug, same success rate)
- v3.3: Added dependency-aware wave scheduling for epics (-20% epic duration)
- v3.4: Added inline prompt refinement for the plan step (+3% success rate)
- v3.5: Switched review model from Opus to Sonnet for non-epic beads (-25% review cost, same approval rate)

Every change is backed by data from ClickHouse. No guessing. No vibes. The analytics dashboard shows the exact before/after for every formula version.

### 8.3 The trust gradient in action

Repo `web-app` started at trust level 0 (supervised). After 200 successful merges with zero reverts over 3 months, the archmage promoted it to level 2 (autonomous). Tasks and bugs auto-merge without human approval. The PM still sees every merge via Slack notifications, but doesn't need to approve anything.

Repo `api-server` is still at trust level 1 (assisted). It handles payment processing, and the team wants human approval on anything touching the billing module. The trust level is per-repo, not per-tower — different repos can have different risk profiles.

### 8.4 The PM's new workflow

The PM files 3-5 design beads per week. Each takes 15 minutes: describe the problem, the desired outcome, the constraints, and the acceptance criteria. The PM approves designs, reviews merge summaries, and occasionally requests changes.

The PM has never opened a terminal. They have never seen a line of code. But they track 10 active epics across 4 repos, know the exact cost and timeline of each, and can request changes with natural language.

The PM's total weekly time investment: 2 hours. This replaces: 5 hours of standups, 3 hours of Jira grooming, 2 hours of "can you check on this" Slack messages.

### 8.5 The cost curve

Month 1: $8,000/month (50 agents, learning phase, high failure rate, many retries)
Month 3: $5,500/month (formula tuning, better specs, fewer retries)
Month 6: $4,200/month (cached learnings, optimized formulas, trust-gated auto-merge reducing review overhead)

The cost per merged bead drops from $16 to $8.40. The output (beads merged per week) increases from 50 to 120. Cost efficiency improves 4x.

### 8.6 The system's personality

After thousands of executions, the system develops observable patterns:
- It knows which repos are flaky (high retry rate) and adjusts timeouts automatically
- It knows which types of tasks cost the most and flags outliers
- It knows which failure classes are common and pre-loads learnings before they're needed
- It knows which formula versions work best for which repo and recommends formula pinning

None of this is "AI learning" in the neural network sense. It's structured data accumulation with mechanical queries. The LLM provides judgment on individual beads. The system provides the memory and patterns that make each judgment better informed.

---

## Appendix: key design decisions

| Decision | Rationale |
|----------|-----------|
| ClickHouse over DuckDB in cluster | Multi-writer, server-based, same columnar model. DuckDB's single-process lock is a dealbreaker. |
| Steward drives scheduling, not operator | Steward has full Dolt access and formula awareness. Operator CRDs add a lossy translation layer. |
| Web dashboard is server-rendered Go + htmx | Ships in the same binary, no frontend build step, fast first paint, works behind proxies. |
| Trust levels are per-repo, not per-tower | Different repos have different risk profiles. Payment processing != marketing site. |
| Formula A/B testing uses labels, not CRDs | Labels are lightweight, queryable, and already part of the bead data model. |
| Repo cache is a PVC, not a node-level volume | PVC works with any storage class. Node volumes require DaemonSets and node affinity. |
| Learnings table is in Dolt, not ClickHouse | Learnings are operational data (queried at recovery time), not analytical data. They need dolt's versioning. |
| Spec lint is an executor action, not a formula step | It runs in every formula without modifying the formula TOML. ZFC-compliant policy enforcement. |
| PM interface is web-only, no CLI | PMs don't use terminals. A web form for design beads removes the entire onboarding barrier. |
| Notifications are push (Slack/email), not pull-only (dashboard) | PMs check Slack constantly. They don't check dashboards unless prompted. |
