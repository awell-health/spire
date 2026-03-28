# SPIRE.md — Agent Work Instructions

This repo is connected to Spire (prefix: **spi**). Use Spire for work
coordination. This document is for agents working beads — read it before
starting any work.

## Roles

| Role | What | You are this if... |
|------|------|--------------------|
| **Archmage** | Human. Writes specs, reviews, steers. | You're the user |
| **Wizard** | Per-bead orchestrator. Drives the formula lifecycle. | You were spawned by `spire summon` |
| **Apprentice** | Implementer. Writes code in isolated worktree. | You were dispatched by a wizard |
| **Sage** | Reviewer. Returns verdict (approve / request changes). | You were dispatched for review |

## Session lifecycle

```bash
spire up                        # ensure services are running
spire collect                   # check inbox
spire claim <bead-id>           # claim work (atomic: verify → set in_progress)
spire focus <bead-id>           # assemble full context
# ... do the work ...
spire send <agent> "done" --ref <bead-id>   # notify others
```

## Formulas drive the lifecycle

Every bead type maps to a formula that defines the phase pipeline:

| Bead type | Formula | Phases |
|-----------|---------|--------|
| epic | `spire-epic` | design → plan → implement (waves) → review → merge |
| bug | `spire-bugfix` | implement → review → merge |
| task, feature, chore | `spire-agent-work` | implement → review → merge |

Formulas are TOML files. Layered resolution (first match wins):
1. Label `formula:<name>` on the bead
2. Bead type mapping (table above)
3. `.beads/formulas/<name>.formula.toml` (tower-level override)
4. Embedded formulas (built into binary)

## Design beads

Design beads capture exploration and decisions BEFORE filing work items.
They are not work — they are thinking artifacts.

```bash
# Create a design bead
spire design "Auth system overhaul"          # → spi-xxx

# Brainstorm — add comments as you explore
bd comments add spi-xxx "Approach A: ..."
bd comments add spi-xxx "Rejected because ..."

# When the design is settled, close it
bd update spi-xxx -s closed

# File the work item and link it
spire file "Auth overhaul" -t epic -p 1
bd dep add <epic-id> spi-xxx --type discovered-from
```

The wizard validates that epics have at least one `discovered-from` dep
pointing to a closed design bead before entering the plan phase. If
missing, the epic is labeled `needs-design` and blocked.

**Important:** Use `discovered-from` deps for design linkage, not `ref:`
labels. `ref:` labels are for message routing only.

## Dependencies

```bash
# Blocking: B blocks A (A can't start until B closes)
bd dep add <task-a> <task-b> --type blocks

# Parent-child: subtask under epic
bd create "Subtask" -t task --parent <epic-id>

# Cross-reference: design bead → epic
bd dep add <epic-id> <design-id> --type discovered-from

# Related: non-blocking association
bd dep add <bead-a> <bead-b> --type related

# Check what's ready (no open blockers)
bd ready --json
```

Available dependency types: `blocks`, `related`, `parent-child`,
`discovered-from`, `caused-by`, `validates`, `supersedes`.

## Filing work

```bash
spire file "Title" -t task -p 2             # prefix auto-detected from repo
spire file "Title" -t epic -p 1             # epic (auto-syncs to Linear)
spire file "Title" -t bug -p 0              # P0 bug
spire design "Title"                        # design bead (not a work item)
```

**Always set priority** (`-p 0` critical → `-p 4` nice-to-have) and
**always set type** (`-t task|bug|feature|epic|chore`).

## The review DAG

Every bead goes through review after implementation. The review has one
invariant: **every path ends with the branch either merged to main or
deleted. No hanging branches.**

```
sage: approve     → merge staging → main, delete branch, close bead
sage: reject ×3   → arbiter decides:
  arbiter: merge  → force-merge → main, delete branch, close bead
  arbiter: split  → merge → main, create child beads, delete branch, close
  arbiter: discard→ delete branch, close bead as wontfix
```

If a terminal step fails (build verification, merge conflict), the bead
is labeled `needs-human` and an alert is sent to the archmage. The bead
stays open with the branch intact so the human can diagnose.

See `docs/review-dag.md` for the full DAG documentation.

## Completing work

When you finish a task, close things in order:

1. **Close molecule steps** — `spire focus <bead-id>` shows your workflow.
   Close each step (design, implement, review, merge) with `bd close <step-id>`
2. **Close the bead** — `bd close <bead-id>`
3. **Push state** — `bd dolt push`
4. **Notify** — `spire send <agent> "done" --ref <bead-id>` if assigned via mail

## Messaging

```bash
spire register <name>                       # register as an agent
spire collect                               # check inbox
spire send <to> "message" --ref <bead-id>   # send a message
spire read <bead-id>                        # mark as read
spire alert "message" --ref <bead-id> -p 1  # priority alert
```

