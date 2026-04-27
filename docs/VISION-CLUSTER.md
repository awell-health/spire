# Cluster-Native Deployment Vision

> The coordination plane runs in Kubernetes.

Cluster-native is how Spire scales from "one laptop's worth of agents"
to "a team's worth of persistent capacity." The steward, the operator,
the dolt database, and the agent pods all live in a Kubernetes cluster.

> **Cluster-native is the server deployment family; cluster-as-truth
> is one operating mode within that family.** A cluster-native tower
> can be operated as cluster-as-truth (single-writer, gateway-attached
> clients only) *or* in a direct-Dolt configuration where laptops keep
> writable local mirrors and reach the cluster's Dolt over remotesapi.
> Most of this document describes patterns that hold for either
> operating mode; the "How laptops participate" section below
> distinguishes them. See
> [deployment-modes.md](deployment-modes.md) for the server/client
> matrix.

## What runs

In a cluster-native deployment:

- **Steward pod** — the coordinator loop, same code as the local-native steward, running as a Deployment
- **Operator pod** — watches `WizardGuild` custom resources and reconciles workload intent into wizard pods
- **Dolt StatefulSet** — the tower's database, with remotesapi enabled for internal cluster traffic and any direct-Dolt attach topologies
- **Wizard pods** — ephemeral, one per in-flight bead, built from the canonical pod spec. A wizard pod is the unit of dispatch: apprentices, sages, and arbiters run as child processes of the wizard inside the pod, the same way they do on a laptop. Each wizard pod has a per-wizard PVC for its staging worktree.
- **Cleric pods** — ephemeral, dispatched separately by the steward when a bead gets hooked. A cleric mounts the failing wizard's PVC to resume its staging worktree in place.
- **ClickHouse** — OLAP backend for agent_runs and bead_lifecycle analytics
- **Optional archive/syncer** — if the operator wants a one-way archive export or other replication path outside the cluster

The entire stack — steward, operator, dolt, OLAP, guild caches — is deployed via a Helm chart. A tower lives in the cluster as a dolt database; repos register through `WizardGuild` CRs, either directly or derived from the tower's repos table.

The canonical `BuildApprenticePod` is the contract today, and cluster-native dispatch routes apprentice work through the operator's intent reconciler keyed on `(Role=Apprentice, Phase={Implement,Fix,ReviewFix})` — see [`pkg/steward/intent/contract.go`](../pkg/steward/intent/contract.go). The remaining gap is the steward-side producer (spi-sb9yob): steward-initiated dispatch (recovery, hooked-step resume) does not yet populate `Role`, `Phase`, or `Runtime.Image`, so those intents are dropped by `intent.Validate`. Executor- and wizard-driven dispatch populates the triple and works end-to-end. See `pkg/steward/cluster_dispatch.go` (KNOWN GAP comment) for the canonical statement.

## Operator-owned dispatch (cluster-native invariant)

Cluster-native is defined by a single boundary: **the operator owns
all child-pod dispatch.** Wizards, apprentices, sages, and clerics in
this mode are materialized by the operator from intents the steward
or wizard emits — no scheduling code path calls `pkg/agent.Spawner.Spawn`
or `backend.Spawn` directly. That includes the steward's
ready-work loop, its review-ready dispatch, its hooked-step / cleric
dispatch, the wizard's review-fix re-entry, and the executor's
implement / fix / sage-review children.

```
wizard / executor / steward decides a child is needed
  → emits a child intent (role + phase + runtime, plus the runtime contract)
  → intent reaches the operator through the shared-state outbox
  → operator pod-builder validates (role, phase, runtime) is supported
  → operator materializes the pod (apprentice / sage / cleric / wizard)
  → child runs; bundle-signal close path back to the parent wizard preserved
    (see `spi-e6m3p6`)
```

This makes one statement true everywhere in the cluster-native code
paths: **`pkg/agent.Spawner.Spawn` is a local-native concept.** It is
not the universal child-dispatch entry point. The cluster-native
invariant is enforced by the static AST test in
[`pkg/executor/cluster_dispatch_invariant_test.go`](../pkg/executor/cluster_dispatch_invariant_test.go)
and by regression tests in `pkg/executor` and `pkg/steward` that wire
a failing spawner into cluster-native dispatch entry points and assert
no `Spawn` call is ever made.

### The role / phase / runtime contract

Every cluster-native child intent carries an unambiguous `(role,
phase, runtime)` triple. The operator's pod-builder uses the triple
to pick a builder and validates it against an allowlist; an
unrecognized triple is rejected at intent-consumption time, not as
an init-container failure inside the dispatched pod.

