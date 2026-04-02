# ERRORS.md — Interrupted Work and Recovery Runbook

This runbook covers how Spire surfaces interrupted, failed, and blocked work
on the board, and how to recover using the built-in recovery commands. It is
aimed at operators (the archmage) who need to diagnose and unblock stuck beads.

> **Boundary:** This document describes existing behavior and recovery policy.
> It does not prescribe new runtime semantics. For troubleshooting tips that
> fall outside the recovery commands (e.g. raw `bd update` mutations), see
> `docs/troubleshooting.md`.

---

## State taxonomy

The board distinguishes four kinds of non-normal beads. They look similar at a
glance but have different causes and different recovery paths.

### Interrupted parent bead

A parent work bead whose latest execution hit a terminal failure. The executor
sets both labels:

- `needs-human` — signals the bead requires operator attention.
- `interrupted:<type>` — machine-readable failure classification.

The board renders these in the **INTERRUPTED** section. They do **not** appear
in READY.

**How to identify:** The bead has `needs-human` AND at least one
`interrupted:*` label. It is a work bead (task, feature, epic), not an alert.

**Recovery target:** Always operate on **this bead** — not on associated alert
beads.

### Alert bead

A separate bead created automatically by the executor's escalation logic
(`EscalateHumanFailure`, `EscalateEmptyImplement`). It carries an
`alert:<type>` label and is linked to the parent bead via a `caused-by`
dependency.

The board renders these in the **ALERTS** section (top of the board).

**How to identify:** The bead has an `alert:*` label. Its title starts with
`[<type>]`. It has a `caused-by` dep pointing to the interrupted parent.

**Not the recovery target.** Alert beads are informational artifacts. Recovery
commands (`resummon`, `dismiss`) auto-close them. Do not manually close or
reopen alert beads — operate on the parent instead.

### Blocked bead

A bead with open (unclosed) dependencies. It appears in the **BLOCKED** section
and is excluded from `bd ready` output.

**How to identify:** `bd show <id>` lists open blockers.

**This is structural, not an error.** Unblock it by closing the blocking bead.
Once all blockers are closed, the bead moves to READY and can be summoned.

### Design approval gate

A bead with `needs-human` but **no** `interrupted:*` label. The executor is
waiting for the archmage to close a linked design bead before the plan phase
proceeds. This is intentional gating — the system is working as designed.

**How to identify:** `needs-human` label present, no `interrupted:*` labels.
The bead typically has a `discovered-from` dependency on an open design bead.

**Recovery:** Close the linked design bead (not the parent work bead). Then
either `resummon` the parent or let the steward pick it up on the next cycle.

---

## Escalation types

Each `interrupted:<type>` label corresponds to a specific failure class. The
same type appears on the alert bead as `alert:<type>`.

| Type | Cause | Typical fix |
|------|-------|-------------|
| `merge-failure` | Fast-forward merge to base branch failed — diverged history, branch protection rules, or auth errors. | Rebase the feature branch manually, fix branch protection settings, or check credentials. Then `resummon`. |
| `build-failure` | Terminal build or test failure that the review-fix loop could not resolve after all retries. | Read the build logs, fix the root cause in the code or test environment. Then `resummon` or `reset --to implement`. |
| `repo-resolution` | Could not resolve the repo from the bead's prefix — missing registration in `spire repo list`, bad entry in the repos table. | Run `spire repo add` or fix the repos table. Then `resummon`. |
| `arbiter-failure` | The arbiter (Opus tie-break) could not resolve a sage/apprentice dispute during review. | Read the review comments, make a judgment call. Update the bead description or add a comment with guidance. Then `resummon`. |
| `review-fix-merge-conflict` | Merge conflict when integrating a review-fix back into the staging branch. | Resolve the conflict in the staging worktree. Then `resummon` or `reset --to review`. |
| `empty-implement` | Apprentice completed the implement phase with zero code changes. The bead likely needs a better description or more context. | Improve the bead description, add implementation hints as comments, or attach reference files. Then `resummon`. |
| `invalid-plan-cycle` | Plan validation failed or the implement phase detected a broken plan cycle (e.g. plan output references non-existent targets, circular deps). | Review the plan output, close invalid subtask children if needed. Then `reset --to plan` to re-plan. |

---

## Recovery commands

### `spire summon <N> [--targets <id,...>]`

Spawns fresh wizard processes to work on beads.

**Preconditions:**
- Target beads must be ready: open status, no open blockers, no live wizard.
- Count `N` specifies how many wizards to spawn (or use `--targets` for specific beads).

**What it mutates:**
- Creates wizard registry entries (PID, bead ID, worktree path).
- Spawns wizard processes that begin formula execution.
- If `--dispatch <mode>` is provided, sets `dispatch:<mode>` label on the bead.

**What it does NOT do:**
- Does not clear labels, executor state, or alert beads.
- Does not kill existing wizards.

**When to use:**
- First-time execution of a bead.
- After manually resetting bead state (rare — prefer `reset` or `resummon`).

