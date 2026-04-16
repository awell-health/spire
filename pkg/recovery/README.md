# pkg/recovery

Diagnosis, classification, and action proposal for interrupted parent beads.

**Important distinction:** "Recovery" in this package is the **data model** —
beads, metadata, failure classification, learnings, and action proposals. The
**agent** that performs recovery is the **cleric** (runs `cleric-default`
formula, action handlers live in `pkg/executor/recovery_phase.go`). This
package provides the diagnostic foundation; the cleric executor code drives the
actual recovery lifecycle.

## Boundaries

| Package | Owns |
|---------|------|
| `pkg/recovery` | Diagnosis, failure classification, action proposal, learning storage |
| `pkg/executor` | Cleric action handlers, cooperative retry protocol, git-aware actions |
| `pkg/steward` | Auto-summoning clerics (claim-then-spawn) |
| `pkg/wizard` | Wizard-side retry: `checkRetryRequest`, skip-to-step, result reporting |

## What this package owns

- **Diagnosis** (`Diagnose`): inspects bead state, attempt history, git/worktree
  status, and executor runtime state to classify the failure mode.
- **Failure classification** (`FailureClass`): categorises interruptions into
  `empty-implement`, `merge-failure`, `build-failure`, `review-fix`,
  `repo-resolution`, `arbiter`, `step-failure`, `unknown`.
- **Action proposal**: ranks recovery actions by likelihood of success, based on
  failure class and historical outcomes.
- **Verification** (`Verify`, `CheckSourceHealth`): checks whether the
  interrupted state has been cleared after a recovery action.
- **Learning storage** (`DocumentLearning`, `FinishRecovery`): writes learning
  metadata to both bead metadata (fast, local) and the `recovery_learnings` SQL
  table (canonical, queryable).

## Cleric lifecycle

The cleric runs the `cleric-default` formula — a 6-step recovery loop:

```
collect_context ──→ decide ──→ execute ──→ verify ──→ learn ──→ finish
                      │                      │
                      │                      └─ failure: loop back to decide
                      │                         (up to max_retries times)
                      │
                      └─ needs_human=true ──→ finish_needs_human
```

### Steps

| Step | Action | What happens |
|------|--------|--------------|
| `collect_context` | `cleric.collect_context` | Mechanical: calls `Diagnose()`, gathers per-bead and cross-bead learnings, reads wizard log tail, builds `FullRecoveryContext` |
| `decide` | `cleric.decide` | Claude-driven: chooses recovery action from ranked list. Checks human guidance, git state heuristics, and historical outcomes. Sets `needs_human=true` if confidence < 0.7 or repeated failures |
| `execute` | `cleric.execute` | Dispatches to git-aware action registry (`RunRecoveryAction`) or legacy actions. Provisions worktree if needed |
| `verify` | `cleric.verify` | Cooperative retry: sets `RetryRequest` on source bead, polls for wizard result (30s intervals, 10min timeout). Loops to decide on failure |
| `learn` | `cleric.learn` | Claude-driven: extracts learning summary, resolution kind, reusability. Writes to bead metadata + SQL table |
| `finish` | `cleric.execute` | Clears retry labels, writes closing comment, closes recovery bead (or leaves open if `needs_human`) |

### Decision priority

1. **Human guidance** (comments with keywords like "rebase", "rebuild", "escalate")
2. **Git-state heuristics** (diverged → rebase, dirty → rebuild)
3. **Claude selection** (with fallback to resummon if Claude unavailable)
4. Actions with 2+ prior failures are excluded; auto-escalates if all are exhausted

## Cooperative retry protocol

The verify step implements a label-based handoff between cleric and wizard:

```
CLERIC                                    WIZARD
  │                                         │
  ├─ SetRetryRequest(target, {              │
  │    FromStep: "build-gate",              │
  │    Attempt: 2,                          │
  │    RecoveryBead: "spi-rc-456"           │
  │  })                                     │
  │  → labels: recovery:retry-from=...      │
  │            recovery:status=waiting       │
  │                                         │
  ├─ Poll every 30s ─────────────────────── │
  │                                         ├─ checkRetryRequest()
  │                                         ├─ Skip phases before FromStep
  │                                         ├─ Run from requested step
  │                                         │
  │                                         ├─ Success:
  │                                         │  SetRetryResult({Success: true})
  │                                         │  → recovery:status=succeeded
  │                                         │
  │                                         └─ Failure:
  │                                            SetRetryResult({Success: false})
  │                                            → recovery:status=failed
  │                                         │
  ├─ Read result                            │
  ├─ Success → learn → finish               │
  └─ Failure → rebuild context → decide     │
```

Labels live on the **target bead** (the one being retried). Graph step names
are mapped to wizard phases via `MapToWizardPhase` (e.g., `verify-build` →
`build-gate`).

## Learning system

Two storage tiers, both written by the `learn` step:

| Tier | Store | Used for |
|------|-------|----------|
| Bead metadata | `store.SetBeadMetadataMap()` | Fast local access, bead-scoped queries |
| `recovery_learnings` SQL | `store.WriteRecoveryLearningAuto()` | Cross-bead aggregation, historical analytics |

The `decide` step queries both tiers to inform future decisions:

- **Per-bead**: past recoveries for the same source bead + failure class
- **Cross-bead**: past recoveries for the same failure class from any source

Learnings marked `reusable=true` are candidates for future recovery of the
same failure class.

## Git-aware actions

Registered in `pkg/executor/recovery_actions.go`:

| Action | What it does | Max retries |
|--------|-------------|-------------|
| `rebase-onto-main` | Fetch origin/main, rebase onto it | 3 |
| `cherry-pick` | Cherry-pick a specified commit | 3 |
| `resolve-conflicts` | Resolve merge conflicts (theirs/ours) | 2 |
| `targeted-fix` | Record apprentice fix request | 3 |
| `rebuild` | Run `go build ./...` | 3 |
| `resummon` | Mark for wizard re-summon | 3 |
| `reset-to-step` | Reset executor to a named step | 2 |
| `escalate` | Set P0 + needs-human label | 1 |

Actions that require a worktree get one via `ProvisionRecoveryWorktree`
(prefers resuming the wizard's staging worktree, falls back to ephemeral
`recovery/<beadID>` branch).

## Steward integration

The steward auto-summons clerics for hooked beads with failure evidence. It
uses the atomic claim pattern (see `pkg/steward/README.md`) — not the agent
registry — to prevent double-summon across instances.

The steward also observes cleric outcomes:
- Recovery bead **closed** with `learning_outcome=clean` → unhook parent, re-summon wizard
- Recovery bead **closed** with `learning_outcome=escalated` → leave hooked for human
- Recovery bead **open** → summon cleric (if not already claimed)

## Key types

| Type | Purpose |
|------|---------|
| `Diagnosis` | Full diagnostic report for an interrupted bead |
| `FailureClass` | Categorisation of the interruption reason |
| `RecoveryActionKind` | Action type enum (reset, resummon, escalate, etc.) |
| `RecoveryActionRequest` / `RecoveryActionResult` | Mechanical action dispatch |
| `RecoveryMetadata` | Typed projection of recovery-specific metadata |
| `VerifyResult` | Post-recovery check that interrupted state is cleared |
| `Deps` | Dependency injection struct for testability |

## Constants

| Constant | Value | Purpose |
|----------|-------|---------|
| `DefaultMaxRecoveryAttempts` | 3 | Retry budget before auto-escalation |
| `DefaultVerifyPollInterval` | 30s | Cooperative verify poll interval |
| `DefaultVerifyTimeout` | 10min | Max wait for wizard retry result |

## Practical rules

1. **This package diagnoses; the executor acts.** Don't put action execution
   logic here — route it through the executor's action registry.
2. **Failure classes are the routing key.** Learnings, action ranking, and
   cross-bead aggregation all key on failure class. Get classification right.
3. **Learnings go to both tiers.** Always write to bead metadata AND the SQL
   table. The SQL table is canonical; metadata is for fast access.
4. **Don't bypass the retry protocol.** The cleric never directly re-runs
   wizard steps — it sets a RetryRequest and waits for the wizard to report
   back. This keeps the wizard simple and label-driven.
