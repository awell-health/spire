# Spire Kubernetes Architecture

This document describes the Kubernetes infrastructure that powers Spire's autonomous coding agents. It covers the operator, CRDs, pod lifecycle, and how all the pieces connect.

## Overview

Spire's k8s layer turns beads (work items) into running agent pods that clone repos, write code, run tests, and push branches — without human intervention.

```
Human files bead          Operator sees it          Pod runs agent
      |                        |                        |
      v                        v                        v
  bd create          BeadWatcher creates         Worker: clone, claim,
  "Fix auth bug"     SpireWorkload CR            focus, implement, test,
  -p 1 -t task              |                    commit, push
                             v                        |
                     WorkloadAssigner              Sidecar: polls inbox,
                     matches to agent              health checks, control
                             |                    channel
                             v                        |
                     AgentMonitor creates          Pod exits, operator
                     per-workload pod              reaps it, agent freed
```

## Components

### Custom Resource Definitions

Three CRDs, all namespaced under `spire.awell.io/v1alpha1`:

**SpireAgent** — represents an entity that can do work.

| Field | Description |
|-------|-------------|
| `spec.mode` | `external` (human's machine) or `managed` (operator creates pods) |
| `spec.prefixes` | Bead prefixes this agent can claim (e.g., `["spi-", "open-"]`) |
| `spec.maxConcurrent` | Max simultaneous workloads (default: 1) |
| `spec.token` | Name of the Anthropic API token to use (references SpireConfig) |
| `spec.image` | Container image for managed agents |
| `spec.repo` | Git repo URL for managed agents to clone |
| `spec.repoBranch` | Branch to clone (default: `main`) |
| `spec.resources` | k8s resource requests/limits for managed pods |
| `status.phase` | `Idle`, `Working`, `Provisioning`, `Stale`, `Offline` |
| `status.currentWork` | List of bead IDs currently assigned |

**SpireWorkload** — represents a bead assignment. Created by BeadWatcher, consumed by WorkloadAssigner.

| Field | Description |
|-------|-------------|
| `spec.beadId` | The bead this workload tracks |
| `spec.priority` | 0 (critical) to 4 (nice-to-have) |
| `spec.prefixes` | Derived from bead ID for agent matching |
| `status.phase` | `Pending`, `Assigned`, `InProgress`, `Stale`, `Done`, `Failed` |
| `status.assignedTo` | Agent name |
| `status.attempts` | How many times this workload has been assigned |

**SpireConfig** — cluster-wide configuration singleton (name: `default`).

| Field | Description |
|-------|-------------|
| `spec.dolthub.remote` | DoltHub remote URL for beads sync |
| `spec.dolthub.credentialsSecret` | k8s Secret name for Dolt creds |
| `spec.tokens` | Map of token names to Secret refs for Anthropic API keys |
| `spec.defaultToken` | Which token to use when agent doesn't specify one |
| `spec.polling.interval` | How often controllers poll (default: `2m`) |
| `spec.polling.staleThreshold` | Time before marking work stale (default: `4h`) |
| `spec.polling.reassignThreshold` | Time before reassigning stale work (default: `6h`) |

### Operator controllers

Three poll-loop controllers run inside the operator process:

**BeadWatcher** (`operator/controllers/bead_watcher.go`)
- Runs `bd dolt pull` to sync from DoltHub
- Runs `bd ready --json` to find beads with no open blockers
- Creates a SpireWorkload CR for each new ready bead
- Marks workloads as `Done` when their bead is closed
- Runs `bd dolt push` to push state changes

**WorkloadAssigner** (`operator/controllers/workload_assigner.go`)
- Lists pending SpireWorkloads and available SpireAgents
- Matches by prefix (`agent.spec.prefixes` intersected with `workload.spec.prefixes`)
- Respects agent capacity (`maxConcurrent`)
- Sorts pending work by priority (lower = more urgent)
- Sends assignment message via `spire send`
- Updates workload status to `Assigned`
- Appends bead ID to `agent.status.currentWork`
- Monitors staleness: sends reminders at `staleThreshold`, reassigns at `reassignThreshold`

**AgentMonitor** (`operator/controllers/agent_monitor.go`)
- **External agents**: tracks `lastSeen` heartbeat, marks offline after `offlineTimeout`
- **Managed agents**: creates/deletes per-workload pods
  - Reaps completed/failed pods and removes bead IDs from `currentWork`
  - Creates pods for newly assigned work
  - Deletes orphaned pods when work is removed
  - Updates agent phase based on pod states

## Pod architecture

Each managed workload gets its own pod with two containers sharing three volumes:

```
Pod: spire-agent-{agent-name}-{bead-id}
 ┌──────────────────────────────────────────────────┐
 │                                                  │
 │  ┌─────────────────┐   ┌──────────────────────┐ │
 │  │  worker          │   │  sidecar              │ │
 │  │                  │   │                       │ │
 │  │  entrypoint.sh   │   │  spire-sidecar        │ │
 │  │  - clone repo    │   │  - poll inbox (10s)   │ │
 │  │  - claim bead    │   │  - /healthz, /readyz  │ │
 │  │  - focus context │   │  - control channel    │ │
 │  │  - run Claude    │   │  - worker monitoring  │ │
 │  │  - validate      │   │  - heartbeat (30s)    │ │
 │  │  - push branch   │   │                       │ │
 │  │  WorkingDir:     │   │  WorkingDir: /data    │ │
 │  │    /workspace    │   │                       │ │
 │  └────────┬─────────┘   └───────────┬───────────┘ │
 │           │                         │              │
 │  ┌────────┴─────────────────────────┴───────────┐ │
 │  │  Shared volumes:                              │ │
 │  │    /comms     — sidecar <-> worker protocol   │ │
 │  │    /workspace — git repo clone                │ │
 │  │    /data      — beads state (.beads/)         │ │
 │  └──────────────────────────────────────────────┘ │
 │                                                   │
 │  RestartPolicy: Never (one-shot)                  │
 │  Labels:                                          │
 │    spire.awell.io/agent: {agent-name}             │
 │    spire.awell.io/bead:  {bead-id}                │
 │    spire.awell.io/managed: "true"                 │
 └───────────────────────────────────────────────────┘
```

### Worker container

Runs `agent-entrypoint.sh`. Lifecycle:

1. **Bootstrap** — create dirs, start heartbeat, set up GitHub auth
2. **Clone** — `git clone --depth=1` from `SPIRE_REPO_URL`
3. **Load config** — read `spire.yaml` for model, timeout, test/build/lint commands
4. **Init state** — `bd init` + `spire sync` in `/data`, register agent
5. **Resolve assignment** — use `SPIRE_BEAD_ID` from env, or parse `/comms/inbox.json`
6. **Claim & focus** — `spire claim`, `spire focus`, `bd show --json`
7. **Branch** — `git checkout -B feat/{bead-id}`
8. **Install** — run detected install command (e.g., `pnpm install`, `go mod download`)
9. **Execute** — run Claude CLI (or custom `SPIRE_AGENT_CMD`) with timeout and stop handling
10. **Validate** — lint, build, test (commands from `spire.yaml`)
11. **Push** — commit, `git push -u origin feat/{bead-id}`
12. **Close** — `bd comments add`, `bd close`, `bd dolt push`
13. **Exit** — write `/comms/result.json`, exit 0 (success) or 1 (failure)

The entrypoint handles all failure modes:
- **Timeout**: `timeout --signal=TERM` wraps the agent command; result = `timeout`
- **Stop**: sidecar writes `/comms/stop`; monitor kills agent; result = `stopped`
- **Test failure**: validation fails; result = `test_failure`
- **Error**: any other failure; result = `error`
- **No changes**: agent ran but produced nothing; result = `error`

The `trap 'finalize "$?"' EXIT` at line 131 guarantees `result.json` is always written.

### Sidecar container

Runs `spire-sidecar`. Four concurrent loops:

| Loop | Interval | What it does |
|------|----------|--------------|
| Inbox | 10s | `spire collect --json` → atomic write to `/comms/inbox.json` |
| Control | 2s | Reads `/comms/control`, dispatches STOP/STEER/PAUSE/RESUME |
| Worker monitor | 5s | Checks `/comms/result.json` (exit) and `/comms/worker-alive` (staleness) |
| Heartbeat | 30s | Writes timestamp to `/comms/heartbeat` |

HTTP endpoints:
- `GET /healthz` — always 200
- `GET /readyz` — 200 if at least one collect has succeeded and isn't stale
- `GET /status` — JSON snapshot of sidecar state

### /comms file protocol

The worker and sidecar communicate through files on the shared `/comms` volume:

| File | Writer | Reader | Purpose |
|------|--------|--------|---------|
| `inbox.json` | sidecar | worker | Messages from `spire collect --json` |
| `result.json` | worker | sidecar, operator | Final outcome (always written on exit) |
| `worker-alive` | worker | sidecar | Heartbeat — touched every 5s |
| `heartbeat` | sidecar | operator | Sidecar heartbeat — written every 30s |
| `stop` | sidecar | worker | Shutdown signal |
| `steer` | sidecar | worker | Course correction message |
| `steer.log` | worker | worker | Accumulated steer messages for agent context |
| `control` | external | sidecar | Control commands (STOP, STEER:msg, PAUSE, RESUME) |
| `prompt.txt` | worker | worker | Generated agent prompt |
| `focus.txt` | worker | worker | Output of `spire focus` |
| `bead.json` | worker | worker | Output of `bd show --json` |

## Environment variables

The operator injects these into the worker container:

| Variable | Source | Required |
|----------|--------|----------|
| `SPIRE_AGENT_NAME` | `agent.metadata.name` | Yes |
| `SPIRE_BEAD_ID` | Assigned bead ID | Yes |
| `SPIRE_REPO_URL` | `agent.spec.repo` | Yes |
| `SPIRE_REPO_BRANCH` | `agent.spec.repoBranch` or `"main"` | Yes |
| `SPIRE_COMMS_DIR` | `/comms` | Yes |
| `SPIRE_WORKSPACE_DIR` | `/workspace` | Yes |
| `SPIRE_STATE_DIR` | `/data` | Yes |
| `DOLTHUB_REMOTE` | `config.spec.dolthub.remote` | Yes |
| `DOLT_REMOTE_USER` | Secret ref | Yes |
| `DOLT_REMOTE_PASSWORD` | Secret ref | Yes |
| `ANTHROPIC_API_KEY` | Secret ref via token routing | Yes |
| `GITHUB_TOKEN` | Secret ref (optional key) | No |

## Token routing

The operator resolves the Anthropic API key through a three-level fallback:

```
agent.spec.token → config.spec.defaultToken → "default"
         ↓
config.spec.tokens[resolved_name]
         ↓
{ secret: "spire-credentials", key: "ANTHROPIC_API_KEY_DEFAULT" }
         ↓
k8s Secret envFrom injection
```

This allows different agents or priority levels to use different API keys (e.g., a `heavy` key with higher rate limits for P0 work).

## Repo configuration

Each repo can have a `spire.yaml` at its root:

```yaml
runtime:
  language: go              # auto-detected from go.mod/package.json/Cargo.toml
  install: ""               # auto-detected
  test: go test ./...       # auto-detected
  build: go build ./...     # optional
  lint: go vet ./...        # optional

agent:
  model: claude-sonnet-4-6  # default
  max-turns: 50             # safety limit
  timeout: 30m              # hard timeout per task

branch:
  base: main
  pattern: "feat/{bead-id}"

pr:
  auto-merge: false
  reviewers: []
  labels: ["agent-generated"]

context:                    # files agent should read before starting
  - CLAUDE.md
  - SPIRE.md
```

If no `spire.yaml` exists, `pkg/repoconfig` auto-detects the runtime:
- `go.mod` → Go, `go test ./...`
- `pnpm-lock.yaml` → TypeScript, `pnpm install`, `pnpm test`
- `yarn.lock` → TypeScript, `yarn`, `yarn test`
- `package.json` → TypeScript, `npm install`, `npm test`
- `pyproject.toml` / `requirements.txt` → Python, `pip install`, `pytest`
- `Cargo.toml` → Rust, `cargo test`

## Images

**`Dockerfile.mayor`** — the mayor/operator image. Contains `spire`, `bd`, `dolt`, `git`. Runs `k8s/entrypoint.sh` which initializes beads state, syncs from DoltHub, and starts `spire mayor`.

**`Dockerfile.agent`** — the worker/sidecar image. Contains everything in the mayor image plus `spire-sidecar`, `claude` CLI, `gh`, `node`, `go`, `python`. Runs `agent-entrypoint.sh`.

## RBAC

The operator runs under a `spire-operator` ServiceAccount with a namespaced Role:

| Resource | Verbs |
|----------|-------|
| `spireagents`, `spireworkloads`, `spireconfigs` | get, list, watch, create, update, patch, delete |
| `*/status` (above CRDs) | get, update, patch |
| `pods` | get, list, watch, create, delete |
| `secrets` | get |

## Metrics

The `agent_runs` Dolt table records every agent execution:

| Column | Description |
|--------|-------------|
| `id` | `run-{8hex}` |
| `bead_id` | Which bead was worked on |
| `model` | `claude-sonnet-4-6`, `claude-opus-4-6`, etc. |
| `role` | `worker` or `refinery` |
| `result` | `success`, `test_failure`, `timeout`, `stopped`, `error` |
| `context_tokens_in/out` | Token usage |
| `duration_seconds` | Wall time |
| `review_rounds` | How many review cycles |
| `files_changed`, `lines_added/removed` | Code diff stats |
| `golden_run` | Flagged as a reference for prompt tuning |

Query with `spire metrics`:

```bash
spire metrics                  # today + this week summary
spire metrics --bead spi-a3f8  # per-bead breakdown
spire metrics --model          # cost by model
spire metrics --json           # machine-readable
```

Cost estimation: Sonnet ~$3/M input, $15/M output. Opus ~$15/M input, $75/M output.

## Staleness and reassignment

The WorkloadAssigner has two time thresholds:

1. **Stale threshold** (default: 4h) — sends a reminder to the assigned agent, marks workload as `Stale`
2. **Reassign threshold** (default: 6h) — unassigns the workload, returns it to `Pending` for re-matching

The AgentMonitor complements this by reaping completed/failed pods and removing their bead IDs from `agent.status.currentWork`, freeing capacity for new assignments.

## File inventory

```
operator/
  main.go                          — operator entrypoint, wires controllers
  api/v1alpha1/
    types.go                       — Go types for all three CRDs
    register.go                    — scheme registration
    zz_generated_deepcopy.go       — DeepCopy implementations
  controllers/
    bead_watcher.go                — syncs beads → SpireWorkload CRs
    workload_assigner.go           — matches workloads to agents
    agent_monitor.go               — manages heartbeats and pods
  go.mod, go.sum                   — separate module (controller-runtime deps)

k8s/
  crds/
    spireagent.yaml                — SpireAgent CRD schema
    spireworkload.yaml             — SpireWorkload CRD schema
    spireconfig.yaml               — SpireConfig CRD schema
  examples/
    agent-external.yaml            — example: human dev as external agent
    agent-managed.yaml             — example: autonomous managed agent
    config.yaml                    — example: SpireConfig with token routing
  mayor.yaml                       — Deployment + RBAC for the operator
  namespace.yaml                   — spire namespace
  secrets.yaml                     — template for spire-credentials Secret
  entrypoint.sh                    — mayor container entrypoint
  minikube-demo.sh                 — one-command local demo setup

cmd/spire-sidecar/main.go         — sidecar binary
agent-entrypoint.sh                — worker entrypoint
Dockerfile.mayor                   — mayor/operator image
Dockerfile.agent                   — worker/sidecar image
spire.yaml                         — this repo's own agent config
pkg/repoconfig/repoconfig.go      — spire.yaml parser + auto-detection
pkg/metrics/recorder.go            — agent_runs table writer
cmd/spire/metrics.go               — spire metrics command
migrations/agent_runs.sql          — agent_runs + golden_prompts DDL
```
