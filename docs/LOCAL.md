# Spire Local Mode

**Status**: Implemented (Phase 2 MVP landed 2026-03-23, v3 engine 2026-03-30, docs refreshed 2026-04-03)
**Date**: 2026-03-21 (updated 2026-04-03)

Spire runs locally on a developer's laptop. No Kubernetes, no cloud
infrastructure. Install the binary, create a tower, register repos, file
work, and agents execute it locally.

---

## Prerequisites

| Dependency | Purpose | Required |
|------------|---------|----------|
| `spire` CLI (`bd` is still a separate dependency today) | CLI for all operations | Yes |
| Docker | Running agents in containers | No (process mode available) |
| DoltHub account (free) | Remote sync of bead state | Yes |
| Anthropic API key | LLM agent execution | Yes |
| GitHub access (PAT or SSH key) | Repo operations (clone, branch, push) | Yes |

---

## Setup Flow

### 1. Install

```
brew install spire
```

Single CLI entry point. Today the supported Homebrew install provides
`spire` plus the `beads` dependency; `bd` is still subprocess-backed under
the hood. Docker remains optional.

### 2. Configure credentials

```
spire config set anthropic-key sk-ant-...
spire config set github-token ghp_...
spire config set dolthub-user myuser
spire config set dolthub-password mypassword
```

Credentials are stored in `~/.config/spire/credentials` (chmod 600).
Simple file-based storage — no OS keychain dependency.

Environment variables (`ANTHROPIC_API_KEY`, `GITHUB_TOKEN`,
`DOLT_REMOTE_USER`, `DOLT_REMOTE_PASSWORD`) override file-based
credentials when set, which is the expected pattern for CI/CD and
containers.

**Exists today**: `spire config set` supports both instance-scoped
settings (`identity`, `dolt.port`, `daemon.interval`, `dolthub.remote`)
and credential keys (`anthropic-key`, `github-token`, `dolthub-user`,
`dolthub-password`). Credentials are stored in
`~/.config/spire/credentials` (chmod 600). `spire config get` reads
credentials (masked by default). `spire config list` shows all.

### 3. Create a tower

```
spire tower create --name my-team
```

This command:
1. Initializes a local Dolt database with the beads schema
2. Generates tower identity (`project_id`, hub prefix)
3. Creates a DoltHub repo (using stored dolthub credentials)
4. Pushes the initial database to DoltHub
5. Writes tower config to `~/.config/spire/towers/my-team.json`

**Tower config format**:
```json
{
  "name": "my-team",
  "project_id": "abc123",
  "prefix": "spi",
  "dolthub_remote": "https://doltremoteapi.dolthub.com/myuser/my-team",
  "database": "beads_spi",
  "created_at": "2026-03-21T10:00:00Z"
}
```

**Exists today**: `spire tower create` is fully implemented. Creates dolt
database, generates tower identity, creates repos table, optionally
pushes to DoltHub, and writes tower config.

### 3b. Join an existing tower (teammates)

```
spire tower attach awell/awell
```

Teammates who didn't create the tower use `tower attach` to join.
This command:
1. Clones the tower's Dolt database from DoltHub
2. Restarts the local dolt server so it discovers the new database
3. Reads tower identity (`project_id`, prefix) from the cloned data
4. Bootstraps `.beads/` workspace and registers custom bead types
5. Writes tower config to `~/.config/spire/towers/<name>.json`

The argument is the DoltHub `org/repo` or full URL. The tower name
is derived from the repo name (override with `--name`).

**Exists today**: `spire tower attach` is fully implemented. Clones
from DoltHub, reads identity via local dolt SQL (no server dependency
for the initial read), restarts server for downstream operations, and
writes local config.

### 4. Register a repo

```
cd ~/code/my-web-app
spire repo add --prefix=web
```

