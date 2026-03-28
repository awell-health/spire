# Spire Kubernetes Cluster — Architecture Review

**Date**: 2026-03-21
**Scope**: Full review of all K8s manifests, Go binaries, CRDs, operator controllers, bead lifecycle, and data flow.

> **Note (2026-03-28):** This review predates the end-state naming convention.
> "Artificer" below refers to what is now the **wizard** (orchestration) and
> **sage** (review). "Workshop pod" is now **wizard pod**. "Sidecar" (per-agent)
> is now **familiar**. The `cmd/spire-artificer/` binary has been removed.

---

## Actors

| Actor | Pod | RPG Name | Lifecycle | Purpose |
|-------|-----|----------|-----------|---------|
| **Dolt** | `spire-dolt` | Archive | Static | Shared SQL database (Dolt). Single source of truth for all beads. PVC-backed (5Gi). |
| **Steward** | `spire-steward` (container 1) | Mayor | Static | Coordinator loop. Every 2 min: find ready work, load roster, round-robin assign, detect stale/timed-out agents, route reviews. |
| **Steward Sidecar** | `spire-steward` (container 2) | Familiar | Static | LLM (Sonnet) message processor. Interprets human messages semantically, applies directives, re-prioritizes, tracks conditions. |
| **Operator** | `spire-operator` | — | Static | K8s controller-runtime. Three loops: BeadWatcher (dolt to SpireWorkload CRs), WorkloadAssigner (workloads to agents), AgentMonitor (agents to pods). |
| **Wizard** | `spire-agent-*` (dynamic) | Wizard | One-shot | AI agent (Claude Code) that does actual work. Claim, focus, design, implement, review, merge. Exits when done. |
| **Artificer** | `spire-workshop-*` (dynamic) | Artificer | One-shot | Code review and merge orchestrator. For epics: watches children, reviews branches, merges. For standalone review: one-shot verdict. |
| **Wizard Sidecar** | Same pod as wizard/artificer (container 2) | Familiar | Paired | Polls inbox (10s), writes to `/comms/inbox.json`, monitors wizard health, heartbeats, accepts STOP/STEER/PAUSE commands. |
| **Human** | Dev machine | Archmage | External | Product engineer. Files work, messages agents, monitors board, summons/dismisses wizards. |

## Pod Topology

### Static (always running, 3 deployments)

```
spire-dolt          — shared database, PVC-backed
spire-steward       — coordinator (steward container + LLM sidecar container)
spire-operator      — k8s controllers (BeadWatcher + WorkloadAssigner + AgentMonitor)
```

### Dynamic (created on demand by operator, 0-N pods)

```
spire-agent-*       — wizard + sidecar    (for tasks/bugs/features/chores)
spire-workshop-*    — artificer + sidecar  (for epics)
spire-review-*      — artificer --mode=review + sidecar (for standalone reviews)
```

All dynamic pods are `RestartPolicy: Never` (one-shot). They share `/comms` (EmptyDir) between main container and sidecar for filesystem-based coordination.

## Connectivity

All pods connect to `spire-dolt.spire.svc:3306` (headless ClusterIP service). No pod-to-pod communication except through the shared dolt database and the `/comms` EmptyDir within a pod.

```
                    ┌────────────────────┐
                    │   spire-dolt:3306  │
                    │   (PVC: 5Gi)       │
                    └────────┬───────────┘
                             │
          ┌──────────────────┼──────────────────────┐
          │                  │                      │
   spire-steward      spire-operator        spire-agent-* (dynamic)
   ├─ steward         ├─ BeadWatcher        ├─ wizard
   └─ sidecar(LLM)   ├─ WorkloadAssigner   └─ sidecar
                      └─ AgentMonitor
```

Environment variables on all pods:
- `DOLT_HOST=spire-dolt.spire.svc`
- `DOLT_PORT=3306`
- `BEADS_DOLT_SERVER_HOST=spire-dolt.spire.svc`
- `BEADS_DOLT_SERVER_PORT=3306`

