# Design: Canonical Worker Runtime Contract — spi-xplwy

> Spec for epic spi-xplwy. Design bead: spi-g0u7n. Sibling spec: spi-h32xj (cleric).

## Purpose

One runtime contract that holds in local process mode, `pkg/agent` k8s mode,
and operator-managed cluster mode. It defines how a worker resolves its
identity, materializes its repo substrate, receives a workspace, and delivers
work across an ownership boundary — with the same observable surface
everywhere.

This document converts the decisions in spi-g0u7n into a concrete type/file
cut and a migration order that keeps local and k8s behavior coherent while
compatibility debt is removed.

---

## 1. Runtime vocabulary and invariants

The contract is expressed as four small types owned by `pkg/executor`, plus
two backend obligations owned by `pkg/agent`. Nothing below introduces a new
package.

### 1.1 `RepoIdentity`

```go
// RepoIdentity is the canonical identity a worker resolves before any role
// code runs. It is derived from tower registration state — never from
// ambient CWD, pod env ad hoc, or CR-only fields.
type RepoIdentity struct {
    TowerName   string // dolt database (== tower identity)
    TowerID     string // stable tower id (config.ResolveActiveTower)
    Prefix      string // bead prefix for this repo (e.g. "spi")
    RepoURL     string // origin URL from the shared repo registration
    BaseBranch  string // default base branch ("main" etc.) from registration
}
```

**Invariants:**

- `RepoIdentity` is always resolved from `pkg/config` tower state plus
  `pkg/store` repo registration for `Prefix`. No path/CWD inference is
  permitted in runtime-critical code.
- A single tower may host multiple prefixes. Runtime code must not assume
  "one prefix per process" (today `graph_store.go` defaults to `"spire"`
  when `dolt.ReadBeadsDBName(os.Getwd)` returns empty — that path goes
  away).
- `RepoIdentity` is immutable for the life of a worker run.

### 1.2 `WorkspaceHandle` (extend existing `WorkspaceState`)

The executor already has `WorkspaceState` in `pkg/executor/graph_state.go`.
We promote the subset that crosses the backend boundary into an exported
value type `WorkspaceHandle` so `pkg/agent` can consume it without depending
on graph-state internals.

```go
type WorkspaceKind string

const (
    WorkspaceKindRepo             WorkspaceKind = "repo"
    WorkspaceKindOwnedWorktree    WorkspaceKind = "owned_worktree"
    WorkspaceKindBorrowedWorktree WorkspaceKind = "borrowed_worktree"
    WorkspaceKindStaging          WorkspaceKind = "staging"
)

type WorkspaceOrigin string

const (
    WorkspaceOriginLocalBind  WorkspaceOrigin = "local-bind"   // preexisting repo checkout
    WorkspaceOriginOriginClone WorkspaceOrigin = "origin-clone" // fresh clone (current repo-bootstrap)
    WorkspaceOriginGuildCache  WorkspaceOrigin = "guild-cache"  // phase 2 — spi-sn7o3
)

// WorkspaceHandle is the contract piece a backend must satisfy before the
// worker's main container/process starts. The executor produces it; the
// backend materializes it; the wizard consumes it by path.
type WorkspaceHandle struct {
    Name       string          // formula workspace name
    Kind       WorkspaceKind
    Branch     string          // resolved branch (may be empty for repo)
    BaseBranch string
    Path       string          // absolute path the worker will see
    Origin     WorkspaceOrigin // how the substrate was produced
    Borrowed   bool            // true iff caller owns cleanup (== same-owner continuation)
}
```

**Invariants:**

- A worker may not mutate a `Borrowed=true` workspace outside its declared
  ownership surface (review/fix loops that share state). Fresh or
  cross-owner runs require `Borrowed=false`.
- `Path` is the single way the worker finds its workspace. `--worktree-dir`
  is how it is plumbed to subprocesses (already the case in `pkg/wizard`).
- Materialization failure is a backend error, not a wizard surprise.
  Backends fail fast on missing substrate (already the rule in
  `pkg/agent/README.md` — we make it load-bearing).

