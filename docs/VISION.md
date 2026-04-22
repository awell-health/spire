# Spire Vision

> An open-source coordination hub for AI engineering agents.

You file work. Agents implement it. Changes land on your repos.

---

## What Spire Is

Spire is infrastructure for directing AI agents to do real engineering work. You describe what you want built — features, bug fixes, tasks — and AI agents clone your repos, write code, and land reviewed changes.

It is not a chatbot. It is not a copilot. It is a work system: a structured graph of tasks, a protocol for agent coordination, and a set of deployment topologies that let a single developer, a small team, or a production cluster run the same coordination model at different scales.

A single developer can run Spire on a laptop. A team can put the coordination plane in a Kubernetes cluster and keep their laptops as control surfaces. The mental model stays the same across all of them.

## Core Concepts

### Tower

A tower is a shared workspace identity. It contains the work graph, registered repos, agent configuration, and state that every participant — developer, agent, cluster — reads and writes. A tower is backed by a Dolt database, which gives it full version history, branch/merge semantics, and the ability to sync across machines through multiple transports.

One tower per team. Multiple developers attach to it. Multiple repos register under it. All agents read from and write to the same tower.

### Beads

Beads are work items: tasks, bugs, features, epics, chores, design beads, recovery beads. They live in the tower's Dolt database and form a directed graph of dependencies and hierarchy. Each bead has a status, priority, assignee, comments, and messages.

Beads are accessed through the beads storage engine's library API. Everything is queryable, scriptable, and JSON-serializable. The work graph is the single source of truth for what needs to happen, what is in progress, and what is done — across every deployment topology.

### Agents

Agents are AI-powered workers. Each agent has a role, a set of capabilities, and a communication protocol. Agents read from the work graph, claim tasks, execute them, and report results. They coordinate through structured messages routed by bead references.

Agents can run as local processes, Docker containers, or Kubernetes pods — and Spire treats those interchangeably. The coordination protocol does not know or care which backend is in use.

### Prefix

Each registered repo gets a unique prefix within the tower. Bead IDs include the prefix for routing: `web-a3f8` belongs to the web repo, `api-b7d0` belongs to the API repo. Prefixes are short (3-4 characters) and human-readable. They make it possible to manage work across many repos in a single graph without ID collisions.

### Deployment Mode, Backend, and Transport

Spire has three axes that together describe any running system:

- **Deployment mode** — where the control plane lives relative to execution. Three modes: local-native (control + execution on a laptop), cluster-native (control + execution in a Kubernetes cluster), attached-reserved (local control driving remote cluster execution through a canonical intent seam).
- **Backend** — how individual agent processes are spawned. Process, Docker, or Kubernetes pod.
- **Transport** — how the tower's Dolt state moves between machines. Local filesystem, remotesapi over a network, or DoltHub.

Deployment mode is the primary axis. Backend and transport choices are constrained within each mode:

- **Local-native** admits any backend (process, docker, or k8s) and any transport (local filesystem, remotesapi, or DoltHub).
- **Cluster-native** requires the k8s backend and a network transport (remotesapi or DoltHub). A cluster pod cannot mount a laptop's filesystem.
- **Attached-reserved** inherits cluster-native's execution-side constraints while keeping the control plane on a laptop.

Within those constraints, the axes compose cleanly. Changing *where* work runs does not change *how* work is coordinated. Adding a new backend does not require touching the steward. Changing transport never changes the protocol. The canonical seams — `WorkloadIntent`, `RepoIdentity`, `WorkspaceHandle`, `HandoffMode`, `RunContext` — are the same in every combination.

## Design Principles

### 1. Local-first, cluster-optional

Spire works on a laptop. `brew install spire && spire up && spire summon 1` gets you a running tower with a local Dolt database, a daemon, and agents executing via subprocesses. Kubernetes is for teams that need persistent agents, autoscaling, and managed infrastructure. It is never required.

### 2. User-first bootstrap

You can create a tower and file work before any infrastructure exists. The cluster adopts the tower, not the other way around. A developer can build a backlog, register repos, and configure priorities before ever deploying a single agent pod. When a cluster comes online, it reads the tower and starts working.

### 3. Explicit over magic

Sync happens when you ask for it. A background daemon can automate sync on an interval, but it is opt-in convenience, not a requirement. There are no hidden background processes mutating your work graph without your knowledge.

### 4. Mode is primary; backend and transport compose within it

A pattern that works in local-native mode means the same thing when the same code runs in a cluster pod. Changing where a tower syncs never changes how work is claimed. Adding a new agent backend never requires touching the steward. Mode gates the set of legal backend and transport choices, but within a mode the axes compose freely through the canonical seams.

### 5. Canonical contracts, not ambient state

Worker runtime is defined by explicit types — repo identity, workspace handle, handoff mode, run context — not by whatever directory the agent happened to be running in. Deployment mode is an explicit value with a reserved "not implemented" option, not a fallback path. Every seam that a future deployment topology could reuse is named and documented.

### 6. Open protocol

Beads and Spire define how agents coordinate: how work is filed, claimed, executed, and completed. The protocol is open. Anyone can build agents that speak it, storage backends that host it, or integrations that extend it. Spire ships with opinionated defaults but none of them are locked in.

## Agent Roles

Spire uses RPG-inspired naming for agent roles. The names convey function, hierarchy, and personality.

### Archmage

You. The human. You write specs, file work, review what agents produce, and make the architecture calls. The archmage's identity is stored in the tower config and used for merge commit attribution.

### Steward

