# Getting Started with Spire

This guide walks you through setting up Spire on your laptop, filing your first task, and getting an agent to land a change for it.

## What you'll need

| Requirement | Purpose | How to get it |
|-------------|---------|---------------|
| `spire` binary | The CLI for everything | `brew tap awell-health/tap && brew install spire` |
| Anthropic API key | Powers agent LLM calls | [console.anthropic.com](https://console.anthropic.com) |
| GitHub token (PAT) | Repo operations (clone, branch, push) | GitHub → Settings → Developer settings → Personal access tokens |
| DoltHub account | Optional — only if you sync tower state via DoltHub | [dolthub.com](https://www.dolthub.com) (free) |

Your GitHub token needs the `repo` and `workflow` scopes.

Spire supports three tower-state transports: local filesystem (single machine), remotesapi (laptop ↔ cluster or laptop ↔ laptop over a direct network), and DoltHub (hosted remote). DoltHub is only required if you pick that transport. See [VISION.md](VISION.md) for the sync model.

---

## Step 1: Install and configure

```bash
# Install
brew tap awell-health/tap && brew install spire

# Verify installation
spire version
```

Store your credentials:

```bash
spire config set anthropic-key sk-ant-...
spire config set github-token ghp_...
spire config set dolthub-user myusername
spire config set dolthub-password dolt_...
```

Credentials are stored at `~/.config/spire/credentials` with `chmod 600`. Environment variables override file values if set (`ANTHROPIC_API_KEY`, `GITHUB_TOKEN`, etc.).

Run the health check:

```bash
spire doctor
```

`spire doctor` checks all prerequisites. Add `--fix` to auto-repair common issues (missing dolt binary, wrong file permissions).

---

## Step 2: Create a tower

A **tower** is your shared workspace. It holds the work graph, registered repos, and agent configuration — all backed by a Dolt database.

```bash
spire tower create --name my-team
```

This command:
1. Initializes a local Dolt database
2. Saves tower config to `~/.config/spire/towers/my-team.json`
3. Optionally pushes to DoltHub if you've configured DoltHub credentials (skip this for a fully local tower)

You only do this once. Other developers can join:
- **Via DoltHub** — `spire tower attach <dolthub-url>` clones from the hosted remote
- **Via remotesapi** — `spire tower attach-cluster <dolt://host:port/db>` connects directly to a cluster's dolt server

See [multiplayer setup](#multiplayer-setup) below.

---

## Step 3: Register your repo

```bash
cd ~/code/my-project
spire repo add
```

This:
1. Detects the Git remote URL automatically
2. Assigns a prefix to this repo (e.g., `myp-`) — all beads from this repo will start with this prefix
3. Creates a `spire.yaml` in your repo if one doesn't exist
4. Creates `.beads/` for local state

You can use multiple repos in the same tower:

```bash
spire repo add --prefix=web ~/code/my-frontend
spire repo add --prefix=api ~/code/my-backend
```

Check what's registered:

```bash
spire repo list
```

---

## Step 4: Start services

```bash
spire up
```

This starts:
- The Dolt SQL server (localhost:3307)
- The sync daemon (pushes/pulls on a 2-minute interval when a remote is configured)

Leave this running. Use `spire status` to check what's running. Use `spire shutdown` to stop everything.

---

## Step 5: File your first task

```bash
spire file "Fix the login button color" -t task -p 2
```

Flags:
- `-t` — type: `task`, `bug`, `feature`, `epic`, `chore`
- `-p` — priority: `0` (P0/critical) through `4` (P4/nice-to-have)

The command returns a bead ID like `myp-a3f8`. This is your task.

You can see your work on the board:

```bash
spire board
```

Or in your terminal with live updates:

```bash
spire watch
```

---

## Step 6: Summon a wizard

A **wizard** is an AI agent that claims a bead, drives the bead's formula, and lands approved work.

```bash
spire summon 1
```

This:
1. Finds the highest-priority ready bead
2. Starts an executor process in an isolated worktree
3. Claims the bead (prevents double-work)
4. Loads context and resolves the bead's formula
5. Runs the formula phases (`plan` → `implement` → `review` → `merge` for normal work)
6. Validates with lint, build, and test commands from `spire.yaml`
7. Pushes the approved result to the repo's base branch and closes the bead

Watch the wizard work:

```bash
spire roster          # show wizard status and progress
spire logs wizard-<bead-id>   # tail a specific wizard log
spire board           # see the bead move through Backlog → Hooked → Done
```

Wizards dispatch apprentices and sages as child processes — by default through the Claude Code CLI, with Codex CLI available as an alternative backend (set `agent.backend` in `spire.yaml`).

---

## Step 7: Review what landed

When the wizard finishes, the default local executor path has already
merged the approved result onto your repo's base branch. Review the
landed diff however you normally inspect changes:

```bash
git log --oneline -1
git show
spire board
```

The important implementation detail is that the default local path does
not open a GitHub PR. It uses sage review inside Spire, then lands the
change directly by merging to `branch.base`.

---

## What just happened

```
You filed a bead → Wizard claimed it → Wizard planned + implemented it → Sage reviewed it → The executor merged it
```

The wizard followed the `task-default` formula: plan → implement → review → merge. For more complex work, use epics with the `epic-default` formula, which includes design validation, planning, wave dispatch, and sage review.

---

## Next steps

**File an epic (larger body of work):**

```bash
spire file "User authentication system" -t epic -p 1
# → returns spi-abc

# Add tasks under the epic
spire file "Implement login page" -t task -p 2 --parent spi-abc
spire file "Add JWT tokens" -t task -p 2 --parent spi-abc
spire file "Add refresh tokens" -t task -p 2 --parent spi-abc

# Set dependencies
bd dep add spi-abc.3 spi-abc.2   # refresh tokens depend on JWT

# Summon wizards — they'll work in priority order, respecting dependencies
spire summon 3
```

**Watch everything:**

```bash
spire board --epic spi-abc   # scoped to one epic
spire watch                  # live tower status
spire metrics                # performance summary
```

**Learn more:**

- [VISION.md](VISION.md) — the mental model, core concepts, and design principles
- [VISION-LOCAL.md](VISION-LOCAL.md) — what local-native mode is designed to optimize for
- [Architecture](ARCHITECTURE.md) — how the pieces fit together
- [CLI reference](cli-reference.md) — every command documented
- [spire.yaml reference](spire-yaml.md) — configure agents per-repo
- [Agent development guide](agent-development.md) — build custom agents

---

## Multiplayer setup

There are two paths, depending on your team's topology.

### Via DoltHub

Use this when both developers sync through a hosted DoltHub remote.

```bash
# Developer B attaches to the existing tower
spire tower attach https://doltremoteapi.dolthub.com/your-org/tower-name

# Configure credentials
spire config set anthropic-key sk-ant-...
spire config set github-token ghp_...
spire config set dolthub-user devb
spire config set dolthub-password dolt_...

# Start services
spire up
```

### Via cluster remotesapi

Use this when your team runs a cluster-native tower and developers attach directly to the cluster's dolt via remotesapi.

```bash
spire tower attach-cluster dolt://dolt.my-cluster.example:50051/my-team

# Configure credentials (no DoltHub needed)
spire config set anthropic-key sk-ant-...
spire config set github-token ghp_...

# Start services (sync goes over remotesapi, not DoltHub)
spire up
```

### Multiple stewards

Each machine runs its own steward by default — `spire up` starts dolt + daemon + steward together. Steward instances are scoped by instance identity (`~/.config/spire/instance.json`) and coordinate through attempt leases in dolt, so multiple stewards can coexist without racing — each only manages its own agents. If a machine should run sync infrastructure only (no assignment), use `spire up --no-steward`.

### Working across multiple towers

Each `spire` command resolves its target tower from (in order): the `--tower` flag, the `SPIRE_TOWER` environment variable, and the active tower binding. Filesystem walk-up is a convenience fallback, not the primary resolution path — use `--tower` or `SPIRE_TOWER` explicitly when you have more than one tower configured.

---

## Troubleshooting

See [troubleshooting.md](troubleshooting.md) for common issues and fixes.

Quick fixes:

```bash
spire doctor --fix    # auto-repair common issues
spire status          # check what's running
spire logs            # tail daemon logs
```
