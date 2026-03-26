# Getting Started with Spire

This guide walks you through setting up Spire on your laptop, filing your first task, and getting an agent to open a pull request for it.

## What you'll need

| Requirement | Purpose | How to get it |
|-------------|---------|---------------|
| `spire` binary | The CLI for everything | `brew tap awell-health/tap && brew install spire` |
| Anthropic API key | Powers agent LLM calls | [console.anthropic.com](https://console.anthropic.com) |
| GitHub token (PAT) | Repo operations (clone, branch, PR) | GitHub → Settings → Developer settings → Personal access tokens |
| DoltHub account | Remote sync of bead state | [dolthub.com](https://www.dolthub.com) (free) |

Your GitHub token needs the `repo` and `workflow` scopes.

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
2. Creates a DoltHub remote for sync and collaboration
3. Saves tower config to `~/.config/spire/towers/my-team.json`

You only do this once. Other developers on your team will use `spire tower attach` (see [multiplayer setup](#multiplayer-setup) below).

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
- The sync daemon (DoltHub push/pull on a 2-minute interval, Linear integration if configured)

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

A **wizard** is an AI agent that claims a bead, implements it, and opens a pull request.

```bash
spire summon 1
```

This:
1. Finds the highest-priority ready bead
2. Creates a git worktree at `/tmp/spire-wizard/wizard-1/<bead-id>`
3. Claims the bead (prevents double-work)
4. Loads context via `spire focus`
5. Runs Claude Code to implement the task
6. Validates with lint, build, and test commands from `spire.yaml`
7. Pushes a branch and opens a pull request

Watch the wizard work:

```bash
spire roster          # show wizard status and progress
spire logs wizard-1   # tail wizard log output
spire board           # see the bead move from READY → IMPLEMENT → REVIEW → DONE
```

---

## Step 7: Review the PR

When the wizard finishes, it opens a pull request on GitHub. The PR includes:
- The implementation
- A link to the bead in the description

Review the PR as you would any other. If you need changes, comment on the PR. The wizard monitors for review feedback and can re-implement based on your comments.

When you're satisfied, merge the PR. The bead closes automatically.

---

## What just happened

```
You filed a bead → Wizard claimed it → Wizard implemented it → Wizard opened a PR → You reviewed and merged
```

The wizard followed the `spire-agent-work` formula: implement → review → merge. For more complex work, use epics with the `spire-epic` formula, which includes planning, wave dispatch, and sage review.

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

- [Concepts: towers, beads, agents](VISION.md) — the mental model
- [Architecture](ARCHITECTURE.md) — how the pieces fit together
- [CLI reference](cli-reference.md) — every command documented
- [spire.yaml reference](spire-yaml.md) — configure agents per-repo
- [Agent development guide](agent-development.md) — build custom agents

---

## Multiplayer setup

To add another developer to your tower:

**Developer B:**

```bash
# Attach to the existing tower
spire tower attach https://doltremoteapi.dolthub.com/your-org/tower-name

# Configure credentials
spire config set anthropic-key sk-ant-...
spire config set github-token ghp_...
spire config set dolthub-user devb
spire config set dolthub-password dolt_...

# Start services
spire up
```

Both developers share the same work graph. Beads sync automatically via DoltHub. Only one machine should run `spire up --steward` at a time (the steward assigns work — two stewards would race).

---

## Troubleshooting

See [troubleshooting.md](troubleshooting.md) for common issues and fixes.

Quick fixes:

```bash
spire doctor --fix    # auto-repair common issues
spire status          # check what's running
spire logs            # tail daemon logs
```
