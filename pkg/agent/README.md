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

## Backend obligations

`SpawnConfig` describes the runtime surface the caller expects. A backend must
either satisfy that surface or fail fast.

Examples:

- If the caller supplies `ExtraArgs = ["--worktree-dir", ...]`, the backend must
  ensure the spawned process can actually see that workspace path.
- If the spawned role needs tower data or repo bootstrap, the backend must
  materialize those prerequisites instead of assuming process-local state.
- Backend-specific convenience must not silently change the bead/executor
  contract. A backend is an implementation of the runtime surface, not a second
  orchestration layer.

## K8s-specific note

The k8s backend currently has a special contract for `RoleWizard`: tower data,
repo bootstrap inputs, and the canonical wizard pod surface are materialized by
the backend before the main container runs.

That kind of runtime materialization belongs here, not in `pkg/wizard`.

The inverse is also true: if apprentice or sage pods need equivalent workspace
or repo surfaces, that must be fixed here or routed differently upstream. It is
not acceptable to rely on process-local path assumptions that a fresh pod
cannot satisfy.

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