Messages use labels for routing: `to:<agent>`, `from:<agent>`, `ref:<bead-id>`.
The `ref:` label on messages associates a message with its subject bead.

## Monitoring

```bash
spire board              # interactive TUI — navigate with h/j/k/l, actions with s/f/c/L
spire board --json       # machine-readable for agents
spire roster             # work grouped by epic, agent status
spire watch              # live tower status
spire logs <wizard-name> # tail wizard logs
spire metrics            # agent performance summary
```

## Commit format

Always reference the bead:

```
<type>(<bead-id>): <message>
```

Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `test`

## Principles (for working on Spire itself)

These are extracted from real failures. Every principle here prevented
or would have prevented a bug we actually shipped.

### 1. All git operations go through WorktreeContext or RepoContext

**NEVER use `exec.Command("git", ...)` directly.** Two types in `pkg/git/` handle git:

- **`git.WorktreeContext`** (`pkg/git/worktree.go`) — operations INSIDE a worktree:
  commit, diff, merge, status, conflict resolution
- **`git.RepoContext`** (`pkg/git/repo.go`) — operations on the MAIN REPO:
  create/delete branches, create/remove worktrees, ff-only merge, push
- **`git.StagingWorktree`** (`pkg/git/staging.go`) — embeds WorktreeContext for
  merge staging: fetch branch, merge, build/test, merge to main

If you need a git operation that doesn't exist on either type, add a
method to `pkg/git/`. Don't bypass the abstraction.

**Why:** Every worktree bug we hit (git config pollution, checkout in
main repo, origin/main not fetched, stale worktrees) came from raw
exec.Command calls that didn't go through the abstraction.

### 2. The main worktree never leaves the base branch

The main worktree (`/Users/.../spire`) is ALWAYS on `main`. All staging,
merging, building, and reviewing happens in worktrees. The only git
operation on the main worktree is `git merge --ff-only` at the end.

**Why:** Checking out branches in the main repo caused the user's
working directory to switch to `epic/spi-swqje` without warning.

### 3. Every branch and worktree is named after a bead

- Branches: `feat/<bead-id>`, `staging/<bead-id>`
- Worktrees: `.worktrees/<bead-id>`
- No generic names like `temp-main` or `merge-staging`

If you see a branch, you know which bead it belongs to. If you see a
worktree, you know which bead is being worked.

### 4. Only main gets pushed to origin

Apprentice feature branches stay LOCAL. The wizard merges them into the
staging worktree locally. Only the final `main` push goes to origin
after ff-only merge. No intermediate branch pushes.

**Why:** Pushing feature branches to origin wastes time, creates
remote branch pollution, and causes non-fast-forward rejections on retry.

### 5. The DAG is truth

Runtime state lives in the bead graph, not labels or registries:
- **Attempt beads** — who is working (not `owner:` labels)
- **Step beads** — which phase (not `phase:` labels)
- **Review round beads** — what the sage said (not comments)

Labels are projections for display. The graph is authority for decisions.

### 6. Beads close AFTER code lands on main

A bead cannot be closed until its code is on main (or explicitly
discarded). Subtask beads close after their branch is merged into
staging. The epic bead closes after staging is ff-only merged to main.

**Why:** Closing beads before merge leaves orphaned code on branches
that nobody knows about.

### 7. Design before implement

Every epic needs a closed design bead (`discovered-from` dep) before
the wizard enters the plan phase. The design captures exploration,
rejected approaches, and decisions. The epic captures execution.

### 8. One source of defaults

Configuration defaults live in ONE place. `spire.yaml` declares what
the repo wants. The executor reads it. Formulas can override. No
`applyDefaults()` in the parser fighting with fallbacks in the consumer.

**Why:** Three layers (repoconfig, wizard.go, formula.go) all set
`maxTurns` with different defaults. The wrong one won every time.

### 9. Investigate before implementing

When fixing bugs, trace the data flow before writing code:
1. Identify the symptom (what's wrong)
2. Trace the data path (where does the value come from)
3. Find the root cause (which layer is broken)
4. Fix the root cause, not the symptom

**Why:** Roster grouping was broken. The symptom was in roster.go.
The root cause was in store.go — `GetIssue()` wasn't populating deps.
Three wizard attempts failed because they fixed the wrong file.

### 10. Commit before validate, never revert

Apprentices must commit their work BEFORE running tests. If tests fail
on code you didn't write, commit anyway. Partial work committed is
ALWAYS better than no work committed.

**Why:** Apprentices spent all their turns fighting test failures,
reverted their own work, and exited with "nothing staged."

### 11. Tests must not pollute the production database

Apprentice worktrees have `.beads/` removed so Claude's exploratory
commands don't create real beads. Tests use mock functions (`*Func`
vars), not the real store. Integration tests are gated behind
`SPIRE_INTEGRATION=1`.

**Why:** Found 46+ junk beads (Subtask 1, dispatch-test, human-plan)
created by wizard test runs polluting the board.