This command:
1. Validates prefix uniqueness against the dolt `repos` table
2. Writes a row to the `repos` table: prefix, repo URL (from `git remote`),
   branch, runtime (auto-detected from repo contents)
3. Sets up `.beads/` in the repo with `metadata.json` pointing at the
   tower's dolt database and ensures required custom bead types exist
4. Pushes the registration to DoltHub

**Exists today**: `spire repo add` registers a repo against an existing
tower. It validates prefix uniqueness against the dolt `repos` table
(source of truth), writes a row, sets up `.beads/` in the repo, generates
`spire.yaml` if missing, and pushes the registration to DoltHub. Tower
resolution uses explicit database context (not ambient CWD).

### 5. File work

```
spire file "Add dark mode support" -t feature -p 2
```

Creates a bead in the local dolt database. Ready for agents to claim.

**Exists today**: `spire file` works. It resolves the prefix from the
current directory, delegates to `bd create`, and supports `--branch` and
`--merge-mode` labels.

### 6. Start the daemon

```
spire up
```

Starts a single background daemon that:
- Runs the dolt SQL server (localhost:3307)
- Syncs with DoltHub on interval (default 2 minutes)
- Runs Linear sync and webhook queue processing
- Optionally starts the steward as a sibling process when `--steward` is passed

`spire up` is best-effort idempotent today: if services are already
running, it will usually report that state and reuse them.

**Exists today**: `spire up` starts the dolt server and a daemon process.
The daemon runs DoltHub sync (pull + push), Linear epic sync, and webhook
queue processing. `spire up --steward` also starts the steward, but as a
separate sibling process rather than a merged loop. Local agent execution
works via `spire summon` (see below).

**Not yet built**: Unified single-process daemon/steward. Single-instance
enforcement. Health endpoint.

### Instance identity

Each Spire installation has a stable instance ID stored in
`~/.config/spire/instance.json` (generated once on first use via
`config.InstanceID()`). The steward uses this to scope agent ownership --
it only manages processes it owns (same instance ID), which prevents
conflicts when multiple machines are attached to the same tower via
DoltHub.

### `spire ready` -- ready-gate workflow

Beads start as `open`. The `spire ready <id>` command transitions a bead
to `ready` status, marking it for steward pickup. This two-step gate
(`open` -> `ready`) lets the archmage control when work becomes eligible
for agent assignment. The steward only queries beads with `status=ready`
in its assignment cycle.

### `spire review` -- read-only context assembly

`spire review <bead-id>` assembles review context from a bead's commit
history: header info, recorded commits, diff stats, full diff, review
history, and tail sections (deps, messages, comments). This is a
read-only inspection command -- it does not modify the bead or trigger
any agent action.

### 7. Monitor

```
spire status          # tower status, agent activity, sync state
spire logs            # follow daemon + agent logs
spire logs wizard-spi-abc   # specific agent
spire board           # interactive board TUI
spire board --json    # machine-readable board for agents/scripts
spire roster          # who's in the tower, what they're working on
spire watch           # live-updating view of all activity
```

**Exists today**: `spire status` shows dolt server and daemon PID/state.
`spire board` opens an interactive Bubble Tea TUI with phase columns
(ALERTS, INTERRUPTED, READY, DESIGN, PLAN, IMPLEMENT, REVIEW, MERGE,
DONE, BLOCKED) and background auto-refresh. The board is fully
interactive:

- Cursor navigation (arrow keys / vim hjkl) across sections and columns
- Inline actions: `s` summon, `S` resummon, `u` unsummon, `r`/`R` reset,
  `x` close, `y`/`Y` approve, `n` reject with feedback
- Inspector pane (`I`): drill-down with details tab (bead info, DAG
  progress, children, dependencies) and logs tab (wizard log streaming)
- Command mode (`:`): vim-style with tab completion via Cobra
- Search/filter (`/`): semantic filtering of beads
- Epic scoping (`e`): toggle epic filter
- Tower switcher (`T`): multi-tower support
- Grok (`g`): deep focus on selected bead
- Trace (`t`): DAG timeline view

