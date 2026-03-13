# Spire Agent Messaging — Design Spec

## Problem

Awell runs multiple repos (panels, grove, release-management, etc.) that share a single Dolt-backed beads database via Spire. Agents working in these repos need to communicate with each other — assigning work, notifying on completions, asking questions. Today there is no ergonomic way to do this. The beads primitives exist (messages, labels, comments, shared database) but there is no agent-facing API.

MCP Agent Mail is unusable because it is pinned to a pre-Dolt beads version.

## Architecture

Spire acts as a **blackboard** — a shared graph that agents read from and write to. The Dolt database is the blackboard. Agents produce nodes (beads) and edges (deps, labels, refs). Higher-level agents can later build semantic meaning on top of the raw graph.

The `spire` CLI is a thin ergonomic layer over `bd`. It owns no state. All data lives in beads.

### Design principles

- Use what beads already has. No new infrastructure.
- The shared Dolt database is the delivery mechanism. No separate message bus.
- Labels are the universal query surface: `to:`, `from:`, `ref:`.
- Identity comes from the repo prefix. No separate auth.
- `spire` shells out to `bd`. It benefits from beads upgrades automatically.

## Commands

### `spire register <name>`

Registers an agent as present in the system. Idempotent — if an agent bead with this name already exists and is open, returns the existing ID.

Creates a bead:
- `bd create --rig=spi --type=task --title="<name>" -p 4 --labels "agent,name:<name>" --silent`

Uses `type=task` (not a custom type) to ensure queryability with standard `bd list` filters. The `agent` label distinguishes these from regular tasks.

Prints the bead ID. Agent appears in the roster. `status=open` means registered.

Roster query: `bd list --rig=spi --label agent --status=open --json`

### `spire unregister <name>`

Finds the agent bead: `bd list --rig=spi --label "agent,name:<name>" --status=open --json`, extracts the ID, then `bd close <id>`. `status=closed` means unregistered.

### `spire send <to> "<message>" [--ref <bead-id>] [--thread <message-id>]`

Sends a message to another agent.

Creates a bead:
- `bd create --rig=spi --type=task -p 3 --title="<message>" --labels "msg,to:<to>,from:<caller>" --silent`
- If `--ref`: adds `ref:<bead-id>` label
- If `--thread`: sets `--parent <message-id>`
- Optional `--priority <0-4>` flag for urgent messages (default: 3)

Uses `type=task` with the `msg` label to distinguish messages. Caller identity is auto-detected from the current repo's prefix.

### `spire collect [<name>]`

Checks the agent's inbox.

Runs: `bd list --rig=spi --label "msg,to:<name>" --status=open --json`

Name defaults to current repo's prefix. Output includes a hint: "run `spire read <id>` to mark as read."

### `spire focus <bead-id>`

Focuses an agent on a bead. Two behaviors depending on state:

**First focus (no molecule exists):**
1. Pours the `spire-agent-work` formula with the bead as context, creating a molecule (child beads representing workflow steps)
2. Assembles and outputs the structured prompt (see below)

**Subsequent focus (molecule already exists):**
1. Detects existing molecule children on the bead
2. Shows current progress through the workflow
3. Assembles and outputs the structured prompt with progress context

**Output format:**

```
--- Task pan-42 ---
Title: Fix staging deploy pipeline
Status: in_progress
Priority: P1
Description: ...

--- Workflow (spire-agent-work) ---
  [x] design      — Design the approach
  [ ] implement   — Implement in worktree (use /worktree-merge-orchestrator)
  [ ] review      — Review implementation
  [ ] merge       — Merge and clean up

--- Referenced by spi-12 ---
From: pan
Subject: deploy is failing on staging

--- Comments (2) ---
...
```

Fetches the bead, its molecule/workflow state, comments, referenced beads (via `ref:` labels), and thread context (parent + children). Plain text output meant to be consumed as agent context.

Detection of existing molecule: check for children of the bead that have labels matching the formula step names, or check `bd mol show` on the bead.

### `spire read <bead-id>`

Marks a message as read: `bd close <bead-id>`.

## Message Schema

All messages are beads in the `spi-` prefix with `type=task` and the `msg` label.

### Label conventions