### 1.3 `HandoffMode`

```go
type HandoffMode string

const (
    HandoffNone         HandoffMode = "none"          // terminal/no-op
    HandoffBorrowed     HandoffMode = "borrowed"      // same-owner continuation
    HandoffBundle       HandoffMode = "bundle"        // canonical cross-owner
    HandoffTransitional HandoffMode = "transitional"  // legacy push — quarantined
)
```

**Invariants:**

- Cross-owner delivery uses `HandoffBundle`. `HandoffTransitional` is
  explicit compatibility debt, not a quiet fallback. Phase 1 marks it;
  phase 5 removes it (see §3).
- `HandoffBorrowed` is **not a delivery protocol** — it is the statement
  that no delivery is needed because workspace ownership did not change.
  The existing implement → sage-review → review-fix chain in
  `task-default` is the canonical borrowed-handoff path.
- The executor is the only place that selects a `HandoffMode`. Apprentice
  and wizard emit the selected artifact or explicit no-op outcome.

### 1.4 `RunContext` (observability surface)

```go
// RunContext is the identity set that every worker run must propagate
// through logs, traces, and metrics. It is assembled by the executor at
// role dispatch and passed to backends via SpawnConfig.
type RunContext struct {
    TowerName        string
    Prefix           string
    BeadID           string
    AttemptID        string
    RunID            string
    Role             SpawnRole          // apprentice|sage|wizard|executor|cleric
    FormulaStep      string
    Backend          string             // "process"|"docker"|"k8s"|"operator-k8s"
    WorkspaceKind    WorkspaceKind
    WorkspaceName    string
    WorkspaceOrigin  WorkspaceOrigin
    HandoffMode      HandoffMode
}
```

**Invariants:**

- Every log/trace/metric emitted from executor, wizard, apprentice, agent,
  and the operator uses this field vocabulary. No ad hoc labels for
  tower/prefix/bead/role once this ships.
- Metric cardinality is controlled: `BeadID`/`AttemptID`/`RunID` stay off
  high-frequency metric labels; they live in logs/traces.

---

## 2. Backend obligations (`pkg/agent`)

A backend MUST, before the main container/process starts:

1. Resolve and propagate `RepoIdentity` via env (already done for wizard
   pods through `BEADS_DATABASE`, `BEADS_PREFIX`, `DOLTHUB_REMOTE`, and
   via `SPIRE_REPO_URL`/`SPIRE_REPO_PREFIX` in the repo-bootstrap init
   container). This env vocabulary becomes the canonical one; no backend
   may invent a parallel set.
2. Materialize the requested `WorkspaceHandle.Path`:
   - `Kind=repo` → nothing to materialize beyond the substrate.
   - `Kind=owned_worktree|staging` → fresh worktree rooted at the
     substrate.
   - `Kind=borrowed_worktree` → existing path must be reachable from the
     worker, unchanged. (This is the k8s-backend gap today for non-wizard
     roles.)
3. Emit `RunContext` fields as pod labels/annotations or process env so
   logs and metrics carry identity even when the worker has not yet
   started speaking.
4. Fail fast (non-zero exit from the init container, typed error from
   `Backend.Spawn`) on missing prerequisites.

### `SpawnConfig` extension

```go
type SpawnConfig struct {
    // ... existing fields ...
    Identity    RepoIdentity    // NEW — replaces ambient BEADS_* fallback
    Workspace   *WorkspaceHandle // NEW — required for apprentice/sage/wizard
    Run         RunContext      // NEW — observability identity
}
```

The existing `ExtraArgs=["--worktree-dir", ...]` flow stays as the runtime
plumbing into wizard subprocesses; `Workspace` is what backends use to
**make** that path exist.

---

## 3. Package/file cut

All changes live in packages that already own the relevant concerns. No
new packages.

### `pkg/executor`

- `graph_state.go`: expose `WorkspaceState → WorkspaceHandle` conversion;
  record `Origin` alongside `Dir`/`Branch`. Extend persisted shape
  back-compat (missing `Origin` → `local-bind`).
