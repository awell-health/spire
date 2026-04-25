# pkg/steward

Tower-level coordination and daemon orchestration.

This package is above per-bead execution. It decides which work should be
assigned, which wizards are idle or stale, and when tower-wide maintenance
work should run.

If `pkg/executor` is the per-bead control plane, `pkg/steward` is the
multi-bead coordinator.

## Deployment-mode dispatch

The steward's scheduling entry (the dispatch step in `TowerCycle`,
`steward.go`) reads `tower.EffectiveDeploymentMode()` from
`pkg/config/deployment_mode.go` and branches on the three values. The
contract here is normative — every cluster-native scheduling path lives
behind these branches.

| `DeploymentMode` value | What this package does |
|------------------------|------------------------|
| `local-native` (default) | Existing direct-spawn loop: the steward calls `backend.Spawn` for each schedulable bead, reading `LocalBindings` for repo bootstrap inputs. This path is the only one allowed to read `LocalBindings.State`, `LocalBindings.LocalPath`, or `cfg.Instances`. |
| `cluster-native` | `cluster_dispatch.go` runs. The steward resolves repo identity through `pkg/steward/identity.ClusterIdentityResolver`, claims an attempt bead and emits a `pkg/steward/intent.WorkloadIntent` through `pkg/steward/dispatch.ClaimThenEmit`, and never creates pods directly. |
| `attached-reserved` | The dispatch step skips with a typed `attached.ErrAttachedNotImplemented` log line. No work is dispatched in this mode today. |

### Cluster-native: the three seams

When `EffectiveDeploymentMode == cluster-native`, the steward composes
exactly three seams. Wiring lives on `StewardConfig.ClusterDispatch`
(see `cluster_dispatch.go`); a nil entry — or any nil field — disables
cluster-native dispatch.

For the bead-level scheduling entry (`dispatchClusterNative`), a nil
seam logs and skips the cycle. For the per-phase dispatch entry
(`dispatchPhase`, used by review-ready and hooked-recovery), a nil
seam fails closed with `ErrClusterDispatchUnavailable` rather than
falling back to `backend.Spawn` — the cluster path must never reach
`backend.Spawn`, even on misconfiguration.

1. **`identity.ClusterIdentityResolver`** — resolves a repo prefix to its
   canonical `ClusterRepoIdentity` using the shared tower repo registry
   (the `repos` table in dolt). The cluster path MUST resolve through
   this seam and never read `LocalBindings.State`,
   `LocalBindings.LocalPath`, or `cfg.Instances`.
2. **`dispatch.AttemptClaimer` + `dispatch.DispatchEmitter` via
   `dispatch.ClaimThenEmit`** — the only allowed dispatch path.
   `ClaimNext` verifies no active attempt exists for the task and
   reserves the next `dispatch_seq`; `Emit` refuses to publish without a
   matching `ClaimHandle`. The `workload_intents` `(task_id,
   dispatch_seq)` composite PK is the canonical ownership seam — no
   in-process busy map, mutex, or `sync.Map` is allowed as a substitute.
   **Steward does not create attempt beads.** Attempt-bead lifecycle is
   wholly the wizard's responsibility: the wizard sees
   `SPIRE_BEAD_ID=<task_id>` and calls `ensureAttemptBead` inside the
   dispatched pod to open (or resume) its own attempt.
3. **`intent.IntentPublisher`** — the scheduler-side exit seam that
   delivers a `WorkloadIntent` to the operator. The operator consumes
   it through `intent.IntentConsumer` and reconciles cluster resources
   to match. This package never imports `k8s.io/*`; the publisher
   transport (a CR apply, in production) is plumbed in by `cmd/spire`.

If a steward-internal cluster path needs an apprentice pod, it MUST
call `pkg/agent.BuildApprenticePod` rather than building the pod shape
locally. There is no in-package pod construction in cluster-native code
paths.

## Bead status lifecycle

Cluster-native dispatch walks a bead through four states. Ownership of
each transition is strict:

```
ready
  ↓ (steward: INSERT workload_intents + UPDATE issue status, one tx)
dispatched
  ↓ (wizard: spire claim, once the pod starts)
in_progress
  ↓ (wizard: close action)
closed
```

Recovery paths (steward-owned):

- `dispatched → ready` via `RecoverStaleDispatched` on a short
  timeout (~5m). Catches pods that never started: image pull error,
  node pressure, crash loop. Holding the dispatched state means the
  slot stays counted against the concurrency cap, so prompt recovery
  is required.
