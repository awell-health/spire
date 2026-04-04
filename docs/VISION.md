# Spire Vision

> An open-source coordination hub for AI engineering agents.

You file work. Agents implement it. Changes land on your repos.

---

## What Spire Is

Spire is infrastructure for directing AI agents to do real engineering work. You describe what you want built -- features, bug fixes, tasks -- and AI agents clone your repos, write code, and land reviewed changes.

It is not a chatbot. It is not a copilot. It is a work system: a structured graph of tasks, a protocol for agent coordination, and a sync layer that lets multiple developers and multiple agents collaborate on the same body of work from anywhere.

A single developer can run Spire on a laptop. A team can share a tower backed by DoltHub, with agents running in a Kubernetes cluster. The experience scales from `brew install` to production infrastructure without changing the mental model.

## Core Concepts

### Tower

A tower is a shared workspace identity. It contains the work graph, registered repos, agent configuration, and sync state. Every tower is backed by a Dolt database, which means it has full version history, branch/merge semantics, and can sync to DoltHub for collaboration.

One tower per team. Multiple developers attach to it. Multiple repos register under it. All agents read from and write to the same tower.

### Beads

Beads are work items: tasks, bugs, features, epics, chores. They live in the tower's Dolt database and form a directed graph of dependencies and hierarchy. Each bead has a status, priority, assignee, comments, and messages.

Beads are powered by the `bd` CLI framework. Everything is queryable, scriptable, and JSON-serializable. The work graph is the single source of truth for what needs to happen, what is in progress, and what is done.

### Agents

Agents are AI-powered workers. Each agent has a role, a set of capabilities, and a communication protocol. Agents read from the work graph, claim tasks, execute them, and report results. They coordinate through structured messages routed by bead references.

Agents can run as local processes, Docker containers, or Kubernetes pods. The protocol is the same regardless of execution environment.

### Prefix

Each registered repo gets a unique prefix within the tower. Bead IDs include the prefix for routing: `web-a3f8` belongs to the web repo, `api-b7d0` belongs to the API repo. Prefixes are short (3-4 characters) and human-readable. They make it possible to manage work across many repos in a single graph without ID collisions.

## Design Principles

### 1. Local-first, cluster-optional

Spire works on a laptop. `brew install spire && spire up && spire summon 1` gets you a running tower with a local Dolt database, a daemon, and agent execution via subprocesses by default. Kubernetes is for teams that need persistent agents, autoscaling, and managed infrastructure. It is never required.

### 2. User-first bootstrap

You can create a tower and file work before any infrastructure exists. The cluster adopts the tower, not the other way around. This means a developer can build up a backlog, register repos, and configure priorities before ever deploying a single agent pod. When the cluster comes online, it reads the tower and starts working.

### 3. Explicit over magic

`spire push` and `spire pull` move state between your machine and DoltHub. You control when sync happens. The background daemon (`spire up`) automates this on an interval, but it is opt-in convenience, not a requirement. There are no hidden background processes mutating your work graph without your knowledge.

### 4. DoltHub as the sync layer

No direct connectivity is needed between your laptop and a cluster. DoltHub mediates all collaboration. It is the "GitHub for your work graph" -- a versioned, mergeable, auditable intermediary. Push from your laptop, pull from the cluster. Push from the cluster, pull from your laptop. The database handles merge semantics.

### 5. Open protocol

Beads and Spire define how agents coordinate: how work is filed, claimed, executed, and completed. The protocol is open. Anyone can build agents that speak it, storage backends that host it, or integrations that extend it. Spire ships with opinionated defaults (Anthropic models, DoltHub sync, GitHub integration) but none of these are locked in.

## The 5-Minute Experience

```bash
brew install spire
spire tower create --name my-team
spire repo add
spire file "Add dark mode" -t feature -p 2
spire up
spire summon 1
```

A short path from zero to an AI agent run on your repo.

