# Design: Cleric Repair Loop on Shared Runtime Primitives — spi-h32xj

> Spec for epic spi-h32xj. Design bead: spi-gua4l. Depends on the runtime
> contract spec at `docs/design/spi-xplwy-runtime-contract.md`.

## Purpose

Collapse the cleric/recovery stack into a repair loop that sits on top of
the canonical runtime primitives from spi-xplwy. Today `pkg/recovery`
presents a broad recovery model while most runtime behavior lives in
`pkg/executor`, `targeted-fix` is a placeholder, and there are two
overlapping action vocabularies (`pkg/recovery.RecoveryActionKind` and
`pkg/executor.RecoveryAction`). The redesign is not a new orchestration
system — it is the existing `cleric-default` formula with sharper package
boundaries, one action taxonomy, real agentic repair on the canonical
workspace contract, and an executable recipe path.

---

## 1. Canonical cleric lifecycle

The `cleric-default` formula already encodes the right spine:

```
collect_context → decide → execute → verify → retry? → learn → finish
                                │
                                └─ on error → retry_on_error → ... → finish_needs_human_on_error
```

We keep that spine. The redesign replaces what each node does and where
it lives:

| Step             | Owner         | Responsibility                                                                                                                       |
|------------------|---------------|--------------------------------------------------------------------------------------------------------------------------------------|
| `collect_context`| `pkg/recovery`| Produce a structured `Diagnosis` + `FullRecoveryContext`. No runtime work beyond read-only inspection.                               |
| `decide`         | `pkg/recovery`| Policy: `FailureClass + History → RepairPlan`. Emits a typed `RepairPlan`, not a free-form action string.                            |
| `execute`        | `pkg/executor`| Orchestrate the chosen `RepairMode` using shared runtime primitives (`WorkspaceHandle`, `SpawnConfig`). No cleric-specific workspace.|
| `verify`         | `pkg/executor`| Run the `VerifyPlan` embedded in the `RepairPlan` against the target bead using the same cooperative retry protocol as today.        |
| `learn`          | `pkg/recovery`| Promote a reusable repair into a `Recipe` if criteria met; persist outcome to metadata + SQL.                                        |
| `finish`         | `pkg/executor`| Close recovery bead and emit the structured outcome the steward consumes (see §4).                                                   |

The key shift: **`pkg/recovery` owns the domain model and policy;
`pkg/executor` owns the runtime.** Today `pkg/executor/recovery_actions.go`
mixes both.

---

## 2. Repair-mode taxonomy

Replace the two parallel vocabularies with one typed `RepairPlan`.

```go
// Package: pkg/recovery

type RepairMode string

const (
    RepairModeNoop       RepairMode = "noop"        // resume without repair
    RepairModeMechanical RepairMode = "mechanical"  // deterministic fn (rebase/rebuild/reset)
    RepairModeWorker     RepairMode = "worker"      // agentic repair subprocess
    RepairModeRecipe     RepairMode = "recipe"      // promoted, executable plan
    RepairModeEscalate   RepairMode = "escalate"    // needs-human terminal
)

type RepairPlan struct {
    Mode       RepairMode
    Action     string           // mechanical fn name OR recipe id OR worker role
    Params     map[string]string
    Workspace  WorkspaceRequest // what workspace the execute step must provision
    Verify     VerifyPlan       // how to confirm success
    Confidence float64
    Reason     string           // human-readable "why this mode"
}

type WorkspaceRequest struct {
    Kind     WorkspaceKind   // from runtime contract — borrowed_worktree typical
    BorrowFrom string        // target bead id if borrowing parent workspace
}

type VerifyPlan struct {
    Kind     VerifyKind       // "rerun-step" | "narrow-check" | "recipe-postcondition"
    StepName string           // for rerun-step
    Command  []string         // for narrow-check
}
```

**Taxonomy rules:**

- `RepairModeMechanical` covers today's `rebase-onto-base`, `cherry-pick`,
  `rebuild`, `reset-to-step`. These are deterministic and promotable to
  recipes.
- `RepairModeWorker` replaces `targeted-fix`'s placeholder and today's
  bespoke `resolve-conflicts` agentic dispatch. A repair worker is a
  normal apprentice run on a borrowed workspace — nothing cleric-specific.
- `RepairModeRecipe` is a mechanical or worker plan that was promoted by
  a prior `learn` step. Executing it uses the exact same runtime paths
  as the un-promoted form.
- `RepairModeEscalate` is terminal. `needs_human=true` is a property of
  the plan, not a separate decision surface.