- `in_progress → ready` via the existing stale detector (hours).
  Catches wizards that died mid-work.

| Transition | Owner |
|---|---|
| `ready → dispatched` | Steward (`DoltPublisher.Publish` emission tx) |
| `dispatched → in_progress` | Wizard (`spire claim`) |
| `in_progress → closed` | Wizard (close action) |
| `dispatched → ready` (stale) | Steward (`RecoverStaleDispatched`, short timeout) |
| `in_progress → ready` (stale) | Steward (`CheckBeadHealth`, long timeout) |

The operator is task-status-agnostic: it reconciles `workload_intents`
rows into apprentice pods, but never mutates `issues.status`. That
boundary is the invariant that keeps `dispatched` meaningful — if the
operator touched status, the meaning would diffuse.

Local-native `spire summon` skips the `dispatched` intermediate: the
local path has no polling loop, so the wizard claim flips
`ready → in_progress` directly. `spire claim` accepts both `ready` and
`dispatched` as valid source statuses so both paths work.

## Concurrency caps

Two layers, both applied at dispatch time against the same in-flight
predicate `status IN ('dispatched', 'in_progress')`:

- **Tower-global cap** — `ClusterDispatchConfig.MaxConcurrent` (helm
  value `steward.maxConcurrent`, flag `--max-concurrent`, env
  `STEWARD_MAX_CONCURRENT`). 0 = unlimited. Enforced in
  `dispatchClusterNative` before emitting any intents; the cycle skips
  dispatch entirely when at or above the cap.
- **Per-guild cap** — `WizardGuild.Spec.MaxConcurrent`. Enforced at
  the operator layer when assigning workloads to guilds.

Before `dispatched` existed, the 50–90s window between emit and wizard
claim wasn't counted anywhere, so both caps under-counted and could
burst through their limits. Including `dispatched` in the predicate
closes that window.

### The LocalBindings rule

> **Cluster-native code paths MUST NEVER read `LocalBindings.State`,
> `LocalBindings.LocalPath`, or `cfg.Instances`.**

`LocalBindings` is per-machine workspace state — bind status and the
local clone path of a repo on this archmage's filesystem. Those values
have no meaning across cluster replicas: another replica of the steward
running in the same tower will see different `LocalBindings` for the
same prefix, or none at all. Treating them as authoritative in cluster
scheduling silently fragments ownership across replicas.

The local-native dispatch path may read `LocalBindings` (it is the only
caller that owns the local workspace). Cluster-native code resolves
repo identity through `identity.ClusterIdentityResolver` and treats
`LocalBindings` as if it did not exist. The boundary is enforced
mechanically: `cluster_dispatch.go` carries no `cfg.Instances` access
or `LocalBindings` dereference, and the cluster identity resolver's
`LocalBindingsAccessor` field is wired with a panicking stub in tests
to prove `Resolve` never touches it.

## What this package owns

- **Ready-work assignment**: find ready beads and assign them to idle agents.
- **Roster usage**: load agent state from the configured backend and compute
  busy vs idle capacity.
- **Stale and timeout enforcement**: detect unhealthy work, warn, dismiss, or
  reset agents based on configured thresholds.
- **Review re-engagement routing**: detect beads that need follow-up review or
  wizard re-entry and route them back into motion.
- **Tower daemon duties**: sync loops, inbox delivery, dead-agent cleanup, and
  tower-level maintenance work.
- **Hooked-step sweeping**: queries step beads by `status=hooked`, checks
  whether the blocking condition has resolved, and re-summons the wizard.
- **Cleric auto-summoning**: detects failure evidence on hooked beads and
  summons clerics to recover them (see section below).
- **Concurrency limiter**: per-tower `MaxConcurrent` enforcement — tracks
  active agents and gates spawning via `ConcurrencyLimiter`.
- **Merge queue**: serializes merge operations to prevent git push contention
  (`MergeQueue`).
- **Trust gradient**: per-repo trust levels that gate review requirements and
  auto-merge permissions (`TrustChecker`). Promotes after consecutive clean
  merges, demotes on reverts/failures.
- **Backend-facing coordination**: local process dispatch vs managed backends.

## Cleric auto-summoning

When a wizard fails, the executor files a recovery bead (type `recovery`)
linked to the hooked parent via a `caused-by` dependency. The steward's
hooked-step sweep detects this failure evidence and summons a cleric to
investigate.

### Flow

