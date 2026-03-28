# Spire Architecture

Spire is an AI agent coordination system. It manages a shared work graph
(beads), routes work to autonomous agents, and synchronizes state across
local machines and Kubernetes clusters via DoltHub.

> **Living document.** Updated 2026-03-26. Where the current implementation
> differs from the target, inline callouts note the gap.

## Deployment Modes

Spire runs in three configurations. All share the same work graph, sync
protocol, and agent protocol.

**Local** -- Developer laptop. The `spire` CLI manages a local Dolt server
and daemon. Agents run as local processes via `spire summon` (each wizard
is a `spire wizard-run` subprocess working in its own git worktree).

**Cluster** -- Kubernetes. The operator watches CRDs and spins up agent
pods. A shared Dolt Deployment holds the work graph. A syncer pod handles
DoltHub push/pull.

**Hybrid** -- Local CLI + remote cluster. DoltHub bridges the two: the
developer pushes beads, the cluster pulls and executes, status flows back
via DoltHub.

```
  Local                  DoltHub                Cluster
  -----                  -------                -------
  spire push ──────────> remote <────────────── syncer pull
  spire pull <────────── remote ──────────────> syncer push
                            |
  Developer B               |
  spire push ──────────> remote
  spire pull <────────── remote
```

No direct connectivity between laptops and cluster is required. DoltHub is
the hub.

## Components

### Spire CLI (`cmd/spire/`)

Single Go binary. Entry point for all operations.

| Category    | Commands                                           |
|-------------|----------------------------------------------------|
| Setup       | `tower create`, `tower attach`, `repo add`, `config`, `push`, `pull`, `sync` |
| Lifecycle   | `up`, `down`, `shutdown`, `status`, `doctor`, `version` |
| Work        | `file`, `design`, `spec`, `claim`, `close`, `advance`, `focus`, `grok` |
| Messaging   | `register`, `unregister`, `send`, `collect`, `read`, `inbox` |
| Coordination| `steward`, `board`, `roster`, `summon`, `dismiss`, `watch`, `alert` |
| Execution   | `wizard-run`, `wizard-review`, `wizard-merge`, `execute`, `wizard-epic` |
| Observability| `logs`, `metrics`                                  |
| Integrations| `connect`, `disconnect`, `serve`, `daemon`          |

When invoked with no arguments: prints the command reference.

### Beads / bd

