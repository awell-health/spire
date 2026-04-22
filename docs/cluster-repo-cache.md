# Cluster Repo-Cache Contract (Phase 2)

> Status: current-state documentation for epic **spi-sn7o3** — the
> WizardGuild-managed repo cache. Read [docs/design/spi-xplwy-runtime-contract.md](design/spi-xplwy-runtime-contract.md)
> first; this phase-2 work layers on top of the canonical runtime
> contract and does not redefine it.

This doc describes the as-implemented cluster repo-cache contract. It
is deliberately a narrow slice: one cache per `WizardGuild` for that
guild's single configured repo, materialized into a per-pod writable
workspace by a bootstrap init container. Repo identity, workspace
ownership, and cross-owner handoff remain owned by the spi-xplwy
runtime contract.

## Component map

| Concern | Owner | File |
|---|---|---|
| Cache spec / status types | CRD (spi-tpzcq) | `operator/api/v1alpha1/wizardguild_types.go` |
| Cache reconciliation (PVC + refresh Job) | operator (spi-myzn5) | `operator/controllers/cache_reconciler.go` |
| Wizard pod mounts + init container | operator (spi-s2dzk) | `operator/controllers/agent_monitor.go` (`applyCacheOverlay`) |
| Worker bootstrap helper | `pkg/agent` (spi-jetfb) | `pkg/agent/cache_bootstrap.go` |
| Bootstrap observability vocabulary | `pkg/agent` (spi-2tu4d) | `pkg/agent/cache_observability.go` |
| Init-container entrypoint | `cmd/spire` | `cmd/spire/cache_bootstrap.go` (`spire cache-bootstrap`) |
| Helm storage defaults | chart (spi-bsngj) | `helm/spire/values.yaml` (`cache.*`) |

## (a) Cache owner

A repo cache is owned by a single `WizardGuild`. One guild → one
configured repo (`WizardGuildSpec.Repo` / `RepoBranch`) → one cache.
The cache is opt-in via `WizardGuildSpec.Cache *CacheSpec`; when nil,
the guild keeps the pre-cache behavior and wizard pods bootstrap
without the cache mount.

`CacheSpec` is declared on the WizardGuild CRD
(`operator/api/v1alpha1/wizardguild_types.go`):

| Field | Type | Default | Meaning |
|---|---|---|---|
| `storageClassName` | string | chart `cache.storageClassName` (empty → cluster default) | StorageClass for the cache PVC. |
| `size` | `resource.Quantity` | chart `cache.defaultSize` (`10Gi`) | Requested PVC capacity. |
| `accessMode` | `corev1.PersistentVolumeAccessMode` | `ReadOnlyMany` (CRD default) | PVC access mode. |
| `refreshInterval` | `metav1.Duration` | `5m` (CRD default) | How often the operator schedules a refresh. |
| `branchPin` | `*string` | nil (upstream default branch) | Pin the cache to a specific branch. |

Notably absent: there is no repo URL on `CacheSpec`. Repo identity
stays authoritative via `WizardGuildSpec.Repo` and the tower/shared
registration established by spi-xplwy. The reconciler reads
`guild.Spec.Repo` verbatim and never mutates the shared repos table.

## (b) Storage and update contract

### PVC

- **Name:** `<guild-name>-repo-cache` (`pvcName` in `cache_reconciler.go`).
- **Namespace:** operator namespace.
- **Labels:** `spire.awell.io/guild=<guild-name>`, `spire.awell.io/cache-role=pvc`, `app.kubernetes.io/name=spire-guild-cache`, `app.kubernetes.io/part-of=spire`.
- **Annotation:** `spire.awell.io/cache-spec-hash=<sha256>` of the
  cache-relevant parts of the guild spec (`storageClassName`, `size`,
  `accessMode`, `refreshInterval`, `branchPin`, `repo`, `repoBranch`).
- **Access mode / size / storageClass:** pulled from `CacheSpec`
  with fallback to chart defaults (`helm/spire/values.yaml`:
  `cache.storageClassName`, `cache.defaultSize`, `cache.defaultAccessMode`).
- **Owner reference:** points at the WizardGuild with `Controller=true`
  so Kubernetes garbage-collects the PVC when the guild is deleted.
  `BlockOwnerDeletion` is intentionally unset so a misbehaving CSI
  driver cannot block guild delete.

