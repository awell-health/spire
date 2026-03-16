# CLAUDE.md — Spire Agent Instructions

## Overview

Spire is a coordination hub for AI agents across repositories. Multiple repos register here, each with their own prefix. Epics created here are automatically mirrored to Linear by the epic agent.

## Using beads in Spire

All `bd` commands work as normal. Spire runs a shared Dolt server on port 3308.

```bash
# List all work across all repos
bd list --json

# List ready work (no open blockers)
bd ready --json

# Create a task (uses the current repo's prefix)
bd create "Fix auth token refresh" -p 1 -t task

# Create an epic (will be auto-synced to Linear)
bd create "User onboarding flow" -p 0 -t epic

# View an issue
bd show <id>

# Claim work
bd update <id> --claim
```

## Prefixes

Each repo has its own prefix. When creating beads from a repo context, the prefix is automatic:

| Repo | Prefix | Example ID |
|------|--------|------------|
| Hub (this repo) | `hub-` | `hub-a3f8` |
| Web app | `web-` | `web-b7d0` |
| API server | `api-` | `api-8a01` |

Additional repos are registered via `setup.sh` or `spire init --satellite`. Check `.beads/config.yaml` for the current prefix map.

## Epics and Linear

When you create a bead with `type=epic`, the epic agent will:

1. Create a corresponding Linear issue
2. Add a `linear:<identifier>` label to the bead
3. Add a comment to the bead with the Linear issue URL
4. Include the bead ID in the Linear issue description

**The bead is the source of truth for the epic's structure** (sub-tasks, deps, hierarchy). **Linear is the source of truth for PM tracking** (status, assignees, sprint planning).

### Epic hierarchy

Use beads' hierarchical IDs for epic breakdown:

```bash
# Create an epic
bd create "Auth system overhaul" -p 0 -t epic
# → spi-a3f8

# Add tasks under the epic
bd create "Implement OAuth2" -p 1 -t task --parent spi-a3f8
# → spi-a3f8.1

bd create "Add MFA support" -p 1 -t task --parent spi-a3f8
# → spi-a3f8.2

# Add sub-tasks
bd create "Google OAuth provider" -p 2 -t task --parent spi-a3f8.1
# → spi-a3f8.1.1
```

## Dependencies

Use `bd dep` to express blocking relationships:

```bash
# Task B blocks Task A
bd dep add <task-a> <task-b>

# Check what's ready to work on
bd ready --json
```

## Cross-repo work

All beads from all registered repos are visible in Spire. To filter by repo prefix:

```bash
bd list --json | jq '.[] | select(.id | startswith("web-"))'
bd list --json | jq '.[] | select(.id | startswith("api-"))'
```

## Important conventions

- **Always set priority** when creating beads: `-p 0` (P0/critical) through `-p 4` (P4/nice-to-have)
- **Always set type**: `-t task`, `-t bug`, `-t feature`, `-t epic`, `-t chore`
- **Claim before working**: `bd update <id> --claim` prevents double-work
- **Use `--json` flag** for programmatic access to bead data
- **Don't manually create Linear issues for epics** — let the agent handle it

## Agent Messaging (Spire)

Spire provides agent-to-agent communication via the shared Dolt database.

### Quick reference

```bash
# Register as an agent
spire register <name>

# Unregister
spire unregister <name>

# Check inbox
spire collect

# Send a message
spire send <agent> "message" [--ref <bead-id>] [--thread <msg-id>] [--priority <0-4>]

# Focus on a task (bonds workflow on first focus)
spire focus <bead-id>

# Mark a message as read
spire read <bead-id>
```

### Session lifecycle

1. `spire collect` — check inbox at session start
2. Work on tasks, communicate as needed
3. `spire send <agent> "status update" --ref <bead-id>` — notify other agents

### Labels

Messages use labels for routing: `to:<agent>`, `from:<agent>`, `ref:<bead-id>`.
Query with: `bd list --rig=spi --label "msg,to:<agent>" --status=open --json`
