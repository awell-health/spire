# pkg/executor

Formula-driven bead execution engine.

This package is the per-bead control plane. It takes a resolved formula and
drives one bead through that formula's lifecycle: resolve branch state, persist
execution state, dispatch agents, walk review logic, and land approved work.

In v3, the executor replaces hardcoded phase pipelines with a DAG-of-steps
model driven by `FormulaStepGraph`. Instead of walking a fixed phase sequence
(plan, implement, review, merge), the interpreter walks a step graph where each
step declares an action opcode, dependencies, conditions, and reset targets.

If `pkg/wizard` is the runtime for one subprocess role, `pkg/executor` is the
thing deciding what role should run next for the bead.

## What this package owns

- **Formula execution**: walk the bead through a formula's step graph.
- **Persistent execution state**: `graph_state.json` (per-step states, counters, workspace states, vars) and `state.json` (agent run recording context).
- **Bead-level orchestration policy**: direct vs wave implementation, review-fix routing, build-fix retries, merge behavior, and terminal-step handling.
- **Action dispatch**: registry of opcodes (`wizard.run`, `graph.run`, `git.merge_to_main`, etc.) mapped to handler functions.
- **Nested graph execution**: sub-graph state persistence, crash-safe cleanup after parent save.
- **Workspace resolution**: create/resume declared workspaces (repo, owned_worktree, borrowed_worktree, staging) with scope and cleanup policies.
- **Shared staging worktree policy**: create or resume a staging worktree (one of several workspace kinds in v3), decide when it should be used, and clean it up at executor exit.
- **Workflow bookkeeping**: attempt beads, step beads, review sub-step beads, and agent-run recording.
- **Planning flows**: task planning and epic planning from formulas and bead context.
- **Merge-to-main flow**: final verification, merge, cleanup, and close behavior for approved work.

## What this package does NOT own

- **Global coordination or capacity management**: the steward decides which beads get executors, how many wizards to run, and when to reset or replace them.
- **Formula authoring or validation**: the workshop/artificer owns creating, testing, validating, and publishing formulas.
- **Per-subprocess apprentice or sage lifecycle**: prompt assembly, Claude timeout handling, validation commands, and commit logic for a spawned worker belong in `pkg/wizard`.
- **Git implementation details**: worktree creation/resume, branch management, merge mechanics, and session baselines belong in `pkg/git`.
- **CLI wiring**: `cmd/spire` constructs `Deps`, resolves formulas, and exposes commands.

## Relationship To Wizard

The executor and wizard should not duplicate each other.

- **executor** chooses the route:
  - normal implementation vs wave dispatch
  - feature-branch fix vs staging-direct review-fix
  - whether a merge step is needed afterward
- **wizard** executes the chosen subprocess flow:
  - prepare the assigned workspace
  - run Claude
  - validate
  - commit
  - write `result.json`

If you find yourself adding prompt text, Claude CLI flags, timeout policy, or commit detection logic to the executor, you are usually leaking wizard responsibilities upward.

## Relationship To Steward And Workshop

