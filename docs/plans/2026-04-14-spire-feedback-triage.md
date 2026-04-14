# Spire Feedback Triage: Interrupted Epic Recovery and Archmage UX

**Date:** 2026-04-14
**Status:** Investigation handoff
**Purpose:** Give the next agent a complete starting point for the colleague feedback review without requiring them to rediscover the same seams.

> This document is triage only. It does **not** propose filing issues yet.
> The archmage explicitly wants discussion before work is filed.

---

## Executive Summary

The feedback does not look like thirteen unrelated bugs.

It clusters around one larger product seam:

- Spire has multiple internal representations of "interrupted work" and recovery state:
  interrupted labels, `needs-human`, graph state, recovery beads, alert beads,
  wizard registry entries, and git/worktree state.
- Those representations are only partially projected into the archmage-facing
  surfaces:
  board, `focus`, escalation comments, `resolve`, `resummon`, and steward
  re-entry.
- As a result, the system often has enough state to continue, but the human
  operator cannot tell what happened, what is safe, or what command actually
  preserves progress.

The most important framing for follow-up work is:

1. This is primarily an **interrupted-work contract** problem.
2. The user-facing failures span **board projection**, **recovery command UX**,
   **epic child outcome handling**, and **git/worktree hygiene**.
3. At least two reports are likely consequences of the same root cause:
   the system does not have one authoritative, archmage-facing model of
   interruption, diagnosis, and re-entry.

---

## Raw Feedback Inventory

### Reported product issues

| # | Feedback | Initial status | Likely seam |
|---|----------|----------------|-------------|
| 1 | Epic disappears from board when interrupted | Open | Board/recovery projection |
| 2 | Related beads filtered out of epic board view | Reported fixed in-session; still needs regression coverage | Board/recovery projection |
| 3 | Escalation comment gives no diagnostic info | Reported fixed in-session; still needs regression coverage | Recovery UX |
| 4 | `resummon` nukes graph state with no warning | Open | Recovery command contract |
| 5 | `resolve` does not actually re-summon | Open | Recovery command contract |
| 6 | Stale branch checkout blocks wizard silently | Open | Git/worktree summon hygiene |
| 7 | Apprentice test failures do not prevent merge to staging | Open | Epic child outcome pipeline |
| 8 | No early exit on child failure | Open | Epic child outcome pipeline |

### Reported expectation gaps

| # | Expectation gap | Initial status | Likely seam |
|---|------------------|----------------|-------------|
| 1 | What to do with an interrupted epic | Open | Recovery command contract + docs/help surface |
| 2 | What the recovery bead is for | Open | Recovery projection + docs/help surface |
| 3 | How to get a wizard to fix a problem it created | Open | Recovery command contract + epic flow docs |
| 4 | Where diagnosis should go | Open | Epic child outcome pipeline + docs/help surface |
| 5 | When manual intervention is expected vs automatic | Open | Recovery command contract + steward UX |

---

## What Appears To Already Be Fixed

These should be treated as "fixed in principle, not yet trusted without
coverage":

- **Epic board scoping for linked beads** was reported fixed in the manual
  session. The likely touchpoint is `pkg/board/categorize.go` in
  `FilterEpic`, which now scans reverse dependencies instead of only children
  and `discovered-from`.
- **Escalation comments now mention logs** was also reported fixed in the
  manual session. The remaining risk is that the fix may cover only one
  escalation path and not every archmage-facing interruption surface.

Recommended follow-up even if both are already landed:

- add regression tests for epic board scoping of `caused-by` and recovery beads
- add regression tests for escalation comments or focus output including the
  log discovery path

---

## Suspected Root Seams

### 1. Board and recovery projection are inconsistent

The board deliberately moves interrupted work out of its phase column and into
`Interrupted`:

- `pkg/board/categorize.go`
- `pkg/board/render.go`

In the default board view, that interrupted state is then summarized as a small
attention line rather than remaining visible in the main phase column area.
That directly matches "the epic vanished unless you know to press `v`."

There is a second projection problem around linked recovery artifacts:

- `pkg/board/categorize.go` recognizes reverse dependencies for epic scoping
- `cmd/spire/focus.go` still looks specifically for `recovery-for`
- `pkg/recovery/diagnose.go` still prefers `recovery-for` for recovery bead
  discovery
