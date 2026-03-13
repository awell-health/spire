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

Registers an agent as present in the system.

Creates a bead:
- `--rig=spi --type=agent --title="<name>" --labels agent --silent`

Prints the bead ID. Agent appears in the roster. `status=open` means registered.

### `spire unregister <name>`

Finds the agent bead by label query, closes it. `status=closed` means unregistered.

### `spire send <to> "<message>" [--ref <bead-id>] [--thread <message-id>]`

Sends a message to another agent.

Creates a bead:
- `--rig=spi --type=message`
- `--title="<message>"`
- `--labels "to:<to>,from:<caller>"`
- If `--ref`: adds `ref:<bead-id>` label
- If `--thread`: sets `--parent <message-id>`

Caller identity is auto-detected from the current repo's prefix.

### `spire collect [<name>]`

Checks the agent's inbox.

Runs: `bd list --rig=spi --type=message --label "to:<name>" --status=open --json`

Name defaults to current repo's prefix. Output includes a hint: "run `spire read <id>` to mark as read."

### `spire focus <bead-id>`

Clears context and reads a bead. Assembles a structured prompt:

```
--- Message spi-12 ---
From: pan
Subject: deploy is failing on staging
Body: ...

--- Referenced: pan-42 ---
Title: Fix staging deploy pipeline
Status: open
Priority: P1
Description: ...

--- Thread (2 replies) ---
spi-12.1 [awp]: looking into it, bad migration
spi-12.2 [awp]: fixed in awp-87
```

Fetches the bead, its comments, referenced beads (via `ref:` labels), and thread context (parent + children). Plain text output meant to be consumed as agent context.

### `spire read <bead-id>`

Marks a message as read: `bd close <bead-id>`.

## Message Schema

All messages are beads in the `spi-` prefix with `type=message`.

### Label conventions

| Label pattern | Purpose |
|---|---|
| `to:<name>` | Recipient (agent prefix or functional name) |
| `from:<name>` | Sender (auto-detected from caller's repo) |
| `ref:<bead-id>` | Links to a bead this message is about |
| `agent` | Marks agent registration beads |

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

Messages are ephemeral. `spire collect` only shows open messages. Closed messages remain in the graph for history.

### Agent registration beads

```
spi-5   type=agent  title="pan"  labels=[agent]  status=open
spi-6   type=agent  title="awp"  labels=[agent]  status=open
```

## Implementation: Go Binary

### Structure

```
cmd/spire/
  main.go          — CLI entry point, arg parsing
  register.go      — register/unregister commands
  send.go          — send command
  collect.go       — collect command
  focus.go         — focus + context assembly
  read.go          — read (close) command
  identity.go      — auto-detect caller prefix from repo context
  bd.go            — shells out to bd, parses JSON output
```

### Key decisions

- **No dependencies beyond stdlib.** Five commands, a few flags each. No framework needed.
- **Shells out to `bd`.** Not a beads client. Calls `bd` as a subprocess.
- **JSON mode everywhere.** All `bd` calls use `--json`. Human-friendly formatting in `spire`.
- **Install target: `~/.local/bin/spire`.** Built by `setup.sh`.

### Identity detection (`identity.go`)

Walks up from cwd looking for `.beads/routes.jsonl`. Parses it to find the primary prefix for the current repo. Falls back to `.beads/config.yaml` `issue-prefix` field. If ambiguous, requires `--as <name>` flag.

### Focus output (`focus.go`)

Assembles structured plain text from:
1. The target bead (title, description, status, priority)
2. Referenced beads (fetched via `ref:` labels)
3. Thread context (parent + sibling messages)
4. Comments on the bead

Output is plain text, not JSON — designed for agent context consumption.

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
