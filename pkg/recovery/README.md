# pkg/recovery

Domain model and policy for the cleric repair loop. This package owns the
diagnostic types, the decide-time policy, the learning/promotion model, and
the canonical `RecoveryOutcome` contract the steward consumes.

**Ownership split (design
[spi-h32xj-cleric-repair-loop](../../docs/design/spi-h32xj-cleric-repair-loop.md)
§1):**

| Package        | Owns                                                                   |
|----------------|------------------------------------------------------------------------|
| `pkg/recovery` | Domain types, diagnosis, decide policy, recipe promotion, outcome I/O  |
| `pkg/executor` | Runtime: cleric step adapters, workspace provisioning, spawn, verify wire protocol |
| `pkg/wizard`   | Wizard-side retry: `checkRetryRequest`, skip-to-step, result reporting |
| `pkg/steward`  | Observes `RecoveryOutcome` via `ReadOutcome`; decides resume vs stays-hooked |

The cleric itself is the agent that runs the `cleric-default` formula. This
package provides the diagnostic + policy foundation; `pkg/executor` drives
the step-by-step runtime.

## Cleric lifecycle

The `cleric-default` formula is a 6-step repair loop:

```
collect_context ──→ decide ──→ execute ──→ verify ──→ learn ──→ finish
                      │          │           │
                      │          │           └─ fail → retry → decide
                      │          │
                      │          └─ error → record_error → finish_needs_human
                      │
                      └─ escalate → finish_needs_human
```

| Step              | Owner          | What happens                                                                                   |
|-------------------|----------------|------------------------------------------------------------------------------------------------|
| `collect_context` | `pkg/recovery` | `Diagnose()` + learnings + wizard log tail → `FullRecoveryContext`                             |
| `decide`          | `pkg/recovery` | `Decide()` returns a typed `RepairPlan` from `Diagnosis + History`                              |
| `execute`         | `pkg/executor` | Provisions the plan's `WorkspaceRequest` via the runtime contract, dispatches by `RepairMode`  |
| `verify`          | `pkg/executor` | Runs the `VerifyPlan` embedded in the plan; cooperative retry for `rerun-step`                 |
| `learn`           | `pkg/recovery` | Extracts learning; promotes a repeated resolution to a `MechanicalRecipe`                      |
| `finish`          | `pkg/executor` | Emits `RecoveryOutcome` via `WriteOutcome` and closes the recovery bead (escalate paths too — see "Cleric-finish ↔ steward-sweep contract" below) |

## Domain types

### `RepairMode` — how a repair is executed

```go
const (
    RepairModeNoop       // resume hooked parent, no repair needed
    RepairModeMechanical // deterministic fn (rebase, cherry-pick, rebuild, reset-to-step)
    RepairModeWorker     // agentic repair subprocess on a borrowed workspace
    RepairModeRecipe     // promoted plan; dispatches through the un-promoted path
    RepairModeEscalate   // terminal; needs-human is a property of the plan
)
```

Taxonomy rules:

- `RepairModeMechanical` covers deterministic actions that are promotable to
  recipes.
- `RepairModeWorker` is the replacement for the retired `targeted-fix`
  placeholder. A repair worker is a normal apprentice on a borrowed
  workspace — no cleric-specific spawn path.
- `RepairModeRecipe` executes via the exact same runtime paths as the
  un-promoted mechanical or worker form (see `Recipe.ToRepairPlan`).
- `RepairModeEscalate` is terminal. `needs_human=true` collapses into the
  plan rather than being a parallel decision surface.
- `RepairModeNoop` lets `decide` resume a hooked bead that simply needs to
  be unparked (e.g. after a human edit cleared the interruption).

### `RepairPlan` — the typed output of `Decide`

```go
type RepairPlan struct {
    Mode       RepairMode
    Action     string            // mechanical fn name OR recipe id OR worker role
    Params     map[string]string
    Workspace  WorkspaceRequest  // what workspace execute must provision
    Verify     VerifyPlan        // how to confirm success
    Confidence float64
    Reason     string
}
```