### 12. File splits must DELETE from the source

When splitting a large file into focused files, DELETE each function
from the source file as you move it. The build must pass after each
move. Never leave duplicates — the compiler will catch them but the
agent won't.

**Why:** Every file split attempt (executor.go, store.go, board.go)
failed because the agent created new files but didn't delete from the
original. The same bug happened 4 times.

### 13. Packages enforce boundaries, not file names

Every distinct concern lives in its own Go package under `pkg/`. The
compiler enforces that packages don't reach into each other's internals.
File-level separation (executor_design.go vs executor_plan.go) is
organization. Package-level separation (pkg/executor vs pkg/store) is
architecture. Both matter but only packages are enforced.

Package structure (landed in v0.22.0):

| Package | What | Depends on |
|---------|------|-----------|
| `pkg/store` | Bead persistence: types, queries, mutations, bead subtypes | beads library |
| `pkg/config` | Tower identity, repo instances, credentials, keychain, identity detection | stdlib only |
| `pkg/formula` | Formula TOML parsing, phase pipeline, layered resolution | toml parser, embedded |
| `pkg/git` | RepoContext, WorktreeContext, StagingWorktree — pure git abstraction | os/exec only |
| `pkg/dolt` | Dolt server lifecycle, binary management, push/pull/sync, merge ownership | config |
| `pkg/agent` | Agent backends (process/docker), spawner, registry | config, dolt |
| `pkg/executor` | Formula execution engine: design/plan/implement/review/merge phases | store, config, formula, git, agent |
| `pkg/integration` | Linear epic sync, webhook handling, OAuth2 connect, HTTP server | store |
| `cmd/spire` | CLI dispatch, flag parsing, bridge wiring (composition root) | everything |

No circular dependencies. Each package depends only downward. New code
goes into the appropriate `pkg/` package, not `cmd/spire/`. The only
code in `cmd/spire/` should be CLI adapters (flag parsing + delegation)
and bridge files that wire cross-package callbacks.

## Code rules

### Where new code goes

New business logic goes into `pkg/`, not `cmd/spire/`:

| If you're writing... | Put it in... |
|---|---|
| Bead queries, mutations, type helpers | `pkg/store/` |
| Tower/config/credential/identity logic | `pkg/config/` |
| Formula parsing or phase definitions | `pkg/formula/` |
| Git operations (branch, worktree, merge) | `pkg/git/` |
| Dolt server, binary, push/pull/sync | `pkg/dolt/` |
| Agent spawn/kill/list/logs | `pkg/agent/` |
| Executor phase handlers | `pkg/executor/` |
| Linear sync, webhooks, OAuth | `pkg/integration/` |
| CLI flag parsing + output formatting | `cmd/spire/` (thin adapter) |

If unsure, check which package already has similar code. `cmd/spire/`
should only contain CLI dispatch, bridge wiring, and surfaces not yet
extracted (board TUI, steward, wizard, observability).

### Use pkg/store, not bd subprocess

**Never use `bdJSON()` or shell out to `bd` for data access in new code.**
Use `pkg/store` directly: `store.GetBead()`, `store.ListBeads()`,
`store.CreateBead()`, etc. The bridge wrappers in `cmd/spire/store_bridge.go`
provide backward-compatible unexported names (`storeGetBead`, etc.) for
existing `cmd/spire/` code.

### Every command must call resolveBeadsDir()

```go
func cmdFoo(args []string) error {
    if d := resolveBeadsDir(); d != "" {
        os.Setenv("BEADS_DIR", d)
    }
    // ... rest of command
}
```

`resolveBeadsDir()` is a bridge to `config.ResolveBeadsDir()`. It checks:
BEADS_DIR env -> cwd walk -> active tower instance -> any instance.

### Import rules

Packages form a DAG. These imports are **forbidden** (would create cycles):

- `pkg/store` must NOT import `pkg/config`, `pkg/dolt`, `pkg/agent`, `pkg/executor`
- `pkg/config` must NOT import `pkg/store`, `pkg/dolt`
- `pkg/git` must NOT import any `pkg/` package (pure stdlib only)
- `pkg/formula` must NOT import `pkg/store`, `pkg/config`

When a package needs something from a "higher" package, use a callback
variable (see `store.BeadsDirResolver` and `config.DoltDataDirFunc`
for examples).

### Verify before removing

Before removing code, features, or references:
- Check `cmd/spire/main.go` dispatch table for command existence
- `grep` for function/type usage before deleting
- Run `go build ./...` after every change

## DANGER — destructive commands

- **NEVER run `bd init --force`** — wipes entire dolt history. No undo.
- **NEVER run `bd init`** on a directory with an existing `.beads/` database.
- **NEVER leave branches hanging** — every branch must be merged or deleted.
- **NEVER use raw `exec.Command("git", ...)` outside `pkg/git/`.**
