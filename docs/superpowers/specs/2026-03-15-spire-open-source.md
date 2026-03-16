# Spire Open Source Conversion

**Date**: 2026-03-15
**Status**: Draft

## What is Spire?

Spire is a coordination hub for AI agents working across multiple repositories. It gives multi-repo teams a shared nervous system — agents register, communicate, share work, and stay in sync through a common graph.

AI coding agents are increasingly autonomous, but they work in isolation. An agent in your frontend repo doesn't know what the agent in your backend repo is doing. When your system spans multiple repos, agents need a way to communicate, delegate, and coordinate. Spire is that shared space.

### How it works

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│  repo: web   │     │  repo: api   │     │  repo: infra │
│  prefix: web-│     │  prefix: api-│     │  prefix: inf-│
│              │     │              │     │              │
│  agent ←─────┼─────┼──── agent ───┼─────┼────→ agent   │
└──────┬───────┘     └──────┬───────┘     └──────┬───────┘
       │                    │                    │
       └────────────────────┼────────────────────┘
                            │
                     ┌──────▼───────┐
                     │    Spire     │
                     │              │
                     │  Shared Dolt │
                     │   database   │
                     │              │
                     │  ┌────────┐  │
                     │  │ beads  │  │  ← issues, messages,
                     │  │ graph  │  │    workflows, deps
                     │  └────────┘  │
                     └──────────────┘
```

Every repo gets a **prefix** (e.g., `web-`, `api-`, `inf-`). All issues, messages, and workflows from all repos live in one shared [beads](https://github.com/steveyegge/beads) graph backed by [Dolt](https://github.com/dolthub/dolt) (a git-native SQL database). Agents communicate by creating labeled beads — messages are just beads with routing labels (`to:api`, `from:web`, `ref:web-42`).

### What you get

**Cross-repo visibility.** One command shows all work across every connected repo. No context-switching between different trackers.

**Agent-to-agent messaging.** An agent in your web repo can send a message to the agent in your API repo: "the endpoint you just changed broke my integration tests." The recipient sees it next time it checks its inbox.

**Structured workflows.** `spire focus <task>` bonds a 4-step workflow (design → implement → review → merge) that agents follow. Each step runs in an isolated git worktree.

**Project management sync.** Epics created in beads automatically mirror to Linear. The bead graph owns the structure (tasks, subtasks, dependencies); Linear owns the PM view (status, assignees, sprints). This is the first integration — others (Jira, GitHub Issues, Notion) can be contributed.

**MCP integration.** An MCP server exposes Spire's messaging as tools for Cursor and Claude Code. Agents can register, send messages, check their inbox, and focus on tasks without leaving their editor.

### The stack

| Layer | Technology | Why |
|-------|-----------|-----|
| Issue tracking | [beads](https://github.com/steveyegge/beads) | Git-native, dependency-aware, agent-optimized (JSON output, `--claim`, `ready` detection) |
| Database | [Dolt](https://github.com/dolthub/dolt) | Git-native SQL — branch, merge, diff your data. Three-way merge resolution. |
| CLI | Go (stdlib only) | Single binary, no deps. Includes webhook server, daemon, and all commands. |
| MCP server | Node.js + `@modelcontextprotocol/sdk` | Exposes messaging to AI editors |
| Webhook receiver | Built into Go binary (`spire serve`) | Also deployable as Cloudflare Worker, GCP Cloud Function, or AWS Lambda via `spire connect` |
| Epic sync | Built into Go daemon (`spire daemon`) | Polls for new epics, mirrors to Linear via GraphQL |
| Build | pnpm + Turbo | Monorepo for MCP server and future Node.js packages |

## Why open source this?

The problem Spire solves — multi-agent coordination across repos — is not specific to any one company. Anyone running AI coding agents across a multi-repo codebase hits the same wall: the agents can't talk to each other.

Open-sourcing lets the community:
- **Use it.** Teams can deploy their own Spire hub and connect their repos.
- **Extend it.** Linear sync is the first PM integration. Jira, GitHub Issues, Notion, Shortcut are obvious next contributions.
- **Improve the protocol.** The messaging conventions (label-based routing, bead-as-message) are a starting point. The community can evolve them.

Awell dogfoods Spire as the first user. The project should work for Awell out of the box, but nothing in the codebase should be Awell-specific.

## What needs to change

### 1. Awell branding → generic

Every file that says "Awell" needs to be genericized. The project should read as a general-purpose tool that any team can adopt.

| File | Current | Target |
|------|---------|--------|
| `README.md` | "Agent communication hub for Awell Health" | Complete rewrite as open-source project page |
| `CLAUDE.md` | "Awell Health's centralized beads tracking server" | Generic agent instructions |
| `AGENTS.md` | References Awell | Generic workflow instructions |
| `package.json` (root) | `@awell-health/spire` | `spire` |
| `packages/mcp-server/package.json` | `@awell/spire-mcp` | `spire-mcp-server` |
| `cursor/spire-messaging.mdc` | "shared issue tracker across Awell repos" | Generic |
| `.beads/config.yaml` | Hardcoded `/Users/jb/awell/panels` | Remove, make example-only |
| `setup.sh` | Hardcoded Awell repo list, `dev@awellhealth.com`, `com.awell.spire-daemon` | Configurable |

### 2. New open-source files

**`LICENSE`** — Apache 2.0. Allows corporate use, requires attribution, patent grant. Standard for infrastructure tooling.

**`CONTRIBUTING.md`** — How to contribute. Should cover:
- Setting up the dev environment (clone, setup.sh, prerequisites)
- Project structure (monorepo layout, what each package does)
- How to add a new PM integration (following the Linear pattern)
- How to register satellite repos
- Code style (Go: stdlib only, no external deps; Node: ES modules)
- PR process
- Issue tracking (we use beads, of course)

**`CODE_OF_CONDUCT.md`** — Contributor Covenant v2.1. Standard, widely recognized.

**`.env.example`** — Root-level file documenting every environment variable:
```bash
# ── Spire identity ──────────────────────────────────────
# Per-repo agent identity (set automatically by setup.sh)
# SPIRE_IDENTITY=web

