# pkg/wizard

Process-facing agent runtime for wizard subprocesses.

This package is where a summoned agent actually does its work once some other
part of the system has decided what role it should play and where it should
run.

In practice, `pkg/wizard` owns:
- apprentice entrypoints (`apprentice run`)
- sage entrypoints (`sage review`)
- prompt assembly for those subprocesses
- Claude invocation, timeout handling, validation, commit, and result writing
- worktree preparation for a single subprocess — owns a fresh worktree when self-managed, or resumes a borrowed worktree via `--worktree-dir` (honored for all modes: implement, review-fix, build-fix). The worktree mount is a *runtime* concern; bundle delivery (Principle 1, spi-1dk71j) is independent.
- bundle production for every commit-producing apprentice mode (implement, review-fix) via `deliverApprenticeWork` — including review-fix, where the executor consumes the bundle into staging
- review-round bead creation/closure and review-specific handoff behavior
- legacy `wizard-epic` helpers that predate the formula executor

## What this package owns

- **Single-agent lifecycle**: start a wizard subprocess, prepare its workspace, run the role-specific flow, and write `result.json`.
- **Apprentice execution**: implementation, review-fix, and build-fix flows in `apprentice run`.
- **Sage execution**: diff review, test execution, verdict production, and review bead updates in `sage review`.
- **Prompt and validation mechanics**: prompt text, Claude CLI invocation, timeout enforcement, lint/build/test validation, and commit logic for one subprocess.
- **Per-process workspace handling**: create a fresh worktree when self-managed, or resume a borrowed worktree when the caller (typically the v3 graph executor) passes `--worktree-dir`. This flag is honored for all modes — implementation, review-fix, and build-fix — not just review-fix.
- **Legacy epic helpers**: `wizard-epic` and related files still live here.

## What this package does NOT own

- **Capacity planning and process fleet management**: the steward decides when to summon, unsummon, reset, or replace wizards.
- **Formula authoring or validation**: the workshop/artificer owns creating, testing, validating, and publishing formulas.
- **Phase policy for a bead's lifecycle**: the executor decides which phase runs next, whether work is wave-based, when to review, and when to merge.
- **Git primitives**: branch/worktree semantics belong in `pkg/git`.
- **CLI wiring and concrete environment dependencies**: `cmd/spire` builds the dependency graph and command surface.

## Relationship To Executor

The clean split is:
- **executor** decides what should happen next for a bead
- **wizard** performs one subprocess role inside the workspace it was given

Examples:
- The executor decides whether a review-fix should happen on a feature branch or in a shared staging worktree.
- The wizard performs the review-fix once that workspace decision has already been made.
- The executor decides whether to skip a post-fix merge.
- The wizard does not make that decision; it just implements, validates, and commits.
- The executor may pass `--worktree-dir` for any mode (implement, review-fix, build-fix); the wizard resumes that workspace without owning its lifecycle.

### Position in the runtime contract

The wizard sits at the bottom of the four-type contract defined in
[docs/design/spi-xplwy-runtime-contract.md §1](../../docs/design/spi-xplwy-runtime-contract.md).
Specifically:

- **One role, one assigned workspace.** A wizard subprocess runs exactly
  one `SpawnRole` (apprentice, sage, or wizard-orchestrator) inside the
  `WorkspaceHandle.Path` the backend materialized. It does not choose
  between kinds, switch branches inside borrowed worktrees, or invent
  workspace policy.
- **Does NOT infer `RepoIdentity`.** Tower name, prefix, repo URL, and
  base branch arrive as env (`BEADS_DATABASE`, `BEADS_PREFIX`,
  `SPIRE_REPO_URL`, etc.) set by the backend on executor orders. The
  wizard reads them; it does not walk the CWD, re-read `.beads/metadata`
  to choose a database, or fall back to a hardcoded default. The
  `pkg/runtime` audit test enforces this.
- **Does NOT choose workspace policy.** Kind, origin, branch, base
  branch, and borrowed-vs-owned are set by the executor before spawn.
- **Emits `RunContext` on every structured log line and in `result.json`.**
  The backend stamps the canonical `SPIRE_*` env on the process; the
  wizard rehydrates `RunContext` via `runtime.RunContextFromEnv` and
  threads it into every log emission (`runtime.LogFields`) and into the
  `run_context` block of `result.json` so parent executors can correlate
  the run without re-parsing.

If a change is about which role runs, which workspace it uses, or
which handoff mode applies, it belongs in `pkg/executor`, not here.

### Deployment-mode awareness at the review-fix seam

A wizard subprocess does its own work without knowing the tower's
`DeploymentMode`. The exception is the small set of call sites that
re-enter the dispatch plane to spawn another child — most notably
`wizard_review.go`'s review-fix re-entry. Those sites are
mode-aware:

- In `local-native`, review-fix re-entry spawns an apprentice through
  `pkg/agent.Spawner.Spawn` like any other child.