`spire tower create` initializes a Dolt database and pushes it to DoltHub. `spire repo add` scans the current directory, assigns a prefix, and records the repo in the tower. `spire file` creates a bead. `spire up` starts the local Dolt server and daemon. `spire summon` provides local capacity by starting an executor, which runs the bead's formula and lands approved work onto the repo's base branch. `spire up --steward` can also start the coordinator loop, but explicit `summon` is still the common local entry point.

You watch the board move. Sages review the implementation. Approved work merges to the repo's base branch. The bead closes.

## Architecture Layers

| Layer | What it is | Where it runs |
|-------|-----------|---------------|
| **Spire Core** | Core CLI surface: tower management, bd-backed work graph, agent protocol, sync | Everywhere |
| **Spire Local** | Local daemon, optional steward, agent spawning (process default, Docker optional) | Laptop |
| **Spire Cluster** | Operator, steward, managed agent pods, autoscaling, PVCs | Kubernetes |
| **Spire Hosted** | Managed towers, team dashboard, GitHub App (future) | Cloud |

### Spire Core

The `spire` CLI wraps `bd` (the beads CLI) today and adds tower management, repo registration, agent messaging, and DoltHub sync. The supported install path ships `bd` alongside `spire`; the user-facing command surface is still a single tool.

### Spire Local

On a laptop, `spire up` starts a local Dolt server and daemon. `spire up --steward` also starts the steward as a sibling process. Agents run as child processes by default, with a Docker backend available when configured. All state lives in `~/.config/spire/` and the local Dolt data directory.

### Spire Cluster

In Kubernetes, Spire deploys via a Helm chart. The chart currently renders explicit `SpireAgent` objects, and the operator manages pods from those CRs. The steward runs as a persistent deployment. Wizard pods are ephemeral -- one per task, terminated on completion. A syncer pod can handle DoltHub push/pull on interval. Secrets (API keys, GitHub tokens) are stored in Kubernetes secrets.

### Spire Hosted (Future)

Managed towers with a web dashboard, GitHub App integration for automatic repo registration, team management with RBAC, and a hosted execution environment. The hosted layer is additive -- it runs the same Spire Core underneath.

## Agent Roles

Spire uses RPG-inspired naming for agent roles. The names are deliberate: they convey function, hierarchy, and personality.

### Archmage

You. The human. You write specs, file work, review what agents produce, and make the architecture calls. You bounce from tower to tower, steering work. The archmage's identity is stored in the tower config and used for merge commit attribution.

### Steward

The global coordinator. One per tower. The steward reads the work graph continuously, identifies ready work (no open blockers, not claimed), summons wizards to handle it, routes messages between agents, and tracks overall progress. It does not write code. It orchestrates capacity — deciding which work to start and when, based on priority, dependencies, and available resources.

### Wizard

The per-bead orchestrator. One per bead, ephemeral. A wizard is summoned to drive a bead through its spell (formula) lifecycle — validating design, generating a plan, dispatching apprentices to implement, consulting sages to review, and sealing the work with a merge. The wizard does not write code directly. It orchestrates the agents that do.

### Apprentice

The implementer. One-shot, ephemeral. An apprentice receives a task from a wizard, works in an isolated git worktree, writes code, runs tests, and pushes a feature branch. When the branch is pushed, the apprentice's job is done. Apprentices are stateless — they read everything they need from the bead and the repo.

### Sage

The reviewer. One-shot, ephemeral. A sage reviews the implementation against the spec and returns a verdict: approve or request changes. If changes are needed, the wizard dispatches a fix apprentice and re-consults the sage. After max rounds, an arbiter (Claude Opus) breaks the tie.

### Arbiter

The tie-breaker. Invoked when a sage and apprentice cannot converge after the maximum number of review rounds. The arbiter (Claude Opus) examines the full review history, the spec, and the code, then renders a binding verdict: accept the implementation, accept the sage's objections, or prescribe a specific resolution. One-shot, ephemeral.

