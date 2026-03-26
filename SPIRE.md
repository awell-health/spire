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

## Code rules (for working on Spire itself)

### Use the store API, not bd subprocess

**Never use `bdJSON()` or shell out to `bd` for data access in new code.**
Use the store API (`ensureStore()`, `storeGetBead()`, `storeListBeads()`,
`storeGetDepsWithMeta()`, etc.) in `store.go`.

### Every command must call resolveBeadsDir()

```go
func cmdFoo(args []string) error {
    if d := resolveBeadsDir(); d != "" {
        os.Setenv("BEADS_DIR", d)
    }
    // ... rest of command
}
```

### Investigation before implementation

When fixing bugs, **trace the data flow** before writing code. The most
common failure mode for agents is modifying the wrong file because the
symptom is in one place but the root cause is elsewhere. Before changing
code:

1. Identify the symptom (what's wrong)
2. Trace the data path (where does the value come from)
3. Find the root cause (which layer is broken)
4. Fix the root cause, not the symptom

Example: roster grouping was broken. The symptom was in roster.go, but
the root cause was in store.go — `GetIssue()` wasn't populating the
Dependencies field, so `bead.Parent` was always empty.

### Verify before removing

Before removing code, features, or references, verify they don't exist:
- Check `cmd/spire/main.go` dispatch table for command existence
- `grep` for function/type usage before deleting
- Run `go build ./...` after every change

## DANGER — destructive commands

- **NEVER run `bd init --force`** — wipes entire dolt history. No undo.
- **NEVER run `bd init`** on a directory with an existing `.beads/` database.
- **NEVER leave branches hanging** — every branch must be merged or deleted.
