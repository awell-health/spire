# pkg/steward

Tower-level coordination and daemon orchestration.

This package is above per-bead execution. It decides which work should be
assigned, which wizards are idle or stale, and when tower-wide maintenance
work should run.

If `pkg/executor` is the per-bead control plane, `pkg/steward` is the
multi-bead coordinator.

## What this package owns

- **Ready-work assignment**: find ready beads and assign them to idle agents.
- **Roster usage**: load agent state from the configured backend and compute
  busy vs idle capacity.
- **Stale and timeout enforcement**: detect unhealthy work, warn, dismiss, or
  reset agents based on configured thresholds.
- **Review re-engagement routing**: detect beads that need follow-up review or
  wizard re-entry and route them back into motion.
- **Tower daemon duties**: sync loops, inbox delivery, dead-agent cleanup, and
  tower-level maintenance work.
- **Backend-facing coordination**: local process dispatch vs managed backends.

## What this package does NOT own

- **Per-bead lifecycle execution**: once work is assigned, the executor owns
  phase progression for that bead.
- **Subprocess runtime details**: prompt assembly, Claude invocation, result
  files, validation, and commit logic belong in `pkg/wizard`.
- **Git semantics**: branches, worktrees, merges, refs, and SHAs belong in
  `pkg/git`.
- **Formula authoring or validation**: formula creation and dry-run belong in
  `pkg/workshop`.
- **Formula interpretation**: the steward assigns work; it does not interpret
  formula graphs.

## Relationship To Wizard And Executor

The clean split is:
- **steward** decides which bead should run and which wizard should take it
- **executor** drives one bead through its lifecycle
- **wizard** performs one subprocess role inside the workspace chosen for it

The steward should not accumulate bead-specific execution logic. If the change
is about review rounds, merge behavior, staging worktrees, or formula steps,
it probably belongs in `pkg/executor`, not here.

## Key entrypoints

| Entry point | Purpose |
|-------------|---------|
| `Cycle` | Run one steward cycle across all configured towers. |
| `TowerCycle` | Run ready-work assignment and health checks for one tower. |
| `CheckBeadHealth` | Detect stale, wedged, or corrupt work and trigger cleanup or alerts. |
| `daemon.go` flows | Run tower-wide background duties like sync, inbox delivery, and dead-agent cleanup. |

## Practical rules

1. **Keep policy tower-level.** This package decides which work should move, not how a bead should execute internally.
2. **Do not duplicate executor state machines.** If a fix requires knowing review-step semantics or formula routing, push that logic down into `pkg/executor`.
3. **Treat steward as capacity and health management.** Summoning, unsummoning, resetting, and replacing workers belong here or just above it.
4. **Use explicit package boundaries.** Assignment decisions belong here; workspace decisions belong in formulas + executor; git mechanics belong in `pkg/git`.
5. **Fail closed on inconsistent work graph state.** If attempt beads or routing state look corrupt, alert instead of assigning aggressively.

## Where new work usually belongs

- Add it to **`pkg/steward`** when the change affects tower-wide assignment, capacity, or health checks.
- Add it to **`pkg/executor`** when the change affects one bead's lifecycle or formula interpretation.
- Add it to **`pkg/wizard`** when the change affects how a summoned subprocess runs.
- Add it to **`pkg/workshop`** when the change affects formula creation, validation, or publishing.
