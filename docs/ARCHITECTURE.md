# Spire Architecture

Spire is an AI agent coordination system. It manages a shared work graph
(beads), routes work to autonomous agents, and synchronizes state across
local machines and Kubernetes clusters via DoltHub.

> **Living document.** Updated 2026-04-03. Where the current implementation
> differs from the target, inline callouts note the gap.

## Deployment Modes

Spire runs in three configurations. All share the same work graph, sync
protocol, and agent protocol.

**Local** -- Developer laptop. The `spire` CLI manages a local Dolt server
and daemon. Agents run as local processes via `spire summon` (each summoned
wizard is a `spire execute` subprocess that orchestrates worktrees and review
steps).

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
| Work        | `file`, `design`, `spec`, `claim`, `close`, `advance`, `focus`, `grok`, `ready`, `review`, `update` |
| Messaging   | `register`, `unregister`, `send`, `collect`, `read`, `inbox` |
| Coordination| `steward`, `board`, `roster`, `summon`, `dismiss`, `watch`, `alert` |
| Execution   | `apprentice run`, `sage review`, `wizard-merge`, `execute`, `wizard-epic` |
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
| `labels`   | issue_id, label -- routing (`msg`, `to:<agent>`, `from:<agent>`, `ref:<bead-id>`), metadata (`feat-branch:`, `updated:`, `needs-human`). Note: `needs-human` is legacy (only used for design approval gates). The current routing model uses `status=hooked` on step beads. |
| `deps`     | blocked, blocker -- dependency graph                       |
| `comments` | issue_id, author, body, created_at                         |
| `metadata` | key-value store (project_id, config)                       |
| `repos`    | prefix, repo_url, branch, runtime, registered_by, registered_at |
| `formulas` | name, version, content, description, published_by, created_at, updated_at |

Key operations: `create`, `update`, `close`, `list`, `show`, `ready`
(returns beads with no open blockers), `dep add`, `children`, `dolt commit`,
`dolt push`, `dolt pull`.

**Hooked status model:** Step beads (children of a parent work bead)
carry their own status: `open` -> `in_progress` -> `hooked` / `completed`
/ `failed`. A step enters `hooked` when it is parked waiting for an
external condition (e.g., a design bead to be closed, human approval).
The parent bead reflects `hooked` when any of its step beads are parked.
The steward's hooked-step sweeper polls hooked step beads each cycle,
checks whether the blocking condition has resolved, and re-summons the
wizard when it has.

### Dolt Database

SQL database with git-like version control. The shared state layer.

| Context  | How it runs                                               |
|----------|-----------------------------------------------------------|
| Local    | `dolt sql-server` on localhost:3307, managed by `spire up`|
| Cluster  | Dolt Deployment + PVC in the `spire` namespace            |
| DoltHub  | Remote for sync between local and cluster                 |

The database holds ALL shared state: beads, repos, agent registrations,
messages, comments, labels, dependencies.

### Steward (`pkg/steward/`, `cmd/spire/steward.go`)

The work coordinator. Runs as `spire steward` (locally or in the steward
pod in k8s). The steward actively assigns work, spawns agents, routes
reviews, and monitors health.

**Cycle (every N minutes):**

1. Commit local dolt changes
2. Query ready beads via `store.GetReadyWork()`
3. Load agent roster via the selected backend (`backend.List()`) and compute idle capacity
4. Assign ready beads to idle agents (round-robin by priority) -- sends assignment message via `spire send`, then spawns agent directly via `backend.Spawn()`
5. Detect standalone tasks ready for review -- spawns reviewer agents (`RoleSage`) for beads with closed implement steps
6. Detect review feedback -- re-engages original wizard when last review verdict is `request_changes`
7. Check bead health (stale warning at `agent.stale`, kill at `agent.timeout`)

> **Current state (2026-04-03):** The steward runs as a sibling process
> via `spire up --steward`. V1.0 target: merge into the daemon as a
> unified single process. Locally, `spire summon` remains available for
> manual capacity alongside steward-driven assignment. In k8s, the
> operator watches WizardGuild CRDs; the target is for the operator to
> read the `repos` table directly and derive agent configurations.

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
Matches pending SpireWorkloads to available WizardGuilds. Sorts by priority.
Checks prefix compatibility and concurrency limits. Marks stale workloads
for reassignment.

**AgentMonitor** (`controllers/agent_monitor.go`):
Tracks agent heartbeats. For managed agents, creates one pod per assigned
bead. Routes by workload type:

| Bead type | Pod type      | Main container                         |
|-----------|---------------|----------------------------------------|
| task/*    | Wizard pod    | `spire execute <bead> --name <name>`   |
| epic      | Wizard pod    | `spire execute <bead> --name <name>`   |
| review    | Sage pod      | `spire sage review --once`             |

Reaps completed/failed pods and removes work from the agent's CurrentWork
list. See [k8s-operator-reference.md](k8s-operator-reference.md#canonical-wizard-pod-contract)
for the canonical wizard pod spec.

**CRDs** (`operator/api/v1alpha1/types.go`):

| CRD             | Purpose                                            |
|-----------------|----------------------------------------------------|
| `WizardGuild`   | Registered agent (external or managed), capabilities, prefixes, image, concurrency |
| `SpireWorkload` | Bead assignment: beadId, priority, type, phase lifecycle (Pending -> Assigned -> InProgress -> Done/Stale/Failed) |
| `SpireConfig`   | Cluster singleton: DoltHub remote, polling config, token references, routing rules |

### Agents

#### Wizard (`cmd/spire/wizard.go`, `cmd/spire/executor.go`)

Per-bead orchestrator. Drives the formula lifecycle for a single bead.

**Local**: `spire execute <bead-id>` runs as the background executor
spawned by `spire summon`. It dispatches apprentices and sages as needed
for the bead's formula.

**k8s**: Runs in a wizard pod as a single container invoking
`spire execute <bead> --name <name>` (no shell entrypoint wrapper). A
`tower-attach` init container primes `/data` from the cluster dolt
server before the wizard starts. See
[k8s-operator-reference.md](k8s-operator-reference.md#canonical-wizard-pod-contract)
for the full pod contract.

```
Lifecycle:
  resolve repo -> claim bead -> load formula
  -> for each phase in formula:
     design: validate linked design bead
     plan:   invoke Claude (Opus) to generate subtask breakdown
     implement: dispatch apprentices in parallel waves (worktrees)
     review: dispatch sage for verdict (approve / request changes)
     merge:  ff-only merge staging branch to the configured base branch
  -> close bead -> write result -> exit
```

Key behaviors:
- Formula-driven: bead type maps to formula (`epic-default`, `bug-default`, `task-default`)
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

#### Cleric

Recovery agent. Summoned when a wizard fails and a recovery bead is
filed with failure evidence. Runs the `cleric-default` formula
(`collect` -> `decide` -> `execute` -> `verify` -> `learn` -> `finish`),
which inspects the failure, decides on a recovery action, executes it,
and extracts learnings for future runs. The cleric can set a
`RetryRequest` on the original bead, enabling cooperative recovery: the
re-summoned wizard checks this at startup via `checkRetryRequest` and
skips ahead to the requested step.

#### Artificer

Formula maker. Crafts and tests the formulas (spells) that wizards
follow, via the Workshop CLI. The artificer role is exclusively for
formula creation — it does not orchestrate epics or review code.

> **Current state (2026-03-29):** The old `cmd/spire-artificer/` binary
> has been removed. Formula work now lives in `spire workshop`, while the
> executor handles task, bug, and epic execution both locally and in k8s.

#### Familiar (`cmd/spire-sidecar/`) — deprecated in wizard pods

Historically the familiar ran alongside the wizard container in every
agent pod, providing inbox polling, a control channel, heartbeats, and
health endpoints over a shared `/comms` emptyDir. On `main` the wizard
pod no longer includes a familiar sidecar: the pod is single-container
(`spire execute ...`) and there is no `/comms` volume. The
`cmd/spire-sidecar/` binary is retained only for historical / non-wizard
uses; it is not deployed into wizard pods.

See [k8s-operator-reference.md — Deprecated: agent-entrypoint.sh / Model A](k8s-operator-reference.md#deprecated-agent-entrypointsh--model-a)
for the removal rationale.

### Formula System (`pkg/formula/`, `pkg/executor/`)

Formulas define the step graph a wizard follows for a given bead type.
Each formula is a v3 TOML file declaring steps with actions, conditions,
opcodes, and behavior configuration. The executor interprets the graph
at runtime, advancing steps based on conditions and persisting state
for crash-safe resume.

**Built-in formulas** (embedded in binary at `pkg/formula/embedded/formulas/`):

| Formula | Bead types | Steps | Description |
|---------|-----------|-------|-------------|
| `epic-default` | epic | design-check → plan → materialize → implement → review → merge → close | Full lifecycle with design validation, Opus planning, child dispatch, sage review, nestable review loops |
| `bug-default` | bug | plan → implement → review → merge → close | Quick fix: wizard plan, single apprentice, sage review, auto-merge |
| `task-default` | task, feature, chore | plan → implement → review → merge → close | Standard work: wizard plan, single apprentice, sage review, auto-merge |
| `cleric-default` | recovery | collect → decide → execute → verify → learn → finish | Recovery lifecycle with cleric-driven decision and learning extraction |
| `subgraph-review` | (sub-graph) | sage-review → arbiter → fix → merge → discard | Nestable review loop, invoked by parent formulas |
| `subgraph-implement` | (sub-graph) | dispatch-children → merge-staging → verify | Epic child dispatch with staging integration |

**Bead type → formula mapping:**

```
epic     → epic-default
bug      → bug-default
task     → task-default
feature  → task-default
chore    → task-default
recovery → cleric-default
(fallback) → task-default
```

**Name resolution** (determines which formula name to load):

1. Label `formula:<name>` on the bead (explicit override)
2. Bead type mapping (table above)

**Content resolution** (determines where to load the formula content):

1. Tower-level -- query `formulas` table in dolt (shared team defaults, synced via daemon)
2. Repo-level -- `.beads/formulas/<name>.formula.toml` (per-repo customization)
3. Embedded -- compiled into the binary (built-in defaults)

Tower provides shared defaults across all repos in a tower. Repo
overrides tower for local customization. Embedded is the fallback.
Teams can publish custom formulas via `spire formula publish` and they
propagate to all machines attached to the tower.

**V3 step graph structure** (from `epic-default.formula.toml`):

```toml
[steps.design-check]
action = "design.validate"   # pure Go, no LLM

[steps.plan]
action = "plan.generate"     # wizard invokes Claude Opus
depends_on = ["design-check"]

[steps.implement]
action = "graph.run"         # nested sub-graph (subgraph-implement)
depends_on = ["plan"]

[steps.review]
action = "graph.run"         # nested sub-graph (subgraph-review)
depends_on = ["implement"]
condition = "steps.implement.outputs.result == 'success'"

[steps.merge]
action = "bead.finish"       # ff-only merge to configured base branch
depends_on = ["review"]
condition = "steps.review.outputs.verdict == 'approve'"
```

Steps declare dependencies, conditions, and actions. The graph
interpreter resolves ready steps, executes them, evaluates conditions,
and persists state after each step for resumability. Sub-graphs
(`graph.run`) nest arbitrarily -- the review phase is itself a step
graph with sage-review, arbiter, fix, merge, and discard steps.

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

The wizard pod is the canonical per-bead runtime. It is a single-container
pod with one init container and `restartPolicy: Never` (one-shot). See
[k8s-operator-reference.md](k8s-operator-reference.md#canonical-wizard-pod-contract)
for the authoritative spec.

```
+----------------------------------------------------------+
| Pod: <agent-name>                                        |
|   restartPolicy: Never                                   |
|   labels: spire.agent, spire.agent.name, spire.bead,     |
|           spire.role=wizard, spire.tower                 |
|                                                          |
| +------------------------------------------------------+ |
| | init: tower-attach                                   | |
| | spire tower attach-cluster                           | |
| |   --data-dir=/data/<db> --database=<db>              | |
| |   --prefix=<prefix> --dolthub-remote=<remote>        | |
| | volumeMounts: /data                                  | |
| +------------------------------------------------------+ |
|                         |                                 |
|                         v                                 |
| +------------------------------------------------------+ |
| | agent (main)                                         | |
| | spire execute <bead-id> --name <agent-name>          | |
| | env: DOLT_DATA_DIR, SPIRE_CONFIG_DIR,                | |
| |      BEADS_DOLT_SERVER_{HOST,PORT},                  | |
| |      SPIRE_AGENT_NAME, SPIRE_BEAD_ID,                | |
| |      SPIRE_TOWER, SPIRE_ROLE=wizard,                 | |
| |      OTEL_*, ANTHROPIC_API_KEY (Secret),             | |
| |      GITHUB_TOKEN (Secret, optional)                 | |
| | volumeMounts: /data, /workspace                      | |
| | resources: 1Gi/250m req, 2Gi/1000m lim (overridable) | |
| +------------------------------------------------------+ |
|                                                          |
|   /data       emptyDir  — beads workspace + spire config |
|   /workspace  emptyDir  — git clone for apprentice bundles|
+----------------------------------------------------------+
```

Volumes:
- `/data` (emptyDir) — beads workspace (dolt data dir) and spire config
  (`/data/spire-config`); primed by the `tower-attach` init container
- `/workspace` (emptyDir) — git clone target for apprentice bundle production

There is no `/comms` volume, no `beads-seed` ConfigMap, and no familiar
sidecar. The init container does all bootstrap work that used to live
in `agent-entrypoint.sh` and the `beads-seed` ConfigMap.

#### Resource tier

| Field            | Default | Override env                 |
|------------------|---------|------------------------------|
| Memory request   | `1Gi`   | `SPIRE_WIZARD_MEMORY_REQUEST`|
| Memory limit     | `2Gi`   | `SPIRE_WIZARD_MEMORY_LIMIT`  |
| CPU request      | `250m`  | `SPIRE_WIZARD_CPU_REQUEST`   |
| CPU limit        | `1000m` | `SPIRE_WIZARD_CPU_LIMIT`     |

Wizard pods are sized for planning and apprentice fan-out, so the
defaults are higher than the generic executor/sage tier.

#### One-shot semantics

`restartPolicy: Never` — on exit, the steward/operator reads the pod
phase (`Succeeded` / `Failed`), records the outcome, and reaps the pod.
There is no in-pod sidecar reporting; the pod's lifetime equals the
wizard process's lifetime.

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

Wizard pods are single-container; there is no in-pod IPC. The steward
pod still uses a `/comms` emptyDir between its own containers (steward
process and steward-sidecar) for LLM-powered message processing, but
that volume does not exist in wizard pods. See the Steward Pod diagram
above.

Wizard ↔ steward coordination flows through the dolt database and
OTLP telemetry, not through files on a shared volume.

## Deprecated: agent-entrypoint.sh / Model A

Earlier revisions of this document described a richer wizard pod
("Model A"): a main container that ran `agent-entrypoint.sh` (a bash
script that cloned the repo, seeded `.beads/` from a ConfigMap, claimed
the bead, invoked Claude, validated, and pushed) plus a familiar
sidecar at `:8080` that polled inboxes, served `/healthz`/`/readyz`,
and coordinated over a shared `/comms` emptyDir.

That model is **removed from main** because it diverged from the code
path actually executed by `pkg/agent/backend_k8s.go`, which spawns a
single-container pod running `spire execute` directly. Only the
canonical wizard pod documented above (and in
[k8s-operator-reference.md](k8s-operator-reference.md#canonical-wizard-pod-contract))
is promised on main.

Tracked under epic **spi-kjh9e** with design **spi-lm26c**. Legacy
references to `agent-entrypoint.sh`, the familiar sidecar at `:8080`,
and `/comms` remain only in dated design/plan archives under
`docs/design/`, `docs/plans/`, `docs/reviews/`, and
`docs/superpowers/specs/`.

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
The operator reads this table and derives WizardGuild configurations from
it. WizardGuild CRDs shown below are auto-generated; do not treat
`spec.repo` as a manually-managed primary registry.

> **Current state:** WizardGuild CRDs are manually applied and the operator
> watches them as the roster source. Target: the operator reads the `repos`
> table and either auto-generates WizardGuild CRDs or bypasses them entirely.

```yaml
# Auto-generated from repos table; do not manage manually.
apiVersion: spire.awell.io/v1alpha1
kind: WizardGuild
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
Contains `spire`, `bd`, `dolt`, `claude` (Claude Code CLI), `gh`, Go,
Node.js, Python. Used for wizard pods (task, epic, and review
workloads). The main container entrypoint is `spire execute` directly;
no shell wrapper and no in-pod sidecar. Runs as non-root user `wizard`.

## k8s Resources

Managed via kustomize (`k8s/kustomization.yaml`):

| Resource         | Kind           | Purpose                              |
|------------------|----------------|--------------------------------------|
| `namespace.yaml` | Namespace      | `spire` namespace                    |
| `crds/`          | CRDs           | WizardGuild, SpireWorkload, SpireConfig |
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
| Formula maker | Artificer  | Creates and manages formulas (spells) via `spire workshop` |
| Companion     | Familiar   | Per-agent companion (sidecar) for messaging and health |
| Recovery agent | Cleric  | Healer/restorer — runs cleric-default formula on recovery beads |
| Dispute resolver | Arbiter | Resolves disputes when sage and apprentice disagree |
| Formula tool  | Workshop   | CLI tool for formula creation, testing, and publishing |
| Database      | Archive    | Dolt database                                   |
| Hub           | Tower      | A Spire coordination instance                   |