- `RepairModeNoop` exists so `decide` can resume hooked beads that
  simply need to be unparked (e.g. after a human edit).

**What goes away:**

- The two separate registries (`pkg/recovery.RecoveryActionKind` +
  `pkg/executor.RecoveryAction`) collapse into one `RepairMode` +
  action-name space.
- `actionTargetedFix` (`recovery_actions.go:422`) disappears — it becomes
  a `RepairModeWorker` plan whose `Action` names the repair role.

---

## 3. Runtime/workspace model (reuses spi-xplwy)

The cleric does not get its own workspace semantics. Every `RepairMode`
maps onto the runtime contract:

| Mode         | `WorkspaceHandle.Kind` | `Borrowed` | `HandoffMode`  |
|--------------|------------------------|-----------|-----------------|
| Noop         | n/a                    | n/a       | `none`          |
| Mechanical   | `owned_worktree` (or `repo` for pure-db ops) | false | `none` (in-place) |
| Worker       | `borrowed_worktree` of the failed bead's workspace | true | `borrowed` |
| Recipe       | whatever the promoted plan requires | inherits from plan | inherits |
| Escalate     | n/a                    | n/a       | `none`          |

Consequences:

- The `ProvisionRecoveryWorktree` helper in
  `pkg/executor/recovery_actions.go` is replaced by a call into the
  shared `resolveWorkspace` with a `WorkspaceRequest`. There is no
  longer a recovery-only worktree fallback path.
- Repair workers run through `pkg/agent.Backend.Spawn` like any other
  apprentice — no cleric-specific spawn code. The k8s backend changes
  landed by spi-xplwy chunk 2 (non-wizard workspace-bearing pods) are
  a hard prerequisite for repair workers to work in cluster mode.
- The cooperative retry protocol (`wizard.RetryRequest` +
  `cleric.verify`) is unchanged in shape but is driven by
  `VerifyPlan.Kind=rerun-step` rather than an implicit assumption.

---

## 4. Observability and steward contract

Every recovery attempt must carry this structured metadata in traces,
metrics, persisted bead metadata, and the outcome the steward reads:

```go
type RecoveryOutcome struct {
    RecoveryAttemptID string
    SourceBeadID      string
    SourceAttemptID   string
    SourceRunID       string
    FailedStep        string
    FailureClass      FailureClass
    RepairMode        RepairMode
    RepairAction      string        // fn name / recipe id / worker role
    WorkerAttemptID   string        // empty unless Mode == worker/recipe(worker-shape)
    WorkspaceKind     WorkspaceKind
    HandoffMode       HandoffMode
    VerifyKind        VerifyKind
    VerifyVerdict     VerifyVerdict // "pass"|"fail"|"timeout"
    Decision          Decision      // "resume"|"escalate"
    RecipeID          string        // if a recipe was executed or promoted
    RecipeVersion     int
}
```

**Steward integration rule:** the steward reads `RecoveryOutcome` from a
typed accessor (`recovery.ReadOutcome(bead store.Bead) (RecoveryOutcome, bool)`).
It must not parse comment text or rely on ad hoc label strings to decide
whether to resume a hooked parent. This removes the "completion-contract
mismatch risk" called out in the architecture-drift assessment.

**Metric/trace vocabulary** reuses the `RunContext` fields from
spi-xplwy and adds:
`recovery_mode`, `recovery_action`, `failure_class`, `verify_verdict`,
`decision`, `recipe_id`. Bead/attempt IDs stay in logs/traces only.

---

## 5. Package/file cut

### `pkg/recovery`

Owns domain + policy. Gets bigger.

- `types.go`: add `RepairMode`, `RepairPlan`, `VerifyPlan`, `VerifyKind`,
  `VerifyVerdict`, `Decision`, `RecoveryOutcome`, `WorkspaceRequest`.
- `classify.go`: unchanged — `FailureClass` stays the routing key.
- `decide.go` (**new**, lifting from
  `pkg/executor/recovery_decide.go`): convert `Diagnosis + History` to
  `RepairPlan`. This is where human-guidance parsing, git-state
  heuristics, and Claude-backed decision currently in
  `recovery_decide.go` move to. Claude invocation stays behind the
  existing `Deps` seam for testability.
- `recipe.go`: add `Recipe.ToRepairPlan()` so a promoted recipe executes
  through the same path as an un-promoted plan. No separate recipe
  runtime.
- `finish.go`: emit `RecoveryOutcome` to metadata + SQL. Single writer.
- `document.go`, `diagnose.go`, `bead_ops.go`, `verify.go`: unchanged
  in intent; update signatures to return/accept the new types.

