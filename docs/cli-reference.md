# Spire CLI Reference

Complete reference for all `spire` commands.

For the `bd` (beads) CLI reference, see the [beads documentation](https://github.com/steveyegge/beads).

---

## Global flags

```
--tower <name>    Override active tower for this command
```

---

## Setup commands

### `spire tower create`

Create a new tower (shared workspace backed by Dolt).

```bash
spire tower create --name my-team [--dolthub org/repo] [--prefix spi]
```

| Flag | Description |
|------|-------------|
| `--name` | Tower name (required) |
| `--dolthub` | DoltHub remote to create/use (e.g., `myorg/my-tower`) |
| `--prefix` | Hub prefix for beads (auto-generated if omitted) |

Creates a local Dolt database, generates tower identity, pushes to DoltHub, and writes `~/.config/spire/towers/<name>.json`.

### `spire tower attach`

Join an existing tower from DoltHub.

```bash
spire tower attach <dolthub-url> [--name local-name]
```

Clones the tower database locally, bootstraps `.beads/`, and writes local tower config. Use this when a second developer joins a team that already has a tower.

### `spire tower list`

List all configured towers.

```bash
spire tower list
```

### `spire tower use`

Set the active tower for subsequent commands.

```bash
spire tower use <name>
```

### `spire repo add`

Register a repo under the active tower.

```bash
spire repo add [path] [--prefix web] [--repo-url https://github.com/org/repo] [--branch main]
```

| Flag | Description |
|------|-------------|
| `--prefix` | Short prefix for bead IDs (e.g., `web` → beads like `web-a3f8`) |
| `--repo-url` | Git remote URL (auto-detected from `git remote` if omitted) |
| `--branch` | Default branch (default: `main`) |

Validates prefix uniqueness, writes to the `repos` table in dolt, creates `.beads/` in the repo, generates `spire.yaml` if missing, and pushes registration to DoltHub.

### `spire repo list`

List repos registered in the active tower.

```bash
spire repo list [--json]
```

### `spire repo remove`

Remove a repo from the tower.

```bash
spire repo remove <prefix>
```

### `spire config`

Read and write config values and credentials.

```bash
spire config set <key> <value>     # store a credential or config value
spire config get <key>             # read a value (masked by default)
spire config get <key> --unmask    # show the actual value
spire config list                  # show all configured keys (masked)
```

**Credential keys:**

| Key | Description |
|-----|-------------|
| `anthropic-key` | Anthropic API key (`sk-ant-...`) |
| `github-token` | GitHub personal access token (`ghp_...`) |
| `dolthub-user` | DoltHub username |
| `dolthub-password` | DoltHub token |

**Config keys:**

| Key | Description |
|-----|-------------|
| `identity` | Your name/identity for bead attribution |
| `dolt.port` | Local dolt server port (default: 3307) |
| `daemon.interval` | Sync interval (default: 2m) |
| `dolthub.remote` | DoltHub remote URL |

Credentials are stored at `~/.config/spire/credentials` (chmod 600). Environment variables override file values: `ANTHROPIC_API_KEY`, `GITHUB_TOKEN`, `DOLT_REMOTE_USER`, `DOLT_REMOTE_PASSWORD`.

### `spire doctor`

Health checks and auto-repair.

```bash
spire doctor [--fix]
```

Checks 11 items in 3 categories:
- **System**: dolt binary, dolt version, Docker available
- **Tower**: tower config, database reachable, `.beads/` present
- **Credentials**: anthropic-key, github-token, dolthub credentials, credential file permissions

`--fix` auto-repairs: downloads missing dolt binary, starts dolt server, fixes credential file permissions, regenerates `.beads/` config.

---

## Sync commands

### `spire push`

Push local database to DoltHub.

```bash
spire push [url]
```

Commits local dolt changes and pushes to the configured DoltHub remote.

### `spire pull`

Pull from DoltHub.

```bash
spire pull [url] [--force]
```

Fast-forward pull by default. Use `--force` to overwrite local changes. The daemon runs this automatically on each sync cycle.

### `spire sync`

Three-way merge pull for diverged histories.

```bash
spire sync --merge
```

Use when a fast-forward pull fails due to diverged history (e.g., two machines filed beads without syncing first).

---

## Lifecycle commands

### `spire up`

Start the dolt server and sync daemon.

```bash
spire up [--interval 2m] [--steward]
```

| Flag | Description |
|------|-------------|
| `--interval` | Daemon sync interval (default: `2m`) |
| `--steward` | Also run the steward for autonomous work dispatch |

`spire up` without `--steward` starts infrastructure only. Add `--steward` to enable autonomous work dispatch. Running it again when already up shows current status.

### `spire down`

Stop the sync daemon (dolt server keeps running).

```bash
spire down
```

### `spire shutdown`

Stop the sync daemon and dolt server.

```bash
spire shutdown
```

### `spire status`

Show running services, agents, and work queue.

```bash
spire status
```

Prints: dolt server state (PID, reachability), daemon state (PID, last sync), active wizard count.

### `spire logs`

Tail agent or system logs.

```bash
spire logs [wizard-name] [--daemon] [--dolt]
```

| Flag | Description |
|------|-------------|
| (no args) | Tail daemon log |
| `wizard-name` | Tail a specific wizard's log |
| `--daemon` | Tail daemon log explicitly |
| `--dolt` | Tail dolt server log |

Logs are stored at `~/.local/share/spire/wizards/<name>.log`.

---

## Work commands

### `spire file`

Create a bead (work item).

```bash
spire file "<title>" [--prefix web] -t <type> -p <priority> [--parent <id>] [--label <label>]
```

| Flag | Description |
|------|-------------|
| `--prefix` | Repo prefix to use (default: current directory's repo) |
| `-t, --type` | Bead type: `task`, `bug`, `feature`, `epic`, `chore` |
| `-p, --priority` | Priority: `0` (P0/critical) – `4` (P4/nice-to-have) |
| `--parent` | Parent bead ID (for sub-tasks) |
| `--label` | Label to add (repeatable) |

**Always set both `-t` and `-p`.** The type determines which formula the wizard uses. The priority determines dispatch order.

### `spire design`

Create a design bead (brainstorming/exploration artifact).

```bash
spire design "<title>" [-p <priority>]
```

Design beads are thinking artifacts, not work items. They appear on the board in the DESIGN column but are filtered out of `spire summon`'s ready queue. Use them to capture exploration before filing implementation tasks.

### `spire spec`

Scaffold a spec and optionally file it as a bead.

```bash
spire spec "<title>" [--no-file] [--break <epic-id>]
```

| Flag | Description |
|------|-------------|
| `--no-file` | Write the spec to disk only, don't create a bead |
| `--break` | Break an existing epic into subtasks based on the spec |

### `spire claim`

Atomically claim a bead.

```bash
spire claim <bead-id>
```

Pulls from DoltHub, verifies the bead is claimable (open, unowned), sets status to `in_progress` with your identity, and pushes. Fails if the bead is already owned by someone else.

Always claim before working: `spire claim` prevents double-work.

### `spire close`

Force-close a bead.

```bash
spire close <bead-id>
```

Removes phase labels, closes molecule steps, and sets status to `closed`. Use for work you're abandoning or that was completed outside the normal flow.

### `spire advance`

Advance a bead to the next formula phase.

```bash
spire advance <bead-id>
```

Moves the bead forward one phase in its formula (e.g., `implement` → `review`). Closes the bead if it's at the last phase.

### `spire focus`

Assemble read-only context for a task.

```bash
spire focus <bead-id>
```

Outputs: bead details, workflow progress, referenced design beads, recent messages, comments. Also pours the workflow molecule on first focus. Wizards call this automatically.

### `spire grok`

Focus with live Linear context.

```bash
spire grok <bead-id>
```

Like `spire focus` but also fetches the linked Linear issue for additional context (requires Linear integration).

### `spire workshop`

Start a wizard workshop for an epic.

```bash
spire workshop <epic-id>
```

Launches the artificer for long-running epic management. Used in k8s workshop pods; for local work, `spire summon` is the typical path.

---

## Agent commands

### `spire summon`

Summon wizard agents to claim and work ready beads.

```bash
spire summon [n] [--targets <ids>] [--auto]
```

| Flag | Description |
|------|-------------|
| `n` | Number of wizards to summon (default: 1) |
| `--targets` | Comma-separated bead IDs to target directly |
| `--auto` | Keep summoning as work becomes available |

Each wizard runs as a local process in an isolated git worktree, driven by the bead's formula. The formula is determined by bead type (`task`→`spire-agent-work`, `bug`→`spire-bugfix`, `epic`→`spire-epic`).

### `spire dismiss`

Dismiss running wizards.

```bash
spire dismiss [n] [--all]
```

Sends SIGINT to wizard processes. Wizards finish their current step before exiting.

### `spire roster`

Show work grouped by epic and agent status.

```bash
spire roster
```

Displays: beads in progress per epic, wizard process status, elapsed time, progress bar. Dead processes are cleaned up automatically.

---

## Messaging commands

### `spire send`

Send a message to an agent or bead.

```bash
spire send <to> "<message>" [--ref <bead-id>] [--thread <msg-id>] [--priority <0-4>]
```

`<to>` can be an agent name or bead ID. Messages are stored in the bead graph and routed via labels (`to:<agent>`, `from:<agent>`).

### `spire collect`

Check inbox for messages (queries the database).

```bash
spire collect [name]
```

Prints new messages addressed to `name` (or your registered identity if omitted). Use `spire inbox` for fast file-based reads in hot paths.

### `spire inbox`

Read the local inbox file.

```bash
spire inbox [agent-name] [--check] [--watch] [--json]
```

| Flag | Description |
|------|-------------|
| `--check` | Silent if empty, non-zero exit if new messages |
| `--watch` | Block until new messages arrive |
| `--json` | Output as JSON |

The daemon writes inbox state to a local file. `spire inbox` reads that file without hitting the database — use it for high-frequency checks (wizard main loops, hooks).

### `spire read`

Mark a message as read.

```bash
spire read <bead-id>
```

---

## Observability commands

### `spire board`

Interactive Kanban board TUI.

```bash
spire board [--mine] [--ready] [--epic <id>] [--json]
```

| Flag | Description |
|------|-------------|
| `--mine` | Show only beads assigned to you |
| `--ready` | Show only ready (unblocked) beads |
| `--epic <id>` | Scope to one epic |
| `--json` | Machine-readable output (no TUI) |

Board columns: READY → DESIGN → PLAN → IMPLEMENT → REVIEW → MERGE → DONE → BLOCKED. Empty columns collapse.

**Keyboard shortcuts (TUI mode):**

| Key | Action |
|-----|--------|
| `↑/↓` | Navigate beads |
| `←/→` | Switch columns |
| `enter` | Select bead |
| `q` | Quit |
| `r` | Refresh |
| `j/k` | Vi-style navigation |

### `spire watch`

Live-updating activity view.

```bash
spire watch [bead-id]
```

Without arguments: shows tower activity (agents, recent events). With a bead ID: shows epic progress with countdown.

### `spire metrics`

Agent run metrics and DORA metrics.

```bash
spire metrics [--bead <id>] [--model] [--json]
```

| Flag | Description |
|------|-------------|
| `--bead <id>` | Filter to one bead |
| `--model` | Break down cost by model |
| `--json` | Machine-readable output |

See [metrics.md](metrics.md) for the full metrics reference.

### `spire alert`

Alert on bead state changes.

```bash
spire alert [bead-id] [--type <type>] [-p <priority>]
```

Sends a priority alert, optionally referencing a bead. Used by wizards to flag issues requiring human attention.

---

## Advanced commands

### `spire register`

Register an agent identity.

```bash
spire register <name>
```

Required for agents that receive messages. Sets up routing labels and inbox.

### `spire unregister`

Unregister an agent identity.

```bash
spire unregister <name>
```

### `spire daemon`

Run the sync daemon directly (without `spire up`).

```bash
spire daemon [--interval 2m] [--once]
```

The daemon handles: DoltHub sync, Linear epic sync, webhook queue processing.

### `spire steward`

Run the work coordinator.

```bash
spire steward [--once] [--dry-run]
```

The steward queries ready beads, checks capacity, and assigns work. In k8s, it creates SpireWorkload CRDs. Locally, it's a planning layer — actual spawning is via `spire summon`.

### `spire serve`

Run the webhook receiver.

```bash
spire serve [--port 8080]
```

Listens for inbound webhooks (e.g., Linear, GitHub) and writes them to the processing queue.

### `spire connect`

Connect an integration.

```bash
spire connect linear
```

Interactive OAuth2 or API key flow. Stores credentials in `~/.config/spire/credentials`.

### `spire disconnect`

Disconnect an integration.

```bash
spire disconnect linear
```

### `spire version`

Print version information.

```bash
spire version
```

Prints: spire version, managed dolt version and path.

---

## Hidden commands (agent use)

These commands are used internally by wizards and are not part of the normal user workflow:

| Command | Description |
|---------|-------------|
| `spire wizard-run <bead-id>` | Run a wizard for a bead (spawned by `summon`) |
| `spire wizard-review` | Run a sage review step |
| `spire wizard-merge` | Run a merge step |
| `spire execute <bead-id>` | Execute a single formula phase |