PVCs are created on first reconcile; once present, the reconciler
leaves the existing PVC alone — resize is out of scope for the
first cut.

### Refresh Job

- **Name:** `<guild-name>-repo-cache-refresh` (`refreshJobName`).
- **Image:** configurable via `CacheReconciler.GitImage`, default
  `alpine/git:latest`.
- **Backoff / TTL:** `BackoffLimit=2`, `TTLSecondsAfterFinished=3600`.
- **Volume:** mounts the cache PVC read-write at `/cache` (the refresh
  Job is the sole writer — wizard pods mount the same PVC read-only).
- **Env:** `SPIRE_REPO_URL=<guild.Spec.Repo>`,
  `SPIRE_BRANCH_PIN=<guild.Spec.Cache.BranchPin or "">`.
- **Script:** `git clone --mirror` on a fresh PVC, `git fetch --prune
  --prune-tags --tags origin` on an existing mirror (mirror lives at
  `/cache/mirror`). Resolves the revision from
  `refs/heads/<BranchPin>` when set or the upstream's symbolic HEAD
  otherwise. Writes the revision to the cache root via an atomic
  rename (see §(d)). Echoes the revision to `/dev/termination-log`.

### Refresh cadence and invalidation

- **Periodic refresh:** On each reconcile tick the reconciler checks
  whether `CacheSpec.RefreshInterval` has elapsed since the last Job's
  completion time (`isRefreshDue`). If due, the existing Job is
  deleted (`PropagationPolicy=Background`) and a new Job is created.
- **Spec change:** On every reconcile the desired `cache-spec-hash`
  is computed and compared to the hash annotation on the existing
  Job. When they differ (any change to `CacheSpec` fields,
  `Repo`, or `RepoBranch`), the Job is recreated so the refresh
  picks up the new spec. The PVC is not recreated — spec changes that
  affect PVC shape (e.g. `size`, `accessMode`) are intentionally not
  propagated to existing PVCs in the first cut.
- **Branch pinning:** When `CacheSpec.BranchPin` is set, the refresh
  script resolves `refs/heads/<BranchPin>`; unset tracks the
  upstream's symbolic HEAD. A branch change flips the spec hash and
  triggers a fresh Job.
- **Transient failures:** A failed refresh Job sets
  `CacheStatus.Phase="Failed"` and populates
  `CacheStatus.RefreshError` from the Job's Failed condition
  (`failureMessageFromJob`). On the next successful refresh the phase
  flips back to `Ready` and `RefreshError` is cleared. The previous
  `Revision` / `LastRefreshTime` are preserved across transient
  `Refreshing`/`Failed` transitions so workers can still read the
  last-known-good revision from status.

## (c) Pod runtime contract

The pod overlay is applied by
`operator/controllers/agent_monitor.go:applyCacheOverlay` and fires
only when `WizardGuild.Spec.Cache != nil`. It mutates the pod spec
produced by the shared builder:

### Volumes

- `repo-cache`: a PVC volume backed by `<guild-name>-repo-cache`,
  marked `ReadOnly: true` at the volume source.
- `workspace`: the shared builder's existing `emptyDir` (remapped
  below).

### Mounts

- **Cache (read-only):** `repo-cache` mounted at
  `pkg/agent.CacheMountPath` (`/spire/cache`) on both the init
  container and the main container, with `ReadOnly: true`.
- **Workspace (writable):** `workspace` mounted at
  `pkg/agent.WorkspaceMountPath` (`/spire/workspace`) on both
  containers. The shared builder's default `/workspace` mount on the
  main container is remapped to `WorkspaceMountPath` in place.

### Init container (`cache-bootstrap`)

Replaces the shared builder's `repo-bootstrap` init container.

- **Name:** `cache-bootstrap`
- **Image:** the same agent image the main container uses.
- **Command:**
  ```
  spire cache-bootstrap \
    --cache-path=/spire/cache \
    --workspace-path=/spire/workspace \
    --prefix=<guild-repo-prefix>
  ```
- **Volumes mounted:** `data` (`/data`), `repo-cache` (read-only, at
  `CacheMountPath`), `workspace` (at `WorkspaceMountPath`).

