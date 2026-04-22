# pkg/agent

Backend-agnostic agent spawning and lifecycle management.

This package is the translation layer between a runtime intent like "spawn an
apprentice for bead `spi-abc`" and the concrete backend mechanism that actually
starts that worker: local process, Docker container, or k8s pod.

## What this package owns

- **Backend abstraction**: `Backend`, `Spawner`, `Handle`, and `Info`.
- **Spawn intent surface**: `SpawnConfig` and `SpawnRole`.
- **Backend factories**: `ResolveBackend`, backend selection, and fallback.
- **Backend-specific process specs**: local process commands, Docker container
  specs, and k8s pod specs.
- **Lifecycle plumbing**: spawn, wait, signal/kill, list, and log streaming.
- **Backend-local tracking**: the local registry used by process-backed agents.
- **Backend-specific runtime materialization**: for example, the k8s wizard pod
  contract that stages tower data and bootstraps a repo checkout before the
  main container starts.

## What this package does NOT own

- **Bead lifecycle policy**: which role runs next, review routing, merge
  routing, and formula interpretation belong in `pkg/executor`.
- **Subprocess behavior**: prompt assembly, Claude execution, validation, and
  result writing belong in `pkg/wizard`.
- **Git/workspace semantics**: worktree creation, resume, merge, and session
  baselines belong in `pkg/git`.
- **Apprentice delivery semantics**: bundle signal-write behavior belongs in
  `pkg/apprentice`; bundle storage belongs in `pkg/bundlestore`.
- **Shared ownership / claiming**: attempt beads and bead ownership belong in
  the store + steward/executor layers, not in the local registry here.

## Relationship To Executor And Wizard

The clean split is:

- **executor** decides that a role should run
- **agent** turns that decision into a backend-specific worker
- **wizard** is the code that worker actually runs

If a change is about *which* role should run, it does not belong here.
If a change is about *how a backend must materialize the runtime surface for a
spawned role*, it probably does.

## Backend obligations (normative)

This section is the normative reference for §2 ("Backend obligations") of
[docs/design/spi-xplwy-runtime-contract.md](../../docs/design/spi-xplwy-runtime-contract.md).
`SpawnConfig` describes the runtime surface the caller expects. A backend
(process, docker, k8s, or operator-managed k8s) MUST, before the main
container or process starts, do **all four** of the following — or fail fast
with a typed error:

1. **Resolve and propagate `RepoIdentity`.** The identity (tower name, tower
   id, prefix, repo URL, base branch) is produced by the executor from active
   tower state and passed in on `SpawnConfig.Identity`. Backends translate
   it into the canonical env vocabulary (`BEADS_DATABASE`, `BEADS_PREFIX`,
   `DOLTHUB_REMOTE`, `SPIRE_REPO_URL`, `SPIRE_REPO_PREFIX`) on the spawned
   worker. Backends MUST NOT re-derive identity from ambient CWD, ad-hoc pod
   env, or CR-only fields.
2. **Materialize `WorkspaceHandle.Path` per `Kind`.**
   - `Kind=repo` — nothing to materialize beyond the substrate.
   - `Kind=owned_worktree` / `Kind=staging` — fresh worktree rooted at the
     substrate, owned by the spawned role.
   - `Kind=borrowed_worktree` — the existing path (owned by the parent
     wizard) must be reachable from the spawned worker, unchanged. The
     worker sees the path on disk; it does not clone or branch inside it.
   The worker finds this path via `--worktree-dir` (plumbed through
   `ExtraArgs`) and `SPIRE_WORKSPACE_PATH`.
3. **Emit `RunContext` as labels/annotations/env.** Every canonical
   `RunContext` field (tower, prefix, bead_id, attempt_id, run_id, role,
   formula_step, backend, workspace_kind, workspace_name, workspace_origin,
   handoff_mode) is stamped onto the pod — as labels for low-cardinality
   keys, annotations for high-cardinality keys, and `SPIRE_*` env on the
   main container — or onto the process env for process/docker backends.
   The `OTEL_RESOURCE_ATTRIBUTES` string uses the same canonical field
   names (underscore form: `bead_id`, not legacy `bead.id`). The parity
   contract is pinned by
   [test/parity/runcontext_parity_test.go](../../test/parity/runcontext_parity_test.go).
4. **Fail fast on missing prerequisites.** Missing `Identity`, missing or
   unreachable `Workspace.Path`, missing tower mount — these are backend
   errors (init container non-zero exit, or typed `Backend.Spawn` error),
   not downstream wizard surprises. No backend may silently fall back to
   process-local state or a default database name.

Backend-specific convenience must not silently change the bead/executor
contract. A backend is an implementation of the runtime surface, not a
second orchestration layer.