1. `SweepHookedSteps` queries beads with `status=hooked`.
2. For each hooked parent, `findFailureEvidence` looks for `caused-by`
   dependents of type `recovery`.
3. If the recovery bead is **closed**: check `learning_outcome` metadata.
   - `"escalated"` — leave hooked for human attention.
   - Anything else (success) — unhook steps, set parent to `in_progress`,
     re-summon the wizard.
4. If the recovery bead is **open**: summon a cleric via the claim-then-spawn
   pattern (see below).

### Claim-then-spawn pattern

The steward uses the same atomic claim pattern as `spire claim`:

1. `CreateAttemptBeadAtomic` — atomically creates an attempt bead on the
   recovery bead. If another agent (on any instance) already claimed it,
   this call rejects and the steward skips the summon.
2. `StampAttemptInstance` — stamps instance ownership metadata on the attempt.
3. `UpdateBead` — sets the recovery bead to `in_progress`.
4. `dispatchPhase` — routes the cleric: in local-native mode this calls
   `backend.Spawn`; in cluster-native mode it emits a phase-keyed
   `WorkloadIntent` via the cluster seam (no `backend.Spawn`).

In cluster-native mode, the cleric dispatch carries
`FormulaPhase = clericDispatchPhase()` (currently `intent.PhaseWizard`,
the canonical bead-level phase). Stamping the recovery bead's type
(`"recovery"`) would emit an unsupported `formula_phase` value the
operator drops — `clericDispatchPhase` keeps the phase choice in one
place so it can switch when the cluster intent contract gains a
dedicated cleric role/phase pair.

The spawned cleric executor calls `ensureGraphAttemptBead` on startup, finds
the attempt bead created by the steward (matching agent name), and reuses it.

This pattern is multi-local safe: two stewards on different machines will
both attempt the atomic claim, but only one succeeds. The loser sees
"active attempt already exists" and skips.

**Do not use registry-based duplicate detection for spawn decisions.** The
agent registry (`wizards.json`) is a local process tracker — it is not an
ownership mechanism. Use attempt beads for ownership; they live in the shared
store and are atomically created.

## What this package does NOT own

- **Per-bead lifecycle execution**: once work is assigned, the executor owns
  phase progression for that bead.
- **Subprocess runtime details**: prompt assembly, Claude invocation, result
  files, validation, and commit logic belong in `pkg/wizard`.
- **Git semantics**: branches, worktrees, merges, refs, and SHAs belong in
  `pkg/git`.
- **Formula authoring or validation**: formula creation and dry-run belong in
  `pkg/workshop`.
- **Formula interpretation**: the steward assigns work; it does not interpret
  formula graphs.

## Relationship To Wizard And Executor

The clean split is:
- **steward** decides which bead should run and which wizard should take it
- **executor** drives one bead through its lifecycle
- **wizard** performs one subprocess role inside the workspace chosen for it

The steward should not accumulate bead-specific execution logic. If the change
is about review rounds, merge behavior, staging worktrees, or formula steps,
it probably belongs in `pkg/executor`, not here.

## Key entrypoints

| Entry point | Purpose |
|-------------|---------|
| `Cycle` | Run one steward cycle across all configured towers. |
| `TowerCycle` | Run ready-work assignment and health checks for one tower. |
| `CheckBeadHealth` | Detect stale, wedged, or corrupt work and trigger cleanup or alerts. |
| `daemon.go` flows | Run tower-wide background duties like sync, inbox delivery, and dead-agent cleanup. |

## Practical rules

1. **Keep policy tower-level.** This package decides which work should move, not how a bead should execute internally.
2. **Do not duplicate executor state machines.** If a fix requires knowing review-step semantics or formula routing, push that logic down into `pkg/executor`.
3. **Treat steward as capacity and health management.** Summoning, unsummoning, resetting, and replacing workers belong here or just above it.
4. **Use explicit package boundaries.** Assignment decisions belong here; workspace decisions belong in formulas + executor; git mechanics belong in `pkg/git`.
5. **Fail closed on inconsistent work graph state.** If attempt beads or routing state look corrupt, alert instead of assigning aggressively.

## Where new work usually belongs

- Add it to **`pkg/steward`** when the change affects tower-wide assignment, capacity, or health checks.
- Add it to **`pkg/executor`** when the change affects one bead's lifecycle or formula interpretation.
- Add it to **`pkg/wizard`** when the change affects how a summoned subprocess runs.
- Add it to **`pkg/workshop`** when the change affects formula creation, validation, or publishing.
