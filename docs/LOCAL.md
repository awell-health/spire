# Spire Local Mode

**Status**: Implemented (Phase 2 MVP landed 2026-03-23, formula system 2026-03-26)
**Date**: 2026-03-21 (updated 2026-03-26)

Spire runs locally on a developer's laptop. No Kubernetes, no cloud
infrastructure. Install the binary, create a tower, register repos, file
work, and agents execute it locally.

---

## Prerequisites

| Dependency | Purpose | Required |
|------------|---------|----------|
| `spire` binary (includes `bd`) | CLI for all operations | Yes |
| Docker | Running agents in containers | No (process mode available) |
| DoltHub account (free) | Remote sync of bead state | Yes |
| Anthropic API key | LLM agent execution | Yes |
| GitHub access (PAT or SSH key) | Repo operations (clone, branch, PR) | Yes |

---

## Setup Flow

### 1. Install

```
brew install spire
```

Single binary. Ships with `bd` embedded. No runtime dependencies beyond
Docker (optional).

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
- Runs the steward (work coordinator) on a 2-minute cycle
- Syncs with DoltHub on interval (default 2 minutes)
- Spawns agents locally when work is ready
- Provides a health endpoint (`localhost:8080/status`)

Only one `spire up` is allowed at a time. Running it again shows current
status.

**Exists today**: `spire up` starts the dolt server and a daemon process.
The daemon runs DoltHub sync (pull + push), Linear epic sync, and webhook
queue processing. The steward runs as a separate command (`spire steward`).
Local agent execution works via `spire summon` (see below).

**Not yet built**: Unified daemon that includes the steward loop
(`spire up --steward`). Docker agent mode. Single-instance enforcement.
Health endpoint.

### 7. Monitor

```
spire status          # tower status, agent activity, sync state
spire logs            # follow daemon + agent logs
spire logs wizard-1   # specific agent
spire board           # interactive board TUI
spire board --json    # machine-readable board for agents/scripts
spire roster          # who's in the tower, what they're working on
spire watch           # live-updating view of all activity
```

**Exists today**: `spire status` shows dolt server and daemon PID/state.
`spire board` opens an interactive Bubble Tea TUI with phase columns
(READY, DESIGN, PLAN, IMPLEMENT, REVIEW, MERGE, DONE) and auto-refresh.
`spire roster` shows work grouped by epic with agent process status,
elapsed time, and progress bars. `spire watch` provides a live-updating
terminal view. `spire logs [wizard-name]` tails wizard log output.
`spire metrics` shows agent performance summary with DORA metrics.

**Not yet built**: Richer `spire status` output showing agent activity
and sync state.

---

## Local Agent Execution

### Docker mode (not yet implemented locally)

Agents run as Docker containers with:
- An ephemeral workspace — the agent clones the repo inside the container
- `ANTHROPIC_API_KEY` and GitHub credentials injected as environment
  variables (from `~/.config/spire/credentials` or env var overrides)
- Network access for git operations and LLM API calls
- One container per wizard, isolated from each other
- No host repo mount — this matches how k8s already works

Container lifecycle:
1. Steward identifies ready work and idle wizard slots
2. Daemon pulls the agent image (`spire-agent:latest`)
3. Container starts with the bead ID, repo URL, and branch as arguments
4. Agent runs: `git clone <repo-url> -b <branch> /workspace`, then
   `spire claim <bead-id>`, then `spire focus <bead-id>`, then executes
   the work using Claude Code
5. On completion, the agent pushes results (git push + spire push),
   container exits; daemon collects the result

**Not yet built**: Local Docker container spawning via `spire summon`.
Image management (pull/build). Container lifecycle tracking. Docker
images exist and work in k8s — local Docker spawning is the gap.

### Process mode (default)

Agents run as local processes. Faster startup, easier debugging. This
is the default and only local execution mode.

```
spire summon 3        # spawns 3 wizard processes
spire roster          # shows wizard status + progress bar
```

Each wizard runs as a background process (`spire wizard-run <bead-id>`)
in its own git worktree:

1. `spire summon N` queries ready beads, picks the top N by priority
2. For each bead, spawns `spire wizard-run <bead-id> --name wizard-N`
3. The wizard process:
   - Resolves the repo from the bead's prefix (repos table)
   - Creates a git worktree at `/tmp/spire-wizard/<name>/<bead-id>`
   - Claims the bead (`spire claim`)
   - Captures focus context (`spire focus`)
   - Loads `spire.yaml` for model, timeout, validation commands
   - Builds a prompt (mirrors `agent-entrypoint.sh` format)
   - Runs `claude --dangerously-skip-permissions -p <prompt> --model <model>`
   - Validates: lint, build, test (from spire.yaml)
   - Commits and pushes branch `feat/<bead-id>`
   - Updates the bead with results (comment, review-ready label)
   - Writes `result.json` for observability
   - Cleans up the worktree
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

The daemon commits and pushes after each steward cycle. Multiple machines
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
`spire wizard-run` process per bead, tracks PIDs in `wizards.json`.
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
| `spire summon` / `dismiss` | Wizard spawning (local: process mode; k8s: CRDs) |
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
| Formula system | 3 built-in formulas (`spire-epic`, `spire-bugfix`, `spire-agent-work`) with layered resolution |
| Executor | Drives formula phases: design → plan → implement (waves) → review (sage) → merge |
| Archmage identity | Tower config stores user identity for merge commit attribution |
| Credential storage | File-based (`~/.config/spire/credentials`, chmod 600), env var overrides |
| Dolt lifecycle | Auto-download binary, version pinning, managed server start/stop |
| Docker agent images | `Dockerfile.agent`, `Dockerfile.steward` |
| goreleaser + CI | Cross-compile, GitHub Actions test/release, SHA256 checksums |
| Homebrew tap | `awell-health/tap` with `bd` and `dolt` as dependencies |
| `spire version` | Prints spire version + managed dolt version and path |
| Smoke test | Docker-based smoke test (`test/smoke/Dockerfile`) validates fresh install |

### Needs to be built

| Component | Description | Blocked by |
|-----------|-------------|------------|
| `bd` embedded in `spire` | Single binary distribution (no separate `bd` install) | Deferred (spi-770) |
| Unified daemon | Merge steward loop into `spire up --steward` | Nothing |
| Docker agent spawning | Start/stop/monitor agent containers locally | Nothing |
| Single-daemon enforcement | Prevent multiple `spire up` from racing | Nothing |
| Cobra CLI migration | `--flag=value` syntax, `--help` on all commands | spi-7ywn |
| Spire TUI interactivity | Board navigation, inspector pane, in-TUI actions | spi-1syd |

---

## Design Constraints

1. **Single binary**. Everything ships as `spire` with `bd` embedded. Dolt
   is NOT embedded but spire manages its lifecycle — auto-downloads dolt if
   missing and starts/stops the dolt server. The user does not need to
   install dolt separately.

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

5. **Docker optional**. Process mode (`--mode=process`) is a first-class
   alternative. Some users will not have Docker. Process mode is also better
   for debugging and development.