- **Steward** is above the executor. It ensures the tower has enough wizard capacity, summons and resets workers, and routes work to ready beads. The steward branches on `pkg/config.DeploymentMode` at its dispatch entry — see [docs/ARCHITECTURE.md → Deployment modes](../../docs/ARCHITECTURE.md#deployment-modes) and [pkg/steward/README.md → Deployment-mode dispatch](../steward/README.md#deployment-mode-dispatch). The executor is unaware of mode; it runs the same formula graph regardless of how the steward chose to dispatch the bead.
- **Workshop** is beside the executor. It defines and validates the formulas that the executor consumes, but it does not run those formulas on live beads. Workshop also defines the `FormulaStepGraph` structure (steps, workspaces, vars) that the executor consumes at runtime.

## Runtime contract types (normative)

This package is the home of the canonical worker-runtime contract defined
in [docs/design/spi-xplwy-runtime-contract.md §1](../../docs/design/spi-xplwy-runtime-contract.md).
The four contract types live here because **workspace ownership and
handoff-mode selection are executor policy** — they are not decisions
that `pkg/wizard`, `pkg/apprentice`, `pkg/agent`, or the operator are
allowed to second-guess.

The canonical types are declared in
[`runtime_contract.go`](runtime_contract.go) and re-exported through
[`pkg/runtime`](../runtime) for backend/observability consumers that
cannot depend on `pkg/executor`.

| Type                | Purpose                                                          |
|---------------------|------------------------------------------------------------------|
| `RepoIdentity`      | Tower-derived identity (tower name/id, prefix, repo URL, base). |
| `WorkspaceHandle`   | Exported workspace contract: `Name`, `Kind`, `Branch`, `BaseBranch`, `Path`, `Origin`, `Borrowed`. |
| `HandoffMode`       | Cross-owner delivery selection: `none`, `borrowed`, `bundle`, `transitional`. |
| `RunContext`        | Observability identity: tower, prefix, bead/attempt/run ids, role, formula step, backend, workspace kind/name/origin, handoff mode. |

### `RepoIdentity` — invariants

- Resolved from `pkg/config` active-tower state plus `pkg/store` repo
  registration. Never from `os.Getwd` or the legacy
  `dolt.ReadBeadsDBName` ingress. The audit test in
  `pkg/runtime/audit_test.go` enforces this for every runtime-critical
  package.
- Immutable for the life of a worker run.
- The executor (and only the executor) assembles `RepoIdentity` at
  dispatch and stamps it onto every `SpawnConfig`. The `"spire"` default
  database fallback is permanently removed; callers see
  `executor.ErrNoTowerBound` / `ErrAmbiguousPrefix` when identity cannot
  be resolved. See `docs/CLI-MIGRATION.md` for the migration guide.

### `WorkspaceHandle` — invariants

- `Kind` is one of `repo`, `owned_worktree`, `borrowed_worktree`,
  `staging`. `Origin` is one of `local-bind`, `origin-clone`, or
  `guild-cache` (reserved for phase 2 — spi-sn7o3).
- `Path` is the single way the worker finds its workspace. Backends
  materialize it; the wizard consumes it via `--worktree-dir`.
- A worker MUST NOT mutate a `Borrowed=true` workspace outside its
  declared ownership surface. Fresh or cross-owner runs require
  `Borrowed=false`.
- Materialization failure is a backend error, not a wizard surprise.
- Workspace selection (which `Kind` / `Origin` for a given step) is
  executor policy — `resolveGraphWorkspace` owns it. Backends are told
  what substrate to use; they do not guess.

### `HandoffMode` — invariants

- Cross-owner delivery uses `HandoffBundle`.
- `HandoffBorrowed` is **not a delivery protocol** — it is the statement
  that no delivery is needed because workspace ownership did not change
  (e.g. implement → sage-review → review-fix on the same worktree).
- `HandoffTransitional` is explicit compatibility debt: quarantined,
  counted, and Warn-logged in `handoff.go`. The
  `SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1` env var promotes the
  deprecation log to a hard error — CI parity lanes set it on so any
  regression that silently reintroduces push transport fails the lane.
- **Only the executor selects `HandoffMode`.** Apprentice and wizard
  emit the selected artifact or an explicit no-op outcome; neither
  infers a mode.

### `RunContext` — invariants

- Every log/trace/metric emitted from executor, wizard, apprentice,
  agent, and the operator uses this field vocabulary (see
  `pkg/runtime/obs.go` for `LogFields`, `LogFieldOrder`, and
  `MetricLabels`).
- Metric cardinality is controlled: `BeadID`, `AttemptID`, and `RunID`
  stay **off** high-cardinality metric labels. They live in logs and
  traces only. `MetricLabels` enforces this.
- The `OTEL_RESOURCE_ATTRIBUTES` string uses the canonical field names
  in underscore form (`bead_id`, not legacy `bead.id`).

Cross-backend parity for env vocabulary, metric labels, and log
suffix is pinned by
[test/parity/runcontext_parity_test.go](../../test/parity/runcontext_parity_test.go).

## Key types and entrypoints

| Type / function          | Purpose |
|--------------------------|---------|
| `Executor`               | In-memory driver for one bead's lifecycle. |
| `GraphState`             | Persistent state: per-step status, counters, workspace states, vars. |
| `StepState`              | Per-step tracking: status, outputs, CompletedCount (mechanical, never reset). |
| `WorkspaceState`         | Runtime state for a declared workspace (kind, dir, scope, cleanup). |
| `ActionResult`           | Return type from action handlers (outputs + error). |
| `NewGraph`               | Construct or resume a v3 graph executor for a bead. |
| `RunGraph`               | Main v3 interpreter: resolve next steps, dispatch actions, persist after each step. |
| `RunNestedGraph`         | Execute a sub-graph without parent cleanup (used by `graph.run` action). |
| `dispatchAction`         | Look up step's action opcode in `actionRegistry`, invoke handler. |
| `resolveGraphWorkspace`  | Create or resume a declared workspace by kind and scope. |
| `buildConditionContext`  | Flatten GraphState into a map for formula condition evaluation. |
| `NewGraphForTest`        | Test constructor: bypasses registry and state loading. |
| `State`                  | Legacy persistent state (phase, wave, review rounds). Used for agent run recording. |
| `Run`                    | Entry point: delegates to `RunGraph`. |

## v3 graph runtime

### Interpreter loop

`RunGraph` picks next-ready steps via `formula.NextSteps()`, evaluates
conditions via `formula.EvalCondition()`, dispatches via `dispatchAction()`,
persists `GraphState` after every step, applies formula-declared resets, and
detects terminal steps. Steps execute sequentially (first ready step taken
each iteration).

### Action registry

The `actionRegistry` maps opcode strings to handler functions:

| Opcode                  | Handler |
|-------------------------|---------|
| `wizard.run`            | Spawn apprentice/sage subprocess via `pkg/wizard` (routes on step.Flow). |
| `check.design-linked`   | Verify a closed design bead is linked via discovered-from dep. |
| `beads.materialize_plan` | Confirm child beads were created by a preceding plan step. |
| `dispatch.children`     | Fan out execution to child beads (epic subtask dispatch). |
| `verify.run`            | Run build/test commands in the declared workspace. |
| `git.merge_to_main`     | Merge staging branch to base branch, push, clean up. |
| `bead.finish`           | Close the bead (and orphan subtasks for epics), mark terminated. |
| `noop`                  | Immediate success — used as terminal signals in nested graphs. |
| `graph.run`             | Load and execute a nested sub-graph inline. |
| `cleric.collect_context` | Adapter around `recovery.Diagnose` + learnings + wizard log tail. |
| `cleric.decide`         | Adapter that calls `recovery.Decide` and writes the `RepairPlan` JSON to the step output. |
| `cleric.execute`        | Reads the decide step's `RepairPlan`, resolves its `WorkspaceRequest` via `resolveGraphWorkspace`, dispatches by `RepairMode` (mechanical fn, worker spawn, recipe replay, escalate). |
| `cleric.verify`         | Dispatches the plan's `VerifyPlan` by `VerifyKind` (`rerun-step` via cooperative retry, `narrow-check` via command exec, `recipe-postcondition` via replay). |
| `cleric.learn`          | Adapter around `recovery.DocumentLearning` and the promotion counter; writes to bead metadata + SQL table. |
| `cleric.finish`         | Emits `RecoveryOutcome` via `recovery.WriteOutcome` and closes the recovery bead (both resume and escalate paths close — see below). Steward reads it via `recovery.ReadOutcome`. |

### Cleric runtime surface

The cleric's domain model and decide-time policy live in `pkg/recovery`
(see [design spi-h32xj](../../docs/design/spi-h32xj-cleric-repair-loop.md)
and [pkg/recovery/README.md](../recovery/README.md)). This package owns
only the runtime glue:

- `actionClericCollectContext`, `actionClericDecide`, `actionClericLearn`
  are thin adapters — they call into `pkg/recovery` and persist results
  into graph outputs.
- `actionClericExecute` is where runtime primitives live: it reads the
  typed `recovery.RepairPlan` produced by decide, provisions the declared
  `WorkspaceRequest` via `resolveGraphWorkspace` (same code path any other
  step uses — no cleric-only worktree fallback), and dispatches by
  `RepairMode`. Mechanical action bodies (rebase, cherry-pick, rebuild,
  reset-to-step) live in `recovery_actions.go` as functions keyed by
  action name. Worker dispatch uses `pkg/agent.Backend.Spawn` like any
  other apprentice.
- `actionClericVerify` dispatches the plan's `VerifyPlan`. The
  `rerun-step` kind sets a `wizard.RetryRequest` (carrying the
  `VerifyPlan`) on the target bead and polls for the wizard's reply.
  `narrow-check` and `recipe-postcondition` execute locally.
- `handleFinish` writes the canonical `recovery.RecoveryOutcome` via
  `recovery.WriteOutcome`. That record is the sole steward contract.
  Both terminal paths — `finish` (happy path) and
  `finish_needs_human` / `finish_needs_human_on_error` (escalate) —
  close the recovery bead. The escalate paths bypass the `learn` step
  and therefore also write an outcome with `Decision=DecisionEscalate`
  before closing, so the steward's hooked-step sweep reads a structural
  terminal signal and leaves the hooked parent parked rather than
  re-claiming an open recovery bead as fresh cleric work (spi-0nkot).

The `actionTargetedFix` function in `recovery_actions.go` is a tombstone
(one-release coexistence per design §9 Q3). It returns a helpful error
pointing callers at `RepairModeWorker`; historical recovery beads may
still reference the old name through resume paths.

### Formula-declared resets

Steps declare `resets = ["step-a", "step-b"]`. On completion, those steps are
set back to pending (status, outputs, timestamps cleared) but `CompletedCount`
is preserved. This enables review-fix loops: the sage-review step resets the
fix step, and the formula routes on `steps.X.completed_count` for loop
termination.

### Workspace resolution

Workspaces declared in the formula graph are resolved at step entry via
`resolveGraphWorkspace`. Four kinds:

- **`repo`** — no worktree; uses the main repo path directly.
- **`owned_worktree`** — created fresh for this execution.
- **`borrowed_worktree`** — resumed from a prior run.
- **`staging`** — shared integration worktree.

Scope (`run` vs `step`) controls whether the workspace persists across the
entire graph run or is released after each step. Cleanup policy (`always`,
`terminal`, `never`) controls when the worktree directory is removed.

### Nested graphs

The `graph.run` action loads a sub-graph formula, creates a sub-agent name
(`<parent>-<stepName>`), persists sub-state to its own `graph_state.json` file,
and calls `RunNestedGraph`. The nested interpreter runs the same dispatch loop
but skips parent-level cleanup (registry removal, staging close, attempt beads).
Parent cleanup of nested state files happens after the parent save — this
ordering is crash-safe.

### Graph state

`GraphState` is the execution state for one bead. It's persisted as JSON at
`~/.config/spire/runtime/<agent-name>/graph_state.json` and saved after every
step. Nested sub-graphs (e.g., `subgraph-implement` inside `epic-default`) get
their own file at `<parent-agent>-<step-name>/graph_state.json`.

```
GraphState
├── bead_id, agent_name           identity
├── formula, formula_source       which formula is running (embedded/repo/tower)
├── entry                         first step to evaluate
├── steps: map[string]StepState   per-step execution state
├── workspaces: map[string]WorkspaceState
├── vars: map[string]string       formula variable bindings
├── active_step                   currently executing step (empty when parked/idle)
├── step_bead_ids                 step name → step bead ID mapping
├── staging_branch, base_branch   git context
├── repo_path, worktree_dir       filesystem context
└── attempt_bead_id               current attempt bead
```

**StepState** tracks one step through its lifecycle:

| Field | Meaning |
|-------|---------|
| `status` | `pending` → `active` → `completed` / `failed` / `hooked` / `skipped` |
| `outputs` | Key-value outputs produced by the action (used in conditions) |
| `completed_count` | Mechanical counter: how many times this step has completed. Never reset — formulas use it for loop termination (`steps.X.completed_count > 0`). |
| `started_at` / `completed_at` | Timestamps |

**Step status transitions:**

```
pending ──→ active ──→ completed     (normal)
                   ──→ failed        (action error → escalation)
                   ──→ hooked        (parked for external condition)
                   ──→ skipped       (condition not met)
```

**Dual-state model — graph state + store beads:**

Step status exists in two places that the interpreter keeps in sync:

1. **GraphState** (file/dolt) — the interpreter's working memory. Drives
   the step-dispatch loop. Persisted after every step.
2. **Step beads in the store** — the externally visible state. The board,
   steward, CLI commands, and recovery agent all read step bead status
   instead of parsing graph state files.

The interpreter mirrors graph state transitions onto step beads via these
store operations (defined in `pkg/store/beadtypes.go`):

| Graph state transition | Store operation | Step bead status |
|------------------------|-----------------|------------------|
| pending → active | `ActivateStepBead` | `in_progress` |
| active → hooked | `HookStepBead` | `hooked` |
| hooked → active (resume) | `UnhookStepBead` then `ActivateStepBead` | `open` → `in_progress` |
| active → completed | `CloseStepBead` | `closed` |

When a step hooks, the interpreter also sets the **parent bead** status to
`hooked` via `UpdateBead`. When a previously-hooked step resumes and no
other steps remain hooked, the parent is restored to `in_progress`. This
is how the board knows a bead is parked without scanning graph state files.

**Hooked vs failed:** Both use `status=hooked` on the step bead. The
difference is failure evidence: step failures create alert beads and
recovery beads (linked via `caused-by`). Approval gates (`human.approve`)
and design waits (`check.design-linked`) hook without creating failure
artifacts. The recovery system uses this distinction — `Diagnose` requires
failure evidence to treat a hooked bead as recoverable.

**Lifecycle on escalation vs clean close:**

- **Clean terminal** (close, verified): `e.terminated = true` → deferred
  cleanup removes the graph state file. The bead is done.
- **Escalation terminal** (implement-failed with status=escalate):
  `e.terminated` stays false → graph state is preserved on disk. This allows
  `reset --to` and `resummon` to load and rewind it.

**WorkspaceState** tracks a declared workspace:

| Field | Meaning |
|-------|---------|
| `kind` | `repo`, `owned_worktree`, `borrowed_worktree`, `staging` |
| `dir` | Absolute path (worktree kinds only) |
| `branch` / `base_branch` | Resolved branch names |
| `start_sha` | Session baseline (for diff stats) |
| `status` | `pending` → `active` → `closed` |
| `scope` | `run` (persists across steps) or `step` (released after each step) |
| `cleanup` | `always`, `terminal`, `never` |

**Storage backends:**

| Backend | When | Implementation |
|---------|------|----------------|
| `FileGraphStateStore` | Local development (default) | JSON files in `~/.config/spire/runtime/` |
| `DoltGraphStateStore` | Cluster mode (`BEADS_DOLT_SERVER_HOST` is remote) | `graph_state` table in Dolt |

`ResolveGraphStateStore()` picks the backend automatically. The graph
interpreter receives it via `Deps.GraphStateStore` (lazy-initialized on
first use if not set).

### Runtime state (legacy)

`Executor.Run()` delegates to `RunGraph`. `State` carries legacy fields
(phase, wave, review rounds) used by `recordAgentRun` for metrics recording.

## Step failure → recovery dispatch

When a graph step errors, the interpreter calls `runRecoveryCycle` instead of
immediately hooking the bead. The cycle runs in-process on the wizard's own
staging workspace:

1. Round-budget guard (`DefaultRecoveryBudget = 3`; overridable via
   `SPIRE_RECOVERY_BUDGET`). Exhaustion escalates.
2. Create (or reuse) a recovery bead — idempotent per `(parent, step, round)`.
3. `recovery.Diagnose` → `Diagnosis`.
4. `recovery.Decide` → typed `RepairPlan`.
5. Dispatch by `RepairMode`:
   - `Mechanical` — `recovery_mechanical.go` wraps the existing
     `mechanicalActions` map (rebase-onto-base, cherry-pick, rebuild,
     reset-to-step) against the wizard's workspace.
   - `Worker` / `Recipe` — `recovery_worker.go` dispatches a repair
     apprentice via `SpawnRepairWorker` (bundle-handoff path). The apprentice
     is structurally identical to a wave apprentice — only the prompt differs.
   - `MergeConflictResolution` (action `resolve-conflicts`) —
     `recovery_merge_conflict.go` runs the Claude-driven resolver on staging.
   - `Escalate` — `recovery_escalate.go` marks needs-human, emits an alert,
     and returns terminal.
   - `Noop` — resume the hooked step as-is.
6. Append `RepairAttempt` to `StepState.RepairAttempts`, clear
   `StepState.CurrentRepair`, persist, and return the outcome.

On success the step is rewound to pending and the interpreter loop
re-dispatches it. On escalate / budget-exhausted the bead is hooked and the
interpreter exits.

Persistence between every phase is the load-bearing rule for crash-resume:
`resumeInFlightRepairs` reads `CurrentRepair.Phase` on wizard startup and
applies seams-14/15 policy — early phases close the cycle as interrupted
(counting toward the round budget), worker-phase crashes honor the
apprentice's delivered bundle.

Inline recovery is opt-in today via `SPIRE_INLINE_RECOVERY=1`; the default
path still hooks-and-escalates so existing production behavior is preserved
while the dispatch surface lands.

## Practical rules

1. **Keep policy here, not runtime details.** The executor decides *what* should happen next, not *how Claude should be invoked* inside a subprocess.
2. **Persist once, reuse everywhere.** Repo path, base branch, staging branch, worktree path, and phase state should be resolved once and then read from executor state.
3. **Use the staging worktree as the shared integration surface.** Child branches and borrowed review/build fixes should converge there; don't invent parallel merge paths casually.
4. **Prefer spawning existing wizard flows over duplicating them.** If a subprocess already knows how to implement, validate, and commit, route to it instead of rebuilding that logic in the executor.
5. **Use formulas as the source of lifecycle truth.** If the change is about phase order, conditions, or review graph structure, start with the formula and workshop model before hardcoding behavior here.
6. **Treat metrics and workflow beads as executor responsibilities.** If a new path changes agent outcomes, review rounds, or terminal behavior, update recording and workflow-bead transitions here.
7. **Action handlers are the v3 extension point.** To add new step behavior, register a new opcode in `actionRegistry`. Don't add ad-hoc step logic to the interpreter loop.
8. **CompletedCount is mechanical and never reset.** Formula-declared resets clear status/outputs but preserve CompletedCount. Formulas route on `steps.X.completed_count` for loop termination. Don't clear it.
9. **Nested graph state is cleaned up after the parent save.** The parent interpreter saves its own step as completed first, then removes the nested state file. This ordering is crash-safe — don't rearrange it.

## Where new work usually belongs

- Add it to **`pkg/executor`** when the change affects bead-level orchestration, phase transitions, review DAG routing, merge policy, or adds a new action opcode, modifies step dispatch, or changes workspace lifecycle policy.
- Add it to **`pkg/wizard`** when the change affects how a spawned apprentice or sage process runs.
- Add it to **`pkg/git`** when the change affects worktree, branch, merge, or commit-detection mechanics.
- Add it to **`pkg/workshop`** when the change affects formula creation, validation, dry-run, or publishing.
