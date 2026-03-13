# Spire

Agent communication hub for Awell Health. A shared [beads](https://github.com/steveyegge/beads) graph backed by [Dolt](https://github.com/dolthub/dolt) that agents use as a blackboard — reading, writing, and messaging through a common database.

## Setup

```bash
git clone git@github.com:awell-health/spire.git
cd spire && ./setup.sh
```

This installs beads, starts a local Dolt server, configures satellite repos, and builds the `spire` CLI to `~/.local/bin/spire`.

## The `spire` CLI

Thin Go binary over `bd`. All state lives in the shared Dolt database.

```bash
spire register <name>       # Announce presence
spire unregister <name>     # Clean exit
spire send <to> "message"   # Send a message (--ref, --thread, --priority)
spire collect               # Check inbox
spire focus <bead-id>       # Focus on a task (pours workflow on first focus)
spire read <bead-id>        # Mark message as read
```

### Messaging

Messages are beads with labels for routing:

```bash
# Send
spire send awp "deploy is broken" --ref pan-42

# Receive
spire collect                    # list unread messages
spire focus <msg-id>             # read message + linked context
spire read <msg-id>              # mark as read
```

Identity is auto-detected from `SPIRE_IDENTITY` env var (set per-repo by `setup.sh`), or override with `--as <name>`.

### Focus and workflows

`spire focus` pours a `spire-agent-work` molecule on first focus, creating a 4-step workflow:

```
design      → Read the task, form a plan
implement   → Work in isolated worktree (.worktrees/<bead-id>, branch feat/<bead-id>)
review      → Verify against requirements, run tests
merge       → Merge branch, clean up worktree
```

Second focus resumes the existing molecule and shows progress.

## Formulas

Workflows are defined as [beads formulas](https://github.com/steveyegge/beads) — composable TOML templates.

```bash
bd formula list                              # See available formulas
bd formula show spire-agent-work             # Inspect steps
bd cook spire-agent-work --var task=pan-42   # Preview resolved steps
bd mol pour spire-agent-work --var task=X    # Create a molecule manually
```

Formulas can be bonded together to create compound workflows — e.g., a process formula + a repo-specific formula = a tailored workflow for that repo.

## Repos

| Repo | Prefix | Example |
|------|--------|---------|
| Spire | `spi-` | `spi-a3f8` |
| Panels | `pan-` | `pan-b7d0` |
| Grove | `gro-` | `gro-8a01` |
| Release Mgmt | `rel-` | `rel-c4e2` |

Each satellite repo redirects to Spire's `.beads/` database via routes.

## Epic agent

Creates Linear issues from beads epics automatically. See `index.js`.

```bash
cp .env.example .env   # Add LINEAR_API_KEY, LINEAR_TEAM_ID
npm start
```