- `pkg/recovery/bead_ops.go` already knows both `caused-by` and `recovery-for`

That points to an incomplete migration from legacy `recovery-for` links to the
newer `caused-by` model. Different surfaces are reading different truths.

**Why this matters**

- The board can hide the source bead.
- `focus` can hide the recovery bead.
- diagnosis and cleanup code can disagree about whether a linked artifact exists.

This seam likely explains:

- issue 1: interrupted epic disappears
- issue 2: related beads filtered out of epic board view
- expectation gap 2: unclear what the recovery bead is for

### 2. Recovery commands do not expose a coherent contract

The current command surfaces imply a simple mental model, but the code does
something more complicated.

`resummon` currently does all of the following:

- kills the old wizard process if alive
- removes wizard registry state
- deletes executor state files and graph state files
- strips `needs-human`
- strips `interrupted:*`
- strips `dispatch:*`
- closes linked alerts
- closes linked recovery beads
- immediately runs `summon`

See:

- `cmd/spire/resummon.go`

Despite that behavior, recovery classification still describes `resummon` as:

- "Clear interrupted state and re-summon wizard"
- `Destructive: false`

See:

- `pkg/recovery/classify.go`

`resolve` has the inverse problem. It records recovery learning and unblocks the
source bead, but it does not actually summon anything. Instead it resets hooked
graph steps if it can find graph state, and its user messaging still implies the
steward will re-summon:

- `cmd/spire/resolve.go`

`focus` also lists `reset`, `reset --hard`, and `resummon` but does not explain
when each preserves progress, when it discards graph state, or when the user
must summon manually:

- `cmd/spire/focus.go`

**Why this matters**

- The human cannot tell which command preserves state.
- The system message over-promises steward behavior that may not exist in the
  current local session.
- The command descriptions and the underlying behavior disagree.

This seam likely explains:

- issue 4: `resummon` wipes graph state with no warning
- issue 5: `resolve` says steward will re-summon when it will not
- expectation gap 1: what to do with an interrupted epic
- expectation gap 3: how to get a wizard to fix a problem it created
- expectation gap 5: when manual intervention is expected vs automatic

### 3. Epic child execution treats process exit as success even when the child declared failure

Apprentice execution currently records test failures as a distinct result, but
does not fail the process when build passes:

- `pkg/wizard/wizard.go`
  - failed tests add `test-failure`
  - result written as `test_failure`
  - process still completes normally

The generic `wizard.run` action path in the executor is already written to trust
`result.json` over subprocess exit status:

- `pkg/executor/graph_actions.go`

But epic child dispatch does not use that path. `dispatch.children` waits on the
child process and only treats non-zero process failure as an error:

- `pkg/executor/action_dispatch.go`

That means a child can:

- fail tests
- label the bead
- write `result.json` with `test_failure`
- exit cleanly
- still be treated as successful by epic dispatch
- still have its branch merged into staging

The subgraph also always proceeds from `dispatch-children` into build
verification at the epic level:

- `pkg/formula/embedded/formulas/subgraph-implement.formula.toml`

So the epic does not stop at first child failure. It keeps going until a later
integration or verify step fails.

**Why this matters**

- Broken child branches can accumulate in staging.
- The first actionable failure signal is delayed.
- Diagnosis gets harder with each additional wave.
- Writing diagnosis on the epic is not enough because apprentices act on child
  beads, not on epic comments.

This seam likely explains:

- issue 7: test failures do not prevent merge to staging
- issue 8: no early exit on child failure
- expectation gap 4: diagnosis goes where

### 4. Git and worktree hygiene is handled too late

The recovery layer can inspect git state:

- `pkg/recovery/diagnose.go`

Wizard worktree creation tries a new branch, then falls back to checking out an
existing branch:

- `pkg/wizard/wizard.go`
- `pkg/git/repo.go`

But `resummon` itself does not do a preflight against stale worktrees, branch
checkouts in the main repo, or branch/worktree collisions before immediately
calling `summon`:

- `cmd/spire/resummon.go`

So the first time the user learns the git state is bad may be after the wizard
has already failed again.

This seam likely explains:

- issue 6: stale branch checkout blocks wizard silently