---

### `spire resummon <bead-id>`

The standard recovery command for interrupted beads. Cleans up the failed
attempt and re-summons a fresh wizard.

**Preconditions:**
- Bead must have the `needs-human` label. If the label was manually removed,
  `resummon` will refuse — use `reset` instead.

**What it mutates:**
1. Kills the old wizard process (SIGTERM with grace period, then SIGKILL).
2. Removes the wizard registry entry.
3. Deletes the executor `state.json`.
4. Strips `needs-human` and all `interrupted:*` labels.
5. Closes all open alert beads linked via `caused-by` or `related` deps.
6. Calls `summon 1 --targets <bead-id>` to start fresh.

**When to use:**
- Bead is interrupted and you've fixed the root cause.
- You've improved the bead description or added context.
- An external blocker (credentials, branch protection) has been resolved.

---

### `spire dismiss [<N>|--all|--targets <id,...>]`

Stops wizard(s) without re-summoning. Use this to free capacity or stop work
you don't want to retry yet.

**Preconditions:**
- At least one wizard must be running (or use `--all`).

**What it mutates:**
1. Sends SIGINT to wizard process(es) for graceful shutdown.
2. Removes wizard registry entries.
3. Closes related alert beads (via `caused-by`/`related` deps).
4. Strips `phase:*` labels.
5. If the bead was `in_progress`, sets status back to `open`. Beads already
   `closed` stay closed.

**What it does NOT do:**
- Does not delete executor `state.json` — a subsequent `summon` may resume
  from stale executor state.
- Does not strip `needs-human` or `interrupted:*` labels.

**When to use:**
- You want to stop a wizard but aren't ready to retry.
- Freeing capacity for higher-priority work.

**Caution:** If you want a clean start after dismissing, use `reset` instead.
A `summon` after `dismiss` may pick up stale executor state.

---

### `spire reset <bead-id> [--to <phase>]`

Rewinds a bead's execution to a specific phase and re-summons. This is a
deeper reset than `resummon` — it can rewind to any phase in the formula.

**Preconditions:**
- `--to <phase>` must name a valid, enabled phase in the formula. If omitted,
  defaults to the first enabled phase (effectively a full restart).

**What it mutates:**
1. Kills the wizard process (SIGTERM with grace period, then SIGKILL).
2. Removes the wizard registry entry.
3. Deletes the executor `state.json`.
4. Strips `needs-human` and all `interrupted:*` labels.
5. Closes open subtask children (plan output beads) — because a phase rewind
   invalidates the plan. Step beads are rewound, not closed.
6. Rewinds step beads:
   - Target phase step -> `in_progress`
   - Steps after target phase -> `open`
   - Steps before target phase -> left closed
7. Sets parent bead status to `in_progress`.
8. Re-summons a fresh wizard.

**When to use:**
- You need to retry from a specific phase (e.g. `--to implement` after fixing
  a build issue, or `--to plan` to re-plan).
- The bead is in an inconsistent label state where `resummon` refuses.
- You want a clean start without destroying worktrees and branches.

---

### `spire reset <bead-id> --hard`

Destructive full reset. Everything `reset` does, plus git cleanup.

> **Last-resort recovery only. Requires explicit human approval.**

**What it mutates (in addition to `reset`):**
1. Removes worktree directories — both `/tmp/spire-wizard/...` and in-repo
   `.worktrees/<bead-id>`.
2. Force-deletes all matching git branches:
   - `epic/<bead-id>`
   - `feat/<bead-id>`
   - `feat/<bead-id>.*`

**When to use:**
- Unrecoverable worktree corruption.
- Conflicted branches that cannot be resolved.
- Starting completely from scratch with no prior state.

**When NOT to use:**
- If `reset --to <phase>` (without `--hard`) would suffice.
- As a first response to any failure — try `resummon` or `reset` first.

---

## Mutation reference table

What each command touches at a glance:

| Artifact | summon | resummon | dismiss | reset | reset --hard |
|----------|--------|----------|---------|-------|--------------|
| Wizard process | spawns new | kills + respawns | kills | kills + respawns | kills + respawns |
| Registry entry | adds | removes + adds | removes | removes + adds | removes + adds |
| Executor state.json | — | deletes | — | deletes | deletes |
| `needs-human` label | — | strips | — | strips | strips |
| `interrupted:*` labels | — | strips | — | strips | strips |
| `phase:*` labels | — | — | strips | — | — |
| Alert beads (caused-by) | — | closes | closes | — | — |
| Subtask children | — | — | — | closes | closes |
| Step beads | — | — | — | rewinds | rewinds |
| Parent bead status | — | (via summon) | -> open | -> in_progress | -> in_progress |
| Worktree dirs | — | — | — | — | removes |
| Git branches | — | — | — | — | force-deletes |

---

## Decision tree: which command to use

Use this flowchart to pick the right recovery command:

