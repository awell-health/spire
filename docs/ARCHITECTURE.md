# Spire Architecture

Spire is an AI agent coordination system. It manages a shared work graph
(beads), routes work to autonomous agents, and synchronizes state across
local machines and Kubernetes clusters via DoltHub.

> **Living document.** Updated 2026-04-03. Where the current implementation
> differs from the target, inline callouts note the gap.

## Deployment modes

Spire's control-plane topology is selected by an explicit, three-valued
contract owned by `pkg/config/deployment_mode.go`. Every scheduling /
dispatch entry point reads the tower's effective mode and branches on it.
The mode is deliberately orthogonal to **worker backend** (process /
docker / k8s) and to **sync transport** (syncer / remotesapi / DoltHub) —
see [Non-goals](#non-goals) below.

| Mode | Wire value | Child dispatch | What runs where |
|------|------------|----------------|-----------------|
| Local-native | `local-native` | In-process spawn via `pkg/agent.Backend.Spawn`. | Full control plane and workers on the archmage's machine. The steward, executor, and wizard each call `pkg/agent.Spawner.Spawn` for their own child workers (apprentice / sage / wizard / cleric). The canonical default for new towers. |
| Cluster-native | `cluster-native` | Operator materializes pods from intents emitted by the steward / executor / wizard. **No code path calls `Spawner.Spawn`.** | Steward runs in a pod, emits bead-level `pkg/steward/intent.WorkloadIntent` without pre-creating an attempt bead, and the operator reconciles those intents into wizard pods. Per-phase intents (review, review-fix, hooked-step resume, cleric) are emitted through the same publisher seam. The archmage's machine is a client of the cluster control plane. |
| Attached-reserved | `attached-reserved` | Operator-owned, per [VISION-ATTACHED.md](VISION-ATTACHED.md). | A local control plane targeting a remote cluster execution surface through the same `WorkloadIntent` seam. **Reserved — not implemented.** Selecting it is a declaration of intent; consumers return `attached.ErrAttachedNotImplemented`. See [docs/attached-mode.md](attached-mode.md) for the full reservation. |

The shared work graph still flows over Dolt + DoltHub regardless of which
mode is in effect: local-native pushes locally, cluster-native syncs via
the syncer pod, and an attached-mode tower would do whatever its sync
transport selection dictates. Sync is not the mode switch.

```
  Local-native                  Cluster-native                          Attached-reserved
  ------------                  --------------                          -----------------
  steward (CLI)                 steward (pod)                           steward (CLI)
    │                             │                                       │
    │ Spawn (process/docker/k8s)  │ Publish(bead-level WorkloadIntent)    │ Publish(WorkloadIntent) → remote
    ▼                             ▼                                       ▼  (transport TBD)
  wizard subprocess             operator reconciler                     ErrAttachedNotImplemented
                                  │
                                  │ BuildWizardPod
                                  ▼
                                wizard pod
                                  │
                                  │ `spire claim`
                                  ▼
                           attempt bead opened/resumed
```

### Cluster-native child-dispatch contract

Cluster-native dispatch carries an explicit `(role, phase, runtime)`
triple across the scheduler-to-reconciler seam. The operator's
pod-builder validates the triple against an allowlist; an unrecognized
combination is rejected at intent-consumption time rather than as an
init-container failure inside the pod. The supported combinations are:

| Role | Phase | Runtime | Pod shape |
|------|-------|---------|-----------|
| `apprentice` | `implement` | `worker` | Apprentice pod (`BuildApprenticePod`). Bundle handoff to the parent wizard. |
| `apprentice` | `fix` | `worker` | Apprentice pod for review-feedback / review-fix re-entry. |
| `sage` | `review` | `reviewer` | Sage pod (`BuildSagePod`). Diff review against staging. |
| `sage` | `review-fix` | `reviewer` | Sage pod for re-review of a fix bundle. |
| `cleric` | `<bead-type>` | `wizard` | Wizard-shaped pod that drives `cleric-default`. Phase MUST classify under `intent.IsBeadLevelPhase` — `recovery` is **not** a supported value. |
| `wizard` | `<bead-type>` or `wizard` | `wizard` | Wizard pod (`BuildWizardPod`). Steward bead-level dispatch, hooked-step resume. |

[VISION-CLUSTER.md → Operator-owned dispatch](VISION-CLUSTER.md#operator-owned-dispatch-cluster-native-invariant)
is the source of truth for this contract; the table above is a
summary. The fail-closed rule for missing seams (no silent local
spawn) and the AST invariant guarding the boundary live there too.

### Shared-state ownership for review feedback

In cluster-native mode the steward does not consult the local wizard
registry (`pkg/registry`, `~/.config/spire/wizards.json`) to find a
remote wizard owner for review-feedback re-engagement. That registry
is per-machine bookkeeping owned by `pkg/agent`'s process backend; it
has no meaning across cluster replicas and would silently fragment
ownership.

Cluster-native control paths look up review-feedback owners through a
shared-state surface that is durable across replicas:

- the `implemented-by` / attempt-bead linkage on the work bead,
- attempt-bead metadata (instance, agent name, started-at, last-seen),
- the typed review outcome stamped on the review/sage step bead.

This is the same shared-state surface the steward already uses for
hooked-step recovery, stale-attempt detection, and `dispatched`-state
concurrency accounting. There is no second ownership plane in
cluster-native — see
[`pkg/steward/README.md` → Cluster-native: the three seams](../pkg/steward/README.md#cluster-native-the-three-seams).

## Non-goals

These are explicit non-goals of the deployment-mode contract. They exist
because each one was, at some point, a tempting shortcut that would have
collapsed an orthogonal dimension into the mode switch.

- **Attached mode is reserved, not implemented.** No execution path exists
  for `DeploymentModeAttachedReserved` today. The runtime surface is the
  stub in `pkg/steward/attached`, which returns
  `attached.ErrAttachedNotImplemented` for any input. Selecting it in a
  tower config is a declaration of intent only. See
  [docs/attached-mode.md](attached-mode.md) for the reservation, the
  seams it will reuse, and the contract a future implementation MUST NOT
  perturb.
- **Sync transport is orthogonal to deployment mode.** A local-native
  tower may sync over DoltHub; a cluster-native tower may sync over
  remotesapi or a syncer pod. Choosing `cluster-native` does not imply
  any particular transport, and choosing a particular transport does not
  imply a particular deployment mode. Code that branches on transport to
  decide topology — or vice versa — violates the contract documented on
  `pkg/config.DeploymentMode`.
- **`LocalBindings` are local-only workspace state.** The `LocalBindings`
  map on the tower config records per-machine workspace facts (bind
  state, local clone path) that have no meaning across cluster replicas.
  Cluster-native scheduling code paths MUST NOT read
  `LocalBindings.State`, `LocalBindings.LocalPath`, or `cfg.Instances`.
  Cluster repo identity comes from
  [`pkg/steward/identity.ClusterIdentityResolver`](#seams) backed by the
  shared tower repo registry; that is the only canonical source.

## Seams

Spire's three-mode contract is held together by four narrow seams owned by
`pkg/steward/intent`, `pkg/steward/identity`, `pkg/steward/dispatch`, and
`pkg/agent`. Each seam has a typed interface, a single canonical
production implementation, and reflection- or parity-based tests that
prevent it from drifting into a second source of truth.

### `WorkloadIntent` (`pkg/steward/intent`)

`WorkloadIntent` is the dispatch-time payload that crosses the
scheduler-to-reconciler boundary in cluster-native (and, eventually,
attached) mode. It carries a minimal `RepoIdentity` (`URL`,
`BaseBranch`, `Prefix`), `FormulaPhase`, `Resources`, and
`HandoffMode` — and **nothing machine-local**. Attempt-bead lifecycle
does not cross this seam: the wizard sees `SPIRE_BEAD_ID=<task_id>` on
startup and creates or resumes its own attempt when it runs `spire claim`.
The package imports
nothing from `k8s.io/*`, `pkg/dolt`, or `pkg/config`, and a reflection
test in `intent_test.go` rejects any new field that smuggles
`LocalBindings`, local paths, or other per-machine state onto the wire.
`IntentPublisher` is the scheduler-side exit seam; `IntentConsumer` is
the reconciler-side entry seam. Both are interfaces so the transport
(today, a Kubernetes CR apply) is pluggable.

### `ClusterIdentityResolver` (`pkg/steward/identity`)

`ClusterIdentityResolver` is the only canonical source of cluster repo
identity. Implementations resolve a repo prefix to its canonical
`ClusterRepoIdentity` (`URL`, `BaseBranch`, `Prefix`) by reading the
shared tower repo registry — the `repos` table backed by
`pkg/store`'s dolt connection. Cluster scheduling code MUST resolve
through this seam and MUST NOT touch `LocalBindings.State`,
`LocalBindings.LocalPath`, or `cfg.Instances`. The boundary is pinned
mechanically: `DefaultClusterIdentityResolver` accepts an audit-only
`LocalBindingsAccessor` and tests wire a panicking stub to prove
`Resolve` never dereferences it. The production implementation is
`SQLRegistryStore`, which reads the tower's `repos` rows via the
shared dolt connection.

### `AttemptClaimer` + `DispatchEmitter` + `ClaimThenEmit` (`pkg/steward/dispatch`)

`pkg/steward/dispatch` formalizes claim-then-emit as the only path by
which cluster-native scheduling may hand work to a reconciler.
`AttemptClaimer.ClaimNext` does **not** create an attempt bead. It
verifies that no active wizard attempt already exists for the task,
reserves the next monotonic `dispatch_seq`, and returns a `ClaimHandle`
carrying `(TaskID, DispatchSeq, ClaimedAt)`. `DispatchEmitter.Emit`
refuses to publish an intent without a matching handle: every
implementation MUST call `dispatch.ValidateHandle` first, which returns
`ErrNoClaimedAttempt` for nil handles or `(TaskID, DispatchSeq)`
mismatches. `ClaimThenEmit` is the orchestrator that wires the two
halves together and is the only allowed dispatch path in cluster-native
code paths. The attempt bead is wizard-owned and is opened later by the
wizard's `spire claim`.

### Pod builders (`pkg/agent`)

`pkg/agent.BuildWizardPod`, `BuildSagePod`, and `BuildApprenticePod`
are the canonical pod-shape constructors. The operator's intent
reconciler chooses among them from `FormulaPhase`; cluster-native
scheduling in `pkg/steward` does not build pods directly. `PodSpec` has
no opaque maps — every field is intentional and missing required inputs
surface as typed `ErrPodSpec*` errors at build time rather than as
init-container failures at runtime. The builders never read process env,
never fall back to ambient CWD, and never hide missing identity behind a
default. Pod-shape parity between callers is enforced by
`pkg/agent/pod_builder_parity_test.go` and
`operator/controllers/pod_builder_parity_test.go`.

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

**Internal bead types:** Spire stores four internal-only types —
`message`, `step`, `attempt`, `review` — alongside user-facing work
beads in the `issues` table. They are created programmatically by the
engine (executor, wizard, dispatch layer), never by `spire file`, and
are hidden from the board, the steward queue, and `bd ready`. The
`IsWorkBead` invariant (`!InternalTypes[type] && Parent == ""`) gates
every dispatch path. See [INTERNAL-BEADS.md](INTERNAL-BEADS.md) for the
full taxonomy, filter sites, and legacy label fallback.

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
It is specific to the steward pod and is not deployed into wizard pods —
wizard pods are single-container (`spire execute`) with no sidecar.
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

### Worker Runtime Contract

> **Authoritative spec:** [docs/design/spi-xplwy-runtime-contract.md §1](design/spi-xplwy-runtime-contract.md).
> This section is a short summary + pointer; read the spec for
> invariants and enforcement points.

The worker runtime is held together by **four types** owned by
`pkg/executor` (re-exported through `pkg/runtime` for backends and
observability) and **two backend obligations** owned by `pkg/agent`.
The contract holds identically across local process mode, `pkg/agent`
k8s mode, and operator-managed cluster mode — by design, not by
coincidence.

**The four types:**

| Type              | Who owns it        | What it pins                                                   |
|-------------------|--------------------|----------------------------------------------------------------|
| `RepoIdentity`    | executor           | Tower-derived identity. Never inferred from ambient CWD.       |
| `WorkspaceHandle` | executor           | The workspace the worker will see (`Kind`, `Path`, `Origin`, `Borrowed`). |
| `HandoffMode`     | executor           | Cross-owner delivery selection (`none`/`borrowed`/`bundle`/`transitional`). |
| `RunContext`      | executor           | Observability identity carried by every log, trace, and metric. |

**The two backend obligations** (see
[pkg/agent/README.md — Backend obligations](../pkg/agent/README.md#backend-obligations-normative)):
resolve and propagate `RepoIdentity`, materialize `WorkspaceHandle.Path`
per `Kind`, emit `RunContext` as labels/annotations/env, and fail fast
on missing prerequisites. The operator (`operator/controllers/
agent_monitor.go`) calls the same `pkg/agent` pod builder as the
k8s backend, so pod-shape parity is structural rather than a promise.

**Push transport is quarantined.** The legacy flow where an apprentice
pushes its feature branch directly (outside the bundle contract) is now
classified as `HandoffMode=transitional`. Every transitional selection
is counted (`spire_handoff_transitional_total`), Warn-logged with full
identity, and — when `SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1` is set (CI
parity lanes default this on) — promoted to a hard error. The
description of apprentice push behavior below under "Apprentice" and
the `spire push`/`dolt push` mentions elsewhere in this document are
**not removed**; they are annotated pending chunk 5b (a separate bead)
which deletes the push path entirely once metrics show zero live use.

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
  > **Quarantined (chunk 5a).** Direct feature-branch push is
  > `HandoffMode=transitional` in the runtime contract — counted,
  > Warn-logged, and failed under `SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1`.
  > Canonical cross-owner delivery is bundle transport (see
  > [pkg/apprentice/README.md](../pkg/apprentice/README.md)). Chunk 5b
  > removes this path.

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
(`collect_context` -> `decide` -> `execute` -> `verify` -> `learn` ->
`finish`), which inspects the failure, decides on a repair plan, executes
it, verifies the outcome, and records a structured `RecoveryOutcome` the
steward consumes.

The redesign in [design/spi-h32xj-cleric-repair-loop.md](design/spi-h32xj-cleric-repair-loop.md)
sharpens the package split: `pkg/recovery` owns the domain types
(`RepairMode`, `RepairPlan`, `VerifyPlan`, `VerifyVerdict`, `Decision`,
`RecoveryOutcome`) and decide-time policy. `pkg/executor` owns the
runtime — cleric step adapters that provision workspaces through the
shared [runtime contract](design/spi-xplwy-runtime-contract.md), dispatch
by `RepairMode`, and drive the cooperative retry protocol.

The cleric can set a `RetryRequest` on the original bead, enabling
cooperative recovery: the re-summoned wizard checks it at startup via
`checkRetryRequest` and skips ahead to the requested step (or honors a
full `VerifyPlan` once the request carries one). The steward reads the
cleric's `RecoveryOutcome` through the typed `recovery.ReadOutcome`
accessor — it does not parse comment text or ad hoc labels.

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
pod with two init containers and `restartPolicy: Never` (one-shot). See
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
| | init: cache-bootstrap                                | |
| | materializes the writable workspace at               | |
| |   pkg/agent.WorkspaceMountPath from the read-only    | |
| |   guild-owned cache PVC, then runs `spire repo       | |
| |   bind-local` so wizard.ResolveRepo resolves it      | |
| | volumeMounts: /data, /spire/workspace, /spire/cache  | |
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
| |      SPIRE_REPO_{URL,BRANCH,PREFIX},                 | |
| |      OTEL_*, ANTHROPIC_API_KEY (Secret),             | |
| |      GITHUB_TOKEN (Secret, optional)                 | |
| | volumeMounts: /data, /spire/workspace, /spire/cache  | |
| | workingDir: /spire/workspace                         | |
| | resources: 1Gi/250m req, 2Gi/1000m lim (overridable) | |
| +------------------------------------------------------+ |
|                                                          |
|   /data            emptyDir  — beads workspace + config  |
|   /spire/workspace emptyDir  — materialized repo root    |
|   /spire/cache     PVC (RO)  — guild-owned cache mirror  |
+----------------------------------------------------------+
```

Volumes:
- `/data` (emptyDir) — beads workspace (dolt data dir) and spire config
  (`/data/spire-config`); primed by the `tower-attach` init container
- `/spire/workspace` (emptyDir) — bead repo checkout materialized by the
  `cache-bootstrap` init container and also used as the git clone target
  when the wizard produces apprentice bundles
- `/spire/cache` (PVC, read-only) — guild-owned repo cache mirror;
  provisioned by the operator's `CacheReconciler` from the
  `WizardGuild.Spec.Cache` declaration

There is no `/comms` volume and no familiar sidecar in the wizard pod.
The two init containers do all bootstrap work that used to live in
`agent-entrypoint.sh` and the `beads-seed` ConfigMap.

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

#### Cache PVC is the canonical substrate

Every operator-managed wizard pod boots from the guild-owned cache PVC
(spi-gvrfv). The operator's cache overlay runs unconditionally — the
shared pkg/agent pod builder's `repo-bootstrap` init container is always
replaced with `cache-bootstrap`, the repo-cache PVC is mounted
read-only at `pkg/agent.CacheMountPath`, and the main container's
writable workspace lives at `pkg/agent.WorkspaceMountPath`. A guild CR
without `spec.cache` gets no PVC provisioned by `CacheReconciler` and
its pods stay `Pending` — declaring `spec.cache` on every managed guild
is now a deployment requirement. See
[cluster-repo-cache.md](cluster-repo-cache.md) for the full contract
(CRD fields, PVC/Job naming, serialization approach, worker
bootstrap, observability vocabulary).

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

## Cluster-resource-health pattern

Spire represents recoverable cluster resources (WizardGuild.Cache,
syncer, ClickHouse, dolt StatefulSet, and future operator/steward
scheduling state) with a uniform two-tier bead shape. The invariant is
"recovery targets a bead": the existing cleric pipeline expects a bead
as the unit of work, and cluster resources — which have no parent bead
from an agent run — need a durable target for that pipeline to address.
The pattern supplies one.

### Two-tier model

| Tier | Substrate | Sync | Status flag | Carries | Lifecycle owner |
|------|-----------|------|-------------|---------|-----------------|
| **Pinned identity bead** | pour | persistent, git-synced | `StatusPinned` + `pinned: true` | static metadata only (resource URI, type, provisioning timestamp, owner references) | operator (create on provisioning, close on CR deletion) |
| **Wisp recovery bead** | wisp | ephemeral, cluster-local, not git-synced | open while unhealthy | `caused-by` edge to pinned id, termination log, condition snapshot, `FailureClass`, `SourceResourceURI` | operator files; cleric `finish` closes; GC reaps |

Exactly one pinned identity bead exists per recoverable cluster
resource. It is immutable after creation. Zero or more wisp recovery
beads exist per pinned identity — one per failure incident.

**Why pour + pinned for identity.** Pours are the persistent, git-synced
substrate, so a pinned identity is discoverable across the graph from
any machine attached to the tower via remotesapi. Immutability after
creation means the row never participates in a write conflict during
sync; the pinned bead's job is to be a stable address, not a mutable
record. Static metadata only — no health field, no last-seen timestamp,
nothing that drifts — keeps the bead aligned with that contract.

**Why wisps for recovery.** Wisps are ephemeral and cluster-local,
which matches the nature of failure records: one per incident,
frequently created and closed, with no value post-recovery. Keeping
them cluster-local avoids flooding DoltHub (or laptop clones) with
cluster-churn noise. GC reaps closed wisps on its cycle, which matches
the transient purpose of a failure record — the durable audit of what
happened lives elsewhere (see Durability & analytics below).

The presence of an open wisp with `caused-by <pinned-id>` **is** the
unhealthy signal. Absence **is** the healthy signal. There is no
mutable health field on the pinned bead to keep in sync with cluster
state — "health is current" because the graph itself is the query.

### Lifecycle

The flow for a cluster resource, using WizardGuild.Cache as the
concrete example:

```
operator reconciler (observes WizardGuild.Cache.Enabled=true)
  → creates pinned identity bead (pour, StatusPinned, pinned:true, static metadata)

cache refresh Job (backoff exhausted)
  → operator files wisp recovery bead
      - caused-by → <pinned identity bead id>
      - metadata: termination log, condition snapshot, FailureClass, SourceResourceURI

steward hooked-sweep
  → claims wisp → cleric runs cleric-default formula

cleric decide (agent-first: Claude reads wisp metadata as diagnosis context)
  → chosen action (mechanical recipe | worker-mode apprentice with cache PVC mount | escalate)

cleric execute
  → BuildApprenticePod with CacheSpec overlay (PVC RW, init-container order)

cleric verify
  → poll CacheRefreshing=False + observedRevision advance

cleric finish
  → closes wisp; writes RecoveryOutcome (bead metadata + recovery_learnings SQL row)
  → GC reaps wisp + wisp_dependencies rows on next cycle

CR deletion (finalizer)
  → operator closes any open wisps FIRST
  → then closes pinned identity bead
```

CR-deletion ordering is a referential-integrity discipline: closing
wisps first ensures no open `caused-by` edge is ever left pointing at a
closed pinned identity.

### Boundary decisions

Three boundaries make the pattern work without bleeding cluster state
into recovery policy or vice versa.

- **`pkg/recovery` stays bead-centric.** The package has no kube state
  awareness. Wisp metadata (`SourceResourceURI`, termination log,
  condition snapshot) carries the resource context; Claude reads it as
  prompt input during diagnosis. This preserves unit-testability
  without k8s fixtures — the cluster is an input string to the
  diagnosis prompt, not a live dependency.

- **Operator is a bead-writer, not a bead-claimer.** The operator's
  new responsibilities are narrow: create a pinned identity bead when
  a CR is provisioned, and file a wisp recovery bead when a Job's
  backoff is exhausted. It does not claim work, dispatch agents, or
  render review verdicts. Claiming, dispatching, and reviewing remain
  with steward and cleric. The seam is clean: the operator writes
  **observed cluster state** into beads; steward and cleric write
  **work state** into beads.

- **Cleric pod shape extends via overlay, not a new builder.**
  `BuildApprenticePod` remains the single source of truth for pod
  shape. Cleric-on-cache recoveries require a `CacheSpec`-aware
  overlay (PVC access mode, init-container order), which is applied to
  the same `PodSpec` the canonical builder already consumes. One pod
  builder, overlay-per-resource-type — not a parallel builder per
  recovery shape.

### Durability & analytics

The graph `caused-by` edge is an in-flight routing mechanism: the
steward's hooked-sweep finds unclaimed wisps, cleric dispatch resolves
the target pinned identity, and nothing more. Post-recovery the wisp
is transient and the edge goes away with it when GC reaps the wisp and
its `wisp_dependencies` rows.

Durable audit lives in `RecoveryOutcome`: written by cleric `finish`
into bead metadata (under `KeyRecoveryOutcome`) and into the
`recovery_learnings` SQL table (see `pkg/recovery/README.md`).
`recovery_learnings` survives wisp GC and is the authoritative source
for cross-incident analytics. Queries by `FailureClass`,
`SourceResourceURI`, or `failure_signature` run against the SQL table,
not the bead graph.

`RecoveryOutcome` itself stays resource-agnostic — the struct has no
cache-specific or syncer-specific fields. Resource-specific data lives
in wisp metadata while the wisp exists, and in the `recovery_learnings`
row's `failure_signature` and `FailureClass` columns after the wisp is
reaped.

### Deployment-mode gating

The operator reconciler runs when
`deployment_mode IN (cluster-native, attached-reserved)`. Local-native
is a no-op for cluster-resource-health: there is no operator, there
are no cluster resources the operator would reconcile, there are no
cleric pods to dispatch against them, and therefore no wisps for
cluster resources.

Mode gating is enforced at the same seams as every other mode-aware
component (see [Deployment modes](#deployment-modes) above and the
mode-gating infrastructure documented there). Components filing
pinned identities or wisp recoveries consult the effective mode; they
never assume the operator is running.

### Failure classes

`FailureClass` grows per-resource-type (`cache-refresh-failure`,
`syncer-failure`, …) rather than a single generic
`resource-unhealthy`. The rationale is sharpness: a promotion signature
(`FailureClass + failure_signature`) keyed on a specific resource-type
failure is useful for mechanical-recipe promotion; a generic bucket
would collapse unrelated incidents into the same key and dilute the
learning. Per-resource-type classes also match the existing enum style
in `pkg/recovery/classify.go`.

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
   spire execute exits; pod enters Succeeded / Failed
   --> AgentMonitor reads pod phase, reaps pod, removes from CurrentWork

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

## OLAP backends (dual-backend factory pattern)

Spire has two OLAP backends and a single factory in `pkg/olap/factory.go`
that dispatches between them. Dolt remains the source of truth; OLAP is
derived / ETL'd.

| Backend | Build tag | Deployment | Why |
|---------|-----------|------------|-----|
| `duckdb` | cgo-only | local single-binary | rich SQL (quantile_cont, epoch, ON CONFLICT), zero ops |
| `clickhouse` | pure-Go | cluster (helm/spire) | no CGO in the agent image; horizontally scalable; native Go driver |

**Selection.** `SPIRE_OLAP_BACKEND` selects the backend (`duckdb` or
`clickhouse`); default is `duckdb`. In the cluster, the helm chart sets
`SPIRE_OLAP_BACKEND=clickhouse` and `SPIRE_CLICKHOUSE_URL=<svc>:9000`
on the steward, operator, and wizard-pod envs when
`clickhouse.enabled=true`. Local installs leave the env unset and fall
through to the DuckDB path.

**Factory split.** Two entry points with different interfaces:

- `olap.OpenBackend(cfg) (ReadWrite, error)` — returns Writer +
  TraceReader. Compiles in both cgo and nocgo builds.
- `olap.OpenStore(cfg) (Store, error)` — returns the full Store
  (adds MetricsReader). Only DuckDB implements MetricsReader, so
  `OpenStore` returns an error for ClickHouse. This makes "metrics
  queries need DuckDB" a build-time contract rather than a runtime
  panic. See the design note at the top of `pkg/olap/factory.go`.

**Schema parity.** `pkg/olap/schema.go` (DuckDB) and
`pkg/olap/clickhouse_schema.go` (ClickHouse) are kept in sync for every
column written by the ETL and receivers: `agent_runs_olap`,
`bead_lifecycle_olap`, `api_events`, `tool_events`, `tool_spans`,
`etl_cursor`, and the aggregate tables (`daily_formula_stats`,
`weekly_merge_stats`, `phase_cost_breakdown`, `tool_usage_stats`,
`failure_hotspots`). ClickHouse uses `MergeTree` for append-only event
tables and `ReplacingMergeTree(synced_at)` for tables that need upsert
semantics (agent_runs_olap, bead_lifecycle_olap, etl_cursor, all
aggregate tables).

**Views.** `pkg/olap/views.go` holds the DuckDB view-refresh DDL
(DELETE + INSERT … ON CONFLICT + DuckDB-native functions like `epoch()`
and `INTERVAL N DAY`). `pkg/olap/clickhouse_views.go` holds the
ClickHouse equivalents, translated to `dateDiff('second', …)`,
`today() - N`, `toStartOfWeek`, and plain INSERTs that rely on
ReplacingMergeTree dedup.

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