## Bead Taxonomy

Every entity in the system is a bead in the shared dolt database, differentiated by type and labels.

| Bead Kind | Created By | Key Labels | Lifecycle |
|-----------|-----------|------------|-----------|
| **Work bead** (task/bug/feature/chore) | `spire file` | (none special) | open → in_progress → closed |
| **Epic bead** | `spire file -t epic` | auto-syncs to Linear | open → in_progress → closed (when all children done) |
| **Child task** | `bd create --parent <epic>` | parent relationship | same as work bead |
| **Molecule (workflow instance)** | `spire focus` (auto on first focus) | `workflow:<task-id>` | epic with 4 children |
| **Molecule step** | Children of molecule | `workflow:<task-id>` | design, implement, review, merge (closed sequentially) |
| **Message** | `spire send` | `msg`, `from:<agent>`, `to:<agent>`, `ref:<bead-id>` | open → closed (via `spire read`) |
| **Agent registration** | `spire register` | `agent`, `name:<name>` | open while agent active |
| **Alert** | Steward health check | `alert` | created on stale detection |
| **Template** | `bd cook --persist` | `template` | persistent recipe for molecules |

**Bead volume per task**: When a wizard focuses on a single task, `spire focus` creates 5 new beads (1 molecule epic + 4 step children). Every `spire send` creates a message bead. A single task's lifecycle easily generates 10-15 beads.

## Operator Controllers

The operator runs three independent control loops, each on a 30-second interval.

### BeadWatcher (`operator/controllers/bead_watcher.go`)

Bridges the dolt database to the Kubernetes CRD world.

```
Dolt (bd ready --json)  →  SpireWorkload CRs
Dolt (bd list --status=closed)  →  SpireWorkload status: Done
```

- Reads `bd ready --json` to discover beads with no open blockers
- Creates a `SpireWorkload` CR for each new ready bead
- Extracts prefix from bead ID (e.g. `spi-` from `spi-a3f8`) for agent matching
- Marks workloads `Done` when their bead is closed in dolt

### WorkloadAssigner (`operator/controllers/workload_assigner.go`)

Matches pending SpireWorkloads to available SpireAgents.

- Sorts pending workloads by priority (lower = more urgent)
- For each: finds an agent that is online, under maxConcurrent, and prefix-compatible
- Sets workload status to `Assigned`, adds bead ID to agent's `CurrentWork[]`
- **Staleness**: after 4h assigned → sends reminder message, marks `Stale`. After 6h → unassigns back to `Pending`

### AgentMonitor (`operator/controllers/agent_monitor.go`)

Manages the actual pods for managed agents.

- **External agents**: tracks heartbeats via `lastSeen`, marks `Offline` after timeout
- **Managed agents**: for each bead in `CurrentWork[]`, ensures a pod exists
  - Routes by workload type:
    - `epic` → workshop pod (artificer + sidecar)
    - `review` → review pod (artificer --mode=review --once + sidecar)
    - default → wizard pod (wizard + sidecar)
  - Reaps completed/failed pods, removes bead from `CurrentWork[]`
  - Deletes orphan pods for work no longer assigned
- Injects Anthropic API key via SpireConfig token routing (prefers `heavy` token for artificer)

## CRDs

### SpireAgent

```yaml
spec:
  mode: managed|external
  capabilities: ["Go", "TypeScript"]
  prefixes: ["spi-", "pay-"]
  token: default|heavy        # which Anthropic API key
  maxConcurrent: 1
  image: spire-agent:dev
  repo: https://github.com/...
  repoBranch: main
status:
  phase: Idle|Working|Stale|Offline|Provisioning
  currentWork: ["spi-a3f8"]
  completedCount: 12
```

### SpireWorkload

```yaml
spec:
  beadId: spi-a3f8
  title: "Fix auth refresh"
  priority: 1
  type: task|bug|feature|epic|review
  prefixes: ["spi-"]
status:
  phase: Pending|Assigned|InProgress|Done|Stale|Failed
  assignedTo: wizard-1
  attempts: 1
```