`WorkspaceRequest.Kind` uses `runtime.WorkspaceKind` from the spi-xplwy
runtime contract. The executor passes the resulting `WorkspaceHandle` to
the action function (mechanical) or to `SpawnConfig` (worker). There is no
cleric-only workspace helper.

### `VerifyPlan` and `VerifyVerdict`

```go
const (
    VerifyKindRerunStep           // re-run a wizard step via cooperative retry
    VerifyKindNarrowCheck         // run a targeted command; exit status is the verdict
    VerifyKindRecipePostcondition // replay a recipe's captured postcondition
)

const (
    VerifyVerdictPass
    VerifyVerdictFail
    VerifyVerdictTimeout
)
```

The cleric's verify step dispatches on `VerifyPlan.Kind`. Legacy
`RetryRequest`s that carry only `FromStep` are honored as
`Kind=rerun-step, StepName=FromStep` — see `pkg/wizard/wizard_retry.go`.

### `Decision` — terminal signal the steward reads

```go
const (
    DecisionResume   // unhook parent; wizard can resume
    DecisionEscalate // leave hooked; a human must intervene
)
```

### `RecoveryOutcome` — the structured record every recovery emits

```go
type RecoveryOutcome struct {
    RecoveryAttemptID string
    SourceBeadID      string
    SourceAttemptID   string
    SourceRunID       string
    FailedStep        string
    FailureClass      FailureClass
    RepairMode        RepairMode
    RepairAction      string
    WorkerAttemptID   string           // empty unless Mode == worker/recipe(worker-shape)
    WorkspaceKind     runtime.WorkspaceKind
    HandoffMode       runtime.HandoffMode
    VerifyKind        VerifyKind
    VerifyVerdict     VerifyVerdict
    Decision          Decision
    RecipeID          string
    RecipeVersion     int
}
```

`WriteOutcome` is the **sole writer** of this record: it persists to bead
metadata under `KeyRecoveryOutcome` and to the `recovery_learnings` SQL
table in one call. `ReadOutcome` is the sole reader. The steward must use
`ReadOutcome` — parsing comment text or ad hoc `recovery:*` labels to
decide resume-vs-escalate is forbidden by design (§4 of the spec).

### `Recipe.ToRepairPlan` — promoted dispatch through the un-promoted path

A `MechanicalRecipe` codifies a successful recovery so it can be replayed
directly on future occurrences of the same failure signature. When the
promotion counter for a signature reaches threshold, `Decide` returns a
`RepairPlan{Mode: RepairModeRecipe}` instead of invoking Claude.
`Recipe.ToRepairPlan()` converts the stored recipe into that plan so
execute dispatches through the same mechanical / worker paths as the
un-promoted form. A single failure demotes the signature back to the
agentic default (see `MarkDemoted` / `PromotionState`).

## What this package owns

- **Diagnosis** (`Diagnose`, `DiagnoseAuto`): inspects bead state, attempt
  history, git/worktree diagnostics, and executor runtime state to classify
  the failure.
- **Failure classification** (`FailureClass` + `classify.go`): maps
  `interrupted:*` labels + step context to one of `empty-implement`,
  `merge-failure`, `build-failure`, `review-fix`, `repo-resolution`,
  `arbiter`, `step-failure`, `unknown`.
- **Decide policy** (`Decide`): produces a `RepairPlan` from
  `Diagnosis + History`. Priority order: attempt-budget guard → human
  guidance → promoted recipe replay → git-state heuristics → Claude → fallback.
- **Action-to-RepairMode mapping** (`actionToRepairMode`): the one place
  that maps a historic action name (`rebase-onto-base`, `resolve-conflicts`,
  `escalate`, …) to its canonical `RepairMode`.
- **Recipe promotion** (`promotion.go`, `recipe.go`): per-signature
  promotion counters and the `MechanicalRecipe` model, including
  `ToRepairPlan`.
- **Outcome I/O** (`finish.go`): `WriteOutcome` and `ReadOutcome` — the
  steward contract. Single writer; single reader.
- **Document/finish** (`document.go`, `finish.go`): writes bead metadata
  and the `recovery_learnings` SQL row.

## What this package does NOT own