`spire roster` shows work grouped by epic with agent process status,
elapsed time, and progress bars. `spire watch` provides a live-updating
terminal view. `spire logs [wizard-name]` tails wizard log output.
`spire metrics` shows agent performance summary with DORA metrics.

**V1.0 target**: Multi-mode TUI with Tab switching between Board, Agents,
Workshop, Messages, and Metrics views.

---

## Local Agent Execution

### Docker mode (available, not the default local path)

Agents run as Docker containers with:
- An ephemeral workspace — the agent clones the repo inside the container
- `ANTHROPIC_API_KEY` and GitHub credentials injected as environment
  variables (from `~/.config/spire/credentials` or env var overrides)
- Network access for git operations and LLM API calls
- One container per wizard, isolated from each other
- Host config and repo paths mounted into the container for local execution

Container lifecycle:
1. `spire summon` (or a steward using the Docker backend) selects ready work
2. Spire starts the agent image (`ghcr.io/awell-health/spire-agent:latest`
   by default, overrideable in `spire.yaml`)
3. Container starts with the bead ID, repo URL, and branch as arguments
4. The container runs the same internal Spire subcommand graph used
   locally (`execute`, `apprentice run`, or `sage review`, depending on role)
5. On completion, the container exits and Spire can inspect it via Docker
   metadata and streamed logs

Configure Docker mode in `spire.yaml`:

```yaml
agent:
  backend: docker
```

**Exists today**: Backend resolution for `docker`, `docker run` spawning,
container labels for discovery, and log streaming. Process mode remains
the best-tested and default local path.

**Still rough**: Image management UX, restart policy, and richer health
surfacing in `spire status`.

### Process mode (default)

Agents run as local processes. Faster startup, easier debugging. This
is the default local execution mode.

```
spire summon 3        # spawns 3 wizard processes
spire roster          # shows wizard status + progress bar
```

Each summoned wizard runs as a background executor process
(`spire execute <bead-id>`) with isolated worktrees underneath it:

1. `spire summon N` queries ready beads, picks the top N by priority
2. For each bead, spawns `spire execute <bead-id> --name wizard-<bead-id>`
3. The executor process:
   - Resolves the repo from the bead's prefix (repos table)
   - Claims the bead (`spire claim`)
   - Captures focus context and resolves the bead's formula
   - Creates a staging worktree and dispatches apprentice/sage subprocesses as needed
   - Runs the formula phases (`plan` → `implement` → `review` → `merge` for standard work)
   - Validates with repo commands from `spire.yaml`
   - Pushes the approved result to the repo's base branch and closes the bead
   - Writes `result.json` for observability
   - Cleans up staging and per-phase worktrees
4. Process PIDs tracked in `~/.config/spire/wizards.json`
5. Logs written to `~/.local/share/spire/wizards/<name>.log`

`spire roster` shows local wizards with elapsed time, progress bar, and
bead assignment. Dead processes are cleaned up automatically.

---

## Directory Structure

```
~/.config/spire/
    config.json             # global config: instances, shell state, editor prefs
    credentials             # API keys and tokens (chmod 600)
    wizards.json            # local wizard registry (process mode)
    towers/
        my-team.json        # tower identity + DoltHub remote URL

~/.local/share/spire/
    dolt.pid                # dolt server PID
    daemon.pid              # daemon PID
    dolt.log                # dolt server stdout
    dolt.error.log          # dolt server stderr
    daemon.log              # daemon stdout
    daemon.error.log        # daemon stderr
    dolt-config.yaml        # dolt server listener config

/opt/homebrew/var/dolt/     # (macOS) or ~/.local/share/dolt/ (Linux)
    .dolt/                  # dolt data directory
    beads_spi/              # database for tower prefix "spi"

~/code/my-web-app/
    .beads/
        metadata.json       # tower identity, project_id, dolt connection
        config.yaml         # dolt host/port, routing mode
        routes.jsonl        # prefix routing (for multi-repo towers)
```