The global coordinator. One per tower. The steward reads the work graph continuously, identifies ready work (no open blockers, not claimed), dispatches wizards through the intent seam, routes messages between agents, and tracks overall progress. It does not write code. It orchestrates capacity — deciding which work to start and when, based on priority, dependencies, and available resources.

### Wizard

The per-bead orchestrator. One per bead, ephemeral. A wizard is dispatched to drive a bead through its spell (formula) lifecycle — validating design, generating a plan, dispatching apprentices to implement, consulting sages to review, and sealing the work with a merge. The wizard does not write code directly. It orchestrates the agents that do.

### Apprentice

The implementer. One-shot, ephemeral. An apprentice receives a task from a wizard, works in an isolated git worktree, writes code, runs tests, and delivers a bundle or feature branch. Apprentices are stateless — they read everything they need from the bead and the repo.

### Sage

The reviewer. One-shot, ephemeral. A sage reviews the implementation against the spec and returns a verdict: approve or request changes. If changes are needed, the wizard dispatches a fix apprentice and re-consults the sage. After max rounds, an arbiter breaks the tie.

### Arbiter

The tie-breaker. Invoked when a sage and apprentice cannot converge after the maximum number of review rounds. The arbiter examines the full review history, the spec, and the code, then renders a binding verdict: accept the implementation, accept the sage's objections, or prescribe a specific resolution. One-shot, ephemeral.

### Cleric

The failure-recovery driver. When a wizard's bead gets stuck — a merge conflict, a flaky test, a failed workspace, a phase that repeatedly errors — the steward dispatches a cleric. The cleric collects context, decides on a repair mode (agentic diagnosis, programmatic recipe, escalation to a human), executes the repair, verifies the outcome, and learns from the result. Successful agentic repairs are promoted to recipes over time, so recurring failure classes graduate from LLM-driven to mechanical.

### Artificer

The formula maker. The artificer crafts and maintains spells (formulas) — the declarative recipes that wizards follow to drive beads through their lifecycle. It works in the Workshop CLI for authoring, validating, testing, and publishing formulas before they are used by the tower. The artificer does not orchestrate epics or review code — that is the wizard's and sage's domain.

## Deployment Modes

Spire supports three deployment modes. Each is explicit and selected by tower config, and each composes the same seams in a different place.

- **Local-native** — the whole coordination plane runs on a laptop. The steward, the dolt database, and the agent processes all live on one machine. Best for solo developers, small teams, and anyone who wants a zero-infrastructure starting point. See [VISION-LOCAL.md](VISION-LOCAL.md).
- **Cluster-native** — the coordination plane runs in a Kubernetes cluster. The steward and operator are pods. The dolt database is a stateful pod with remotesapi enabled. Apprentices and sages are ephemeral pods dispatched through the operator's intent reconciler. Laptops attach to the cluster's dolt via remotesapi to monitor, file work, and interact with the graph. See [VISION-CLUSTER.md](VISION-CLUSTER.md).
- **Attached (reserved)** — a laptop runs the control plane while execution happens on a remote cluster. The local scheduler publishes a canonical intent; a remote consumer reconciles it into pods. Reserved today, not implemented, but the seams exist and are held to strict invariants. See [VISION-ATTACHED.md](VISION-ATTACHED.md).

## Sync Model

A tower's state is a Dolt database. Multiple machines can read and write it, and the database handles merge semantics. Three transports move state between machines:

- **Local filesystem** — the default when one laptop owns a tower. No network required.
- **Remotesapi** — a direct network protocol to a dolt server, typically a cluster's. Laptops use this to attach to a cluster-hosted tower; cluster components use it internally.
- **DoltHub** — the public hosted remote. Used when laptops and clusters have no direct connectivity, or when a team wants a durable intermediary.

All three are first-class. A tower can use any combination. `spire push` / `spire pull` move state explicitly; a background daemon automates them on an interval as opt-in convenience.

### Merge Semantics

Each field has a single owner. Conflicts are resolved by ownership, not by timestamp.

- **Status fields** (status, owner, assignee) are authored by whichever component holds execution; user edits to these fields are overwritten on the next sync.
- **Content fields** (title, description, priority, type) are authored by the human; execution-side edits are overwritten on the next sync.
- **Comments and messages** are append-only; both sides append without conflict.

This model is specified here. Enforcement lands when multi-machine conflicts become common enough to matter.

## Auth Model

Spire requires credentials for three services:

- **DoltHub** — database sync (when using the DoltHub transport)
- **GitHub** — repo access, branch pushes
- **Anthropic** — LLM agent execution

On a laptop, credentials are stored in `~/.config/spire/credentials` (chmod 600). Environment variables override file-based credentials for CI/CD and ephemeral environments. In a cluster, credentials are Kubernetes secrets mounted into pods.

Tower-level secrets are scoped to the tower. A developer attaching to a tower brings their own credentials — no shared API keys required for local use.

---

## The 5-Minute Experience

```bash
brew install spire
spire tower create --name my-team
spire repo add
spire file "Add dark mode" -t feature -p 2
spire up
spire summon 1
```

A short path from zero to an AI agent run on your repo. This is the local-native entry point. The cluster-native and attached paths layer on top of the same CLI and the same tower.

---

## Why This Matters

The gap between "AI can write code" and "AI ships features" is coordination. Today's AI coding tools are reactive — they wait for you to ask. Spire is proactive. You describe the work. Agents execute it. The work graph ensures nothing falls through the cracks.

This is not about replacing developers. It is about giving every developer a team of agents that can handle the backlog while they focus on the work that requires human judgment. File the task, review the result, ship the feature. The middle part — the implementation grind — is what agents are for.

Spire makes that real, today, on your laptop, with your repos, under your control — and scales to a cluster without changing the mental model.