`spire cache-bootstrap` (`cmd/spire/cache_bootstrap.go`) calls, in
order:

1. `agent.MaterializeWorkspaceFromCache(ctx, cachePath, workspacePath, prefix)`
2. `agent.BindLocalRepo(ctx, workspacePath, prefix)`

#### MaterializeWorkspaceFromCache

Verifies cache readiness (see §(d)), then runs `git clone
--no-hardlinks <cachePath> <workspacePath>`. The comment in
`pkg/agent/cache_bootstrap.go` records why this strategy was chosen:
`git worktree add` would need to write `.git/worktrees/<id>` back into
the cache (impossible on a read-only mount and a shared-state hazard
across pods). `--no-hardlinks` is used deliberately so the workspace
is fully independent of the cache filesystem, and the step is
idempotent — if `workspacePath/.git` already exists the clone is
skipped, so pod restarts don't re-clone.

#### BindLocalRepo

Performs the local-only bind the worker needs. It shells out to
`spire repo bind-local --prefix --path --repo-url --branch`, which
writes only to the tower's LocalBindings and the global Instances map.
`SPIRE_REPO_URL` and `SPIRE_REPO_BRANCH` env vars (wired by the
operator from the guild spec) supply the URL and branch. It does
**not** call `spire repo add` and does **not** touch the shared
`repos` dolt table — repo identity comes from the `prefix` argument,
which the caller derived from canonical tower/shared registration.

### Main container

- **Working directory:** `pkg/agent.WorkspaceMountPath` — the
  materialized workspace is the repo root, so cwd-sensitive code
  (`resolveBeadsDir`, `ResolveBackend("")`) lands correctly.
- **Env:** unchanged from phase-1. The shared builder / operator
  overlay already populates `SPIRE_REPO_URL`, `SPIRE_REPO_BRANCH`,
  `SPIRE_REPO_PREFIX`, `DOLT_DATA_DIR`, and the canonical
  observability identity env vars from spi-xplwy. `applyCacheOverlay`
  does **not** touch env, so `pkg/executor` and `pkg/wizard` see the
  same runtime surface as phase-1.

This matches the canonical `WorkspaceHandle` contract from spi-xplwy
with `Origin = WorkspaceOriginGuildCache` (`guild-cache`); see
`pkg/executor/runtime_contract.go` for the enum declaration and
`docs/design/spi-xplwy-runtime-contract.md §1` for the backend
obligations.

## (d) Failure and consistency model

### Reconciler-side serialization

The reconciler uses a **generation marker file written atomically via
rename** (documented in the `cache_reconciler.go` file header). At the
end of each successful refresh, the Job script writes:

```sh
TMP="/cache/.spire-cache-revision.tmp"
FINAL="/cache/.spire-cache-revision"
printf '%s\n' "$REV" > "$TMP"
sync
mv "$TMP" "$FINAL"
```

POSIX rename within the same filesystem is atomic, so readers never
observe a half-written revision file. This was chosen over
snapshot-promote-by-rename because a marker file is one tiny extra
file on the PVC (rather than a full shadow tree), and the refresh
Job can `git fetch` in place on the existing mirror without an
intermediate copy.

### Worker-side readiness check

`pkg/agent/cache_bootstrap.go:checkCacheReady` inspects the cache
root for two marker files:

| File | Meaning (as implemented on worker side) |
|---|---|
| `CACHE_READY` | Present → cache is complete and safe. File contents are treated as the cache revision token and surfaced on `LabelCacheRevision`. |
| `CACHE_REFRESHING` | Present → a refresh is in flight; cache must be treated as unsafe. |

When either condition indicates the cache is unsafe — `CacheMountPath`
does not exist, `CACHE_REFRESHING` is present, or `CACHE_READY` is
missing / unreadable — the helper returns the exported sentinel:

```go
var ErrCacheUnavailable = errors.New("agent: guild repo cache is unavailable (stale or mid-update)")
```

`MaterializeWorkspaceFromCache` propagates the wrapped error up to
`spire cache-bootstrap`, which exits non-zero. The init container
fails, the main container does not start, and the operator's normal
Job/pod retry behavior reschedules the pod — once the reconciler
republishes readiness, the next pod attempt succeeds.

### Handshake drift (known gap)