# ── Linear integration (optional) ──────────────────────
# Required for epic agent and webhook processing
# LINEAR_API_KEY=lin_api_...
# LINEAR_TEAM_ID=<uuid>
# LINEAR_PROJECT_ID=<uuid>          # optional
# LINEAR_WEBHOOK_SECRET=<secret>    # for webhook signature verification

# ── DoltHub (optional, for webhook queue) ──────────────
# DOLTHUB_API_TOKEN=<token>
# DOLTHUB_OWNER=<org>
# DOLTHUB_DATABASE=<db>

# ── Dolt server ────────────────────────────────────────
# Set automatically by setup.sh
# BEADS_DOLT_SERVER_HOST=127.0.0.1
# BEADS_DOLT_SERVER_PORT=3307
# BEADS_DOLT_SERVER_MODE=1
# BEADS_DOLT_AUTO_START=0
```

### 3. setup.sh → configurable

The hardest genericization. Currently, `setup.sh` has a hardcoded `REPOS` array:

```bash
REPOS=(
  "spi|spire"
  "pan|panels"
  "gro|grove"
  "rel|release-management"
)
```

**New approach:**

1. The hub is always the current directory. Its prefix is read from `.beads/config.yaml` (or defaults to the directory name).
2. Satellite repos are listed in `satellites.conf` — a simple file, one entry per line:
   ```
   # prefix|relative-path (relative to parent of this repo)
   # Example:
   # web|my-web-app
   # api|my-api-server
   ```
3. If `satellites.conf` doesn't exist, setup.sh only configures the hub (no satellites). This is a valid single-repo setup.
4. Create `satellites.conf.example` showing the format.

Other setup.sh changes:
- Dolt init email: `dev@awellhealth.com` → `spire@localhost` (or detect from git config)
- LaunchAgent plist name: `com.awell.spire-daemon` → `com.spire.daemon`
- Remove `~/awell/` path assumptions — use relative paths from the hub
- Keep macOS LaunchAgent support (document Linux systemd as a contribution opportunity)

### 4. README.md — complete rewrite

The README is the front door. It should make someone understand what Spire is and want to try it within 30 seconds.

**Structure:**

1. **Title + one-liner.** "Spire — a coordination hub for AI agents across repositories"
2. **The problem** (2-3 sentences). AI agents work in isolation. Multi-repo teams need agents that can talk to each other.
3. **How Spire works** (diagram + 3-4 bullets). Shared beads graph, agent messaging, workflows, PM sync.
4. **Quick start.** Clone, run setup.sh, register an agent, send a message. 5 commands to see it working.
5. **The `spire` CLI.** Command reference with examples.
6. **Connecting repos.** How to add satellites.
7. **Integrations.** Linear (ships built-in). How to add more.
8. **Architecture.** Monorepo layout, what each package does, how data flows.
9. **MCP integration.** For Cursor and Claude Code users.
10. **Contributing + License.**

### 5. CLAUDE.md / AGENTS.md — light genericization

These are agent instruction files. The content is mostly generic already — beads commands, messaging protocol, conventions. Changes are surgical:

- Remove "Awell Health" from the overview
- Replace the hardcoded prefix table with example prefixes (or reference `satellites.conf`)
- Keep all the beads workflow instructions verbatim — they're the value

### 6. go.mod

Currently `module github.com/awell-health/spire`. This can stay as-is for now — the module path should match the actual GitHub URL, and changing it requires the repo to exist at the new location. When/if the repo moves to a different org, update the module path then.

## What stays the same

- **All Go source code** (`cmd/spire/*.go`) — no Awell references in the code itself
- **MCP server logic** (`packages/mcp-server/`) — generic; just needs package.json rename
- **Beads formulas** (`.beads/formulas/`) — generic
- **Spec documents** (`docs/superpowers/specs/`) — keep as historical context
- **Plan documents** (`docs/superpowers/plans/`) — keep as historical context
- **Tests** — no changes needed

### What gets removed

- **`apps/webhook/`** — Next.js webhook app. Replaced by `spire serve` (Go HTTP handler in the binary) and serverless deploy templates in `spire connect linear`. No more Node.js dependency for the webhook receiver.
- **`packages/epic-agent/`** — Node.js epic polling agent. Logic folded into `spire daemon` (Go). The daemon already runs a pull→process→push loop; epic sync becomes another step in that loop.

This means the only Node.js package remaining is the MCP server (`packages/mcp-server/`), which depends on `@modelcontextprotocol/sdk`.

## What Linear integration looks like from the outside

Linear sync is the first "PM integration" and serves as the template for others. Worth documenting how it works so contributors can follow the pattern for Jira/GitHub Issues/etc.

**The pattern:**

1. **`spire daemon`** polls beads for new epics → creates Linear issues via GraphQL → labels the bead with `linear:<identifier>`.
2. **`spire serve`** (or a deployed serverless function) receives Linear webhook events → writes to the `webhook_queue` table.
3. **`spire daemon`** reads the queue → creates/updates beads from Linear events.

All three are in the Go binary. `spire connect linear` sets everything up — OAuth, team selection, webhook deployment, credential storage.

The bead graph is the source of truth for structure (tasks, subtasks, dependencies). The PM tool is the source of truth for tracking (status, assignees, sprints).

A new integration (say, Jira) would follow the same pattern:
1. `spire connect jira` — OAuth, project picker, webhook deploy
2. Daemon logic to sync epics to Jira and process Jira webhook events back to beads

This should be documented in CONTRIBUTING.md as the "integration guide."

## Rollout

1. Implement all changes on a branch.
2. Verify Awell's setup still works (setup.sh with `satellites.conf` containing their repos).
3. Merge to main.
4. Make the GitHub repo public.
5. Announce.

## Distribution: `brew install spire`

The primary install path is Homebrew. `brew install spire` should give you a working CLI.

### What gets installed

Just the `spire` Go binary. It's a single static binary with zero runtime dependencies (stdlib only). Everything is in the binary:

- `spire init` — set up a hub
- `spire connect linear` — OAuth + deploy webhook + configure
- `spire serve` — webhook receiver
- `spire daemon` — sync loop (pull, epic sync, webhook processing, push)
- `spire register/send/collect/focus/read` — agent messaging

The MCP server (`packages/mcp-server/`) is the only Node.js component remaining, installed separately for Cursor/Claude Code users.

### GoReleaser + Homebrew tap

[GoReleaser](https://goreleaser.com) is the standard toolchain for distributing Go CLI tools via Homebrew. It handles cross-compilation, GitHub releases, and Homebrew formula generation.

**Flow:**

1. Tag a release: `git tag v0.1.0 && git push --tags`
2. GoReleaser (via GitHub Actions) automatically:
   - Cross-compiles for macOS (arm64, amd64) and Linux (arm64, amd64)
   - Creates a GitHub release with tarballs and checksums
   - Updates the Homebrew formula in the tap repo

**User experience:**

```bash
brew tap awell-health/tap        # or spire-hub/tap — depends on org decision
brew install spire

spire version
# spire v0.1.0

spire connect linear              # set up Linear integration
```

### Homebrew tap repo

A separate repo (e.g., `awell-health/homebrew-tap`) containing the auto-generated formula. GoReleaser pushes to this repo on each release. The formula looks roughly like:

```ruby
class Spire < Formula
  desc "Coordination hub for AI agents across repositories"
  homepage "https://github.com/awell-health/spire"
  version "0.1.0"

  on_macos do
    on_arm do
      url "https://github.com/awell-health/spire/releases/download/v0.1.0/spire_darwin_arm64.tar.gz"
      sha256 "..."
    end
    on_intel do
      url "https://github.com/awell-health/spire/releases/download/v0.1.0/spire_darwin_amd64.tar.gz"
      sha256 "..."
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/awell-health/spire/releases/download/v0.1.0/spire_linux_arm64.tar.gz"
      sha256 "..."
    end
    on_intel do
      url "https://github.com/awell-health/spire/releases/download/v0.1.0/spire_linux_amd64.tar.gz"
      sha256 "..."
    end
  end

  def install
    bin.install "spire"
  end

  test do
    assert_match "spire", shell_output("#{bin}/spire version")
  end
end
```

### `.goreleaser.yaml`

Lives in the repo root:

```yaml
project_name: spire

builds:
  - main: ./cmd/spire
    binary: spire
    env:
      - CGO_ENABLED=0
    goos:
      - darwin
      - linux
    goarch:
      - amd64
      - arm64
    ldflags:
      - -s -w -X main.version={{.Version}}

archives:
  - format: tar.gz

brews:
  - repository:
      owner: awell-health    # or spire-hub
      name: homebrew-tap
    homepage: "https://github.com/awell-health/spire"
    description: "Coordination hub for AI agents across repositories"

release:
  github:
    owner: awell-health
    name: spire

changelog:
  use: github-native
```

### GitHub Actions workflow

`.github/workflows/release.yml`:

```yaml
name: Release
on:
  push:
    tags:
      - 'v*'

permissions:
  contents: write

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26'
      - uses: goreleaser/goreleaser-action@v6
        with:
          version: '~> v2'
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          HOMEBREW_TAP_GITHUB_TOKEN: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}
```

### Post-install: `spire init`

After `brew install spire`, the user needs to set up a hub. Today that's `setup.sh`. For the open-source version, this becomes a proper CLI command:

```bash
spire init                        # initialize current dir as a Spire hub
spire init --satellite ../api     # add a satellite repo
spire connect linear              # set up Linear integration
```

`spire init` replaces the hub-setup parts of `setup.sh` (dolt server, beads init, routes). This is cleaner than asking users to clone the repo and run a shell script — they just `brew install` and `spire init`.

## Open questions

- **GitHub org**: Keep under `awell-health` or move to a dedicated org (e.g., `spire-hub`)? Affects go.mod, clone URLs, all docs.
- **npm scope**: The packages are private/unpublished, so scope doesn't matter much. But if we ever publish the MCP server, we'd want a consistent scope.
- **Linux support**: setup.sh is macOS-only (LaunchAgent). Should we add systemd support in this pass, or leave it as a contribution opportunity?
- **Logo / branding**: Does Spire need visual identity for the repo?
