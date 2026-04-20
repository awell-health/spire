# Spire Concepts

This document explains the core concepts in Spire: towers, beads, agents, prefixes, and the sync model. Understanding these will help you reason about how the system works and why it is designed the way it is.

---

## Tower

A **tower** is a shared workspace. It holds:
- The work graph (all beads, dependencies, hierarchy)
- Registered repos and their prefixes
- Agent configuration
- Sync state

Every tower is backed by a [Dolt](https://www.dolthub.com/blog/2022-04-12-dolt-is-git-for-data/) database — a SQL database with Git semantics. This means the tower has full version history, branching, merging, and can be pushed to [DoltHub](https://www.dolthub.com) for remote collaboration.

**One tower per team.** Multiple developers attach to it. Multiple repos register under it. All agents read from and write to the same tower.

```
                     Tower (Dolt database)
                     ├── work graph (beads)
                     ├── repos + prefixes
                     ├── agent registry
                     └── messages
                           |
          ┌───────────────-+────────────────┐
          |                                 |
  Developer A (laptop)               Developer B (laptop)
  spire push → DoltHub ← spire pull  spire push → DoltHub ← spire pull
```

### Creating a tower

```bash
spire tower create --name my-team
```

This initializes a local Dolt database and pushes it to DoltHub. You only do this once. Other developers join with `spire tower attach`.

### Joining a tower

```bash
spire tower attach https://doltremoteapi.dolthub.com/your-org/tower-name
```

This clones the tower database locally and bootstraps the local config. Both developers then share the same work graph, synced automatically via DoltHub.

---

## Beads

**Beads** are work items. They are the atoms of the work graph.

Each bead has:
- **ID**: prefix + short hash (e.g., `web-a3f8`)
- **Title**: one-line description
- **Type**: `task`, `bug`, `feature`, `epic`, or `chore`
- **Status**: `open`, `in_progress`, or `closed`
- **Priority**: 0 (P0/critical) through 4 (P4/nice-to-have)
- **Assignee**: who owns it (human or agent)
- **Parent**: parent bead ID (for hierarchy)
- **Dependencies**: blocking relationships
- **Comments** and **messages**: collaboration history

Beads form a directed graph:

```
spi-a3f8 (epic: Auth system)
├── spi-a3f8.1 (task: Implement login page)
├── spi-a3f8.2 (task: Add JWT tokens)
│   └── spi-a3f8.2.1 (task: Token refresh)
└── spi-a3f8.3 (task: Add MFA)
    └── [blocked by spi-a3f8.2]
```

### Filing a bead

```bash
spire file "Fix the login button" -t bug -p 2
# → myp-b7d0
```

### Bead states

| Status | Meaning |
|--------|---------|
| `open` | Waiting to be worked on |
| `in_progress` | Claimed by an agent or developer |
| `closed` | Done (or abandoned) |

A bead is **ready** when it is open and has no open blocking dependencies. `bd ready --json` returns the ready queue.

### Beads CLI

Beads are powered by the `bd` CLI. All bead operations go through `bd`:

```bash
bd list --json           # all beads
bd ready --json          # beads with no open blockers
bd show <id>             # bead details
bd dep add <blocked> <blocker>  # add a dependency
```

The `spire file`, `spire claim`, and `spire close` commands wrap `bd` with additional sync and validation logic.

---

## Agents

**Agents** are AI workers. They read beads from the work graph, claim them, do the work, and report results.

### Agent roles

| Role | What it does |
|------|-------------|
| **Wizard** | Drives a bead end-to-end: claims bead → plans → dispatches work → lands approved change |
| **Sage** | Reviews wizard output, approves or requests changes |
| **Artificer** | Creates and tests formulas with `spire workshop` |
| **Steward** | Cluster coordinator: reads the ready queue, creates workloads, assigns agents |

### How agents work

1. Agent reads the ready queue (`bd ready --json`)
2. Claims a bead (`spire claim <id>`) — atomic pull-claim-push
3. Assembles context (`spire focus <id>`)
4. Executes the bead's formula (plan → implement → review → merge for standard work)
5. Reports results, closes the bead

### Summoning agents locally

```bash
spire summon 3        # spawn 3 wizard processes
spire roster          # watch their progress
spire dismiss --all   # send SIGINT to stop them
```

Each wizard runs as a subprocess in an isolated git worktree. Wizards are Claude Code agents driven by a structured prompt formula.

### Agent communication

Agents communicate through messages stored in the bead graph:

```bash
# Send a message to an agent
spire send wizard-1 "Blocked on auth — please check spi-a3f8.2 first" --ref spi-a3f8.3

# Check inbox
spire collect

# Read inbox without hitting the database (fast path)
spire inbox --watch
```

Messages are routed via labels (`to:<agent>`, `from:<agent>`, `ref:<bead-id>`). The inbox file is updated by the daemon so agents can poll it cheaply.

---

## Prefixes

Each registered repo gets a **prefix** — a short identifier that scopes all beads from that repo.

```
Repo                    Prefix    Example bead ID
----                    ------    ---------------
your-org/web-app   →    web-      web-a3f8
your-org/api-server →   api-      api-b7d0
your-org/mobile    →    mob-      mob-c91e
```

Prefixes:
- Are unique within a tower (no two repos share a prefix)
- Are 2–6 characters, lowercase, alphanumeric
- Are included in every bead ID from that repo
- Allow routing: `spire claim` and the operator use the prefix to match beads to agents

When you run `spire repo add`, you are assigned a prefix. You can specify one with `--prefix`:

```bash
spire repo add --prefix web ~/code/web-app
spire repo add --prefix api ~/code/api-server
```

To filter beads by repo:

```bash
bd list --json | jq '.[] | select(.id | startswith("web-"))'
```

### Prefix routing in the cluster

In Kubernetes, each `WizardGuild` CRD declares which prefixes it handles:

```yaml
spec:
  prefixes: ["web-", "api-"]
```

The operator only assigns workloads where the bead prefix matches. This lets you run separate agent pools for different repos or teams, all sharing the same tower.

---

## Sync model

Spire uses **DoltHub** as the sync layer between all participants (developers, agents, clusters). There is no direct connectivity between machines — DoltHub is the hub.

```
Developer A ──push──> DoltHub <──pull── Cluster
Developer B ──push──> DoltHub <──push── Cluster (after work completes)
```

### How sync works

| Operation | Command | What happens |
|-----------|---------|--------------|
| Push | `spire push` | Commits local dolt changes → pushes to DoltHub |
| Pull | `spire pull` | Fast-forward pull from DoltHub → local |
| Merge | `spire sync --merge` | Three-way merge when histories diverged |

The **daemon** (`spire up`) automates push/pull on a configurable interval (default: 2 minutes). It also handles Linear epic sync and webhook queue processing.

### Conflict resolution

Dolt uses Git merge semantics. Row-level conflicts are rare because:
- Each bead has a unique ID (no two agents claim the same bead — `spire claim` is atomic)
- Agents write to different rows (different bead IDs)
- The daemon pulls before pushing each cycle

When conflicts do occur (two machines filed beads without syncing), use `spire sync --merge` to trigger a three-way merge.

### Local-first design

You can file beads, build a backlog, and configure agents entirely offline. Push to DoltHub when ready. The cluster will pick up work on its next pull cycle.

This also means the cluster can go down without losing work — everything is in DoltHub, and your local copy is always consistent with the last sync.

### Sync in the cluster

In Kubernetes, a **syncer CronJob** handles periodic `spire pull && spire push`:

```yaml
# Applied by the Helm chart; runs every 2 minutes
kind: CronJob
spec:
  schedule: "*/2 * * * *"
```

Or enable the chart's syncer directly (`syncer.enabled: true` in values.yaml).

---

## Putting it together

```
You create a tower (Dolt database, pushed to DoltHub)
  └── You register repos (each gets a prefix)
       └── You file beads (work items in the graph)
            └── You run spire up (daemon starts syncing)
                 └── You summon capacity (or run the steward)
                      └── Wizard claims bead, implements, and gets reviewed
                           └── Approved work merges to the base branch and the bead closes
```

For deeper reading:
- [VISION.md](VISION.md) — design philosophy and product direction
- [ARCHITECTURE.md](ARCHITECTURE.md) — component details and data model
- [Getting started guide](getting-started.md) — hands-on walkthrough
