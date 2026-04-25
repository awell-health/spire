# CLAUDE.md — Spire Agent Instructions

## Required reading

Before starting work, read these files in order to understand the system:

1. `README.md` — what Spire is, quick start, CLI overview
2. `docs/VISION-LOCAL.md` — local-native deployment vision (paired with `VISION-CLUSTER.md` and `VISION-ATTACHED.md` for the other modes)
3. `docs/PLAN.md` — current roadmap and priorities
4. `docs/ARCHITECTURE.md` — components, data model, agent roles, pod architecture
5. `docs/ZFC.md` — MUST-READ: Zero Framework Cognition boundaries; Spire is orchestration, not local product reasoning
6. `docs/VISION.md` — product direction and design philosophy

These are not optional. Agents that skip this context produce work that conflicts with the system's design.

## Role-scoped CLI

As of v0.44.0, the `spire` CLI is organized by agent role. Each of the
five agent roles has its own parent command with scoped subcommands:

| Role | Parent command | Scoped verbs |
|------|----------------|--------------|
| apprentice | `spire apprentice` | `submit` |
| wizard | `spire wizard` | `claim`, `seal` |
| sage | `spire sage` | `accept`, `reject` |
| cleric | `spire cleric` | `diagnose`, `execute`, `learn` |
| arbiter | `spire arbiter` | `decide` |

See `docs/cli-reference.md` for the full role-grouped catalog, the
Artificer (`spire workshop`) surface, the multi-role Common commands
(`focus`, `grok`, `send`, `collect`, `read`), the Archmage top-level
surface, and the Deprecated verbs slated for removal in v1.0.

### The `SPIRE_ROLE` env var

When the executor spawns a role-specific agent session, it sets the
`SPIRE_ROLE` environment variable to one of: `apprentice`, `wizard`,
`sage`, `cleric`, `arbiter`. The scaffolder installs a
`.claude/spire-hook.sh` script that reads this variable on
`SubagentStart` and prints the role's command catalog into the session.

**Cross-role isolation is enforced:** a session running with
`SPIRE_ROLE=apprentice` only sees apprentice-scoped commands plus the
common multi-role verbs in its hook output — never `sage accept`,
`wizard claim`, or any other role's verbs. The catalog data lives in
`pkg/scaffold/` (see `pkg/scaffold/README.md`) and is the single source
of truth shared by hook output and `docs/cli-reference.md`.

## Package README rule

Before changing code under `pkg/`, read that package's `README.md` if one
exists. These package READMEs define local ownership boundaries and are
mandatory context for implementation agents.

Current package READMEs exist in:

- `pkg/executor/README.md`
- `pkg/wizard/README.md`
- `pkg/git/README.md`
- `pkg/steward/README.md`
- `pkg/formula/README.md`
- `pkg/workshop/README.md`
- `pkg/recovery/README.md`
- `pkg/scaffold/README.md`

## Overview

Spire is a coordination hub for AI agents across repositories. Multiple repos register here, each with their own prefix. Epics created here are automatically mirrored to Linear by the daemon.

## Filing work

**Use `spire file` to create beads, not `bd create`.** `spire file` handles
prefix resolution, design linkage, and repo registration automatically.

```bash
# Create a task
spire file "Fix auth token refresh" -t task -p 1

# Create an epic
spire file "User onboarding flow" -t epic -p 0

# Create a task linked to a design bead
spire file "Implement the thing" -t task -p 1 --design spi-xxx

# Create a subtask under an epic
spire file "My subtask" -t task -p 1 --parent spi-xxx
```

> **Internal bead types** (`message`, `step`, `attempt`, `review`) are
> programmatic-only — created by the engine, not by `spire file`. Don't
> hand-file them. See [docs/INTERNAL-BEADS.md](docs/INTERNAL-BEADS.md)
> for the taxonomy, invariant, and filter sites.

### Design-first workflow

Always start with a design bead before filing tasks or epics:

```bash
# 1. Create a design bead to capture thinking
spire design "What we're exploring"

# 2. Add comments as the design evolves
bd comments add <design-id> "Key decision: ..."

# 3. Close the design when ready
bd update <design-id> --status closed

# 4. File work linked to the design
spire file "The epic" -t epic -p 1 --design <design-id>
```

The `--design` flag creates a `discovered-from` dependency. The executor
validates that epics have a closed design bead before entering the plan phase.
`spire focus` and `spire grok` surface linked design beads as context.

## Reading beads

All `bd` commands work as normal. Spire runs a shared Dolt server on port 3307.

```bash
# List all work across all repos
bd list --json

# List ready work (no open blockers)
bd ready --json

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

**IMPORTANT:** When creating tasks that belong to an epic, ALWAYS use `--parent`:

```bash
spire file "my subtask" -t task -p 1 --parent spi-xxx
# → spi-xxx.1  (hierarchical ID, visible relationship)
```

Never create standalone tasks for epic subtasks — they get orphaned IDs
with no visible connection to the epic.

Use beads' hierarchical IDs for epic breakdown:

```bash
# Create an epic linked to its design
spire file "Auth system overhaul" -t epic -p 0 --design spi-xyz
# → spi-a3f8

# Add tasks under the epic
spire file "Implement OAuth2" -t task -p 1 --parent spi-a3f8
# → spi-a3f8.1

spire file "Add MFA support" -t task -p 1 --parent spi-a3f8
# → spi-a3f8.2

# Add sub-tasks
spire file "Google OAuth provider" -t task -p 2 --parent spi-a3f8.1
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

### Package structure

Business logic lives in `pkg/` packages, not `cmd/spire/`. See
SPIRE.md Principle 13 for the full package map and dependency rules.

| Package | What |
|---------|------|
| `pkg/store` | Bead persistence (queries, mutations, types) |
| `pkg/config` | Tower, credentials, identity, keychain |
| `pkg/formula` | Formula TOML parsing, phase pipeline |
| `pkg/git` | RepoContext, WorktreeContext, StagingWorktree |
| `pkg/dolt` | Dolt server lifecycle, push/pull/sync |
| `pkg/agent` | Agent backends (process/docker), spawner |
| `pkg/executor` | Formula execution engine (all phases) |
| `pkg/integration` | Linear sync, webhooks, OAuth |
| `cmd/spire` | CLI dispatch + bridge wiring only |

### Use pkg/store, not bd subprocess

**Never use `bdJSON()` or shell out to `bd` for data access in new code.**
Use `pkg/store` directly: `store.GetBead()`, `store.ListBeads()`,
`store.CreateBead()`, etc. Within `cmd/spire/`, bridge wrappers
(`storeGetBead`, etc.) in `store_bridge.go` provide backward-compatible
unexported names.

For blocked/ready detection, use `store.GetBlockedIssues()` which
returns blocker IDs.

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

`resolveBeadsDir()` delegates to `config.ResolveBeadsDir()`. It checks:
BEADS_DIR env -> cwd walk -> active tower instance -> any instance.

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

`spire focus` assembles context: bead details, workflow progress, related deps, messages, comments. It pours a `task-default` molecule on first focus.

### Design-to-work linkage

Design beads capture exploration and decisions before filing work items. Link them with a `discovered-from` dependency — not `ref:` labels (those are for message routing only).

```bash
# Create a design bead
spire design "Auth system overhaul"   # → spi-xxx

# When ready to file a work item, link it:
spire file "Auth overhaul epic" -t epic -p 1 --design spi-xxx
# --design creates a discovered-from dep automatically

# Or link manually after filing:
bd dep add <new-id> spi-xxx --type discovered-from
```

The executor validates that epics have at least one `discovered-from` dep pointing to a closed design bead before entering the plan phase. `spire focus` and `spire grok` display related deps (including design links) grouped by dependency type.

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
