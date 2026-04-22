# Cluster-Native Deployment Vision

> The coordination plane runs in Kubernetes.

Cluster-native is how Spire scales from "one laptop's worth of agents" to "a team's worth of persistent capacity." The steward, the operator, the dolt database, and the agent pods all live in a Kubernetes cluster. Laptops attach to the cluster's dolt via remotesapi to monitor, file work, and interact with the graph — but coordination and execution happen in the cluster, not on a developer's machine.

## What runs

In a cluster-native deployment:

- **Steward pod** — the coordinator loop, same code as the local-native steward, running as a Deployment
- **Operator pod** — watches `WizardGuild` custom resources and reconciles workload intent into wizard pods
- **Dolt StatefulSet** — the tower's database, with remotesapi enabled for laptop attach and for internal cluster traffic
- **Wizard pods** — ephemeral, one per in-flight bead, built from the canonical pod spec. A wizard pod is the unit of dispatch: apprentices, sages, and arbiters run as child processes of the wizard inside the pod, the same way they do on a laptop. Each wizard pod has a per-wizard PVC for its staging worktree.
- **Cleric pods** — ephemeral, dispatched separately by the steward when a bead gets hooked. A cleric mounts the failing wizard's PVC to resume its staging worktree in place.
- **ClickHouse** — OLAP backend for agent_runs and bead_lifecycle analytics
- **Optional syncer** — if the cluster also pushes to DoltHub for backup or cross-cluster sync

The entire stack — steward, operator, dolt, OLAP, guild caches — is deployed via a Helm chart. A tower lives in the cluster as a dolt database; repos register through `WizardGuild` CRs, either directly or derived from the tower's repos table.

Future work (spi-sj18k) will move apprentice execution out of the wizard pod into dedicated apprentice pods dispatched through the operator's intent reconciler. The canonical `BuildApprenticePod` exists today and is exercised by parity tests, but cluster-native dispatch still runs apprentice work in-wizard — matching the local-native shape.

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

A laptop talks to a cluster-native tower through `spire tower attach-cluster <dolt://.../db>`. This points the laptop's CLI at the cluster's dolt via remotesapi. From there, the laptop can:

- `spire file` to create beads
- `spire focus` / `spire board` to read and watch
- `spire push` / `spire pull` to sync any local-authored state

The laptop does **not** run a steward in this topology — the cluster's steward owns dispatch. A laptop that wants to drive remote execution from a local control plane is in attached mode, not cluster-native (see [VISION-ATTACHED.md](VISION-ATTACHED.md)).

## How it connects to the other modes

The coordination protocol is identical across modes. The steward code in the cluster is the same code that runs locally. The wizard pod spec is derived from the same canonical pod builder used by the local k8s backend. Every invariant is enforced once, in code, and exercised in tests across both surfaces.

Transport: the cluster's dolt exposes remotesapi on an internal service. External laptops attach via an ingress or port-forward. DoltHub remains available as an optional secondary transport for backups or for cross-cluster sync.

## What it does not do

- **No local execution** — if a laptop wants to spawn agent processes, that is local-native, not cluster-native
- **No hybrid scheduling** — a single tower runs in exactly one deployment mode at a time
- **No per-developer isolation** — cluster-native is shared team infrastructure; RBAC and approval gates are future work