- **Step execution / runtime orchestration.** `pkg/executor` owns the
  cleric step adapters (`cleric.collect_context`, `cleric.decide`,
  `cleric.execute`, `cleric.verify`, `cleric.learn`, `cleric.finish`) and
  the workspace / spawn plumbing behind them.
- **Workspace provisioning.** Execute calls `resolveGraphWorkspace` with
  the plan's `WorkspaceRequest` and receives a `WorkspaceHandle` from the
  runtime contract. There is no longer a recovery-only worktree fallback.
- **Wizard-side retry.** `checkRetryRequest`, skip-to-step, and result
  reporting live in `pkg/wizard`.
- **Interrupt signaling.** The executor decides when a step hooks and
  writes the failure evidence a cleric later reads.

## Decide priority

1. **Attempt-budget guard** — if `len(history) ≥ MaxAttempts`, emit
   `RepairModeEscalate`.
2. **Human guidance** — a comment whose first imperative token matches a
   keyword in `guidanceMap` (e.g. `rebase`, `rebuild`, `escalate`) wins,
   unless that action has already failed twice.
3. **Promoted recipe replay** — if the failure signature has crossed its
   promotion threshold, dispatch via `Recipe.ToRepairPlan`.
4. **Git-state heuristics** — conflicts → `resolve-conflicts`, diverged or
   behind base → `rebase-onto-base`, dirty → `rebuild`.
5. **Claude-backed decision** — `Deps.ClaudeRunner` invoked with the rich
   context prompt (diagnosis, wizard log tail, ranked actions, learnings,
   stats). Claude's choice is overridden to `escalate` if it matches an
   action with ≥2 prior failures; `needs_human` is set when confidence
   drops below `0.7`.
6. **Fallback** — when Claude is unavailable or returns an error, emit
   `resummon` at confidence `0.4–0.5`.

## Cooperative retry protocol

The cleric never directly re-runs wizard steps. For `VerifyKindRerunStep`,
`pkg/executor/recovery_phase.go` sets a `wizard.RetryRequest` on the
target bead and polls for the wizard's reply. Labels live on the **target
bead** (the one being retried). The wire protocol is unchanged from the
legacy shape; `VerifyPlan` simply rides alongside. Legacy requests that
omit `VerifyPlan` default to `Kind=rerun-step, StepName=FromStep`.

## Learning system

Two storage tiers, both populated by `learn` → `finish`:

| Tier          | Store                                   | Used for                              |
|---------------|-----------------------------------------|---------------------------------------|
| Bead metadata | `KeyRecoveryOutcome` (JSON blob)        | Fast local access; steward `ReadOutcome` |
| SQL           | `recovery_learnings` table              | Cross-bead aggregation and analytics  |

`WriteOutcome` is the single authoritative writer. It also mirrors a small
set of fields (`resolved_at`, `failure_class`, `source_bead`,
`resolution_kind`, `verification_status`, `reusable`) to individual
metadata keys for decide-time query compatibility. The retired
`learning_outcome` scalar is intentionally not written.

Learnings marked `reusable=true` (i.e. `Decision=DecisionResume`) are
candidates for promotion on a subsequent match of the same failure
signature.

## Cleric-finish ↔ steward-sweep contract

The cleric's terminal step (`finish` / `finish_needs_human` /
`finish_needs_human_on_error`) always **closes** the recovery bead. Both
Decision outcomes share this single completion contract:

| Decision           | Recovery bead status | Steward hooked sweep              |
|--------------------|----------------------|-----------------------------------|
| `DecisionResume`   | closed (terminal)    | unhooks parent, re-summons wizard |
| `DecisionEscalate` | closed (terminal)    | leaves parent parked for human    |

An *open* recovery bead means a cleric is either running or needs to be
summoned. The sweep relies on status=closed as the "done" signal and on
`ReadOutcome` to decide between resume and stays-hooked.

