# pkg/wizard

Process-facing agent runtime for wizard subprocesses.

This package is where a summoned agent actually does its work once some other
part of the system has decided what role it should play and where it should
run.

In practice, `pkg/wizard` owns:
- apprentice entrypoints (`wizard-run`)
- sage entrypoints (`wizard-review`)
- prompt assembly for those subprocesses
- Claude invocation, timeout handling, validation, commit, and result writing
- worktree preparation for a single subprocess, including borrowed-worktree vs owned-worktree handling
- review-round bead creation/closure and review-specific handoff behavior
- legacy `wizard-epic` helpers that predate the formula executor

## What this package owns

- **Single-agent lifecycle**: start a wizard subprocess, prepare its workspace, run the role-specific flow, and write `result.json`.
- **Apprentice execution**: implementation, review-fix, and build-fix flows in `wizard-run`.
- **Sage execution**: diff review, test execution, verdict production, and review bead updates in `wizard-review`.
- **Prompt and validation mechanics**: prompt text, Claude CLI invocation, timeout enforcement, lint/build/test validation, and commit logic for one subprocess.
- **Per-process workspace handling**: create a fresh worktree when this process owns it, or resume a borrowed worktree when the caller already chose the workspace.
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

## Key entrypoints

| Entry point | Purpose |
|-------------|---------|
| `CmdWizardRun` | Apprentice subprocess entrypoint for normal implementation, review-fix, and borrowed-worktree flows. |
| `cmdBuildFix` | Specialized build-fix apprentice path working directly in an existing worktree. |
| `CmdWizardReview` | Sage subprocess entrypoint for reviewing a diff and producing a verdict. |
| `ReviewHandleApproval` | Review-side terminal handoff when the sage approves. |
| `CmdWizardEpic` | Legacy epic orchestrator entrypoint. |

## Practical rules

1. **Use `pkg/git` for worktree and branch semantics.** `pkg/wizard` may choose between "create" and "resume", but it should not hand-roll git mechanics.
2. **Keep this package focused on one subprocess at a time.** If the change is about the bead-level phase graph, routing policy, or cross-phase coordination, it probably belongs in `pkg/executor`.
3. **Borrowed worktrees are not owned here.** If the caller supplied `--worktree-dir`, this package must not clean it up or create/switch branches inside it.
4. **Preserve the existing apprentice contract.** If you add a new apprentice mode, keep prompt, timeout, validation, commit, and `result.json` behavior consistent unless the change is intentional and documented.
5. **Treat `wizard-epic` as legacy-specialized code.** New formula-lifecycle work should usually extend `pkg/executor`, not the older epic loop, unless you are explicitly maintaining that command.

## Where new work usually belongs

- Add it to **`pkg/wizard`** when the change is about how an apprentice or sage subprocess runs.
- Add it to **`pkg/executor`** when the change is about which subprocess to run, when to run it, or how phases transition.
- Add it to **`pkg/git`** when the change is about worktrees, branches, merges, refs, or commit detection.
- Add it to **`pkg/workshop`** when the change is about authoring or validating formulas.