- `workspace.go`: `resolveWorkspace` returns a `WorkspaceHandle` in
  addition to the existing `*spgit.WorktreeContext`. Selection between
  local-bind / origin-clone (/ guild-cache later) is made here so
  backends are told what substrate to use, not asked to guess.
- `graph_store.go`: **delete** the `dolt.ReadBeadsDBName(os.Getwd)` +
  `"spire"` fallback at line 200. `ResolveGraphStateStore` must receive
  `RepoIdentity` from the executor caller. This is the single most
  important ambient-CWD removal.
- `graph_actions.go` / `action_dispatch.go`: populate `SpawnConfig.Run`
  and `SpawnConfig.Workspace` on every `wizard.run` / `graph.run` /
  `cleric.execute` dispatch. Remove any path where `ExtraArgs` carries
  workspace intent without `Workspace` being set.
- `apprentice_bundle.go`: label handoff artifacts with `HandoffMode`; do
  not accept `HandoffTransitional` once §5 of the migration is done.

### `pkg/agent`

- `agent.go`: extend `SpawnConfig` with the three new fields above.
- `backend_process.go`: trivial — set env from `Identity` and `Run`,
  validate `Workspace.Path` exists.
- `backend_docker.go`: mount `Workspace.Path`, inject env.
- `backend_k8s.go`:
  - factor out `buildWizardPod` into a shared builder keyed on
    `SpawnConfig.Role` + `Workspace.Kind`. Today the wizard branch at
    `backend_k8s.go:168` is the only path with tower-attach +
    repo-bootstrap init containers.
  - apprentice/sage pods get the same two init containers when the
    formula declares a non-`repo` workspace. For `Kind=borrowed_worktree`
    they additionally mount the parent wizard's `/workspace` PVC
    (introduce a shared-PVC claim owned by the parent wizard-pod, or run
    the child as a container in the parent pod — decision tracked as
    implementation choice in chunk 2 below).
  - `NewK8sBackend`: read `Identity` from the executor-provided
    `SpawnConfig` rather than re-reading `BEADS_DATABASE`/`BEADS_PREFIX`
    from env on every spawn.

### `pkg/wizard`

- `wizard.go`, `wizard_review.go`: accept `RunContext` via env set by the
  backend; emit it on every structured log line and in `result.json`.
  No behavior change beyond observability.

### `pkg/apprentice`

- `submit.go`: record `HandoffMode` in the bundle signal so the executor
  does not re-infer it.

### `pkg/git`

- No structural changes. Continues to own worktree/merge primitives and
  remains the only place that calls into git.

### `operator/`

- `controllers/agent_monitor.go`, `controllers/bead_watcher.go`: replace
  the operator's private pod-shape code with a call into the shared
  `pkg/agent` builder. The operator continues to own reconciliation and
  scheduling; it stops owning a second runtime surface. The operator's
  `SpireAgent` CR keeps its fields, but they are translated into
  `SpawnConfig` before pod construction.

### Docs

- `pkg/agent/README.md`: make the "backend obligations" section the
  normative reference for §2 above.
- `pkg/executor/README.md`: document `WorkspaceHandle` / `HandoffMode` /
  `RunContext` once they exist.
- `docs/ARCHITECTURE.md`: update the "worker runtime" section to point
  at this contract; retire references to push transport once §5 lands.

---

## 4. Migration order

Each chunk is a separately landable bead under spi-xplwy. Order is
designed so local execution never regresses and k8s execution never
gains a new ambiguity.

**Chunk 1 — define vocabulary (no behavior change).**

Introduce `RepoIdentity`, `WorkspaceHandle`, `HandoffMode`, `RunContext`
in `pkg/executor`. Wire them through the executor internally. Keep the
existing `BEADS_*` env reads in `pkg/agent` as the authoritative source;
just copy them into `SpawnConfig.Identity` where it is cleaner. This is
a pure refactor; tests pass without behavioral diffs.

**Chunk 2 — converge k8s backend on shared pod builder.**

