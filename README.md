# Spire

A coordination hub for AI agents working across multiple repositories.

AI coding agents are powerful but isolated — an agent in your frontend repo doesn't know what the agent in your backend repo is doing. Spire connects them through a shared graph where agents register, communicate, track work, and stay in sync.

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│  repo: web   │     │  repo: api   │     │  repo: infra │
│  prefix: web-│     │  prefix: api-│     │  prefix: inf-│
│              │     │              │     │              │
│  agent ←─────┼─────┼──── agent ───┼─────┼────→ agent  │
└──────┬───────┘     └──────┬───────┘     └──────┬───────┘
       │                    │                    │
       └────────────────────┼────────────────────┘
                            │
                     ┌──────▼───────┐
                     │    Spire     │
                     │  shared Dolt │
                     │   database   │
                     └──────────────┘
```

Built on [beads](https://github.com/steveyegge/beads) (git-native issue tracking) and [Dolt](https://github.com/dolthub/dolt) (git-native SQL database).

## Why Spire

### The blackboard pattern

The [blackboard pattern](https://en.wikipedia.org/wiki/Blackboard_(design_pattern)) is a multi-agent architecture where independent agents read from and write to a shared, append-only knowledge store — the "blackboard." Each agent operates autonomously, scraping, processing, or analyzing different types of information at its own frequency, and posting its findings as events to the common log. No agent directly communicates with or depends on another; the blackboard is the sole coordination mechanism. Derived state and contextual layers are built on top of the raw event stream, giving each agent (or the system as a whole) a rich, evolving picture of what's known — without any single agent needing to hold all of that context itself.

The benefits for multi-agent coordination and shared learning are significant. Because the log is append-only and immutable, every observation is preserved, creating a durable institutional memory that compounds over time. Layering semantic search on top of the blackboard means agents don't need to know exactly what was recorded or by whom — they can query by meaning and surface relevant context across agent boundaries. This enables emergent collaboration: an agent handling a new task can draw on patterns observed by entirely unrelated agents, making the system collectively smarter with each interaction. The result is a loosely coupled architecture that produces tightly informed behavior — agents stay simple and independent, but the system as a whole learns.

### Coordinating autonomous agents

As AI agents become more autonomous — running continuously, making decisions, shipping code — they need infrastructure to coordinate. Today's agents are session-bound: they start, do work, and stop. The context dies with the session.

Spire makes agent coordination durable. An agent can `spire focus` a task, work on it across multiple sessions, and any other agent can pick up where it left off. The bead graph preserves everything: what was tried, what worked, what's blocked, who said what. This is the foundation for teams of agents that operate independently but stay aligned — the kind of coordination that fully autonomous agents will require.

## What you get

**Cross-repo visibility.** One command shows all work across every connected repo.

**Agent-to-agent messaging.** An agent in your web repo can message the agent in your API repo. Messages are beads with routing labels — no external message bus.

**Structured workflows.** `spire focus` bonds a 4-step workflow (design → implement → review → merge) that agents follow in isolated git worktrees.

**Project management sync.** Epics mirror to Linear automatically. The bead graph owns structure; Linear owns PM tracking. More integrations welcome.

**MCP integration.** An MCP server exposes Spire messaging as tools for Cursor and Claude Code.

## Install

```bash
brew tap awell-health/tap
brew install spire
```

Or build from source:

```bash
git clone https://github.com/awell-health/spire.git
cd spire && go build -o ~/.local/bin/spire ./cmd/spire
```

### Prerequisites

- [beads](https://github.com/steveyegge/beads) (`brew install beads`)
- [Dolt](https://github.com/dolthub/dolt) (included with beads)
- Go 1.26+ (to build from source)

## Quick start

```bash
# Install
brew tap awell-health/tap
brew install spire

# Initialize a hub in your repo
cd my-project && spire init

# Start services
spire up

# Register as an agent
spire register hub

# Send a message to another agent
spire send api "the auth endpoint is returning 500s" --ref web-42

# Check your inbox
spire collect

# Focus on a task (bonds a workflow on first focus)
spire focus web-42
```

## The `spire` CLI

All state lives in the shared Dolt database. The CLI is a thin Go binary over `bd`.

| Command | Description |
|---------|-------------|
| `spire` | Status (if init'd) or init (if not) |
| `spire init` | Initialize repo (`--prefix`, `--hub`, `--standalone`, `--satellite=<hub>`, `--dolthub=<url>`) |
| `spire sync [--hard] [url]` | Sync with a DoltHub remote (handles divergent histories) |
| `spire repo list` | List all init'd repos (`--json`) |
| `spire register <name>` | Register an agent in the roster |
| `spire unregister <name>` | Clean exit |
| `spire send <to> "msg"` | Send a message (`--ref`, `--thread`, `--priority`) |
| `spire collect` | Check inbox for unread messages |
| `spire focus <bead-id>` | Focus on a task (pours workflow on first focus) |
| `spire grok <bead-id>` | Focus + live Linear context |
| `spire read <bead-id>` | Mark a message as read |
| `spire connect <service>` | Connect an integration (e.g., `spire connect linear`) |
| `spire disconnect <service>` | Disconnect an integration |
| `spire serve` | Run the webhook receiver (`--port`) |
| `spire daemon` | Run the sync daemon (`--interval`, `--once`) |

### Messaging

Messages are beads with labels for routing:

```bash
# Send a message
spire send api "deploy is broken" --ref web-42