- In `cluster-native`, those call sites emit an intent (role
  `apprentice`, phase `fix`) through the operator seam. They do not
  call `Spawner.Spawn`. Wizard pods materialize apprentice and sage
  pods only by way of intent emission — the operator is the dispatch
  authority, not the wizard. See
  [docs/VISION-CLUSTER.md → Operator-owned dispatch](../../docs/VISION-CLUSTER.md#operator-owned-dispatch-cluster-native-invariant).

`wizard.Summon`-style helpers and other dispatch-time entry points
are contract-only emit points in cluster-native paths: they assemble
the runtime contract (`RepoIdentity`, `WorkspaceHandle`,
`HandoffMode`, `RunContext`) and hand it to the publisher. The
runtime portion of the wizard — prompt assembly, Claude invocation,
validation, commit, result writing — is unchanged across modes.

### Review-feedback ownership in cluster-native

In cluster-native mode the wizard's local registry row is **not** the
ownership surface for review-feedback re-engagement. The wizard's
process registry (`~/.config/spire/wizards.json`, owned by
`pkg/agent`) is per-machine bookkeeping; another steward replica on
another machine cannot rely on it to find the wizard that produced
the staging branch.

Cluster-native review-feedback re-engagement instead reads from a
shared-state surface that is durable across replicas — the
`implemented-by` / attempt-bead linkage, attempt metadata, and the
typed review outcome. The wizard rehydrates its identity from
attempt-bead metadata + env (`SPIRE_BEAD_ID`, `SPIRE_AGENT_NAME`,
`BEADS_DATABASE`) when it reboots into the next review-feedback
cycle; it does not require its old registry row to exist.

In local-native mode the registry row remains the wizard's local
identity ledger and is used for stale detection / orphan sweep.
`pkg/agent.ProcessBackend.Spawn` is still its sole creator. The
[Registry writes are forbidden here](#registry-writes-are-forbidden-here)
rule applies in both modes.

See
[docs/ARCHITECTURE.md → Shared-state ownership for review feedback](../../docs/ARCHITECTURE.md#shared-state-ownership-for-review-feedback)
and [pkg/steward/README.md → Shared-state ownership for review feedback](../steward/README.md#shared-state-ownership-for-review-feedback).

## Retry-from-step (cooperative recovery)

When a cleric (recovery agent) sets a `RetryRequest` on a bead, the
wizard checks it at startup via `checkRetryRequest` in
`wizard_retry.go`. If a request is present, the wizard skips ahead to
the requested step (via `shouldSkipTo`), executes it, and reports the
outcome back to the recovery agent via `SetRetryResult`. This enables
cooperative recovery without the cleric needing to drive the wizard's
internal lifecycle.

Multiple retry requests are deduplicated — only the latest (highest
`AttemptNumber`) is honored. Stale requests are cleared automatically.

## Key entrypoints

| Entry point | Purpose |
|-------------|---------|
| `CmdWizardRun` | Apprentice subprocess entrypoint for implementation, review-fix, and build-fix. Honors `--worktree-dir` for all modes — the v3 executor passes a managed workspace when the graph declares one. |
| `cmdBuildFix` | Specialized build-fix apprentice path working directly in an existing worktree. |
| `CmdWizardReview` | Sage subprocess entrypoint for reviewing a diff and producing a verdict. |
| `ReviewHandleApproval` | Review-side terminal handoff when the sage approves. |
| `CmdWizardEpic` | Legacy epic orchestrator entrypoint. |

## Registry writes are forbidden here

Wizard, handoff, and re-engagement code in this package MUST NOT call
`registry.Add` / `registry.Upsert` (and `Deps.RegistryAdd` no longer
exists, so this is enforced at compile time). The only legitimate
creator of a wizard registry row is `pkg/agent.ProcessBackend.Spawn` —
see [pkg/agent/README.md → Registry lifecycle](../agent/README.md#registry-lifecycle)
for the canonical ownership table.

What this package may do:

- `registry.Update(...)` to stamp `Phase`, `PhaseStartedAt`, `Worktree`
  on the row that `backend.Spawn` already created. `CmdWizardRun` and
  `CmdWizardReview` do this on entry.
- `Deps.RegistryRemove(...)` to clean up after a `backend.Spawn`
  failure (e.g. `WizardReviewHandoff` removes the partially-created
  reviewer entry if the spawn errors out).
- `Deps.RegistryUpdate(...)` to stamp the real PID once the spawn
  handle returns (the placeholder was 0 inside the backend's Add).

What this package may **not** do:

- Pre-register an entry "so the row is visible early." The dual-write
  with `backend.Spawn` was the spi-30nma0 bug. Visibility shifts by
  milliseconds without it; consumers must tolerate that.
- Self-register in the child runtime via `registry.Upsert` or any other
  creator. Stamping (`Update`) is the child's only legitimate write.

## Practical rules

1. **Use `pkg/git` for worktree and branch semantics.** `pkg/wizard` may choose between "create" and "resume", but it should not hand-roll git mechanics.
2. **Keep this package focused on one subprocess at a time.** If the change is about the bead-level phase graph, routing policy, or cross-phase coordination, it probably belongs in `pkg/executor`.
3. **Borrowed worktrees are not owned here.** If the caller supplied `--worktree-dir` (common in v3 graph execution for all modes, not just review-fix), this package must not clean it up or create/switch branches inside it.
4. **Preserve the existing apprentice contract.** If you add a new apprentice mode, keep prompt, timeout, validation, commit, and `result.json` behavior consistent unless the change is intentional and documented.
5. **Treat `wizard-epic` as legacy-specialized code.** New formula-lifecycle work should usually extend `pkg/executor`, not the older epic loop, unless you are explicitly maintaining that command.
6. **Never write the wizard registry from this package.** See "Registry writes are forbidden here" above; `backend.Spawn` is the sole creator.

## Where new work usually belongs

- Add it to **`pkg/wizard`** when the change is about how an apprentice or sage subprocess runs.
- Add it to **`pkg/executor`** when the change is about which subprocess to run, when to run it, or how phases transition.
- Add it to **`pkg/git`** when the change is about worktrees, branches, merges, refs, or commit detection.
- Add it to **`pkg/workshop`** when the change is about authoring or validating formulas.