The work graph engine. External dependency
([github.com/steveyegge/beads](https://github.com/steveyegge/beads)).
Spire shells out to the `bd` binary for all work graph mutations.

**Data model (Dolt SQL tables):**

| Table      | Purpose                                                    |
|------------|------------------------------------------------------------|
| `issues`   | id, title, description, status, priority, type, owner, parent, timestamps |
| `labels`   | issue_id, label -- routing (`msg`, `to:<agent>`, `from:<agent>`, `ref:<bead-id>`), metadata (`owner:`, `review-ready`, `feat-branch:`) |
| `deps`     | blocked, blocker -- dependency graph                       |
| `comments` | issue_id, author, body, created_at                         |
| `metadata` | key-value store (project_id, config)                       |
| `repos`    | prefix, repo_url, branch, runtime, registered_by, registered_at |

Key operations: `create`, `update`, `close`, `list`, `show`, `ready`
(returns beads with no open blockers), `dep add`, `children`, `dolt commit`,
`dolt push`, `dolt pull`.

### Dolt Database

SQL database with git-like version control. The shared state layer.

| Context  | How it runs                                               |
|----------|-----------------------------------------------------------|
| Local    | `dolt sql-server` on localhost:3307, managed by `spire up`|
| Cluster  | Dolt Deployment + PVC in the `spire` namespace            |
| DoltHub  | Remote for sync between local and cluster                 |

The database holds ALL shared state: beads, repos, agent registrations,
messages, comments, labels, dependencies.

### Steward (`cmd/spire/steward.go`)

The work coordinator. Runs as `spire steward` (locally or in the steward
pod in k8s). Core responsibility: assigning ready work to agents by
summoning wizards (locally via `spire summon`, in k8s via SpireWorkload CRs).

**Cycle (every N minutes):**

1. Commit local dolt changes
2. Query ready beads (`bd ready`)
3. Load agent roster (derived from the `repos` table in dolt)
4. Assign ready beads to idle agents by summoning wizards (round-robin by priority)
5. Detect standalone tasks ready for review (`review-ready` label)
6. Detect tasks with review feedback for wizard re-engagement
7. Check bead health (stale warning at `agent.stale`, pod kill at `agent.timeout`)

> **Current state (2026-03-26):** In k8s, the operator watches SpireAgent
> CRDs for the roster. Locally, `spire summon` drives agent lifecycle
> directly — no steward assignment needed. Target: the operator reads the
> `repos` table directly and derives agent configurations; SpireAgent CRDs
> are auto-generated or eliminated.

Assignment modes:
- **External agents**: steward sends an assignment message via `spire send`
- **Managed agents (k8s)**: steward updates the SpireWorkload CR; the operator creates the pod

### Steward Sidecar (`cmd/spire-steward-sidecar/`)

LLM-powered message processor that runs alongside the steward in k8s.
Distinct from the familiar (per-agent companion): the steward-sidecar is
an LLM-powered processor specific to the steward, whereas the familiar
(`cmd/spire-sidecar/`) handles messaging and health for wizard pods.
Uses the Anthropic API with tool use to process messages sent to the steward.

**Capabilities (via tools):**
- `list_beads`, `show_bead`, `update_bead`, `create_bead`, `close_bead`
- `send_message`, `steer_wizard`, `add_comment`, `add_dependency`
- `get_roster`, `list_agents_work`

Maintains persistent state (directives, tracking conditions, pending
actions) across session restarts. Automatically checkpoints and restarts
when context usage exceeds a configurable threshold.

### Operator (`operator/`)

Kubernetes controller built on controller-runtime. Three concurrent
control loops:

**BeadWatcher** (`controllers/bead_watcher.go`):
Reads `bd ready --json` from the shared dolt server. Creates SpireWorkload
CRs for new ready beads. Updates workload status when beads close.

**WorkloadAssigner** (`controllers/workload_assigner.go`):
Matches pending SpireWorkloads to available SpireAgents. Sorts by priority.
Checks prefix compatibility and concurrency limits. Marks stale workloads
for reassignment.

**AgentMonitor** (`controllers/agent_monitor.go`):
Tracks agent heartbeats. For managed agents, creates one pod per assigned
bead. Routes by workload type:

| Bead type | Pod type      | Main container     |
|-----------|---------------|--------------------|
| task/*    | Wizard pod    | `agent-entrypoint.sh` (runs Claude Code) |
| epic      | Wizard pod    | `agent-entrypoint.sh` (runs Claude Code) |
| review    | Wizard pod    | `spire wizard-review --once`              |

Reaps completed/failed pods and removes work from the agent's CurrentWork
list.

**CRDs** (`operator/api/v1alpha1/types.go`):

| CRD             | Purpose                                            |
|-----------------|----------------------------------------------------|
| `SpireAgent`    | Registered agent (external or managed), capabilities, prefixes, image, concurrency |
| `SpireWorkload` | Bead assignment: beadId, priority, type, phase lifecycle (Pending -> Assigned -> InProgress -> Done/Stale/Failed) |
| `SpireConfig`   | Cluster singleton: DoltHub remote, polling config, token references, routing rules |

### Agents

#### Wizard (`cmd/spire/wizard.go`, `cmd/spire/executor.go`)

Per-bead orchestrator. Drives the formula lifecycle for a single bead.

**Local**: `spire wizard-run <bead-id>` runs as a background process
spawned by `spire summon`. For epics, the wizard dispatches apprentices
in parallel worktrees and consults sages for review.

**k8s**: Runs in a wizard pod (`agent-entrypoint.sh`). Clones the repo
inside the container.

```
Lifecycle:
  resolve repo -> claim bead -> load formula
  -> for each phase in formula:
     design: validate linked design bead
     plan:   invoke Claude (Opus) to generate subtask breakdown
     implement: dispatch apprentices in parallel waves (worktrees)
     review: dispatch sage for verdict (approve / request changes)
     merge:  ff-only merge staging branch to main
  -> close bead -> write result -> exit
```

Key behaviors:
- Formula-driven: bead type maps to formula (`spire-epic`, `spire-bugfix`, `spire-agent-work`)
- For epics: creates staging branch, dispatches apprentices per wave, merges wave results
- For standalone tasks: dispatches a single apprentice, then reviews and merges
- Branch state file tracks staging/base/repo as single source of truth
- Writes result.json for observability (local: `~/.local/share/spire/wizards/`)

#### Apprentice

Implementation agent. One-shot: receives a task, writes code, exits.

Dispatched by the wizard during the implement phase. Each apprentice
works in an isolated git worktree on a feature branch (`feat/{bead-id}`).

- Reads `spire.yaml` for model, timeouts, test/build/lint commands
- Runs Claude Code with `--dangerously-skip-permissions -p <prompt>`
- Validates output (lint, build, test) before pushing
- Commits and pushes branch for the wizard to merge into staging

#### Sage

Review agent. One-shot: reviews code, returns a verdict.

Dispatched by the wizard during the review phase. Reviews the staging
branch diff against the bead spec.

- Verdicts: `approve`, `request_changes`
- Revision rounds: if changes requested, wizard spawns a review-fix apprentice and re-reviews
- Arbiter escalation: after max rounds, Claude Opus tie-break decides final action

#### Artificer (not yet built)

Formula maker. Will craft and test the formulas (spells) that wizards
follow, via the Workshop CLI. The artificer role is exclusively for
formula creation — it does not orchestrate epics or review code.

> **Current state (2026-03-28):** The old `cmd/spire-artificer/` binary
> has been removed. The wizard now handles all execution (including epics
> and reviews) both locally and in k8s. The artificer role will be
> reintroduced when the Workshop CLI is built for formula management.

#### Familiar (`cmd/spire-sidecar/`)

Per-agent companion (sidecar). Runs alongside wizard containers in
every agent pod (k8s only). The directory name `cmd/spire-sidecar/`
is an implementation detail; the user-facing name is "familiar."

**Loops:**
- Inbox polling: `spire collect --json` on interval, writes to `/comms/inbox.json`
- Control channel: watches `/comms/control` for STOP, STEER, PAUSE, RESUME commands
- Wizard monitor: watches `/comms/result.json` and `/comms/wizard-alive` for liveness
- Heartbeat: writes timestamp to `/comms/heartbeat` every 30s

**Health endpoints:** `/healthz`, `/readyz`, `/status` (JSON snapshot including work context)

### Formula System (`cmd/spire/formula.go`, `cmd/spire/executor.go`)

Formulas define the phase pipeline a wizard follows for a given bead type.
Each formula is a TOML file declaring ordered phases with role, model,
timeout, and behavior configuration.

**Built-in formulas** (embedded in binary at `cmd/spire/embedded/formulas/`):

| Formula | Bead types | Phases | Description |
|---------|-----------|--------|-------------|
| `spire-epic` | epic | design → plan → implement → review → merge | Full lifecycle with design validation, Opus planning, wave dispatch, sage review |
| `spire-bugfix` | bug | implement → review → merge | Quick fix: single apprentice, sage review, auto-merge |
| `spire-agent-work` | task, feature, chore | implement → review → merge | Standard work: single apprentice, sage review, auto-merge |

**Bead type → formula mapping:**

```
epic    → spire-epic
bug     → spire-bugfix
task    → spire-agent-work
feature → spire-agent-work
chore   → spire-agent-work
(fallback) → spire-agent-work
```

**Layered resolution** (first match wins):

1. Label `formula:<name>` on the bead (explicit override)
2. Bead type mapping (table above)
3. `.beads/formulas/<name>.formula.toml` (tower-level customization)
4. Embedded formulas (built into the binary)

This means teams can override formulas per-tower without modifying the
binary. A custom `spire-epic.formula.toml` in `.beads/formulas/` takes
precedence over the embedded default.

**Phase configuration** (from `spire-epic.formula.toml`):

```toml
[phases.design]     # wizard validates linked design bead (pure Go, no LLM)
[phases.plan]       # wizard invokes Claude Opus to generate subtask breakdown
[phases.implement]  # apprentices in parallel waves, each in a worktree
[phases.review]     # sage reviews staging branch, verdict-only
[phases.merge]      # ff-only merge staging branch to main
```

See [epic-formula.md](epic-formula.md) for the full lifecycle diagram.

### Daemon (`cmd/spire/daemon.go`)

Background process for sync and integrations. Runs locally via `spire up`
or `spire daemon`.

**Cycle (per tower):**
1. Sync derived configs from tower config (single source of truth)
2. DoltHub sync: pull then push (`runDoltSync()`)
3. Sync unsynced epics to Linear (via Linear API)
4. Process webhook queue (from `spire serve` or serverless functions)
5. Process webhook event beads

### Syncer (k8s only)

Dedicated pod for DoltHub remote sync. Handles `dolt pull` and `dolt push`
on the shared cluster database. Decoupled from the steward and operator so
that sync failures don't block work assignment.

> **Current state (2026-03-26):** The daemon syncs automatically on each
> cycle (`runDoltSync()` does pull then push). Manual `spire push` /
> `spire pull` available for immediate sync. k8s syncer pod is a separate
> deployment for cluster-side sync.

## Pod Architecture

### Wizard Pod

```
+--------------------------------------------------+
| Pod: spire-agent-<agent>-<bead-id>               |
|                                                   |
| +------------------+   +----------------------+  |
| | init: seed-beads |   | ConfigMap: beads-seed|  |
| | (alpine:3.20)    |<--| metadata.json        |  |
| +------------------+   | routes.jsonl         |  |
|         |               | config.yaml          |  |
|         v               +----------------------+  |
| +------------------+   +---------------------+   |
| | wizard           |   | familiar            |   |
| | agent-entrypoint |   | spire-sidecar       |   |
| | .sh              |   | :8080               |   |
| | workdir:/workspace|  | workdir:/data        |   |
| +------------------+   +---------------------+   |
|         |         |           |                   |
|    /workspace  /comms      /comms   /data         |
|    (emptyDir)  (emptyDir)  (shared) (emptyDir)    |
+--------------------------------------------------+
```

Volumes:
- `/comms` (emptyDir) -- filesystem-based IPC between wizard and familiar
- `/workspace` (emptyDir) -- git clone of the target repo
- `/data` (emptyDir) -- beads state (`.beads/` seeded by initContainer)
- `beads-seed` (ConfigMap) -- metadata.json, routes.jsonl, config.yaml

> **Current state:** Agent pods require the `beads-seed` ConfigMap to
> initialize `.beads/` state. Target: `bd init --stealth` or a committed
> `metadata.json` in the repo eliminates the ConfigMap seeding step.

### Wizard Pod (Epic)

Epic beads route to wizard pods the same way task beads do (see
AgentMonitor routing table above). The wizard handles epic orchestration
in k8s the same way it does locally. Uses the "heavy" API token (Opus)
for planning and review.

### Steward Pod

```
+----------------------------------------------------+
| Deployment: spire-steward                          |
|                                                    |
| +--------------------+   +----------------------+ |
| | steward            |   | steward-sidecar      | |
| | spire steward      |   | spire-steward-sidecar| |
| | --no-assign        |   | --model claude-sonnet | |
| +--------------------+   +----------------------+ |
|         |                        |                 |
|      /data (PVC)              /comms (emptyDir)    |
+----------------------------------------------------+
```

## Inter-Container Communication

Containers within a pod communicate via the shared `/comms` emptyDir:

| File              | Writer    | Reader    | Purpose                        |
|-------------------|-----------|-----------|--------------------------------|
| `inbox.json`      | familiar  | wizard    | Messages from other agents     |
| `control`         | familiar  | wizard    | STOP, STEER:msg, PAUSE, RESUME|
| `steer`           | familiar  | wizard    | Steering corrections (live)    |
| `steer.log`       | wizard    | wizard    | Accumulated steering history   |
| `stop`            | familiar  | wizard    | Shutdown signal                |
| `result.json`     | wizard    | familiar  | Run outcome (triggers familiar shutdown) |
| `wizard-alive`    | wizard    | familiar  | Heartbeat (liveness check)     |
| `heartbeat`       | familiar  | operator  | Familiar liveness              |
| `bead.json`       | wizard    | familiar  | Current work context           |
| `prompt.txt`      | wizard   | wizard   | Assembled Claude prompt        |
| `focus.txt`       | wizard   | wizard   | Spire focus output             |

## Configuration

### spire.yaml (per-repo)

```yaml
runtime:
  language: go              # go | typescript | python | rust (auto-detected)
  test: go test ./...
  build: go build ./cmd/spire/
  lint: go vet ./cmd/spire/

agent:
  model: claude-sonnet-4-6  # default model for wizards in this repo
  max-turns: 30             # Claude Code turn limit
  stale: 10m                # steward warning threshold
  timeout: 15m              # steward kill threshold

branch:
  base: main
  pattern: "feat/{bead-id}"

pr:
  auto-merge: false
  labels: ["agent-generated"]

context:                    # files/dirs read before work begins
  - CLAUDE.md
  - SPIRE.md
```

Auto-detection walks the directory tree for spire.yaml. If absent, defaults
are inferred from `go.mod`, `package.json`, `Cargo.toml`, etc.

### .beads/metadata.json (per-workspace)

```json
{
  "database": "dolt",
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_database": "spi",
  "project_id": "<uuid>"
}
```

### .beads/config.yaml

```yaml
dolt.host: "127.0.0.1"      # local: 127.0.0.1, k8s: spire-dolt.spire.svc
dolt.port: 3307              # local default
storage:
  provider: gcs
  bucket: my-bucket
```

### k8s CRD Examples

The `repos` table in dolt is the source of truth for registered repos.
The operator reads this table and derives SpireAgent configurations from
it. SpireAgent CRDs shown below are auto-generated; do not treat
`spec.repo` as a manually-managed primary registry.

> **Current state:** SpireAgent CRDs are manually applied and the operator
> watches them as the roster source. Target: the operator reads the `repos`
> table and either auto-generates SpireAgent CRDs or bypasses them entirely.

```yaml
# Auto-generated from repos table; do not manage manually.
apiVersion: spire.awell.io/v1alpha1
kind: SpireAgent
metadata:
  name: wizard-1
  namespace: spire
spec:
  mode: managed              # "managed" (operator creates pods) or "external"
  image: ghcr.io/awell-health/spire-agent:latest
  repo: https://github.com/org/repo.git    # derived from repos.repo_url
  repoBranch: main                          # derived from repos.branch
  prefixes: ["web-"]                        # derived from repos.prefix
  maxConcurrent: 1
  capabilities: ["implement"]
```

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: SpireConfig
metadata:
  name: default
  namespace: spire
spec:
  dolthub:
    remote: org/spire-db
    credentialsSecret: dolthub-creds
  polling:
    interval: 2m
    staleThreshold: 4h
    reassignThreshold: 6h
  tokens:
    default:
      secret: anthropic-api-key
      key: ANTHROPIC_API_KEY
    heavy:
      secret: anthropic-opus-key
      key: ANTHROPIC_API_KEY
  defaultToken: default
```

## Security Model

- Credentials are never stored in Dolt or synced via DoltHub
- Local: API keys in `~/.config/spire/credentials` (chmod 600). Not
  keychain, not bare environment variables.
- Cluster: Kubernetes Secrets, injected as env vars via SpireConfig token
  refs. Never baked into images or ConfigMaps.
- DoltHub access control gates who can read/write the work graph
- GitHub tokens are scoped per-agent (optional per-agent token override)
- Agent pods run with `RestartPolicy: Never` and no host access

## Data Flow: Filing and Executing Work

```
1. User files work
   spire file "Add dark mode" -t feature -p 2
   --> creates bead in local dolt

2. User pushes
   spire push
   --> pushes to DoltHub

3. Cluster syncer pulls
   syncer pod: dolt pull
   --> bead appears in cluster dolt

4. BeadWatcher discovers bead
   bd ready --json
   --> creates SpireWorkload CR (phase: Pending)

5. WorkloadAssigner matches
   workload priority + agent prefixes + capacity
   --> sets phase: Assigned, updates agent.CurrentWork

6. AgentMonitor creates pod
   wizard pod (type determines formula: task, epic, review)
   --> pod runs: clone, claim, focus, implement, test, push

7. Wizard completes
   writes result.json, familiar detects exit
   --> AgentMonitor reaps pod, removes from CurrentWork

8. Status flows back
   syncer: dolt push --> DoltHub
   user: spire pull --> sees updated status
```

## Merge Ownership (Sync Conflicts)

Field-level ownership prevents conflicts during DoltHub sync. Each field
has a single authority; the other side's changes are discarded on conflict.
Append-only tables (comments, messages) never conflict because rows are
only inserted, never updated.

| Field category              | Authority       | Conflict resolution       |
|-----------------------------|-----------------|---------------------------|
| Status (status, owner)      | Cluster         | Cluster wins              |
| Content (title, description, priority) | User   | User wins                 |
| Append-only (comments, messages, labels) | Both  | No conflict (insert-only) |

## Docker Images

**Steward image** (`Dockerfile.steward`):
Contains `spire`, `spire-steward-sidecar`, `spire-operator`, `bd`, `dolt`,
`kubectl`. Used for the steward pod and operator pod.

**Agent image** (`Dockerfile.agent`):
Contains `spire`, `spire-sidecar` (familiar), `bd`, `dolt`,
`claude` (Claude Code CLI), `gh`, Go, Node.js, Python. Used for wizard
pods (task, epic, and review workloads). Runs as non-root user `wizard`.

## k8s Resources

Managed via kustomize (`k8s/kustomization.yaml`):

| Resource         | Kind           | Purpose                              |
|------------------|----------------|--------------------------------------|
| `namespace.yaml` | Namespace      | `spire` namespace                    |
| `crds/`          | CRDs           | SpireAgent, SpireWorkload, SpireConfig |
| `beads-pvc.yaml` | PVC            | Dolt database storage                |
| `steward-pvc.yaml`| PVC           | Steward state persistence            |
| `beads-seed.yaml`| ConfigMap      | .beads/ seed for agent pods          |
| `spire-config.yaml`| SpireConfig  | Cluster configuration singleton      |
| `dolt.yaml`      | Deployment+Svc | Shared Dolt SQL server               |
| `steward.yaml`   | Deployment     | Steward + steward-sidecar            |
| `operator.yaml`  | Deployment     | Operator (bead watcher + assigner + monitor) |
| `syncer.yaml`    | CronJob/Deploy | DoltHub sync (optional)              |

## Naming Conventions (RPG Theme)

| Role          | Name       | Description                                     |
|---------------|------------|-------------------------------------------------|
| User          | Archmage   | You. Writes specs, files work, reviews, steers  |
| Coordinator   | Steward    | Global work coordinator, assigns tasks          |
| Orchestrator  | Wizard     | Per-bead orchestrator, drives formula lifecycle  |
| Implementer   | Apprentice | Writes code in isolated worktrees, one-shot     |
| Reviewer      | Sage       | Reviews code, returns verdict, one-shot         |
| Formula maker | Artificer  | Creates and manages formulas (spells) via the Workshop CLI (not yet built) |
| Companion     | Familiar   | Per-agent companion (sidecar) for messaging and health |
| Dispute resolver | Arbiter | Resolves disputes when sage and apprentice disagree |
| Formula tool  | Workshop   | CLI tool for formula creation and testing (not yet built) |
| Database      | Archive    | Dolt database                                   |
| Hub           | Tower      | A Spire coordination instance                   |