### 5. The archmage-facing recovery story is not documented in-product

There is no single surface that answers all of the following clearly:

- what failed
- where the logs are
- whether the wizard is still running
- whether the recovery bead is actionable or informational
- whether `resolve`, `resummon`, or `reset` preserves progress
- whether the steward will re-enter automatically
- where diagnosis should be written so the next worker will actually consume it

This is partly a docs problem, but more importantly it is a product-surface
problem:

- `focus` is closest, but still incomplete
- board affordances surface commands, not the operational model
- recovery beads duplicate information without stating their purpose

This seam likely explains most of the expectation-gap section even where the
underlying code technically "works."

---

## Symptom-To-Seam Mapping

### Issue 1: Epic disappears from board when interrupted

Primary seam:

- board/recovery projection

Current hypothesis:

- interrupted work is intentionally removed from phase columns
- default board view compresses that into a count-only attention line

Main files:

- `pkg/board/categorize.go`
- `pkg/board/render.go`

### Issue 2: Related beads filtered out of epic board view

Primary seam:

- board/recovery projection

Current hypothesis:

- some epic scoping paths were only considering child beads and
  `discovered-from`
- linked recovery/alert beads arrived through dependency edges instead

Main files:

- `pkg/board/categorize.go`
- `cmd/spire/focus.go`

### Issue 3: Escalation comment gives no diagnostic info

Primary seam:

- recovery UX

Current hypothesis:

- escalation surfaces assumed the user would discover logs independently
- fix may already be partially landed, but coverage should confirm the command
  actually appears in the most important interruption flows

Main files:

- executor escalation paths
- `cmd/spire/focus.go`
- `cmd/spire/logs.go`

### Issue 4: `resummon` nukes graph state with no warning

Primary seam:

- recovery command contract

Current hypothesis:

- the command grew from "retry" into "destructive reset plus summon" without
  the surface contract being updated

Main files:

- `cmd/spire/resummon.go`
- `pkg/recovery/classify.go`

### Issue 5: `resolve` does not re-summon

Primary seam:

- recovery command contract

Current hypothesis:

- command messaging still assumes steward-managed re-entry
- local/no-steward mode leaves the user holding the next step manually

Main files:

- `cmd/spire/resolve.go`
- `pkg/steward/steward.go`

### Issue 6: Stale branch checkout blocks wizard silently

Primary seam:

- git/worktree summon hygiene

Current hypothesis:

- preflight happens too late or not at all
- recoverable git/worktree state is discovered only after summon fails

Main files:

- `cmd/spire/resummon.go`
- `pkg/wizard/wizard.go`
- `pkg/git/repo.go`

### Issue 7: Apprentice test failures do not prevent merge to staging

Primary seam:

- epic child outcome pipeline

Current hypothesis:

- child dispatch ignores `result.json` and treats clean process exit as success
- test failure is modeled as data, but dispatch is still using process exit as
  the merge gate

Main files:

- `pkg/wizard/wizard.go`
- `pkg/executor/action_dispatch.go`
- `pkg/executor/graph_actions.go`

### Issue 8: No early exit on child failure

Primary seam:

- epic child outcome pipeline

Current hypothesis:

- epic implement subgraph only escalates after later verify steps
- no per-wave stop condition exists when a child already failed in a way the
  system knows about

Main files:

- `pkg/executor/action_dispatch.go`
- `pkg/formula/embedded/formulas/subgraph-implement.formula.toml`

---

## Recommended Workstreams

These are the workstreams I would hand to separate agents if we choose to
delegate later.

### Workstream A: Interrupted board and recovery projection

Scope:

- `pkg/board/*`
- `cmd/spire/focus.go`
- recovery link discovery helpers

Questions to answer:

- Should interrupted epics remain in their phase column with a visible failure
  badge, instead of moving to a separate section?
- Should the default board view expand interrupted beads by default?
- Should `focus` and board share one dependency-link helper for recovery and
  alert discovery?
- Is `recovery-for` still supported intentionally, or should all archmage-facing
  reads normalize onto `caused-by`?

Expected outputs:

- a clear display contract for interrupted work
- regression tests for epic scoping of alerts and recovery beads

### Workstream B: Recovery command contract and archmage UX

Scope:

- `cmd/spire/resummon.go`
- `cmd/spire/resolve.go`
- `cmd/spire/recover.go`
- `cmd/spire/focus.go`
- `pkg/recovery/*`

Questions to answer:

- Is `resummon` supposed to be destructive?
- If yes, should it prompt or be renamed?
- If no, should the current destructive behavior move to another command?
- In local mode without a steward, should `resolve` auto-summon?
- What is the canonical archmage workflow for "wizard failed, I diagnosed it,
  now continue"?

Expected outputs:

- one coherent recovery flow
- user-facing command descriptions that match reality
- in-product guidance on recovery bead purpose and next actions

### Workstream C: Epic child outcome and early-exit semantics

Scope:

- `pkg/executor/action_dispatch.go`
- `pkg/executor/graph_actions.go`
- `pkg/wizard/wizard.go`
- `pkg/formula/embedded/formulas/subgraph-implement.formula.toml`

Questions to answer:

- Should child dispatch trust `result.json` the same way `wizard.run` does?
- Which child outcomes should block merge into staging?
- Should the epic stop after first child failure, after first wave failure, or
  only after a configurable threshold?
- Where should archmage diagnosis live so the next worker actually consumes it?

Expected outputs:

- a single mechanical success/failure contract for child agents
- tests covering `test_failure` and early-exit behavior

### Workstream D: Git/worktree preflight for resummon and summon

Scope:

- `cmd/spire/resummon.go`
- `pkg/wizard/wizard.go`
- `pkg/git/*`

Questions to answer:

- What git/worktree states are recoverable automatically?
- What should be cleaned or surfaced before `summon` runs?
- Should `resummon` run a dry preflight and fail with actionable instructions
  before clearing graph state?

Expected outputs:

- deterministic preflight behavior
- explicit user messaging when manual git cleanup is required

### Workstream E: Archmage-facing documentation and in-product affordances

Scope:

- board hints
- `focus` output
- recovery bead comments/descriptions
- CLI help text

Questions to answer:

- What minimal explanation must the product give at interruption time?
- What recovery bead text is redundant vs actually useful?
- How do we make the manual-vs-automatic mode obvious?

Expected outputs:

- shorter, clearer interruption guidance
- no requirement to read source to choose the next command

---

## Suggested Verification Matrix

Any serious fix set should include scenario-level coverage for the following:

1. Interrupt an epic in `implement`.
   The epic remains plainly visible in the default board view.

2. Scope the board to that epic.
   Linked alert and recovery beads remain visible even if they are not children.

3. Trigger an interruption with logs available.
   The archmage-facing surface points to the log command directly.

4. Run `spire resummon` on an interrupted epic.
   The user is told exactly what state will be discarded or preserved.

5. Run `spire resolve` with no steward active.
   The user is not told the steward will do something that will not happen.

6. Leave the feature branch checked out in the main repo and then retry.
   The system surfaces a concrete preflight error or auto-cleans the safe case.

7. Make an apprentice return `test_failure` with exit code 0.
   The child is not merged into staging as a success.

8. Make one child fail in wave 1 of an epic.
   The epic stops at the intended boundary and reports the failure immediately.

9. Add human diagnosis to the intended location.
   The next agent actually receives that context without relying on the epic
   comment thread alone.

---

## Open Product Decisions

These should be answered before or during implementation because the code can be
made "consistent" in more than one direction:

1. Should interrupted work move out of its phase column at all?
2. Is a recovery bead meant to be an internal tracking artifact or a first-class
   operator task?
3. Is `resummon` a retry, a destructive reset, or both?
4. In local mode, should recovery commands behave differently when no steward is
   running?
5. Should epic child failure stop the current wave, the whole implement graph,
   or only later review/verify stages?
6. Where is the canonical place for actionable human diagnosis:
   source bead comment, recovery bead comment, or a dedicated child task?

---

## Immediate Recommendation

If we delegate this later, do **not** split the work by the numbered feedback
items. Split it by seams:

1. interrupted board/recovery projection
2. recovery command contract
3. epic child outcome pipeline
4. git/worktree preflight
5. archmage-facing docs and affordances

That decomposition lines up with the code boundaries better than the symptom
list and gives us a real chance of finding one root cause behind multiple
reported failures.
