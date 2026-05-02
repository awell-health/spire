# pkg/lifecycle

Exclusive owner of the (task bead status × attempt bead × wizard-registry
entry) state machine.

## Entrypoints

| Entry | Purpose | Calls |
|-------|---------|-------|
| `BeginWork` | Local-summon path. Sweeps stale state, upserts the wizard registry entry, creates the attempt, flips the bead to `in_progress`. | `cmd/spire/summon.go` |
| `ClaimWork` | Reclaim or claim path (local + cluster). Creates an attempt and transitions to `in_progress`; upserts the registry entry only in `ModeLocal`. | `cmd/spire/claim.go` |
| `EndWork` | Closes the attempt, cascades alert/recovery children, transitions the bead, removes the registry entry. | `cmd/spire/{resummon,reset}.go`, `pkg/steward` |
| `OrphanSweep` | Reaper. Closes attempts whose wizards are no longer alive. | `pkg/steward/daemon.go`, `BeginWork` (defensive scoped sweep) |

## Liveness contract

Every liveness decision in this package is a **fresh**
`wizardregistry.Registry.IsAlive` call. There is no in-package snapshot
of the wizard set, no per-tick `liveAgents` map, no cached probe result.

The race-safety guarantee documented on
[`wizardregistry.Registry`](../wizardregistry/registry.go) is the
load-bearing contract. Concretely: `IsAlive` and `Sweep` consult the
authoritative source on each call, so a wizard upserted concurrently
with a sweep cannot be mis-classified as dead. This closes the
spi-5bzu9r OrphanSweep race at the architectural layer rather than via
per-impl workarounds.

The fail-open posture: when a registry call returns a transient error
(anything other than `ErrNotFound`), `OrphanSweep` skips that candidate
rather than reaping it. Mis-classifying a registry-read blip as "dead"
is exactly the failure mode this package is designed to prevent.

## Mode portability

`OrphanSweep` runs in both `local-native` and `cluster-native` modes
through the same code path. The injected `wizardregistry.Registry`
implementation hides the mode-specific liveness oracle:

| Mode | Implementation | Liveness oracle |
|------|----------------|-----------------|
| local-native | `pkg/wizardregistry/local` (or `cmd/spire`'s adapter that projects the rich `pkg/agent` registry shape onto the same contract) | `process.ProcessAlive` — a zombie-aware PID probe |
| cluster-native | `pkg/wizardregistry/cluster` | live k8s pod-phase query (`Running` ⇔ alive) |

`OrphanSweep` (and every other consumer of the contract — AgentMonitor
liveness queries, steward, board, trace, summon) takes a
`wizardregistry.Registry` as its liveness oracle. The legacy
`pkg/registry` package is removed (spi-p6unf3); the
[`pkg/wizardregistry`](../wizardregistry/README.md) contract is the
sole sanctioned wizard-tracking surface across modes.

The `Wizard.ID` field is opaque to `lifecycle`: in `ModeLocal` it is
the agent name (`wizard-<bead-id>`); in `ModeCluster` it is the pod
name. The attempt bead's `agent:<id>` label carries the same opaque
key, so Scan B can route from attempt → liveness check without knowing
the underlying mode.

## Dependencies

`Deps` is the narrow surface — bead reads/writes, attempt
create/close/list, label add/remove, alert cascade. Implementations
live in:

- `cmd/spire/lifecycle_bridge.go` — wires to the cmd-side store bridge.
- `pkg/steward/lifecycle_deps.go` — wires to `pkg/store` directly for the
  daemon tick.

The two adapters intentionally differ on `CreateAttemptBead`: the
cmd-side adapter uses the atomic claim variant
(`storeCreateAttemptBeadAtomic`), the steward-side uses the plain
`store.CreateAttemptBead`. Both satisfy the `Deps` contract.

## What this package does NOT own

- The executor's close path (`pkg/executor/graph_actions.go:actionBeadFinish`)
  is an explicit carve-out and bypasses `EndWork`. Changes there must
  preserve the same end-state shape but they do not call through this
  package.
- Wizard-registry mechanics (file format, locking, pod-list semantics)
  live in `pkg/wizardregistry` and its sub-packages.
- The PID-stamp step that runs after `summon.SpawnWizard` lives in
  `pkg/summon`; this package only writes the placeholder entry from
  `BeginWork`.

## CI gate: no new direct bead.status writes

`pkg/lifecycle` is the sole sanctioned writer of `bead.status`. To stop
that invariant from rotting as new code lands, CI runs
`scripts/check-lifecycle-gate.sh`, which greps the repo for direct
status-write patterns outside this package and fails on any addition.

### What it matches

Across every `*.go` file (excluding `*_test.go`, `vendor/`, and
`pkg/lifecycle/`), the gate flags:

1. `UpdateBead(...)` calls with a `"status"` map key on the same line.
2. `.Status = "..."` string-literal assignments.
3. `"status": "..."` literal map/struct keys with string values.

Each match is normalized to `<path>:<trimmed-line>` and diffed against
`scripts/lifecycle-gate-allowlist.txt`.

### Run locally

```bash
bash scripts/check-lifecycle-gate.sh   # exits 0 if no new violations
make lifecycle-gate                    # same, via Makefile
```

CI runs the same script in `.github/workflows/ci.yml` (the `build` job).

### Grandfathered call sites and the allowlist

Landing 1 (this landing) only enforces *no new additions*. The existing
~85 direct writes — most concentrated in `pkg/executor/graph_interpreter.go`,
`pkg/executor/executor_dag.go`, `pkg/executor/action_dispatch.go`,
`pkg/wizard`, `pkg/gateway`, `pkg/steward`, `pkg/summon`, and a handful
of `cmd/spire/` commands — are seeded into
`scripts/lifecycle-gate-allowlist.txt` so the gate stays green while
Landing 2 deletes them one by one.

**When migrating a grandfathered call site through `pkg/lifecycle` in
Landing 2:** delete the corresponding entry from the allowlist (or
re-run `bash scripts/check-lifecycle-gate.sh --regenerate` after the
migration, which rewrites the allowlist from the current matches). The
gate notes removed entries but does not fail on them — that lets
incremental migration land without an allowlist update on every PR.

**When intentionally adding a sanctioned new direct write (rare —
should almost always go through `lifecycle.RecordEvent`):** run
`bash scripts/check-lifecycle-gate.sh --regenerate`, commit the
updated allowlist alongside the change, and explain the carve-out in
the PR description. A reviewer should push back unless the reason
matches the existing carve-outs documented above (e.g., the executor's
`actionBeadFinish` close path).

## Lifecycle predicates (Landing 1, Task 2)

The predicate seam — `lifecycle.IsActive(b)`, `lifecycle.IsMutable(b)`,
`lifecycle.IsDispatchable(b)` — is defined in `predicates.go` (Task 2
of this landing). Callers should use these predicates instead of
switching on raw `bead.status` strings, so that the meaning of "active"
or "dispatchable" can evolve in this package as Landing 3 introduces
new statuses (`awaiting_review`, `needs_changes`, `awaiting_human`,
`merge_pending`) without rippling string comparisons through every
caller. The predicate bodies stay in `pkg/lifecycle`; this gate
enforces that the *write* side stays here too.
