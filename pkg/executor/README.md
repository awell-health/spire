# pkg/executor

Formula-driven bead execution engine.

This package is the per-bead control plane. It takes a resolved formula and
drives one bead through that formula's lifecycle: resolve branch state, persist
execution state, dispatch agents, walk review logic, and land approved work.

If `pkg/wizard` is the runtime for one subprocess role, `pkg/executor` is the
thing deciding what role should run next for the bead.

## What this package owns

- **Formula execution**: walk the bead through declared phases and review DAG steps.
- **Persistent execution state**: `state.json`, phase tracking, wave index, review rounds, build-fix rounds, and worktree path persistence.
- **Bead-level orchestration policy**: direct vs wave implementation, review-fix routing, build-fix retries, merge behavior, and terminal-step handling.
- **Shared staging worktree policy**: create or resume the single staging worktree, decide when it should be used, and clean it up at executor exit.
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

- **Steward** is above the executor. It ensures the tower has enough wizard capacity, summons and resets workers, and routes work to ready beads.
- **Workshop** is beside the executor. It defines and validates the formulas that the executor consumes, but it does not run those formulas on live beads.

## Key types and entrypoints

| Type / function | Purpose |
|-----------------|---------|
| `Executor` | In-memory driver for one bead's formula lifecycle. |
| `State` | Persistent execution state written under runtime storage. |
| `New` | Construct or resume an executor for a bead. |
| `Run` | Main phase loop for the bead. |
| `ensureStagingWorktree` | Create or resume the single shared staging worktree for this execution. |
| `executeReview` | Walk the review DAG and dispatch sage/fix/arbiter/terminal steps. |
| `executeMerge` | Verify and land approved work on the base branch. |
| `recordAgentRun` | Persist metrics and actual run outcomes. |

## Practical rules

1. **Keep policy here, not runtime details.** The executor decides *what* should happen next, not *how Claude should be invoked* inside a subprocess.
2. **Persist once, reuse everywhere.** Repo path, base branch, staging branch, worktree path, and phase state should be resolved once and then read from executor state.
3. **Use the staging worktree as the shared integration surface.** Child branches and borrowed review/build fixes should converge there; don’t invent parallel merge paths casually.
4. **Prefer spawning existing wizard flows over duplicating them.** If a subprocess already knows how to implement, validate, and commit, route to it instead of rebuilding that logic in the executor.
5. **Use formulas as the source of lifecycle truth.** If the change is about phase order, conditions, or review graph structure, start with the formula and workshop model before hardcoding behavior here.
6. **Treat metrics and workflow beads as executor responsibilities.** If a new path changes agent outcomes, review rounds, or terminal behavior, update recording and workflow-bead transitions here.

## Where new work usually belongs

- Add it to **`pkg/executor`** when the change affects bead-level orchestration, phase transitions, review DAG routing, or merge policy.
- Add it to **`pkg/wizard`** when the change affects how a spawned apprentice or sage process runs.
- Add it to **`pkg/git`** when the change affects worktree, branch, merge, or commit-detection mechanics.
- Add it to **`pkg/workshop`** when the change affects formula creation, validation, dry-run, or publishing.