The marker-file handshake between the two sides is currently **not
congruent**:

- The refresh Job writes `/cache/.spire-cache-revision` via atomic
  rename; no `CACHE_REFRESHING` sentinel is written while the Job
  runs (refresh in-flightness is inferred by the reconciler from the
  Job's own state and surfaced on `CacheStatus.Phase="Refreshing"`).
- The worker reads `CACHE_READY` (presence + contents = revision) and
  `CACHE_REFRESHING` (presence = in-flight) at the cache root.

Neither side currently writes/reads the same filenames. This gap is
why phase-1 origin-clone bootstrapping continues to be the live path
— see §(g) for the migration order and the specific condition /
metric operators check before flipping a guild to the cache-backed
flow in cluster. The drift is tracked under epic spi-sn7o3 and is
expected to resolve before cache-backed startup is verified in
cluster; one side's marker semantics will be adopted by the other.

## (e) Observability vocabulary

All names are declared in `pkg/agent/cache_observability.go` and used
consistently by the bootstrap helper (`pkg/agent/cache_bootstrap.go`)
and the reconciler (`operator/controllers/cache_reconciler.go`).

### Label keys

| Key | Where it appears | Value space |
|---|---|---|
| `spire.io/bootstrap-source` | Init container logs / metric labels | Phase-2 cluster wizards: `guild-cache` (`BootstrapSourceGuildCache`). Other code paths (local process, origin clone) own their own values. |
| `spire.io/cache-revision` | Init container logs, intended for pod annotation | Contents of `CACHE_READY` — high cardinality, log/annotation only, never a metric label. |
| `spire.io/startup-phase` | Init container log lines | `cache-ready`, `workspace-derive`, `local-bind-bootstrap` (`StartupPhase*`). |
| `spire.io/cache-freshness` | Init container log lines | `fresh`, `stale`, `refreshing` (string values owned by the bootstrap helper + reconciler). |

The canonical runtime identity keys from spi-xplwy (tower, repo
prefix, role, backend, workspace kind, handoff mode) are emitted by
every bootstrap log line via `runtime.LogFields(run)` and
`runtime.MetricLabelsString(run)`. No new identity keys are introduced
by phase-2 — it reuses the spi-xplwy vocabulary verbatim.

### Metric names

| Name | Kind | Source | What it measures |
|---|---|---|---|
| `spire_cache_refresh_duration_seconds` | histogram | operator reconciler / refresh Job | Wall time of a refresh cycle. |
| `spire_cache_staleness_seconds` | gauge | worker at bind time | Seconds since last successful refresh observed by a binding pod. |
| `spire_bootstrap_duration_seconds` | histogram | init container | Wall time of `MaterializeWorkspaceFromCache` + `BindLocalRepo`. |
| `spire_bootstrap_success_total` | counter | init container | Bootstrap attempts, partitioned by a low-cardinality `result` label (`success`/`failure`). |

Because the init container is short-lived, the bootstrap helper emits
these as structured log lines (`[cache-bootstrap] metric=<name>
value=<n> result=<r> source=guild-cache labels=<canonical-ids>`)
rather than scraping from a Prometheus registration. Log-to-metric
conversion at the collector side is the authoritative aggregation
path.

### Status conditions (CRD)

Declared in `operator/api/v1alpha1/wizardguild_types.go`:

| Condition | Status semantics |
|---|---|
| `CacheReady` | True when the cache exists and has been refreshed successfully at least once — wizard pods can safely bootstrap from it. |
| `CacheRefreshing` | True while a refresh Job is in-flight. Informational; workers never block on this because the reconciler serializes refresh. |
| `CacheFailed` | True when the most recent refresh failed and no newer successful refresh has superseded it. Message carries the same detail as `CacheStatus.RefreshError`. |

The reconciler maintains `CacheStatus.Phase` (enum: `Pending`,
`Ready`, `Refreshing`, `Failed`), `CacheStatus.Revision`,
`CacheStatus.LastRefreshTime`, and `CacheStatus.RefreshError`. Until
`WizardGuildStatus.Conditions` is wired on the CRD, the reconciler
emits a log line at each status update containing the phase → condition
mapping (`conditionForPhase`) for operator tooling to grep on.

### Example status

```yaml
status:
  cache:
    phase: Ready
    revision: 1f2e3d4c5b6a7890abcdef1234567890abcdef12
    lastRefreshTime: "2026-04-22T00:15:03Z"
    refreshError: ""
```

After a transient failure:

```yaml
status:
  cache:
    phase: Failed
    revision: 1f2e3d4c5b6a7890abcdef1234567890abcdef12   # preserved
    lastRefreshTime: "2026-04-22T00:15:03Z"              # preserved
    refreshError: "BackoffLimitExceeded: Job has reached the specified backoff limit"
```

## (f) Layering boundary

Phase-2 strictly layers on top of the canonical runtime contract from
**spi-xplwy** (`docs/design/spi-xplwy-runtime-contract.md`).
Specifically:

- **Repo identity** remains owned by the executor's `RepoIdentity`
  type. It is resolved from tower/shared registration (not from cache
  state). The reconciler reads `guild.Spec.Repo` and uses it verbatim;
  `BindLocalRepo` does not call `spire repo add` and does not touch
  the shared `repos` table.
- **Workspace ownership** remains owned by the executor's
  `WorkspaceHandle` type. Phase-2 adds a new
  `WorkspaceOrigin=guild-cache` value; it does not add new fields to
  `WorkspaceHandle`, change `Kind` semantics, or touch the borrowed-
  worktree handoff path.
- **Cross-owner handoff** is not modified. `HandoffMode` is still the
  executor's enum; the cache-backed bootstrap produces a local
  workspace indistinguishable from the phase-1 origin-clone workspace
  as far as the main container is concerned.
- **Observability identity** (tower, prefix, role, backend, workspace
  kind, handoff mode) reuses the spi-xplwy `RunContext` labels
  verbatim. No new identity keys are introduced.
- **Pod-builder parity** is preserved: `applyCacheOverlay` is an
  overlay on top of the shared builder output, not a replacement.
  `pkg/executor` and `pkg/wizard` require no edits — if a change in
  either would be needed, the runtime surface has drifted and it is
  a boundary violation to escalate rather than patch around.

## (g) Migration order

Phase-1 origin-clone bootstrapping remains the default and continues
to function. Cache-backed bootstrap is gated per-guild by
`WizardGuildSpec.Cache`:

1. **Default (no `Cache` set):** the operator applies the shared
   builder's `repo-bootstrap` init container, and wizard pods clone
   the repo from origin at pod start. This path is unchanged.
2. **Opt-in (`Cache` set):** the cache reconciler provisions the PVC
   and refresh Job; `applyCacheOverlay` replaces `repo-bootstrap`
   with `cache-bootstrap` for that guild's pods.

### Verification criteria before flipping a guild to the cache-backed flow

Operators must confirm all of the following before relying on the
cache for production traffic in a given cluster:

| Check | Signal |
|---|---|
| Cache is healthy for the guild | `WizardGuild.Status.Cache.Phase == "Ready"` and the `CacheReady` condition is True. |
| Revision is current | `WizardGuild.Status.Cache.Revision` matches `git ls-remote <repo> refs/heads/<branch-or-HEAD>`, and `LastRefreshTime` is within `CacheSpec.RefreshInterval`. |
| No failed refreshes outstanding | `RefreshError` is empty and `CacheFailed` condition is not True. |
| Bootstrap succeeds end-to-end | `spire_bootstrap_success_total{source="guild-cache",result="success"}` increases with each wizard pod start, and `spire_bootstrap_success_total{...,result="failure"}` stays flat. |
| Bootstrap latency is acceptable | `spire_bootstrap_duration_seconds{source="guild-cache"}` p95 is below the origin-clone baseline for the same repo. |
| Handshake is congruent | See §(d) handshake drift — the marker-file mismatch between reconciler and worker must be resolved. Until then, cache-backed bootstrap will abort with `ErrCacheUnavailable` even on a freshly-refreshed cache, so leave `Cache` unset on production guilds. |

If any check fails, unset `WizardGuildSpec.Cache` on the guild. The
operator removes the overlay on the next reconcile, new pods fall
back to origin-clone, and existing pods continue with whatever they
bootstrapped with. The PVC and refresh Job are retained (owner-
reference GC only fires on guild delete) — revisit them once the
underlying issue is fixed.
