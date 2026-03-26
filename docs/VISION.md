# Spire Vision

> An open-source coordination hub for AI engineering agents.

You file work. Agents implement it. PRs appear on your repos.

---

## What Spire Is

Spire is infrastructure for directing AI agents to do real engineering work. You describe what you want built -- features, bug fixes, tasks -- and AI agents clone your repos, write code, and open pull requests.

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

Spire works on a laptop. `brew install spire && spire up` gets you a running tower with a local Dolt database, a steward, and agent execution via Docker or subprocesses. Kubernetes is for teams that need persistent agents, autoscaling, and managed infrastructure. It is never required.

### 2. User-first bootstrap

You can create a tower and file work before any infrastructure exists. The cluster adopts the tower, not the other way around. This means a developer can build up a backlog, register repos, and configure priorities before ever deploying a single agent pod. When the cluster comes online, it reads the tower and starts working.

### 3. Explicit over magic

`spire push` and `spire pull` move state between your machine and DoltHub. You control when sync happens. The background daemon (`spire up`) automates this on an interval, but it is opt-in convenience, not a requirement. There are no hidden background processes mutating your work graph without your knowledge.

### 4. DoltHub as the sync layer

No direct connectivity is needed between your laptop and a cluster. DoltHub mediates all collaboration. It is the "GitHub for your work graph" -- a versioned, mergeable, auditable intermediary. Push from your laptop, pull from the cluster. Push from the cluster, pull from your laptop. The database handles merge semantics.

### 5. Open protocol

Beads and Spire define how agents coordinate: how work is filed, claimed, executed, and completed. The protocol is open. Anyone can build agents that speak it, storage backends that host it, or integrations that extend it. Spire ships with opinionated defaults (Anthropic models, DoltHub sync, GitHub PRs) but none of these are locked in.

## The 5-Minute Experience

```bash
brew install spire
spire tower create --name my-team
spire repo add
spire file "Add dark mode" -t feature -p 2
spire up
```

Five commands from zero to an AI agent opening a PR on your repo.

`spire tower create` initializes a Dolt database and pushes it to DoltHub. `spire repo add` scans the current directory, assigns a prefix, and records the repo in the tower. `spire file` creates a bead. `spire up` starts the steward, which reads the work graph, finds the new task, spawns a wizard agent, and the wizard clones the repo, implements the feature, and opens a pull request.

You watch the PR appear. You review it. You merge it. The bead closes.

## Architecture Layers

| Layer | What it is | Where it runs |
|-------|-----------|---------------|
| **Spire Core** | Single binary: tower management, bead graph (bd), agent protocol, sync, CLI | Everywhere |
| **Spire Local** | Local steward, agent spawning (Docker/processes), background daemon | Laptop |
| **Spire Cluster** | Operator, managed agent pods, persistent steward, autoscaling, PVCs | Kubernetes |
| **Spire Hosted** | Managed towers, team dashboard, GitHub App (future) | Cloud |

### Spire Core

The `spire` binary embeds `bd` (the beads CLI) and adds tower management, repo registration, agent messaging, and DoltHub sync. It is the only dependency. It compiles to a single static binary for macOS and Linux.

### Spire Local

On a laptop, `spire up` starts a local Dolt server and a steward process. The steward watches the work graph and spawns agents as needed. Agents run as Docker containers (default) or child processes. All state lives in `~/.config/spire/` and the local Dolt data directory.

### Spire Cluster

In Kubernetes, Spire deploys via a Helm chart. The operator watches the tower's repos table and manages agent pods. The steward runs as a persistent deployment. Wizard pods are ephemeral -- one per task, terminated on completion. A syncer pod handles DoltHub push/pull on interval. Secrets (API keys, GitHub tokens) are stored in Kubernetes secrets.

### Spire Hosted (Future)

Managed towers with a web dashboard, GitHub App integration for automatic repo registration, team management with RBAC, and a hosted execution environment. The hosted layer is additive -- it runs the same Spire Core underneath.

## Agent Roles

Spire uses RPG-inspired naming for agent roles. The names are deliberate: they convey function, hierarchy, and personality.

### Archmage

You. The human. You write specs, file work, review what agents produce, and make the architecture calls. You bounce from tower to tower, steering work. The archmage's identity is stored in the tower config and used for merge commit attribution.