### Artificer

The formula maker. The artificer crafts and maintains spells (formulas) — the TOML-based recipes that wizards follow to drive beads through their lifecycle. It works at the Workshop CLI (`spire workshop`) for authoring, validating, testing, and publishing formulas before they are used by the tower. The artificer does not orchestrate epics or review code — that is the wizard's and sage's domain.

### Familiar

A per-agent companion that runs alongside each wizard or apprentice. The familiar manages all communication between its agent and the Archive (the tower's Dolt database) — reading and writing beads, relaying messages, handling control signals (STOP, STEER, PAUSE, RESUME), posting liveness heartbeats, and serving health endpoints. In k8s it runs as a sidecar container (`cmd/spire-sidecar/`); locally it runs as a goroutine within the agent process.

### Workshop (CLI Tool)

The Workshop is the dedicated CLI where the artificer creates, validates, and tests spells (formulas). It provides a local sandbox for iterating on formula definitions — phase pipelines, model requirements, context rules — before publishing them to the tower for wizards to consume.

## Sync Model

Spire uses DoltHub as a three-way sync intermediary:

```
Laptop  <-->  DoltHub  <-->  Cluster
```

There is no direct connection between laptop and cluster. All state flows through DoltHub.

### Merge Semantics

Each field has a single owner. Conflicts are resolved by ownership, not by timestamp.

| Field type | Owner | Rule |
|-----------|-------|------|
| Status fields (status, owner, assignee) | Cluster | Cluster is authoritative; user edits to these fields are overwritten on next pull |
| Content fields (title, description, priority, type) | User | User is authoritative; cluster edits to these fields are overwritten on next push |
| Comments and messages | Append-only | Both sides append; no conflicts possible |

### Sync Commands

- `spire push` -- push local state to DoltHub
- `spire pull` -- pull remote state from DoltHub
- `spire up` -- start background daemon that syncs on interval (default: 2 minutes)

The daemon is a convenience. Manual push/pull always works. You are never forced into automatic sync.

## Auth Model

Spire requires credentials for three services:

| Service | Purpose | Credential key |
|---------|---------|---------------|
| DoltHub | Database sync | `dolt_remote_user` / `dolt_remote_password` |
| GitHub | Repo access, branch pushes | `github_token` (PAT v1), GitHub App (v2) |
| Anthropic | LLM agent execution | `anthropic_api_key` |

On a laptop, credentials are stored in `~/.config/spire/credentials` (chmod 600). This file is the canonical credential store. Environment variables (`DOLT_REMOTE_USER`, `ANTHROPIC_API_KEY`, etc.) can override file-based credentials for CI/CD and ephemeral environments, but they are not the primary storage mechanism. In a cluster, credentials are stored in Kubernetes secrets and mounted into agent pods.

Tower-level secrets are scoped to the tower. A developer attaching to a tower brings their own credentials -- no shared API keys required for local use.

## Roadmap

### Shipped: Local Experience + V3 Engine

The single-developer, single-laptop experience is complete. The v3 graph
executor is the only execution engine. Tower-level formula sharing,
recovery beads, and an interactive board TUI are all live.

- Single `spire` CLI with `bd` subprocess wrapper and `pkg/store` API
- `spire tower create` / `spire tower attach` / `spire repo add`
- Local agent execution via subprocesses (Docker optional)
- DoltHub sync with `spire push` / `spire pull` and daemon auto-sync
- `spire file` / `spire claim` / `spire focus` / `spire summon` workflow
- V3 graph executor with declarative step graphs, conditions, nestable sub-graphs
- Tower-level formulas stored in dolt, synced across machines
- Recovery as a first-class bead type with prior-learning lookup
- Interactive board TUI with cursor navigation, inline actions, inspector pane

### V1.0: Production-Ready Open Source

- Complete v2 dead code removal
- Operational steward (unified daemon, concurrency limits, ready-gate workflow)
- Kubernetes / Helm operational (bootstrap job, repos-table-derived agents)
- Workshop as a Claude Code skill (formula design, simulation, testing)
- Multi-mode TUI (Board, Agents, Workshop, Messages, Metrics via Tab)
- Multi-backend agent support (Claude Code, Codex CLI, Cursor CLI)

See [PLAN.md](PLAN.md) for the full v1.0 roadmap.

### Post-V1.0: Product

- Hosted towers (managed DoltHub + managed compute)
- Web dashboard for work graph visualization
- GitHub App for zero-config repo registration
- Team features: audit logs, RBAC, approval gates
- Usage analytics and cost tracking for LLM spend
- MCP tool surface for formula extensibility

---

## Open Questions

### Observability before flexibility

Spire is infrastructure for agentic development workflows. Before making
the pipeline fully extensible with custom tools and MCP integrations, we
need deep observability into what the existing pipeline produces.

**Resolved in v0.30-v0.32:**

- **Formula versioning and metrics**: Tower-level formulas are stored in
  dolt with full commit history. The `agent_runs` table now tracks
  `formula_name`, `formula_version`, and `formula_source` (tower, repo,
  or embedded). Correlation between formula changes and agent performance
  is now possible.

- **How opinionated should the graph be?** Resolved: v3 step graphs are
  fully declarative and user-definable. Users author arbitrary step
  graphs via the Workshop CLI (`spire workshop`). Spire provides
  opinionated built-in formulas but does not lock in the structure.
  Steps declare actions, conditions, dependencies, and opcodes —
  composable primitives with observable structure.

**Still open:**

- **What to measure**: Token cost per step. Review round efficiency.
  Formula evolution over time. Time-to-merge by task type and formula.
  The `agent_runs` table captures per-run metrics, but dashboards and
  trend analysis are not built yet. These metrics should drive formula
  tuning before we add tool extensibility.

- **Primitives vs opinions**: The v3 graph resolved this toward
  "composable primitives with observable structure." But the next
  question is: should formulas be able to invoke external tools (MCP
  servers, custom scripts, webhooks)? This is deferred past v1.0 until
  we have enough observability data to know which extension points
  matter.

### Autonomous exploration ("YOLO mode")

What if an agent could file its own beads? A meta-agent gets a broad
goal ("make spire faster", "explore authentication"), reads the codebase,
and generates work items — tasks, bugs, design beads — that go through
the normal pipeline. The meta-agent is a strategist; wizards execute.

This is the most powerful and most dangerous capability. Key questions:

- **Guardrails**: How do you prevent runaway bead filing? Budget limits
  (max beads, max cost)? Human approval gates before execution? Beads
  filed as `needs-human` by default?
- **Tools**: The meta-agent needs the spire API (create beads, summon
  wizards, monitor progress). The MCP tools (`spire_focus`,
  `spire_send`, `spire_collect`) are the surface.
- **Feedback loop**: The meta-agent monitors the beads it filed. If a
  wizard fails, the meta-agent adjusts strategy. This is closed-loop
  autonomous engineering.
- **Trust gradient**: Start with "propose only" (agent suggests beads,
  human approves), graduate to "file and execute" as trust builds.

Not ready to design yet. Capture the concept, revisit after the
observability and formula flexibility questions are resolved.

---

## Why This Matters

The gap between "AI can write code" and "AI ships features" is coordination. Today's AI coding tools are reactive -- they wait for you to ask. Spire is proactive. You describe the work. Agents execute it. The work graph ensures nothing falls through the cracks.

This is not about replacing developers. It is about giving every developer a team of agents that can handle the backlog while they focus on the work that requires human judgment. File the task, review the result, ship the feature. The middle part -- the implementation grind -- is what agents are for.

Spire makes that real, today, on your laptop, with your repos, under your control.