Factor `backend_k8s.go` so the tower-attach + repo-bootstrap init
containers are produced from `SpawnConfig` for every role whose
`Workspace` requires substrate. Decide the shared-workspace-for-child
question (shared PVC per parent vs co-tenant container). First working
path lands behind a `SPIRE_K8S_SHARED_WORKSPACE=1` gate so local k8s
parity can be tested against the legacy flat-pod path.

**Chunk 3 — remove ambient CWD / default "spire".**

Delete `graph_store.go:200` fallback. Thread `RepoIdentity` from
`cmd/spire` callers into `ResolveGraphStateStore`. Any command that is
supposed to work outside a bound repo must now either pass identity
explicitly or fail with a clear "no tower bound" error. This is the
riskiest chunk for CLI ergonomics — land with a migration guide.

**Chunk 4 — operator uses shared builder.**

Replace the operator's inline pod construction with the `pkg/agent`
shared builder. Verify parity via operator integration tests before
removing the old code.

**Chunk 5 — mark and then remove transitional handoff.**

Phase 5a: every push-transport path emits `HandoffMode=transitional` and
a deprecation log line. Phase 5b (separate bead): remove the push path
once metrics show zero use for a full weekly cycle.

**Chunk 6 — observability plumbing.**

Every log/trace/metric in executor, wizard, apprentice, agent, and the
operator uses `RunContext` field names. Alerts and dashboards updated in
the same chunk (we do not ship a half-relabeled observability surface).

**Chunk 7 — docs/tests sweep.**

Update READMEs, ARCHITECTURE.md, and formula docs. Add the parity test
matrix in §5 as a CI lane.

---

## 5. Test matrix

| Dimension                       | Local process | `pkg/agent` k8s | Operator k8s |
|---------------------------------|---------------|-----------------|--------------|
| Wizard w/ owned workspace       | existing      | existing        | new (chunk 4) |
| Apprentice w/ borrowed workspace| existing      | **new (chunk 2)** | new (chunk 4) |
| Sage w/ borrowed workspace      | existing      | **new (chunk 2)** | new (chunk 4) |
| Cross-owner bundle delivery     | existing      | existing        | new (chunk 4) |
| Transitional push delivery      | gated (chunk 5) | gated (chunk 5) | gated (chunk 5) |
| Multi-prefix tower              | **new (chunk 3)** | **new (chunk 3)** | **new (chunk 4)** |
| No ambient-CWD regression       | **new (chunk 3)** | **new (chunk 3)** | **new (chunk 4)** |
| `RunContext` on logs/traces     | new (chunk 6) | new (chunk 6) | new (chunk 6) |

Cells marked **new** are gaps that must be closed as the referenced
chunk lands. Cells marked `existing` must remain green through every
chunk; regressions block chunk merge.

---

## 6. Non-goals

- Guild-cache optimization (`WorkspaceOriginGuildCache` is reserved but
  not implemented here; spi-sn7o3 remains the phase-2 track).
- Provider/model changes.
- Cleric redesign beyond using the `WorkspaceHandle` / `RunContext`
  primitives this spec introduces. See the sibling spec for spi-h32xj.
- Steward capacity model changes.

---

## 7. Open questions for planning-agent to resolve

1. Shared workspace transport for k8s child pods: PVC-per-parent vs
   co-tenant container vs ephemeral volume? Chunk 2 must pick one; the
   simplest (co-tenant apprentice container in the parent wizard pod)
   matches the existing "wizard pod owns the workspace" mental model
   but complicates lifecycle. A parallel prototype lane in chunk 2 is
   acceptable.
2. Whether the operator should disappear as a code path entirely once
   chunk 4 lands, or continue to exist as a thin reconciler on top of
   the shared builder. The design assumes the latter to stay consistent
   with `docs/ARCHITECTURE.md`.
3. CLI ergonomics for chunk 3: do we require every CLI command to carry
   an explicit `--tower`/`--prefix`, or is "resolve from the active
   tower with an error if ambiguous" good enough? Recommendation: the
   latter, since the active-tower concept already exists in
   `pkg/config`.
