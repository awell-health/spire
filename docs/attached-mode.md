# Attached Mode (Reserved)

**Status**: Reserved — not implemented.

Attached mode is the third deployment topology named by
`pkg/config/deployment_mode.go`. It is declared by setting a tower's
`deployment_mode` to `attached-reserved`. Today that value is a
declaration of intent only; no execution path exists and Spire refuses
to dispatch work in attached mode.

This document pins what attached mode *is*, what seams it will reuse,
and — most importantly — what it MUST NOT change about the existing
local-native and cluster-native contracts. It exists so that a future
track implementing attached mode has an unambiguous starting line and
so that reviewers can reject PRs that stretch the seam in the wrong
direction.

---

## Shape

Attached mode is a **local control plane targeting a remote cluster
execution surface through an explicit remote seam**. Concretely:

- The archmage's machine continues to run the local control plane:
  tower config, repo registration, bead mutation, design and plan
  authorship, and every CLI the archmage types.
- Execution — the actual apprentice pod, workspace materialization,
  and worker process — runs on a remote cluster.
- The two halves are connected only by the
  `pkg/steward/intent.IntentPublisher` / `IntentConsumer` seam. The
  local side publishes a `WorkloadIntent`; the remote side consumes it
  and reconciles cluster resources.

It is emphatically **not** a new runtime, a new scheduler, or a new
ownership model. It is a deployment-time rewiring of *where* the
existing scheduler writes its intent and *where* the existing
reconciler reads it.

---

## Seams it will reuse

Attached mode does not invent new plumbing. It composes the seams that
local-native and cluster-native already own:

| Seam | Package | Role in attached mode |
|------|---------|-----------------------|
| `WorkloadIntent` | `pkg/steward/intent` | The dispatch-time payload written by the local control plane and read by the remote execution surface. Carries `AttemptID`, `RepoIdentity`, `FormulaPhase`, `Resources`, `HandoffMode` — and nothing machine-local. |
| `IntentPublisher` | `pkg/steward/intent` | The local side's exit seam. The attached-mode publisher targets a remote transport (the specific shape is TBD) instead of an in-cluster CR apply. |
| `IntentConsumer` | `pkg/steward/intent` | The remote side's entry seam. Reuses the operator's existing reconciler contract. |
| `RepoIdentity` | `pkg/steward/intent` | Carries only `URL`, `BaseBranch`, `Prefix`. The remote side resolves any per-cluster state through its own identity resolver. |
| `DeploymentModeAttachedReserved` | `pkg/config` | The explicit switch. Consumers that observe this value today MUST return a typed not-implemented error rather than falling back silently. |

The reflection-based tests in `pkg/steward/intent/intent_test.go`
already guard the intent shape against smuggled machine-local state.
Attached mode inherits those guards without weakening them.

---

## What attached mode MUST NOT change

A future attached-mode implementation is a composition, not a rewrite.
It MUST NOT perturb any of the following:

### 1. The spi-xplwy runtime contract

`docs/design/spi-xplwy-runtime-contract.md` defines the canonical
worker runtime contract that holds identically in local process mode,
`pkg/agent` k8s mode, and operator-managed cluster mode. Attached mode
is another deployment of the same runtime, not a new one. Specifically:

- `RepoIdentity`, `WorkspaceHandle`, and the backend obligations owned
  by `pkg/agent` remain authoritative.
- The worker continues to resolve its identity from tower registration
  state and repo registration — never from ambient CWD, pod env, or CR
  fields specific to attached mode.
- The executor still drives the per-bead phase pipeline. Attached mode
  does not get a private scheduler or a private executor.

### 2. Repo-identity ownership

`pkg/steward/identity` is the only canonical source of cluster
repo identity. `LocalBindings.State` and `LocalBindings.LocalPath` do
not appear in cluster scheduling paths, and they will not appear in
attached-mode scheduling paths either. The `RepoIdentity` carried in
`WorkloadIntent` stays minimal (`URL`, `BaseBranch`, `Prefix`); the
remote side resolves credentials and clone paths through its own
resolver.

### 3. Attempt-bead ownership

The attempt bead remains the canonical ownership seam. `AttemptID` on
`WorkloadIntent` is the bead ID of the attempt bead that owns the
work. Attached mode does not introduce a parallel ownership channel,
does not assign work ownership to a pod name, a CR UID, or any
remote-side identifier. Ownership transitions continue to flow through
the existing claim / in_progress / close verbs on the attempt bead.

### 4. Orthogonality of mode, backend, and transport

Per the godoc on `DeploymentMode`, deployment mode is orthogonal to
worker backend (`agent.backend=process/docker/k8s`) and to sync
transport (syncer / remotesapi / DoltHub). A future attached-mode
implementation MUST preserve that orthogonality. In particular, it
MUST NOT conflate "my deployment mode is attached" with "my worker
backend is k8s" or "my sync transport is remotesapi".

---

## Not implemented

As of the current tree, no execution path exists for
`DeploymentModeAttachedReserved`. The only runtime surface is the
stub in `pkg/steward/attached`:

- `pkg/steward/attached.AttachedDispatch(ctx, intent)` returns
  `ErrAttachedNotImplemented` for any input.
- The error message points here
  (`docs/attached-mode.md`) so an operator who hits it knows where to
  read for context.
- The package exports no other symbols. A reviewer who finds a second
  exported symbol in `pkg/steward/attached` before attached mode has
  been designed end-to-end should reject the change.

Selecting `deployment_mode = "attached-reserved"` in a tower config is
therefore a declaration of intent, not a runnable configuration.
Consumers that observe this value MUST return a typed "not
implemented" error rather than silently falling back to local-native
or cluster-native.

When attached mode graduates from reserved to implemented, this
document — and the stub — will be replaced by the real design.