### Steward

The global coordinator. One per tower. The steward reads the work graph continuously, identifies ready work (no open blockers, not claimed), assigns tasks to available agents, routes messages, and tracks overall progress. It does not write code. It orchestrates capacity.

### Wizard

The per-bead orchestrator. One per bead, ephemeral. A wizard is summoned to drive a bead through its formula lifecycle — validating design, generating a plan, dispatching apprentices to implement, consulting sages to review, and sealing the work with a merge. The wizard does not write code directly. It orchestrates the agents that do.

### Apprentice

The implementer. One-shot, ephemeral. An apprentice receives a task from a wizard, works in an isolated git worktree, writes code, runs tests, and pushes a feature branch. When the branch is pushed, the apprentice's job is done. Apprentices are stateless — they read everything they need from the bead and the repo.

### Sage

The reviewer. One-shot, ephemeral. A sage reviews the implementation against the spec and returns a verdict: approve or request changes. If changes are needed, the wizard dispatches a fix apprentice and re-consults the sage. After max rounds, an arbiter (Claude Opus) breaks the tie.

### Artificer

The formula maker. Crafts and tests the formulas that wizards follow. In k8s, the artificer runs in a workshop pod for long-running epic management. Locally, the wizard + executor handle formula execution directly.

### Familiar (Sidecar)

A per-agent companion that runs alongside each agent in k8s pods. The familiar handles inbox polling, control signals (STOP, STEER, PAUSE, RESUME), liveness heartbeats, and health endpoints. It is the agent's interface to the messaging layer.

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
| GitHub | Repo access, PRs | `github_token` (PAT v1), GitHub App (v2) |
| Anthropic | LLM agent execution | `anthropic_api_key` |

On a laptop, credentials are stored in `~/.config/spire/credentials` (chmod 600). This file is the canonical credential store. Environment variables (`DOLT_REMOTE_USER`, `ANTHROPIC_API_KEY`, etc.) can override file-based credentials for CI/CD and ephemeral environments, but they are not the primary storage mechanism. In a cluster, credentials are stored in Kubernetes secrets and mounted into agent pods.

Tower-level secrets are scoped to the tower. A developer attaching to a tower brings their own credentials -- no shared API keys required for local use.

## Roadmap

### Phase 1: Local Experience

The single-developer, single-laptop experience. Everything works offline except DoltHub sync.

- Single `spire` binary with embedded `bd`
- `spire tower create` / `spire tower attach`
- Local agent execution via Docker
- DoltHub sync with `spire push` / `spire pull`
- `spire file` / `spire claim` / `spire focus` workflow
- Steward assigns work from the local graph

### Phase 2: Cluster

Persistent infrastructure for teams that want always-on agents.

- Helm chart for Kubernetes deployment
- Operator with auto-discovery from the tower's repos table
- Persistent steward deployment
- Ephemeral wizard pods (one per task, auto-terminated)
- Syncer pod for continuous DoltHub sync
- Provider-agnostic file storage (GCS, S3, local PVC)

### Phase 3: Multiplayer

Multiple developers sharing a tower and collaborating on work.

- Shared repo registration with prefix uniqueness enforcement
- Multi-developer attach flow (`spire tower attach <dolthub-url>`)
- Agent messaging across developers (`spire send` / `spire collect`)
- Conflict resolution documentation and tooling
- Notification hooks (Slack, email, webhooks)

### Phase 4: Product

The transition from open-source tool to product offering, while keeping the core open.

- Hosted towers (managed DoltHub + managed compute)
- Web dashboard for work graph visualization
- GitHub App for zero-config repo registration
- Team features: audit logs, RBAC, approval gates
- Usage analytics and cost tracking for LLM spend

---

## Why This Matters

The gap between "AI can write code" and "AI ships features" is coordination. Today's AI coding tools are reactive -- they wait for you to ask. Spire is proactive. You describe the work. Agents execute it. The work graph ensures nothing falls through the cracks.

This is not about replacing developers. It is about giving every developer a team of agents that can handle the backlog while they focus on the work that requires human judgment. File the task, review the PR, ship the feature. The middle part -- the implementation grind -- is what agents are for.

Spire makes that real, today, on your laptop, with your repos, under your control.
