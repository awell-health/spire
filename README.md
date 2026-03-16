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
# Initialize a Spire hub
spire init

# Connect a satellite repo
spire init --satellite ../my-api-server

# Register as an agent
spire register hub

# Send a message to another agent
spire send api "the auth endpoint is returning 500s" --ref web-42

# Check your inbox
spire collect

# Focus on a task (bonds a workflow on first focus)
spire focus web-42

# Mark a message as read
spire read hub-12
```

## The `spire` CLI

All state lives in the shared Dolt database. The CLI is a thin Go binary over `bd`.

| Command | Description |
|---------|-------------|
| `spire init` | Initialize a Spire hub (or add a satellite with `--satellite`) |
| `spire register <name>` | Register an agent in the roster |
| `spire unregister <name>` | Clean exit |
| `spire send <to> "msg"` | Send a message (`--ref`, `--thread`, `--priority`) |
| `spire collect` | Check inbox for unread messages |
| `spire focus <bead-id>` | Focus on a task (pours workflow on first focus) |
| `spire grok <bead-id>` | Focus + live Linear context |
| `spire read <bead-id>` | Mark a message as read |
| `spire serve` | Run the webhook receiver |
| `spire daemon` | Run the sync daemon (pull, sync epics, process webhooks, push) |
| `spire connect linear` | Set up Linear integration (OAuth, team picker, webhook deploy) |

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

Identity is auto-detected from `SPIRE_IDENTITY` env var (set per-repo by `setup.sh`), or override with `--as <name>`.

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

### Using setup.sh

Create a `satellites.conf` in your Spire hub directory:

```
# prefix|directory-name (relative to parent dir)
web|my-web-app
api|my-api-server
```

Then run `./setup.sh`. It configures redirects, routes, Cursor integration, and the daemon.

### Manual setup

```bash
# In the satellite repo
mkdir -p .beads
echo "../spire/.beads" > .beads/redirect
echo 'export SPIRE_IDENTITY="web"' > .envrc
```

## Linear integration

Spire syncs epics to Linear and processes Linear webhook events back into beads.

```bash
spire connect linear
```

This walks you through:
1. **OAuth2 authentication** — opens your browser, no API keys to copy
2. **Team selection** — pick your Linear team
3. **Webhook deployment** — deploys a receiver to Cloudflare Workers, GCP, AWS, or self-hosted
4. **Done** — epics sync automatically via the daemon

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

Or let `setup.sh` configure it automatically for all connected repos.

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
├── setup.sh            # Hub + satellite setup
└── satellites.conf     # Your satellite repos (gitignored)
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