The allowlist is the `intent.Allowed` map in
[`pkg/steward/intent/contract.go`](../pkg/steward/intent/contract.go);
the table below mirrors it row-for-row.

| Role | Phase | Runtime | Pod shape |
|------|-------|---------|-----------|
| `wizard` | `implement` | `wizard` | Wizard pod (`BuildWizardPod`); the per-bead orchestrator that drives the formula. |
| `apprentice` | `implement` | `worker` | Apprentice pod (`BuildApprenticePod`); fresh worktree; bundle handoff back to parent wizard. |
| `apprentice` | `fix` | `worker` | Apprentice pod; diagnostic fix worker; can use a borrowed worktree. |
| `apprentice` | `review-fix` | `worker` | Apprentice pod; post-review re-engagement after a sage `request_changes`. |
| `sage` | `review` | `reviewer` | Sage pod (`BuildSagePod`); diff-only review against the staging branch. |
| `cleric` | `recovery` | `wizard` | Cleric pod (`BuildClericPod`); failure-recovery driver that runs the `cleric-default` formula. The operator routes by `Role=cleric`, not by `formula_phase=recovery`. |

`intent.Validate` rejects any pair not in this table (and any intent
missing `Runtime.Image`); `agent.SelectBuilder` then picks the builder
from the matching `(Role, Phase)` entry. Both sides share a single
source of truth in `pkg/steward/intent/contract.go`.