### `pkg/executor`

Owns runtime. Gets smaller.

- `recovery_phase.go`: keep `actionClericCollectContext`,
  `actionClericDecide`, `actionClericLearn` as thin adapters — they
  call into `pkg/recovery` and persist the results in graph outputs.
  `actionClericExecute` and `actionClericVerify` become the **only**
  places that touch runtime primitives (workspace, spawn, retry
  protocol).
- `recovery_actions.go`: **remove** the `RecoveryAction` registry and
  `actionTargetedFix`. Mechanical actions move to a table keyed by
  action name inside `actionClericExecute`, consuming a
  `WorkspaceHandle`. Deterministic action bodies (rebase, rebuild,
  reset-to-step, cherry-pick) stay but become functions taking a
  `RepairPlan` + `WorkspaceHandle`, returning a mechanical
  `Recipe` for promotion on success.
- `recovery_actions_agentic.go`: the agentic `resolve-conflicts` path
  becomes the canonical `RepairModeWorker` implementation. The current
  bespoke wiring to spawn an apprentice with a conflict bundle becomes
  a general "spawn repair worker on borrowed workspace" helper. There
  is no longer an "agentic" subcategory — a worker is a worker.
- `recovery_protocol.go`: unchanged wire protocol; the cleric sets a
  `RetryRequest` with `VerifyPlan` context and polls for result. The
  wizard side is already in `pkg/wizard/wizard_retry.go`.
- `recovery_context.go`, `recovery_collect.go`: stay as helpers for the
  `collect_context` step adapter.
- `recovery_decide.go`: **delete** after content migrates to
  `pkg/recovery/decide.go`.
- `recovery_relapse.go`, `recovery_merge_test.go`: trace-only metadata
  updates to match the new outcome schema.

### `pkg/wizard`

- `wizard_retry.go`: extend `RetryRequest` to carry `VerifyPlan` (today
  it only carries `FromStep`). Backward compat: when `VerifyPlan` is
  unset, default to `Kind=rerun-step` with `StepName=FromStep`.
- No lifecycle changes. Wizard does not learn anything new about cleric.

### `pkg/steward`

- Replace every ad hoc `recovery:*` label lookup with
  `recovery.ReadOutcome`. `learning_outcome=clean|escalated` becomes
  `Outcome.Decision=resume|escalate`.

### `pkg/git`

- No changes. Mechanical git actions continue to call `pkg/git`
  primitives; they do not introduce new ones.

### Formulas

- `cleric-default.formula.toml`: unchanged structurally. Update
  step-level docs to reflect that `decide.outputs` is a `RepairPlan`
  JSON object rather than a loose `action` string, and that
  `verify.outputs` is a `VerifyVerdict`.

### Docs

- `pkg/recovery/README.md`: document `RepairMode`, `RepairPlan`,
  `RecoveryOutcome`, and state plainly that runtime orchestration lives
  in `pkg/executor`.
- `pkg/executor/README.md`: prune the recovery section so it only
  describes the adapter/runtime surface.
- `docs/ARCHITECTURE.md`: update the recovery section to point at this
  spec.

---

## 6. Migration sequence

Each chunk is a separately landable bead under spi-h32xj. The order is
chosen so the cleric is always functional and `cleric-default.formula.toml`
never needs a breaking change.

**Chunk 1 — types only.**

Add `RepairMode`, `RepairPlan`, `VerifyPlan`, `RecoveryOutcome` in
`pkg/recovery`. Update `RecoveryActionResult`/`RecoveryActionRequest` to
carry a `RepairPlan` reference alongside the legacy fields. No behavior
change.

**Chunk 2 — move `decide` policy into `pkg/recovery`.**

Lift `pkg/executor/recovery_decide.go` content into
`pkg/recovery/decide.go`. `actionClericDecide` becomes a 10-line adapter
that calls `recovery.Decide(...)` and writes the `RepairPlan` JSON to
`steps.decide.outputs.plan`. Keep the old `outputs.chosen_action` for
compatibility until chunk 6.

**Chunk 3 — unify action vocabulary.**

Collapse `pkg/executor.RecoveryAction` registry and
`pkg/recovery.RecoveryActionKind` into one action-name space keyed by
`RepairMode`. Delete `actionTargetedFix`; `targeted-fix` intent now maps
to a `RepairModeWorker` plan. Mechanical action functions gain a new
signature `(plan RepairPlan, ws WorkspaceHandle) (*Recipe, error)` but
their bodies are unchanged.