1. **Bead is interrupted** (`needs-human` + `interrupted:*`)?
   - Have you fixed the root cause? -> **`resummon`**
   - Need to retry from an earlier phase? -> **`reset --to <phase>`**
   - Worktree or branches are corrupted? -> **`reset --hard`** (get human approval first)

2. **Bead is blocked** (open dependencies)?
   - Close the blocking bead. The bead becomes ready. Then **`summon`**.

3. **Design approval gate** (`needs-human`, no `interrupted:*`)?
   - Close the linked design bead. Then **`resummon`** or let the steward pick it up.

4. **Just want to stop a wizard** (no retry needed)?
   - **`dismiss`**. Use `reset` later if you want a clean start.

5. **Bead in inconsistent state** (`needs-human` removed manually, stale executor state)?
   - **`reset`** — it works without preconditions and clears everything.

---

## Destructive recovery policy

`reset --hard` is the only command that touches git branches and worktree
directories. It is destructive and irreversible.

**Rules:**
- Never script or automate `reset --hard`. Always run manually with human
  judgment.
- Before using it, check whether `reset --to <phase>` (without `--hard`) would
  fix the problem. Most failures don't require destroying branches.
- After using it, the bead re-summons fresh with no prior state, branches, or
  worktrees — as if it were being executed for the first time.

---

## Common playbooks

### Merge failure

The wizard couldn't fast-forward merge to the base branch.

1. Check `spire board` or `bd show <id>` — look for `interrupted:merge-failure`.
2. Read the alert bead or bead comments for the specific error.
3. Common causes: diverged base branch, branch protection rules, expired credentials.
4. Fix: rebase the feature branch if needed, or fix auth/protection settings.
5. Run `spire resummon <id>`.

### Build failure

Terminal build or test failure after all review-fix retries.

1. Check `interrupted:build-failure` on the parent bead.
2. Read the wizard logs (`spire logs <wizard-name>`) and the alert bead for details.
3. Fix the underlying build issue — this might mean updating deps, fixing flaky tests, or adjusting the bead description.
4. Run `spire resummon <id>` to retry, or `spire reset <id> --to implement` to start the implement phase fresh.

### Empty implement

The apprentice produced no code changes.

1. Check `interrupted:empty-implement`.
2. The bead description is likely too vague or the task scope is unclear.
3. Add implementation hints, acceptance criteria, or reference files as bead comments.
4. Run `spire resummon <id>`.

### Invalid plan cycle

Plan validation failed or the implement phase hit a broken plan cycle.

1. Check `interrupted:invalid-plan-cycle`.
2. Review the plan output — subtask children may reference non-existent targets or have circular deps.
3. Run `spire reset <id> --to plan` to re-plan from scratch.

### Arbiter failure

The Opus arbiter couldn't resolve a sage/apprentice review dispute.

1. Check `interrupted:arbiter-failure`.
2. Read the review comments to understand the disagreement.
3. Make a judgment call: add a comment on the bead with explicit guidance.
4. Run `spire resummon <id>`.

### Corrupted worktree or branches

The git state is beyond repair (orphaned worktrees, conflicted branches that
can't be resolved).

1. Confirm the corruption: try `git worktree list`, check branch state.
2. If `reset --to <phase>` could work, try that first.
3. If truly unrecoverable: get human approval, then `spire reset <id> --hard`.

### Dead wizard / orphaned state

A wizard process died without cleaning up (crash, OOM, machine reboot).

1. Run `spire status` — look for wizards with stale PIDs.
2. Run `spire dismiss --targets <id>` to clean up the registry.
3. If the bead should be retried: `spire reset <id>` for a clean start, or
   `spire summon 1 --targets <id>` to resume from existing executor state.

---

## Gotchas

- **`resummon` requires `needs-human`**: If the label was manually stripped,
  `resummon` refuses. Use `reset` for beads in inconsistent label states.

- **`dismiss` leaves executor state**: A subsequent `summon` on a dismissed
  bead may resume from stale `state.json`. Use `reset` if you want a clean
  start.

- **`reset` closes subtask children**: A phase rewind invalidates the plan, so
  plan-output subtasks are closed. Step beads are rewound (not closed) — they
  track formula progress.

- **Alert beads after `reset`**: `resummon` and `dismiss` auto-close alert
  beads, but `reset` does not. After `reset`, stale alerts may linger on the
  board. They are harmless (the parent is re-summoned), but you may want to
  close them manually.

- **`needs-human` alone vs. `needs-human` + `interrupted:*`**: This distinction
  is load-bearing for board rendering. `needs-human` alone = design approval
  gate (normal board treatment). `needs-human` + `interrupted:*` = error state
  (INTERRUPTED section). Do not add or remove these labels independently.

- **Prefer recovery commands over raw `bd update`**: The troubleshooting FAQ
  covers `bd update --status open` / `bd update --owner ""` as escape hatches.
  For normal recovery, always use `resummon`, `reset`, or `dismiss` — they
  handle the full state cleanup that raw mutations miss.