# Check inbox
spire collect

# Read full context
spire focus web-42

# Mark as read
spire read hub-12
```

Identity is auto-detected from `SPIRE_IDENTITY` env var (set per-repo by `spire init`), or override with `--as <name>`.

### Workflows

`spire focus` bonds a `spire-agent-work` molecule on first focus:

```
design      → Read the task, form a plan
implement   → Work in isolated worktree (.worktrees/<id>, branch feat/<id>)
review      → Verify against requirements, run tests
merge       → Merge branch, clean up worktree
```

## Connecting repos

Every repo gets a **prefix** (e.g., `web-`, `api-`, `inf-`). All issues, messages, and workflows live in one shared database.

```bash
# Initialize the hub first
cd my-hub && spire init
# → picks prefix, role=hub, runs bd init, injects shell env

# Connect a satellite
cd ../my-web-app && spire init --satellite=hub --prefix=web
# → creates .beads/redirect, regenerates routes, writes .envrc

# Or non-interactively
spire init --prefix=api --satellite=hub

# See all repos
spire repo list
```

`spire init` handles redirects, routes, env vars, and config registration. Run it again to reconfigure.

### Syncing with an existing DoltHub database

If you have an existing DoltHub database, `spire sync` pulls it into your local hub — handling divergent histories that `bd dolt pull` cannot.

```bash
# Fresh hub: hard reset to remote (nothing local to lose)
spire sync --hard https://doltremoteapi.dolthub.com/org/db

# Or inline during init
spire init --hub --prefix=myproject --dolthub=https://doltremoteapi.dolthub.com/org/db

# Existing hub with local issues: stash → pull → reimport
spire sync https://doltremoteapi.dolthub.com/org/db
```

Set `DOLT_REMOTE_USER` and `DOLT_REMOTE_PASSWORD` for DoltHub auth. `spire init --dolthub` auto-selects `--hard` for empty databases and `--merge` for non-empty ones.

## Linear integration

Spire syncs epics to Linear and processes Linear webhook events back into beads.

```bash
spire connect linear
```

This walks you through:
1. **OAuth2 authentication** — opens your browser, no API keys to copy
2. **Team selection** — pick your Linear team
3. **Webhook setup** (optional) — provide a URL for your webhook receiver (`spire serve`, or a deployed serverless function)
4. **Done** — credentials saved to system keychain, epics sync automatically via the daemon

The bead graph is the source of truth for task structure (subtasks, dependencies, hierarchy). Linear is the source of truth for PM tracking (status, assignees, sprints).

## MCP server

For Cursor and Claude Code users. Exposes Spire messaging as MCP tools:

| Tool | Purpose |
|------|---------|
| `spire_register` | Register in the agent roster |
| `spire_send` | Send a message |
| `spire_collect` | Check inbox |
| `spire_read` | Mark a message as read |
| `spire_focus` | Get full context for a bead |
| `spire_roster` | List registered agents |

### Setup

Add to your `.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "spire": {
      "command": "node",
      "args": ["/path/to/spire/packages/mcp-server/index.js"],
      "env": { "SPIRE_IDENTITY": "web" }
    }
  }
}
```

Or configure it as part of your Cursor/Claude Code setup.

## Architecture

```
spire/
├── cmd/spire/          # Go CLI — all commands, daemon, webhook server
├── packages/
│   └── mcp-server/     # MCP server for Cursor/Claude Code
├── .beads/
│   ├── formulas/       # Workflow templates (spire-agent-work)
│   └── routes.jsonl    # Multi-repo routing config
├── docs/superpowers/
│   ├── specs/          # Design specifications
│   └── plans/          # Implementation plans
├── cursor/             # Cursor IDE rules
└── cmd/spire/init.go   # spire init — replaces setup.sh
```

| Component | Technology | Role |
|-----------|-----------|------|
| CLI | Go (stdlib only) | Single binary — commands, daemon, webhook server |
| Database | [Dolt](https://github.com/dolthub/dolt) | Git-native SQL — branch, merge, diff your data |
| Issue tracking | [beads](https://github.com/steveyegge/beads) | Dependency-aware, agent-optimized |
| MCP server | Node.js | Exposes messaging to AI editors |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Issues are tracked with beads:

```bash
bd create "Fix something" -p 2 -t bug --description="What's broken"
```

## License

[Apache License 2.0](LICENSE)