The dolt data directory is shared across all repos on the machine. It is
managed by the dolt server process started by `spire up`. Default locations:

| Platform | Data directory | Overrride |
|----------|---------------|-----------|
| macOS | `/opt/homebrew/var/dolt` | `DOLT_DATA_DIR` |
| Linux | `~/.local/share/dolt` | `DOLT_DATA_DIR` |

---

## Sync Behavior

DoltHub is the remote store. Local dolt is the working copy.

| Command | Action |
|---------|--------|
| `spire push` | Commit local dolt changes, push to DoltHub |
| `spire pull` | Pull from DoltHub, merge into local dolt |
| Daemon auto-sync | Both directions on interval when `spire up` is running |

**Merge contract — field-level ownership**:

- **Cluster owns status fields**: `status`, `owner`, `assignee`. These are
  set by the steward and agent lifecycle (claim, close, reassign). Users
  should not edit these directly.
- **User owns content fields**: `title`, `description`, `priority`, `type`.
  These are set at creation time and updated by the user or agent working
  the bead.
- **Append-only fields**: `comments` and `messages` are append-only. Rows
  are never updated or deleted, only inserted.

The daemon pulls then pushes on each sync cycle. Multiple machines
running against the same DoltHub remote must coordinate via the steward
(one active steward at a time).

**Exists today**: `spire push` commits working-set changes, sets the CLI
remote, and runs `dolt push origin main`. Handles divergent history with
`--force`. `spire pull` pulls from DoltHub. Both handle credential
injection via environment variables (`DOLT_REMOTE_USER`,
`DOLT_REMOTE_PASSWORD`). The daemon runs DoltHub sync (pull + push) on
each cycle automatically via `runDoltSync()`.

**Not yet built**: Conflict detection and reporting based on field-level
ownership rules. Sync status reporting in `spire status`.

---

## Summoning and Dismissing Agents

```
spire summon 3                    # summon 3 wizards
spire summon --targets spi-7v2    # summon an exact bead ID
spire dismiss 1                   # dismiss one wizard (least busy first)
spire dismiss --all               # dismiss all wizards
spire roster                      # work grouped by epic, plus agent process status
```

**Implemented**: `spire summon` queries ready beads, spawns one
`spire execute` process per bead, tracks PIDs in `wizards.json`.
`spire dismiss` sends SIGINT to wizard processes. `spire roster` shows
local wizard status with elapsed time and progress bars. k8s mode
creates/deletes SpireAgent CRDs.

---

## What Exists Today vs What Needs to Be Built

### Exists and works