### K8s and operator parity

The k8s backend's shared pod builder (`BuildPod` on `K8sBackend`) is the
single source of truth for pod shape. The operator (`operator/controllers/
agent_monitor.go`) calls into that builder so the canonical labels,
annotations, env, and init containers are identical on both surfaces. The
byte-level parity guarantee is enforced by
`operator/controllers/pod_builder_parity_test.go`.

Apprentice, sage, and wizard pods share the builder — there is no
wizard-only special case. `Kind=borrowed_worktree` apprentices additionally
mount the parent wizard's workspace volume; all other pod shape decisions
are keyed off `SpawnConfig.Role` + `Workspace.Kind`.

## Apprentice pod shape: `BuildApprenticePod` is canonical

`pkg/agent` is the single source of truth for apprentice pod shape, and
`BuildApprenticePod(spec PodSpec) (*corev1.Pod, error)` (in
[`pod_builder.go`](pod_builder.go)) is the canonical constructor. Both
the steward's cluster-native dispatch path
([`pkg/steward/cluster_dispatch.go`](../steward/cluster_dispatch.go))
and the operator's intent reconciler
([`operator/controllers/intent_reconciler.go`](../../operator/controllers/intent_reconciler.go))
translate their own state into a `PodSpec` and call this function;
neither hand-rolls pod shape. See [docs/ARCHITECTURE.md →
Seams](../../docs/ARCHITECTURE.md#seams) for the architectural framing.

Rules that hold inside this package:

- **One canonical constructor.** `BuildApprenticePod` is the only public
  way to materialize an apprentice pod. Internal helpers (env build,
  label build, init-container build) stay private.
- **Explicit `PodSpec` fields, no opaque maps.** Every input the
  apprentice pod needs (image, identity, workspace, handoff, resources,
  cache PVC overlay, OTLP endpoint) is a typed field on `PodSpec`.
  Missing required inputs surface as typed `ErrPodSpec*` errors at
  build time rather than as init-container failures at runtime.
- **No process-env fallbacks.** The function never reads process env,
  never falls back to ambient CWD, and never hides missing identity
  behind a default. Callers plumb identity explicitly from their own
  configuration surfaces.
- **Parity is enforced, not promised.** Pod shape stability is pinned by
  `pkg/agent/pod_builder_test.go` (golden shape: required env vars,
  volumes, restart policy, labels, no stale env keys),
  `pkg/agent/pod_builder_parity_test.go`, and
  `operator/controllers/pod_builder_parity_test.go`. A second
  pod-construction code path under `pkg/steward/` or `operator/` is a
  contract violation and should be rejected in review.

When the legacy `(*K8sBackend).BuildPod` path under `backend_k8s.go`
(retained for the wizard-pod shape and the spawner lifecycle) eventually
graduates onto `BuildApprenticePod`, this package's pod-shape surface
collapses to a single function — that is the target end-state for the
spi-sj18k epic.

## Key types

| Type / function | Purpose |
|-----------------|---------|
| `Backend` | Unified interface for spawn/list/logs/kill across backends. |
| `Spawner` | Narrow interface for spawn-only call sites. |
| `Handle` | Lifecycle handle for one running worker. |
| `SpawnConfig` | Backend-agnostic spawn intent. |
| `SpawnRole` | Worker role (`apprentice`, `sage`, `wizard`, `executor`). |
| `ResolveBackend` | Factory for the configured backend. |
| `K8sBackend` | k8s pod-backed implementation. |
| `ProcessBackend` | local process implementation. |
| `DockerBackend` | Docker-backed implementation. |

## Practical rules

1. **Keep this package backend-facing.** It translates execution intent into a
   runnable worker; it does not decide lifecycle policy.
2. **Fail fast on missing runtime prerequisites.** Missing repo/bootstrap/workspace
   inputs are backend errors, not downstream wizard surprises.
3. **Treat the registry as local bookkeeping only.** It is not a shared claim or
   ownership mechanism.
4. **Do not smuggle formula policy into backend code.** Pod/process specs may
   materialize prerequisites, but they must not decide review/merge behavior.
5. **Document backend-only contracts here.** If k8s requires a special pod
   surface, this README should say so explicitly.

## Where new work usually belongs

- Add it to **`pkg/agent`** when the spawn surface, backend specs, logs/wait
  behavior, or backend materialization rules change.
- Add it to **`pkg/executor`** when role routing, step routing, or workspace
  selection policy changes.
- Add it to **`pkg/wizard`** when subprocess runtime behavior changes.
- Add it to **`pkg/git`** when worktree or merge semantics change.
- Add it to **`pkg/apprentice`** or **`pkg/bundlestore`** when delivery artifact
  behavior changes.
