# CLAUDE.md — Spire Agent Instructions

## Required reading

Before starting work, read these files in order to understand the system:

1. `README.md` — what Spire is, quick start, CLI overview
2. `docs/LOCAL.md` — local execution model, setup, wizard lifecycle
3. `docs/PLAN.md` — current roadmap and priorities
4. `docs/ARCHITECTURE.md` — components, data model, agent roles, pod architecture
5. `docs/VISION.md` — product direction and design philosophy

These are not optional. Agents that skip this context produce work that conflicts with the system's design.

## Overview

Spire is a coordination hub for AI agents across repositories. Multiple repos register here, each with their own prefix. Epics created here are automatically mirrored to Linear by the daemon.

## Using beads in Spire

All `bd` commands work as normal. Spire runs a shared Dolt server on port 3307.

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

Additional repos are registered via `spire repo add`. Check `spire repo list` for the current prefix map.

## Epics and Linear

When you create a bead with `type=epic`, the daemon will:

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

## Code rules

### Use the store API, not bdJSON

**Never use `bdJSON()` or shell out to `bd` for data access in new code.** Use the store API (`ensureStore()`, `storeListBoardBeads()`, `storeGetBlockedIssues()`, etc.) in `store.go`. The `bd` subprocess requires `.beads/` in the cwd, is slower, and breaks when run from other directories.

`bdJSON` is legacy — only `spire_test.go` still uses it. All production code paths (board, watch, roster, collect) have been migrated.

For blocked/ready detection, use `storeGetBlockedIssues()` (calls `store.GetBlockedIssues()`) which returns blocker IDs. Do not use `hasBlockingDeps()` with dependency counts from `bd list`.

### Every command must call resolveBeadsDir()

Any command that reads beads must call `resolveBeadsDir()` + `os.Setenv("BEADS_DIR", d)` at entry. This makes the command work from any directory (not just inside a registered repo). Pattern:

```go
func cmdFoo(args []string) error {
    if d := resolveBeadsDir(); d != "" {
        os.Setenv("BEADS_DIR", d)
    }
    // ... rest of command
}
```

`resolveBeadsDir()` checks: BEADS_DIR env → cwd walk → active tower instance → any instance.

## DANGER — destructive commands

- **NEVER run `bd init --force`**. This is equivalent to `git reset --hard` to the initial commit — it wipes the ENTIRE dolt history, destroying all beads irreversibly. There is no undo.
- **NEVER run `bd init`** on a directory that already has a `.beads/` database unless you are certain the database is empty and disposable.

## Important conventions

- **Always set priority** when creating beads: `-p 0` (P0/critical) through `-p 4` (P4/nice-to-have)
- **Always set type**: `-t task`, `-t bug`, `-t feature`, `-t epic`, `-t chore`
- **Claim before working**: `bd update <id> --claim` prevents double-work
- **Use `--json` flag** for programmatic access to bead data
- **Don't manually create Linear issues for epics** — the daemon syncs them automatically

## Spire CLI

Spire is a single binary that manages the full lifecycle: dolt server, daemon, messaging, and work claiming.

### Setup

```bash
# Create a tower (first time only)
spire tower create --name my-team

# Register a repo under the tower
spire repo add
spire repo add --prefix=web /path/to/web-app

# List registered repos
spire repo list

# Remove a repo
spire repo remove <prefix>
```

### Lifecycle

```bash
# Start everything (dolt server + daemon)
spire up [--interval 2m]

# Stop daemon only (dolt keeps running for other repos)
spire down

# Stop everything (daemon + dolt)
spire shutdown

# Check what's running
spire status
```

After a reboot, run `spire up` to restore services.

### Work management

```bash
# Claim a bead (verify → set in_progress)
spire claim <bead-id>

# Focus on a task (read-only context assembly + workflow molecule)
spire focus <bead-id>

# Deep focus with live Linear context
spire grok <bead-id>
```

`spire claim` verifies the bead isn't closed or owned by someone else, then sets it to in_progress. Use it before starting work.

`spire focus` assembles context: bead details, workflow progress, referenced beads, messages, comments. It pours a `spire-agent-work` molecule on first focus.

### Agent messaging

```bash
# Register as an agent
spire register <name>

# Unregister
spire unregister <name>

# Check inbox
spire collect

# Send a message
spire send <agent> "message" [--ref <bead-id>] [--thread <msg-id>] [--priority <0-4>]

# Mark a message as read
spire read <bead-id>
```

### Integrations

```bash
# Connect Linear (OAuth2 or API key)
spire connect linear

# Disconnect
spire disconnect linear

# Run webhook receiver
spire serve [--port 8080]
```

### Session lifecycle

1. `spire up` — ensure services are running
2. `spire collect` — check inbox
3. `spire claim <bead-id>` — claim work
4. `spire focus <bead-id>` — get context
5. Work on the task
6. `spire send <agent> "status update" --ref <bead-id>` — notify others

### Labels

Messages use labels for routing: `to:<agent>`, `from:<agent>`, `ref:<bead-id>`.
Query with: `bd list --rig=spi --label "msg,to:<agent>" --status=open --json`

## Commit format

Always reference the bead in commit messages:

```
<type>(<bead-id>): <message>
```

Examples:
- `feat(spi-a3f8): add OAuth2 support`
- `fix(xserver-0hy): handle nil pointer in rate limiter`
- `chore(pan-b7d0): upgrade dependencies`

Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `test`