### SpireConfig

```yaml
spec:
  polling:
    interval: "2m"
    staleThreshold: "4h"
    reassignThreshold: "6h"
  tokens:
    default: {secret: spire-credentials, key: ANTHROPIC_API_KEY_DEFAULT}
    heavy: {secret: spire-credentials, key: ANTHROPIC_API_KEY_HEAVY}
  defaultToken: default
```

## Data Flow: End-to-End

### Filing and assignment

```
Human: spire file "Fix bug" -t bug -p 1
                │
                ▼
         ┌──────────┐
         │   Dolt    │  bead spi-xxxx created (status: open)
         └────┬─────┘
              │ bd ready --json (every 30s)
              ▼
     ┌────────────────┐
     │  BeadWatcher   │  creates SpireWorkload CR (phase: Pending)
     └────────┬───────┘
              │ (every 30s)
              ▼
     ┌────────────────┐
     │ WorkloadAssigner│  matches to SpireAgent, sets phase: Assigned
     └────────┬───────┘
              │ (every 30s)
              ▼
     ┌────────────────┐
     │  AgentMonitor  │  creates wizard pod (wizard + sidecar containers)
     └────────────────┘
```

### Wizard work cycle

```
Wizard pod starts:
  1. agent-entrypoint.sh: clone repo, seed .beads/
  2. spire claim <bead-id>         → verify + set in_progress
  3. spire focus <bead-id>         → assemble context, pour molecule (5 new beads)
  4. Work through molecule steps:
     a. Design: read spec or create one
     b. Implement: create worktree, write code, run tests
     c. Review: self-review, fix issues
     d. Merge: merge branch, run tests, close steps
  5. bd close <bead-id>
  6. spire send <sender> "done"
  7. Pod exits (RestartPolicy: Never)
```

### Review loop

```
Wizard finishes implementation:
  bd update <id> --add-label review-ready
              │
              ▼
     Steward cycle: detectReviewReady()
              │ creates SpireWorkload type=review
              ▼
     WorkloadAssigner → AgentMonitor → review pod (artificer --mode=review --once)
              │
              ▼
     Artificer reviews code:
       ├─ approved      → wizard merges
       ├─ request_changes → adds review-feedback label
       │                    → steward detects → messages wizard → wizard fixes → re-labels review-ready
       └─ rejected      → bead closed with reason
```

### Health monitoring (two layers)

**Layer 1 — Steward (fast, minutes)**:
- Checks `updated:` label timestamp on in_progress beads every 2 min
- After `agent.stale` (default 10m from spire.yaml): logs STALE warning
- After `agent.timeout` (default 15m): kills the pod via `kubectl delete pod`

**Layer 2 — WorkloadAssigner (slow, hours)**:
- Checks `assignedAt` on SpireWorkloads
- After `staleThreshold` (default 4h): sends reminder message, marks Stale
- After `reassignThreshold` (default 6h): unassigns work back to Pending for re-matching

## Spire Commands by Actor

| Command | Human | Steward | Wizard | Artificer | Steward Sidecar | Purpose |
|---------|:-----:|:-------:|:------:|:---------:|:---------------:|---------|
| `spire init` | X | | | | | Register a repo with prefix |
| `spire up` / `down` | X | | | | | Start/stop local dolt + daemon |
| `spire file` | X | | | | | Create new work |
| `spire board` | X | | | | | View work queue (TUI or JSON) |
| `spire claim <id>` | X | | X | | | Verify not closed/owned, set in_progress |
| `spire focus <id>` | X | | X | | | Assemble context, pour molecule on first focus |
| `spire send <to> "msg"` | X | X | X | | X | Send message bead |
| `spire collect` | X | | | | | Check inbox |
| `spire read <id>` | X | | X | | | Mark message as read (close it) |
| `spire summon N` | X | | | | | Spin up N wizard pods |
| `spire dismiss N` | X | | | | | Kill N wizard pods |
| `spire steward` | | X | | | | Main coordination loop |
| `bd ready` | | X | | | | Find beads with no open blockers |
| `bd list` | X | X | X | X | X | Query beads by label/status |
| `bd show <id>` | X | | X | X | X | Inspect a single bead |
| `bd children <id>` | | | X | X | | List children of epic/molecule |
| `bd mol progress` | | | X | | | Check workflow step completion |
| `bd close <id>` | | | X | X | | Close a step or bead |
| `bd update --add-label` | | X | X | X | X | Modify bead labels |
| `bd dolt commit` | | X | | | | Commit working set on shared server |