| Label pattern | Purpose |
|---|---|
| `msg` | Marks message beads (distinguishes from regular tasks) |
| `to:<name>` | Recipient (agent prefix or functional name) |
| `from:<name>` | Sender (auto-detected from caller's repo) |
| `ref:<bead-id>` | Links to a bead this message is about |
| `agent` | Marks agent registration beads |
| `name:<name>` | Agent's name on registration beads |

### Threading

Replies use beads' native parent-child hierarchy:

```
spi-12  "pan: deploy is failing on staging"        [to:awp, from:pan]
  spi-12.1  "awp: looking into it, bad migration"  [to:pan, from:awp]
  spi-12.2  "awp: fixed in awp-87"                 [to:pan, from:awp, ref:awp-87]
```

`--thread spi-12` sets the parent. First message in a conversation has no parent.

### Lifecycle

- **open** = unread/unacknowledged
- **closed** = read

Messages are transient — once read, they're done. `spire collect` only shows open messages. Closed messages remain in the graph for history.

### Agent registration beads

```
spi-5   type=task  title="pan"  labels=[agent, name:pan]  status=open
spi-6   type=task  title="awp"  labels=[agent, name:awp]  status=open
```

## Work Formula: `spire-agent-work`

A beads formula that encodes how agents should work on tasks. Poured automatically by `spire focus` on first focus.

### Steps

```
design      — Understand the task, read relevant code, form an approach
implement   — Implement in an isolated worktree (use /worktree-merge-orchestrator)
review      — Review the implementation against the task requirements
merge       — Merge the worktree and clean up
```

### Formula definition

```toml
[formula]
name = "spire-agent-work"
description = "Standard agent work protocol. Focus → design → implement in worktree → review → merge."
type = "workflow"

[variables]
task = { description = "The bead ID being worked on", required = true }

[[steps]]
name = "design"
title = "Design approach for {{task}}"
description = "Read the task, explore relevant code, form a plan. Do not write code yet."

[[steps]]
name = "implement"
title = "Implement {{task}}"
description = "Use /worktree-merge-orchestrator to implement in an isolated git worktree."
depends_on = ["design"]

[[steps]]
name = "review"
title = "Review implementation of {{task}}"
description = "Review the changes against the original task requirements. Run tests."
depends_on = ["implement"]

[[steps]]
name = "merge"
title = "Merge {{task}}"
description = "Merge the worktree back to the working branch. Clean up."
depends_on = ["review"]
```

### Installation

The formula file lives at `.beads/formulas/spire-agent-work.formula.toml` in the Spire repo. Since all satellites redirect to Spire's `.beads/`, the formula is accessible from any repo via beads' formula search path.

### How `spire focus` uses it

```
1. bd show <bead-id> --json                              # fetch the task
2. bd children <bead-id> --json                           # check for existing molecule
3. if no molecule children:
     bd mol pour spire-agent-work --var task=<bead-id>    # pour the formula
     bd dep add <first-step> <bead-id>                    # link molecule to task
4. bd mol progress <molecule-id> --json                   # get workflow state
5. assemble and output the focus prompt
```

## Implementation: Go Binary

### Structure

```
cmd/spire/
  main.go          — CLI entry point, arg parsing
  register.go      — register/unregister commands
  send.go          — send command
  collect.go       — collect command
  focus.go         — focus, molecule pour/resume, context assembly
  read.go          — read (close) command
  identity.go      — auto-detect caller prefix from repo context
  bd.go            — shells out to bd, parses JSON output

.beads/formulas/
  spire-agent-work.formula.toml  — standard agent work protocol
```

### Key decisions

- **No dependencies beyond stdlib.** Five commands, a few flags each. No framework needed.
- **Shells out to `bd`.** Not a beads client. Calls `bd` as a subprocess.
- **JSON mode everywhere.** All `bd` calls use `--json`. Human-friendly formatting in `spire`.
- **Install target: `~/.local/bin/spire`.** Built by `setup.sh`.

### Identity detection (`identity.go`)

Detection strategy (highest priority first):

1. **`--as <name>` flag** — explicit override, always wins.
2. **`SPIRE_IDENTITY` env var** — set by `.envrc` or shell config during repo registration.
3. **`.beads/config.yaml` `issue-prefix` field** — fallback to the repo's configured prefix.

`setup.sh` writes `SPIRE_IDENTITY=<prefix>` into each satellite repo's `.envrc` (or equivalent) during registration, making detection reliable without fallback heuristics.

### Focus output (`focus.go`)

Assembles structured plain text from:
1. The target bead via `bd show <id> --json` (title, description, status, priority)
2. Molecule state — pour `spire-agent-work` on first focus, then `bd mol progress` for workflow status
3. Referenced beads — parse `labels` array from JSON, extract `ref:*` prefixed labels, fetch each with `bd show <ref-id> --json`
4. Thread context — if bead has a parent, fetch parent + siblings via `bd children <parent-id> --json`
5. Comments via `bd comments <id> --json`

Output is plain text, not JSON — designed for agent context consumption. The goal is minimal context: only what the agent needs to act on the current step.

## Error Handling

- **`spire register`** — idempotent. If an open agent bead with this name exists, returns existing ID.
- **`spire send` to unknown agent** — warn that no agent bead found for recipient, but create the message anyway (recipient may register later).
- **`spire register` when already registered** — return existing bead ID, no error.
- **`spire read` on already-closed bead** — no-op, print "already read."
- **`bd` failures** (Dolt down, network error) — propagate stderr from `bd` with added context ("spire: failed to send message: <bd error>").

## Integration

### setup.sh additions

New step after routes/redirects:

```bash
# ── 6. Build spire CLI ──
# Check Go is installed
# go build -o ~/.local/bin/spire ./cmd/spire
# Ensure ~/.local/bin is in $PATH
# Register the hub: spire register spi
```

Each satellite repo gets `SPIRE_IDENTITY=<prefix>` written to its `.envrc` during step 5.

### Agent session lifecycle

```
1. spire register pan          # announce presence
2. spire collect               # check inbox
3. ... do work ...
4. spire send awp "done with pan-42" --ref pan-42
5. spire unregister pan        # clean exit (optional)
```

### CLAUDE.md / AGENTS.md additions

Each repo gets a messaging section documenting the five commands.

## Out of scope (v1)

- **Broadcast / `to:all`** — later, as a label convention
- **Semantic indexing agents** — the blackboard supports them, separate design
- **File locking / gate beads** — orthogonal to messaging
- **Epic agent changes** — continues as-is, already reads the graph