Leaving an escalated recovery bead open would cause the steward to
re-claim it as fresh work on every sweep cycle, re-spawning a cleric
against the same parked parent forever. The `finish_needs_human` and
`finish_needs_human_on_error` paths bypass `learn`, so `handleFinish`
persists `Decision=DecisionEscalate` via `WriteOutcome` before closing
— keeping `WriteOutcome` the sole writer and giving the sweep a
structural signal rather than label/comment parsing (see spi-0nkot).

`findFailureEvidence` in `pkg/steward` picks the **latest** recovery bead
(newest `CreatedAt`, ID as tiebreak) so a parent carrying historical
caused-by edges from prior recoveries is evaluated against its most
recent attempt.

## Action surface (names used across the stack)

These are the action strings a `RepairPlan` may carry. They are mapped to a
`RepairMode` by `actionToRepairMode` and to a `WorkspaceRequest` by
`workspaceFromAction`. Both mappings are the single source of truth for
dispatch.

| Action                | Mode       | Typical workspace         |
|-----------------------|------------|---------------------------|
| `rebase-onto-base`    | Mechanical | `owned_worktree`          |
| `cherry-pick`         | Mechanical | `owned_worktree`          |
| `rebuild`             | Mechanical | `owned_worktree`          |
| `reset-to-step`       | Mechanical | `repo` (graph-state only) |
| `reset-hard`          | Mechanical | `repo` (graph-state only) |
| `resolve-conflicts`   | Worker     | `borrowed_worktree`       |
| `resummon`            | Worker     | `borrowed_worktree`       |
| `reset`               | Worker     | `borrowed_worktree`       |
| `triage`              | Worker     | `borrowed_worktree`       |
| `targeted-fix`        | Worker     | `borrowed_worktree`       |
| `verify-clean`        | Noop       | n/a                       |
| `escalate`            | Escalate   | n/a                       |

Worker dispatch (`SpawnRepairWorker` in `pkg/executor`) switches on
`plan.Action`: `resolve-conflicts` assembles a conflict bundle and runs
the conflict-marker validation gates; the other worker actions
(`resummon`, `reset`, `triage`, `targeted-fix`) render a generic
repair prompt and delegate success verification to the cleric's
`verify` step. All worker spawns share a single `SpawnConfig`
construction site (`ctx.BuildRuntimeContract`, wired to
`(*Executor).withRuntimeContract`) so the canonical Identity /
Workspace / RunContext fields required by the k8s substrate validator
are populated — never a hand-built process-only config (spi-6wiz9).

The legacy `actionTargetedFix` mechanical-dispatch tombstone in
`pkg/executor/recovery_actions.go` is unrelated to the worker-mode
action above: it exists only so stale paths that still call the
function by name fail loudly with a pointer at `RepairModeWorker`.

## Constants

| Constant                      | Value  | Purpose                                          |
|-------------------------------|--------|--------------------------------------------------|
| `DefaultMaxRecoveryAttempts`  | 3      | Retry budget before auto-escalation              |
| `DefaultVerifyPollInterval`   | 30s    | Cooperative verify poll interval                 |
| `DefaultVerifyTimeout`        | 10min  | Max wait for wizard retry result                 |
| `KeyRecoveryOutcome`          | string | Metadata key; sole writer `WriteOutcome`; sole reader `ReadOutcome` |

## Practical rules

1. **Domain + policy here; runtime in `pkg/executor`.** Don't put
   workspace, spawn, or wire-protocol code here. Policy decisions flow out
   of this package as a typed `RepairPlan`; runtime dispatch happens on the
   executor side.
2. **`WriteOutcome` is the single writer.** No other code path should
   emit a `RecoveryOutcome` record. The steward reads only through
   `ReadOutcome`.
3. **`FailureClass` is the routing key.** Learnings, action ranking, and
   cross-bead aggregation all key on it. Get classification right in
   `classify.go` before touching decide priority.
4. **`actionToRepairMode` and `workspaceFromAction` are paired.** Any
   new action name must appear in both — the second is in `recipe.go` and
   governs promoted-recipe dispatch. Out-of-sync mappings produce plans
   that fail dispatch.
5. **Don't bypass the retry protocol.** `VerifyKindRerunStep` always goes
   through `RetryRequest` on the target bead. The cleric never mutates a
   target bead's branch state directly.