The operator uses **no spire/bd commands** — it operates entirely through the K8s API (controller-runtime client) against SpireAgent, SpireWorkload, and SpireConfig CRs, plus `bd ready --json` in the BeadWatcher.

## Resource Budget

| Component | CPU Request | CPU Limit | Memory Request | Memory Limit |
|-----------|-----------|---------|--------------|------------|
| dolt | 100m | 500m | 256Mi | 512Mi |
| steward | 50m | 200m | 128Mi | 256Mi |
| steward sidecar | 50m | 200m | 128Mi | 512Mi |
| operator | 50m | 200m | 128Mi | 256Mi |
| wizard (dynamic) | per SpireAgent spec | | | |
| artificer (dynamic) | 100m | — | 256Mi | — |

## Persistent Storage

| PVC | Size | Access | Used By | Content |
|-----|------|--------|---------|---------|
| `spire-beads-data` | 5Gi | RWO | dolt | Dolt database (all beads state) |
| `spire-steward-data` | 1Gi | RWO | steward + sidecar | Steward working dir + config |

Dynamic pods use EmptyDir volumes only (ephemeral).

## Configuration Seeding

The `beads-seed` ConfigMap provides `.beads/` config to all pods via init containers:
- `metadata.json` — database backend, server mode, project_id
- `config.yaml` — beads CLI routing mode
- `routes.jsonl` — label routing rules

Init container (`seed-beads`): Alpine, copies ConfigMap files to `/data/.beads/`, initializes git repo. Runs before main containers start.

## What Would Happen If...

### A wizard gets stuck mid-implementation
Two layers catch it. Steward (fast): kills the pod after 15 min of no updates. WorkloadAssigner (slow): unassigns after 6 hours and returns work to Pending. No work gets permanently stuck.

### Two agents try to claim the same bead
`spire claim` checks the `owner:` label and `status` before updating. Dolt serializes writes. Second claim fails with "already in progress (owner: X)". Steward's `findBusyAgents()` also skips owned beads during assignment.

### The dolt server restarts
PVC persists data, so the database survives. But the project_id may change (see Issues section). In-flight wizard pods have stale metadata.json — their bd commands fail until the pod timeout kills them and new pods are created with fresh init containers.

### Someone files work from their dev machine while agents run in k8s
Works. Human's `spire file` writes to the shared dolt server. BeadWatcher reads from the same server within 30s. New bead gets a SpireWorkload, gets assigned, wizard pod starts.

### An epic has tasks across multiple repo prefixes
Supported. Epic `spi-epic1` can have children `pay-task1`, `doc-task2`. Each child's SpireWorkload gets the appropriate prefix. WorkloadAssigner matches by prefix — `pay-task1` goes to the agent with `prefixes: ["pay-"]`.

### More ready beads than available agents
Steward assigns round-robin until all agents are busy. Remaining beads stay in `bd ready`. WorkloadAssigner does the same at the CRD level. When a wizard finishes, the agent becomes available and the next-highest-priority bead gets assigned.

### A review gets rejected
Artificer adds `review-feedback` label. Steward's `detectReviewFeedback()` messages the wizard owner. Wizard makes fixes, re-labels `review-ready`. Cycle repeats up to 3 rounds (`ARTIFICER_MAX_REVIEW_ROUNDS`) before escalation.