| Component | Implementation |
|-----------|---------------|
| `bd` CLI (via `pkg/bd` wrapper) | Bead CRUD, dolt operations, dependencies, labels, molecules |
| `spire tower create` | Initialize tower: dolt db, identity, repos table, DoltHub push, config |
| `spire tower attach` | Clone tower from DoltHub, bootstrap `.beads/`, write local config |
| `spire repo add` | Register repo against tower: prefix uniqueness check, dolt repos table, DoltHub push |
| `spire up` / `down` / `shutdown` | Dolt server + daemon lifecycle management |
| `spire status` | Dolt server and daemon PID/reachability check |
| `spire file` | Bead creation with prefix resolution and label support |
| `spire claim` | Atomic pull, verify, set in_progress, push |
| `spire focus` | Context assembly and workflow molecule |
| `spire push` / `pull` | DoltHub remote push/pull with credential handling |
| `spire send` / `collect` | Agent-to-agent messaging via bead labels |
| `spire register` / `unregister` | Agent registration |
| `spire board` | Interactive Bubble Tea TUI with phase columns and auto-refresh |
| `spire roster` | Work grouped by epic, agent processes with elapsed time/progress |
| `spire summon` / `dismiss` | Wizard/executor spawning (local: process default, Docker optional; k8s: CRDs) |
| `spire watch` | Live-updating terminal view |
| `spire logs` | CLI log reader for wizard and daemon logs |
| `spire metrics` | Agent performance summary with DORA metrics |
| `spire alert` | Priority alerts with bead references |
| `spire design` | Create design beads for brainstorming before filing tasks |
| `spire steward` | Work coordinator with ready-assess-assign cycle |
| `spire config` | Instance-scoped config + credential get/set/list |
| `spire connect linear` | Linear OAuth2 integration |
| `spire daemon` | Background process: DoltHub sync + Linear sync + webhook processing |
| `spire doctor` | 11 checks in 3 categories, `--fix` auto-repair |
| V3 formula system | Built-in v3 step-graph formulas (epic, bugfix, agent-work, recovery) with tower -> repo -> embedded resolution |
| V3 graph executor | Drives declarative step graphs with conditions, opcodes, nestable sub-graphs, crash-safe resume |
| Tower formula sharing | `spire formula list/show/publish/remove` — formulas in dolt, synced via daemon |
| Recovery system | First-class recovery bead type with dedicated formula, structured metadata, prior-learning lookup |
| Archmage identity | Tower config stores user identity for merge commit attribution |
| Credential storage | File-based (`~/.config/spire/credentials`, chmod 600), env var overrides |
| Dolt lifecycle | Auto-download binary, version pinning, managed server start/stop |
| Docker agent images | `Dockerfile.agent`, `Dockerfile.steward` |
| goreleaser + CI | Cross-compile, GitHub Actions test/release, SHA256 checksums |
| Homebrew tap | `awell-health/tap` with `beads` as a dependency; `dolt` is auto-managed by Spire |
| `spire version` | Prints spire version + managed dolt version and path |
| Smoke test | Docker-based smoke test (`test/smoke/Dockerfile`) validates fresh install |

### V1.0 remaining work

| Component | Description | Priority |
|-----------|-------------|----------|
| Complete v2 removal | Remove remaining v2 code paths in cmd/spire/, pkg/wizard, pkg/board, tests | 1 |
| Unified daemon | Merge steward loop into `spire up` as single process | 1 |
| Single-daemon enforcement | Prevent multiple `spire up` from racing | 1 |
| Multi-mode TUI | Tab-based switching: Board, Agents, Workshop, Messages, Metrics | 2 |
| Multi-backend support | Codex CLI and Cursor CLI as alternative agent backends | 2 |
| Workshop skill | Claude Code skill for formula design, simulation, testing, installation | 2 |
| `bd` embedded in `spire` | Single binary distribution (no separate `bd` install) | Deferred (spi-770) |

---

## Design Constraints

1. **Single install surface**. Spire aims to feel like one tool even though
   `bd` is still a separate binary today. Dolt is NOT embedded, but Spire
   manages its lifecycle — auto-downloads dolt if missing and starts/stops
   the dolt server. The user does not need to install dolt separately.

2. **Offline-first**. All operations work against the local dolt database.
   DoltHub sync is background and best-effort. Filing beads, claiming work,
   and running agents all work without internet access (LLM calls excepted).

3. **One steward per tower**. Only one machine should run `spire up` with
   the steward active at a time. Multiple machines can file beads and push
   to DoltHub, but only the steward assigns work and monitors agent health.

4. **File-based credentials**. Secrets are stored in
   `~/.config/spire/credentials` (chmod 600). Environment variables
   (`ANTHROPIC_API_KEY`, `DOLT_REMOTE_PASSWORD`) override file-based
   credentials for CI and containers.

5. **Docker optional**. Process mode is the first-class default. Some users
   will not have Docker. Process mode is also better for debugging and
   development; set `agent.backend: docker` when you want containers.