The runtime field is the canonical worker runtime contract — see
[ARCHITECTURE.md → Worker Runtime Contract](ARCHITECTURE.md#worker-runtime-contract).
Identity (`URL`, `BaseBranch`, `Prefix`) comes from the shared tower
`repos` registry through `pkg/steward/identity.ClusterIdentityResolver`,
never from per-machine `LocalBindings`. Workspace materialization is the
backend obligation defined in
[`pkg/agent/README.md` → Backend obligations](../pkg/agent/README.md#backend-obligations-normative);
the operator's pod-builder reads the backend obligations through
`pkg/agent.PodSpec` rather than re-deriving them.

> **Steward producer gap (known follow-up):** the steward's
> `dispatchPhaseClusterNative` does not yet populate `Role`, `Phase`,
> or `Runtime.Image` on the intents it emits, so its phase-level and
> bead-level emits are dropped by `intent.Validate` until the producer
> migration lands. Executor- and wizard-side emits (apprentice/sage
> children) populate the triple through `childIntentForApprentice` /
> `childIntentForSage` and are unaffected. See
> [`pkg/steward/cluster_dispatch.go`](../pkg/steward/cluster_dispatch.go).

### Cleric dispatch

Cleric dispatch in cluster-native is a wizard-shaped workload: the
pod runs `spire execute <recovery-bead>` and drives the
`cleric-default` formula the same way a normal wizard drives a
task formula. Under the new contract the intent carries `role=cleric`
and `phase=recovery`; the operator routes by `Role=cleric` and
materializes a cleric pod via `BuildClericPod`. The legacy
`formula_phase=recovery` routing key is gone — routing is keyed on
the `(Role, Phase)` pair, not on `formula_phase`.

### Shared-state ownership for review feedback

Review-feedback re-engagement does not consult the local wizard
registry (`~/.config/spire/wizards.json`) in cluster-native paths.
That registry is per-machine bookkeeping owned by `pkg/agent`'s
process backend; it has no meaning across cluster replicas. Instead,
review-feedback owners are looked up through a shared-state surface
that is durable across replicas and pod restarts:

- the `implemented-by` / attempt-bead linkage on the work bead,
- attempt metadata (instance, agent name, started-at) carried on the
  attempt bead,
- the typed review outcome stamped onto the review/sage step bead.

This is the substrate that lets the operator close a request-changes
loop without depending on which steward replica or which laptop wrote
the original wizard's row. It is the same shared-state surface the
steward already uses for hooked-step recovery and for stale-attempt
detection — there is no second ownership plane.

### Fail closed, never fall back

Every cluster-native dispatch site fails closed when a seam is
missing. There is no silent local-spawn fallback — the steward logs
and skips the cycle, or the entry point returns a typed error, but
the work does not leak onto the local backend. Concretely:

- `pkg/steward/cluster_dispatch.go` skips dispatch if
  `ClusterDispatchConfig` or any of its required fields
  (`Resolver`, `Claimer`, `Publisher`) is nil.
- `dispatchPhaseClusterNative` returns an error if the resolver or
  publisher is missing — phase-level dispatch sites surface that
  error; they do not call `backend.Spawn` to recover.
- Executor child-dispatch sites in cluster-native mode either emit
  an intent or return an error. The static AST invariant test
  rejects any new `Spawner.Spawn` call site that is reachable from
  a cluster-native dispatch path.

The fail-closed posture is the contract review feedback expects: a
visible alert is recoverable; a silent local fall-back fragments
ownership across the cluster and the operator's view of the world.

## Bead status lifecycle

In cluster-native deployments, a work bead walks through four states:

```
ready → dispatched → in_progress → closed
```

The `dispatched` state covers the 50–90s window between the steward
emitting a `WorkloadIntent` and the wizard pod starting and running
`spire claim`. Holding an explicit state for that window matters
because concurrency caps — both tower-global (`steward.maxConcurrent`)
and per-guild (`WizardGuild.Spec.MaxConcurrent`) — count in-flight work
as `status IN ('dispatched', 'in_progress')`. Without the
`dispatched` state the caps would under-count and burst past their
limits while pods boot.

The steward owns `ready → dispatched` (atomic with the
`workload_intents` INSERT) and the two stale-recovery paths
(`dispatched → ready` on a short timeout, `in_progress → ready` on a
long timeout). The wizard owns `dispatched → in_progress` at claim and
`in_progress → closed` at seal. The operator stays task-status-agnostic
— it reconciles `workload_intents` into pods, never touches
`issues.status`. Local-native `spire summon` skips `dispatched`
entirely; the local path has no polling loop, so claim flips
`ready → in_progress` directly.

## Who it's for

- Teams that want agents running around the clock, not tied to a developer's laptop being open
- Teams that want agents working in parallel at a scale that exceeds any one machine's CPU or memory budget
- Organizations that need centralized credentials, audit logs, and observability for agent-driven work
- Anyone running Spire as shared team infrastructure instead of personal tooling

## What it optimizes for

- **Persistent capacity** — the steward is always on; beads get picked up without anyone being awake
- **Parallelism** — the operator can spawn many wizard and apprentice pods concurrently, bounded by Kubernetes resource budgets and a per-tower concurrency limit
- **Fast cold starts** — the guild-cache PVC pre-warms repo checkouts so each apprentice pod materializes its workspace from a shared cache rather than cloning from origin
- **Cluster-scale observability** — agent_runs and bead_lifecycle flow into ClickHouse for multi-agent, multi-tower analytics
- **Centralized credentials** — Anthropic, GitHub, and DoltHub creds are Kubernetes secrets, not per-developer files

## Recovery when a wizard is unsummoned

Wizard pods are ephemeral and can be unsummoned mid-bead — by a crash, an eviction, a node rotation, or a deliberate steward teardown. When that happens, the bead transitions to `hooked` status but its work-in-progress does not disappear: the per-wizard PVC persists and holds the staging worktree exactly as the wizard left it.

The steward detects the hooked bead and dispatches a cleric pod. The cleric mounts the same PVC, resumes the staging worktree in place, and runs the standard cleric loop: collect context, decide on a repair mode, execute, verify, learn. Agentic repairs that succeed and repeat are promoted to programmatic recipes over time, so recurring failure classes graduate from LLM-driven to mechanical.

This is the cluster-native counterpart to local-native's in-process recovery: same cleric, same decide/execute/verify/learn loop, same promotion pipeline — just with a PVC instead of a local worktree path.

## Cluster-resource-health

Every recoverable cluster resource Spire manages — WizardGuild.Cache
today, and syncer, ClickHouse, dolt StatefulSet, and broader
operator/steward scheduling state as they come online — follows the
same pinned-identity + wisp-recovery shape. A persistent **pinned
identity bead** names the resource in the work graph; a transient
**wisp recovery bead** carries each failure incident through the
existing cleric pipeline.

This shape is a durable commitment. Every new cluster resource Spire
provisions adopts it; there is no second model for "resources that
behave a little differently."

### Why this shape

The pattern earns the complexity of two bead tiers by delivering four
user-visible properties at once:

- **Discoverable** — the pinned identity bead is on the graph, so any
  attached laptop sees the resource via remotesapi without touching
  the cluster.
- **Recoverable** — each failure is filed as a wisp, and the existing
  cleric pipeline already knows how to claim, diagnose, repair,
  verify, and learn from it. No new recovery plane.
- **Quiet** — wisps stay cluster-local and are not git-synced, so
  laptops never see the churn of cluster-resource failures in their
  clones.
- **Current** — the presence or absence of an open wisp is the health
  signal. There is no stale metadata on the pinned bead to
  archaeology through.

### What this is not

This is not a generic Kubernetes operator framework. Spire's operator
reconciles only the resources Spire itself provisions, and the
cluster-resource-health pattern applies only to those. It exists
because Spire has agent-driven recovery recipes for those specific
resources — not as a path for users to express arbitrary
Kubernetes-resource health in beads. A user deploying their own
workloads in the same cluster does not inherit the pattern for those
workloads.

### Boundary between operator and recovery engine

The operator observes cluster state and writes beads. `pkg/recovery`
consumes beads and drives recovery. Neither imports the other's
concerns. That split is what keeps `pkg/recovery` unit-testable without
a cluster — wisp metadata is the input it sees, not a live API server —
and what keeps the operator free of recovery-policy logic. Cleric
dispatch, decide-time policy, verify wiring, and learning promotion
all stay on the recovery side of the line.

### Cross-references

See [ARCHITECTURE.md — Cluster-resource-health pattern](ARCHITECTURE.md#cluster-resource-health-pattern)
for the mechanism: the two-tier bead model, the operator's
bead-writer contract, the lifecycle, and the overlay shape for
cleric-on-resource pods.

Neither of the other deployment modes participates:
[VISION-LOCAL.md](VISION-LOCAL.md) describes a mode with no operator
and no cluster resources, so no pinned identities and no wisps for
cluster resources exist; [VISION-ATTACHED.md](VISION-ATTACHED.md)
inherits the cluster-native pattern on the remote execution surface.

## How laptops participate

There are two distinct laptop-side topologies for a cluster-native
tower. They are not interchangeable. See
[deployment-modes.md](deployment-modes.md) for the full matrix.

### Cluster-attach via gateway (the cluster-as-truth path)

The recommended path for cluster-native production towers. The laptop
attaches to the cluster's HTTPS gateway with a bearer token; every
mutation tunnels over `/api/v1/*` to the cluster Dolt, which is the
single writer.

```bash
spire tower attach-cluster --tower <name> --url https://<gateway-host> --token <bearer>
```

In this topology:

- The laptop has **no local Dolt mirror** for the tower.
- `spire push` / `spire pull` are **not used** — there is nothing to
  sync into.
- `spire file`, `spire focus`, and the desktop UI all route through the
  gateway.
- The cluster's steward owns dispatch; the laptop is a frontend.

### Direct-Dolt attach via remotesapi (server-remote topology)

A laptop may also attach by pointing its local Dolt mirror at the
cluster's remotesapi endpoint, treating the cluster as a remote Dolt
server in a `server-remote` topology. This is the historical path and
remains valid for towers that are *not* operating as cluster-as-truth —
see [deployment-modes.md](deployment-modes.md). It is **not** the
cluster-as-truth path: the laptop keeps a writable local mirror and
participates in `push` / `pull` / `sync`. Pick this only when the
operator has explicitly chosen a direct-Dolt topology over a
gateway-attached one.

The laptop never runs its own steward against a cluster-native tower in
either topology — the cluster's steward owns dispatch. A laptop that
wants to drive *remote execution* from a local control plane is in
attached mode, not cluster-native (see
[VISION-ATTACHED.md](VISION-ATTACHED.md)).

## How it connects to the other modes

The coordination protocol is identical across modes. The steward code in the cluster is the same code that runs locally. The wizard pod spec is derived from the same canonical pod builder used by the local k8s backend. Every invariant is enforced once, in code, and exercised in tests across both surfaces.

Transport: the cluster's dolt exposes remotesapi on an internal service. External laptops attach via an ingress or port-forward. DoltHub remains available as an optional secondary transport for backups or for cross-cluster sync.

## What it does not do

- **No local execution** — if a laptop wants to spawn agent processes, that is local-native, not cluster-native
- **No hybrid scheduling** — a single tower runs in exactly one deployment mode at a time
- **No per-developer isolation** — cluster-native is shared team infrastructure; RBAC and approval gates are future work

## Cutover from legacy DoltHub-backed towers

Existing Awell towers that ran in the DoltHub-backed/bidirectional-sync topology must be cut over to cluster-as-truth gateway-mode through a controlled operator procedure. The single-writer invariant for cluster-as-truth is non-negotiable: **the cluster Dolt database is the only writer**; legacy local writers (laptop daemons, stewards, direct DoltHub PATs) must be quiesced and credentials revoked before any laptop attaches through the gateway. Bidirectional sync between cluster and DoltHub is not an option — DoltHub becomes archive-only or disabled.

The full operator procedure — inventory legacy writers, quiesce daemons, stop cluster syncers, revoke DoltHub write credentials, clean local config, attach through the gateway, validate end-to-end, and rollback via GCS restore — lives in [docs/runbooks/cluster-as-truth-cutover.md](runbooks/cluster-as-truth-cutover.md).
