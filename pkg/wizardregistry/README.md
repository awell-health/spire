# pkg/wizardregistry — wizard liveness & ownership boundary

`wizardregistry` is the unified contract for tracking wizards across
local-native and cluster-native deployments. Liveness predicates,
sweep semantics, and ownership lookups all flow through one
`Registry` interface so consumers (OrphanSweep, AgentMonitor liveness,
steward, board, trace, summon) never branch on mode.

## Purpose

Before this package, local-native and cluster-native each had their
own answer to "is this wizard alive?":

- **Local**: `pkg/registry` returned a snapshot of `~/.config/spire/wizards.json`
  and probed PIDs via `syscall.Kill`. Race-prone (snapshot lag) and
  zombie-blind.
- **Cluster**: `AgentMonitor` checked pod phase ad-hoc inline with
  reaping decisions, with no contract that other consumers could
  depend on.

`wizardregistry.Registry` collapses both into a single, race-safe
contract. The OrphanSweep race incident [`spi-5bzu9r`] — a parent bead
reverting from `in_progress` to `open` because a concurrent sweep
mis-classified a fresh attempt as orphan — is closed by the contract,
not by ad-hoc fixes at every call site.

## Interface contract

The canonical declaration lives in [`registry.go`](registry.go); read
it before touching any caller. The methods at a glance:

| Method | Contract |
|--------|----------|
| `List(ctx) ([]Wizard, error)` | Snapshot of currently-registered wizards. Returns a copy — caller may retain and mutate. |
| `Get(ctx, id) (Wizard, error)` | Lookup by ID. Returns `ErrNotFound` when no entry exists. |
| `Upsert(ctx, w) error` | Add or replace by `w.ID`. Read-mostly backends MUST return `ErrReadOnly`. |
| `Remove(ctx, id) error` | Delete by ID. Returns `ErrNotFound` when no such entry. Read-mostly backends MUST return `ErrReadOnly` regardless. |
| `IsAlive(ctx, id) (bool, error)` | Fresh authoritative read — no caching across calls. Returns `(false, ErrNotFound)` if entry is missing; `(false, nil)` if entry exists but underlying process/pod is gone; `(true, nil)` if alive. |
| `Sweep(ctx) ([]Wizard, error)` | Subset of registered wizards whose process/pod is dead. Predicate-only — MUST NOT remove entries. |

The `Wizard` value is mode-tagged: `PID` is meaningful only when
`Mode == ModeLocal`; `PodName` and `Namespace` are meaningful only
when `Mode == ModeCluster`. The unused fields for the other mode are
zero-valued. Readers MUST inspect `Mode` before reading mode-specific
fields.

## Race-safety rule

`IsAlive` and `Sweep` MUST consult the authoritative source on each
call. Implementations MUST NOT cache liveness across calls or operate
on a snapshot of the wizard set captured before the per-entry liveness
check. This rule is load-bearing for the OrphanSweep race fix from
[`spi-5bzu9r`]: a wizard upserted between snapshot capture and per-entry
predicate evaluation must never be mis-classified as dead.

Concretely, an implementation that lists entries, captures the slice,
and then evaluates liveness for each captured entry violates the
guarantee. Sweep implementations MUST either hold the lock (or
equivalent serialization) across both list and per-entry liveness
evaluation, OR re-read the authoritative source on each per-entry
check.

## Local impl — `pkg/wizardregistry/local`

Backed by a JSON file under `~/.config/spire` guarded by an OS file
lock. Liveness goes through the zombie-safe
[`pkg/process.ProcessAlive`] probe (kill -0 + platform-specific zombie
detection); zombie PIDs are reported dead, closing the parallel
zombie gap that [`spi-k2bz93`] left in the legacy `pkg/registry`.

Sweep holds both an in-process mutex and the cross-process flock
across the full iteration — a concurrent Upsert serializes against
that critical section, so a fresh upsert is never mis-classified as
dead.

## Cluster impl — `pkg/wizardregistry/cluster`

Backed by live Kubernetes API reads. Each `IsAlive` issues a fresh
`kube-apiserver` query filtered to the wizard-role label in the
operator's namespace. No result is cached.

Read-mostly: `Upsert` and `Remove` always return `ErrReadOnly`.
Wizard-pod creation and deletion are owned by the operator's
reconciliation loop; clients query liveness but never mutate the
cluster registry. The conformance suite skips Upsert/Remove cases
when `ErrReadOnly` is returned, so the cluster impl passes the same
suite as the local impl by faithfully reporting its constraint.

A pod is alive iff `Status.Phase == Running` and `DeletionTimestamp`
is nil. Pending, Failed, Succeeded, Unknown, or terminating pods all
report dead.

## Adding a new backend (e.g. Redis)

1. Implement the `Registry` interface in a new package under
   `pkg/wizardregistry/<name>`.
2. Run the conformance suite at
   [`pkg/wizardregistry/conformance`](conformance/conformance.go) —
   pass it before merging. Read-mostly backends pass by returning
   `ErrReadOnly` from Upsert/Remove; the suite skips those cases.
3. Wire it into the binary entry: cmd/spire (local CLI) and
   operator/main.go (cluster) are the two places that construct
   concrete Registry instances. New backends become reachable by
   plumbing them through the same construction sites.
4. Update the wizard-liveness boundary section of
   [`docs/ARCHITECTURE.md`](../../docs/ARCHITECTURE.md) so the
   architecture doc keeps pace.

The design bead [`spi-e5ywp1`] carries the rationale for the
contract shape; read it before proposing structural changes.

## Mode-portability — who consumes the interface

| Caller | Site | What it asks |
|--------|------|--------------|
| `pkg/beadlifecycle.OrphanSweep` | daemon tick / `BeginWork` | Fresh per-iteration `IsAlive` for every candidate attempt. No snapshot of liveAgents — the [`spi-5bzu9r`] race-fix concretization. |
| `operator/controllers.AgentMonitor` | reconciler | Liveness queries (where added in future) consult `Registry.IsAlive`; pod reaping and stale-pod deletion stay k8s-native because they hold the live pod object. |
| `pkg/steward` | daemon | Inbox delivery + dead-letter reaping read `agent.RegistryList` directly (rich-fields path); cluster-portable callers go through the Registry. |
| `pkg/board` | Agents tab | `Registry.List` + `Registry.IsAlive` for the live agent panel. |
| `pkg/trace` | `/api/v1/beads/{id}/trace` | `Registry.Get` for the active wizard's start time → `ElapsedMs`. |
| `pkg/summon` | spawn flow | `Registry.List` for duplicate-guard; `Registry.Upsert` to stamp PID after spawn. |

Consumers never know which impl backs the interface.

[`spi-5bzu9r`]: ../../spi-5bzu9r
[`spi-e5ywp1`]: ../../spi-e5ywp1
[`spi-k2bz93`]: ../../spi-k2bz93
[`pkg/process.ProcessAlive`]: ../process/process.go
