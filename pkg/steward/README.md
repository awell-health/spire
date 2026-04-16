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
- **Hooked-step sweeping**: queries step beads by `status=hooked`, checks
  whether the blocking condition has resolved, and re-summons the wizard.
- **Cleric auto-summoning**: detects failure evidence on hooked beads and
  summons clerics to recover them (see section below).
- **Concurrency limiter**: per-tower `MaxConcurrent` enforcement — tracks
  active agents and gates spawning via `ConcurrencyLimiter`.
- **Merge queue**: serializes merge operations to prevent git push contention
  (`MergeQueue`).
- **Trust gradient**: per-repo trust levels that gate review requirements and
  auto-merge permissions (`TrustChecker`). Promotes after consecutive clean
  merges, demotes on reverts/failures.
- **Backend-facing coordination**: local process dispatch vs managed backends.

## Cleric auto-summoning

When a wizard fails, the executor files a recovery bead (type `recovery`)
linked to the hooked parent via a `caused-by` dependency. The steward's
hooked-step sweep detects this failure evidence and summons a cleric to
investigate.

### Flow

1. `SweepHookedSteps` queries beads with `status=hooked`.
2. For each hooked parent, `findFailureEvidence` looks for `caused-by`
   dependents of type `recovery`.
3. If the recovery bead is **closed**: check `learning_outcome` metadata.
   - `"escalated"` — leave hooked for human attention.
   - Anything else (success) — unhook steps, set parent to `in_progress`,
     re-summon the wizard.
4. If the recovery bead is **open**: summon a cleric via the claim-then-spawn
   pattern (see below).

### Claim-then-spawn pattern

The steward uses the same atomic claim pattern as `spire claim`:

1. `CreateAttemptBeadAtomic` — atomically creates an attempt bead on the
   recovery bead. If another agent (on any instance) already claimed it,
   this call rejects and the steward skips the summon.
2. `StampAttemptInstance` — stamps instance ownership metadata on the attempt.
3. `UpdateBead` — sets the recovery bead to `in_progress`.
4. `backend.Spawn` — starts the cleric executor process.

The spawned cleric executor calls `ensureGraphAttemptBead` on startup, finds
the attempt bead created by the steward (matching agent name), and reuses it.

This pattern is multi-local safe: two stewards on different machines will
both attempt the atomic claim, but only one succeeds. The loser sees
"active attempt already exists" and skips.

**Do not use registry-based duplicate detection for spawn decisions.** The
agent registry (`wizards.json`) is a local process tracker — it is not an
ownership mechanism. Use attempt beads for ownership; they live in the shared
store and are atomically created.

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
