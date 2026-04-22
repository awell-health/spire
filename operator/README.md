# operator

Spire's Kubernetes operator. **Pure reconciler of `WorkloadIntent`,
never a scheduler.**

This module contains the controller-runtime binary that runs in the
cluster when Spire is in `cluster-native` mode. It watches the
scheduler-to-reconciler seam (`pkg/steward/intent.WorkloadIntent`),
materializes apprentice pods through the canonical pod constructor in
`pkg/agent`, and reconciles supporting resources (PVCs, refresh Jobs,
heartbeats). Architectural framing lives in
[docs/ARCHITECTURE.md → Deployment modes / Seams](../docs/ARCHITECTURE.md#deployment-modes).

## Role: reconciler, not scheduler

The canonical cluster-native flow is:

```
steward (pod)                      operator                       cluster
  │ ClaimThenEmit                    │ Consume(WorkloadIntent)      │
  │   └─ atomic attempt-bead claim   │   └─ reconcile pod state     │
  │ Publish(WorkloadIntent) ────────▶│ BuildApprenticePod ─────────▶│ apprentice pod
```

The operator does not decide what work should run, when it should run,
or which agent should take it. Those are scheduler responsibilities and
they live in `pkg/steward`. The operator's job is to drive cluster
state toward the intents the steward has already published.

Concretely this means:

- **No second scheduler in this module.** The operator never reads `bd
  ready` to decide what to dispatch, never opens an attempt bead, and
  never picks a guild for a workload. It only reconciles intents the
  steward has emitted.
- **Pod creation goes through `pkg/agent.BuildApprenticePod`.** Pod
  shape is owned by `pkg/agent`. There is no in-operator pod shape
  logic; new fields go on `pkg/agent.PodSpec` and are plumbed through.
  See [pkg/agent/README.md → Apprentice pod shape](../pkg/agent/README.md#apprentice-pod-shape-buildapprenticepod-is-canonical).
- **Repo identity comes from `pkg/steward/identity.ClusterIdentityResolver`.**
  CR fields like `WizardGuild.Spec.Repo` and `Spec.RepoBranch` are
  treated as projection-only. The operator reconciles them to the
  resolver's output rather than making scheduling decisions from them.
  CRs can drift; the shared tower `repos` registry is authoritative.

## The `OperatorEnableLegacyScheduler` gate

`BeadWatcher` and `WorkloadAssigner` (the original cluster scheduler
loops in `controllers/bead_watcher.go` and
`controllers/workload_assigner.go`) are **transitional**. They predate
the explicit deployment-mode contract and are retained only so that
existing cluster installs have a co-existence path while the
spi-sj18k migration completes.

A boolean gate decides whether they start:

| Surface | Value |
|---------|-------|
| CLI flag | `--enable-legacy-scheduler` (`main.go`) |
| Env var | `$SPIRE_OPERATOR_ENABLE_LEGACY_SCHEDULER` (when wired in helm) |
| CR spec field | `SpireConfigSpec.EnableLegacyScheduler` |
| Default | **`false`** (canonical cluster-native) |

When the gate is `false` (the default), the legacy `WorkloadAssigner`
is not added to the controller manager. The operator runs only the
canonical reconcilers — `IntentWorkloadReconciler` (consumes
`WorkloadIntent`), `AgentMonitor` (heartbeats and pod tracking), and
`CacheReconciler` (guild repo cache PVCs/Jobs). The startup log line
records the gate's effective value.

When the gate is `true`, the legacy loops start alongside the intent
reconciler so a single cluster can serve both control-plane revisions
during cutover. This co-existence mode is explicitly a transitional
state — every legacy scheduler file under `controllers/` carries a
top-of-file comment marking it transitional and pointing at
[`pkg/config/deployment_mode.go`](../pkg/config/deployment_mode.go).
The gate is expected to be removed entirely once installs have
migrated and `BeadWatcher` / `WorkloadAssigner` can be deleted.

## Layout

| Path | Purpose |
|------|---------|
| `main.go` | Entry point. Parses flags, wires the controller manager, evaluates the legacy-scheduler gate, registers reconcilers. |
| `api/v1alpha1/` | CRD types (`WizardGuild`, `SpireWorkload`, `SpireConfig`). |
| `controllers/intent_reconciler.go` | Canonical: consumes `pkg/steward/intent.WorkloadIntent` and reconciles apprentice pods via `pkg/agent.BuildApprenticePod`. |
| `controllers/agent_monitor.go` | Heartbeat tracking, pod lifecycle, completion reaping. Always on. |
| `controllers/cache_reconciler.go` | Materializes guild repo cache PVCs and refresh Jobs. Always on; inactive when no guild declares `Spec.Cache`. |
| `controllers/bead_watcher.go` | **Transitional.** Legacy `bd ready`-driven workload creation. Gated off by default. |
| `controllers/workload_assigner.go` | **Transitional.** Legacy guild assignment loop. Gated off by default. |

See the comments in each legacy controller file for the migration plan
and the precise removal criteria.

## Practical rules

1. **Never add a new scheduler.** New cluster-native dispatch logic
   belongs upstream in `pkg/steward`. The operator's job is to react to
   what the steward has emitted.
2. **Never reimplement pod shape.** Reach for
   `pkg/agent.BuildApprenticePod`. If `PodSpec` is missing a field you
   need, add it to `pkg/agent` (with parity-test coverage) rather than
   constructing a `corev1.Pod` here.
3. **Never read CR fields as the source of truth for repo identity.**
   `URL`, `BaseBranch`, and `Prefix` come from
   `identity.ClusterIdentityResolver`. CR fields are projections of
   that resolver's output and may be stale.
4. **Never start `BeadWatcher` / `WorkloadAssigner` in new code paths.**
   Both are transitional and live behind the
   `OperatorEnableLegacyScheduler` gate. Code that depends on them
   directly (rather than on the canonical intent reconciler) is
   regressing the contract.