**Chunk 4 — execute on shared workspace contract.**

`actionClericExecute` stops calling `ProvisionRecoveryWorktree`. It calls
`resolveWorkspace` with the plan's `WorkspaceRequest` and hands the
resulting `WorkspaceHandle` to the action function (mechanical) or to
`SpawnConfig` (worker). This chunk depends on spi-xplwy chunk 2 being
merged so k8s apprentice/sage pods can accept a borrowed workspace.

**Chunk 5 — verify becomes `VerifyPlan`-driven.**

`actionClericVerify` reads `VerifyPlan` from the decide step output and
sets `RetryRequest.VerifyPlan`. The wizard honors it; legacy
`FromStep`-only requests remain supported behind the default branch in
`wizard_retry.go`.

**Chunk 6 — single outcome schema + steward adapter.**

`finish` writes `RecoveryOutcome` to bead metadata + `recovery_learnings`
SQL. Steward reads via `recovery.ReadOutcome`. Remove the legacy
`learning_outcome` / `recovery:status=*` label paths after one release
of coexistence.

**Chunk 7 — recipe executability.**

`Recipe.ToRepairPlan()` plus the executor paths that consume it. Recipe
promotion on `learn` stays (already in `pkg/recovery/promotion.go`); the
new piece is that a recipe can now be *executed* via the same paths as
its un-promoted form. Add a regression test that a promoted
`rebase-onto-base` recipe and an un-promoted one produce byte-identical
post-conditions.

**Chunk 8 — docs + tests sweep.**

Update READMEs, ARCHITECTURE.md, and add the behavioral tests listed in
§7 as a CI lane. Remove `recovery_decide.go` and any other dead code
from `pkg/executor`.

---

## 7. Test matrix

| Scenario                                                       | Where                                    |
|----------------------------------------------------------------|------------------------------------------|
| `FailureClass=merge-failure` → `RepairMode=mechanical` (rebase)| `pkg/recovery/decide_test.go`            |
| `FailureClass=build-failure` → `RepairMode=recipe` if covered  | `pkg/recovery/decide_test.go`            |
| `FailureClass=review-fix` → `RepairMode=worker`                | `pkg/recovery/decide_test.go`            |
| Mechanical rebase on `WorkspaceKind=owned_worktree`            | `pkg/executor/recovery_phase_test.go`    |
| Mechanical rebase on k8s owned workspace                       | `pkg/agent/backend_k8s_test.go` (cluster lane) |
| Repair worker on `WorkspaceKind=borrowed_worktree` (local)     | `pkg/executor/v3_cleric_retry_test.go`   |
| Repair worker on k8s borrowed workspace                        | `pkg/agent/backend_k8s_test.go`          |
| `VerifyPlan=rerun-step` pass → `Decision=resume`               | existing cooperative-retry test (extend) |
| `VerifyPlan=narrow-check` command pass                         | new                                      |
| `VerifyPlan=recipe-postcondition`                              | new                                      |
| Steward reads `RecoveryOutcome` and unhooks parent             | `pkg/steward/recovery_test.go`           |
| Steward sees `Decision=escalate` and leaves parent hooked      | `pkg/steward/recovery_test.go`           |
| Recipe promoted from mechanical repair produces same outcome when re-executed | `pkg/recovery/promotion_test.go`, `pkg/executor/recovery_phase_test.go` |
| Legacy `FromStep`-only `RetryRequest` still honored            | `pkg/wizard/wizard_retry_test.go`        |

---

## 8. Non-goals

- Redesigning review/fix or any non-recovery formula.
- Guild-cache work.
- A cleric-only workspace or transport architecture — this spec
  specifically forbids inventing one.
- Changing Claude prompts for `decide`/`learn` beyond what the schema
  change requires.
- Changing the max-retry / needs-human thresholds; those are policy
  knobs independent of this refactor.

---

## 9. Open questions for planning-agent to resolve

1. Should `RepairPlan` JSON be the single `decide.outputs` payload, or
   kept as a parallel typed field alongside the existing scalar outputs
   for one release? Recommendation: parallel for chunks 2–5, single
   payload from chunk 6 onward.
2. Where does `Recipe.ToRepairPlan()` live — `pkg/recovery` (domain) or
   a sub-package? Recommendation: `pkg/recovery`; a recipe is a domain
   concept.
3. Do we keep `actionTargetedFix` as a tombstone "raises helpful error
   pointing at worker mode" for one release, or delete outright?
   Recommendation: tombstone for one release; `targeted-fix` appears in
   historical recovery beads and we do not want to break resume paths.
