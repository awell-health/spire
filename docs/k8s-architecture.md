# Spire Kubernetes Architecture

This document describes the Kubernetes infrastructure that powers Spire's autonomous coding agents. It covers the operator, CRDs, pod lifecycle, and how all the pieces connect.

## Deployment modes

Spire has three deployment modes: **local-native** (single-machine, local
filesystem), **cluster-native** (multi-user cluster, gateway-fronted Dolt),
and **attached-reserved** (reserved; not implemented). Within
cluster-native, individual clients attach in **gateway mode** — this is
`TowerConfig.Mode` / client routing, not a fourth `DeploymentMode`.

This document describes cluster-native. Local-native and
attached-reserved are out of scope here; see [VISION-LOCAL.md](VISION-LOCAL.md)
and [attached-mode.md](attached-mode.md).

## Overview

For cluster-native production towers, the cluster-hosted Dolt database
accessed through the gateway is the canonical bead-graph host. DoltHub
serves as seed-only on first install and as a one-way archive; it is
not an active writable mirror. Desktop/laptop clients attach via the
gateway and route mutations through `/api/v1/*` over HTTP.

Spire's k8s layer turns beads (work items) into running agent pods that clone repos, write code, run tests, and push branches — without human intervention.

```
Archmage files bead       Operator sees it          Pod runs wizard
      |                        |                        |
      v                        v                        v
  spire file        SpireWorkload (or          `spire execute <bead>`:
  "Fix auth bug"     IntentReconciler)          claim, plan, dispatch
  -p 1 -t task      via gateway HTTP            apprentices, review,
        |                    |                  merge, close, exit
        v                    v                          |
   gateway pod        WorkloadAssigner                  v
   (writes to         matches to agent           Pod exits, operator
   cluster Dolt)             |                   reaps it via pod phase,
                             v                   agent freed
                     AgentMonitor creates
                     per-workload pod
```

The data flow is: archmage → gateway-mode client → HTTP → gateway →
cluster Dolt (writes). DoltHub is fed one-way from the cluster as
archive, never read back as truth. GCS backup is the canonical
disaster-recovery substrate.

## Components

### Custom Resource Definitions

Three CRDs, all namespaced under `spire.awell.io/v1alpha1`:

**WizardGuild** — represents an entity that can do work.

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
| `spec.dolthub.remote` | DoltHub remote URL — first-install seed only in cluster-as-truth installs (no bidirectional sync) |
| `spec.dolthub.credentialsSecret` | k8s Secret name for Dolt creds (HTTPS clone for first-install seed) |
| `spec.tokens` | Map of token names to Secret refs for Anthropic API keys |
| `spec.defaultToken` | Which token to use when agent doesn't specify one |
| `spec.polling.interval` | How often controllers poll (default: `2m`) |
| `spec.polling.staleThreshold` | Time before marking work stale (default: `4h`) |
| `spec.polling.reassignThreshold` | Time before reassigning stale work (default: `6h`) |

### Operator controllers

Three poll-loop controllers run inside the operator process:

**BeadWatcher** (`operator/controllers/bead_watcher.go`) — transitional
legacy scheduler, off by default in cluster-as-truth installs
- Reads beads directly from the in-cluster Dolt server (the canonical
  bead-graph host); no DoltHub pull/push on its loop. Bidirectional
  cluster ↔ DoltHub sync was removed because both sides writing produced
  non-fast-forward push rejections and merge conflicts that silently
  diverged the two stores (witnessed 2026-04-26). Cluster-as-truth makes
  the cluster the single writer; DoltHub receives one-way archive
  pushes only.
- Runs `bd ready --json` to find beads with no open blockers
- Creates a SpireWorkload CR for each new ready bead
- Marks workloads as `Done` when their bead is closed

**WorkloadAssigner** (`operator/controllers/workload_assigner.go`)
- Lists pending SpireWorkloads and available WizardGuilds
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

Each managed workload gets a **single-container wizard pod** with two
init containers that bootstrap the beads data directory and the repo
checkout, plus two emptyDir volumes. The authoritative spec lives in
[k8s-operator-reference.md — Canonical wizard pod contract](k8s-operator-reference.md#canonical-wizard-pod-contract);
this section summarizes.

```
Pod: {agent-name}
 ┌────────────────────────────────────────────────────────────┐
 │                                                            │
 │  ┌──────────────────────────────────────────────────────┐ │
 │  │  init: tower-attach                                  │ │
 │  │  spire tower attach-cluster                          │ │
 │  │    --data-dir=/data/<db> --database=<db>             │ │
 │  │    --prefix=<prefix> --dolthub-remote=<remote>       │ │
 │  │  (--dolthub-remote = first-install seed only)        │ │
 │  │  volumeMounts: /data                                 │ │
 │  └────────────────────┬─────────────────────────────────┘ │
 │                       │                                    │
 │                       v                                    │
 │  ┌──────────────────────────────────────────────────────┐ │
 │  │  init: cache-bootstrap                               │ │
 │  │  spire cluster cache-bootstrap                       │ │
 │  │    --cache-path=/spire/cache                         │ │
 │  │    --workspace-path=/spire/workspace                 │ │
 │  │    --prefix=<prefix>                                 │ │
 │  │  volumeMounts: /data, /spire/cache, /spire/workspace │ │
 │  └────────────────────┬─────────────────────────────────┘ │
 │                       │                                    │
 │                       v                                    │
 │  ┌──────────────────────────────────────────────────────┐ │
 │  │  agent (main)                                         │ │
 │  │  spire execute <bead-id> --name <agent-name>         │ │
 │  │  - loads formula, claims bead                         │ │
 │  │  - plans, dispatches apprentices, reviews, merges    │ │
 │  │  - exits 0 on success / non-zero on failure          │ │
 │  │  volumeMounts: /data, /spire/workspace, /spire/cache │ │
 │  │  workingDir:  /spire/workspace                        │ │
 │  └──────────────────────────────────────────────────────┘ │
 │                                                            │
 │  Shared volumes:                                           │
 │    /data            — beads workspace + config (emptyDir)  │
 │    /spire/workspace — materialized repo root (emptyDir)    │
 │    /spire/cache     — guild-owned cache mirror (PVC, RO)   │
 │                                                            │
 │  restartPolicy: Never  (one-shot)                          │
 │  priorityClassName: spire-agent-default                    │
 │  Labels:                                                   │
 │    spire.agent:      "true"                                │
 │    spire.agent.name: {agent-name}                          │
 │    spire.bead:       {bead-id}                             │
 │    spire.role:       wizard                                │
 │    spire.tower:      {tower-name}                          │
 └────────────────────────────────────────────────────────────┘
```

### Init container: tower-attach

The first init container, `tower-attach`, runs
`spire tower attach-cluster` with `--data-dir`, `--database`, `--prefix`,
and `--dolthub-remote` flags. It primes the `/data` volume with the
beads workspace (dolt data dir) and spire config so the main container
can open dolt immediately. The `--dolthub-remote` flag captures the
first-install seed source only; in cluster-as-truth installs DoltHub
is not used as an active writable mirror.

This replaces both the older `beads-seed` ConfigMap bootstrap and the
`agent-entrypoint.sh` workspace-setup flow.

### Init container: cache-bootstrap

The second init container, `cache-bootstrap`, runs
`spire cluster cache-bootstrap` — which calls the pkg/agent helpers
`MaterializeWorkspaceFromCache` then `BindLocalRepo` — to materialize
the writable workspace at `/spire/workspace` from the guild-owned
read-only cache mirror at `/spire/cache`, and writes a local binding
into the tower config on `/data`. The cache PVC is provisioned by the
operator's `CacheReconciler` from the `WizardGuild.Spec.Cache`
declaration; declaring `spec.cache` on every managed guild is now a
deployment requirement (spi-gvrfv retired the older `repo-bootstrap`
origin-clone path). See
[cluster-repo-cache.md](cluster-repo-cache.md) for the full cache
contract (CRD fields, PVC/Job naming, serialization, worker
bootstrap, observability vocabulary).

### Main container: agent

The main container is named `agent` and runs
`spire execute <bead-id> --name <agent-name>` directly — no shell
wrapper. The wizard process itself drives the formula lifecycle: claim,
plan, dispatch apprentices, review, merge, close, exit.

Lifecycle steps formerly split between `agent-entrypoint.sh` and the
Go wizard are now all inside the wizard process.

### One-shot semantics

- `restartPolicy: Never` — the pod is one-shot; k8s never restarts it.
- On wizard exit, the pod enters `Succeeded` (exit 0) or `Failed`
  (non-zero). The steward/operator observes the phase and reaps the pod.
- There is no in-pod sidecar, no `/comms` volume, no filesystem IPC.
  Coordination with the steward happens via dolt and OTLP telemetry.

## Environment variables

The steward/operator injects these into the main (`agent`) container:

| Variable | Value / source | Required |
|----------|----------------|----------|
| `DOLT_DATA_DIR` | `/data` | Yes |
| `SPIRE_CONFIG_DIR` | `/data/spire-config` | Yes |
| `BEADS_DOLT_SERVER_HOST` | In-cluster dolt service (e.g. `spire-dolt.{ns}.svc`) | Yes |
| `BEADS_DOLT_SERVER_PORT` | `3306` | Yes |
| `SPIRE_AGENT_NAME` | Agent identity | Yes |
| `SPIRE_BEAD_ID` | Assigned bead ID | Yes |
| `SPIRE_TOWER` | Tower name | Yes |
| `SPIRE_ROLE` | `wizard` | Yes |
| `SPIRE_REPO_URL` | Git remote URL for the bead's prefix | Yes |
| `SPIRE_REPO_BRANCH` | Tracked branch identity | Yes |
| `SPIRE_REPO_PREFIX` | Bead prefix — keys `cfg.Instances[prefix]` for `wizard.ResolveRepo` | Yes |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP collector (steward) | Yes |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | Yes |
| `OTEL_TRACES_EXPORTER`, `OTEL_LOGS_EXPORTER` | `otlp` | Yes |
| `OTEL_RESOURCE_ATTRIBUTES` | `bead.id=…,agent.name=…,tower=…` | Yes |
| `ANTHROPIC_API_KEY` | Secret ref (`ANTHROPIC_API_KEY_DEFAULT`) | Yes |
| `GITHUB_TOKEN` | Secret ref (optional key) | No |

### Resource tier

Wizard pods run in their own resource tier. Defaults:

| Field            | Default | Override env                 |
|------------------|---------|------------------------------|
| Memory request   | `1Gi`   | `SPIRE_WIZARD_MEMORY_REQUEST`|
| Memory limit     | `2Gi`   | `SPIRE_WIZARD_MEMORY_LIMIT`  |
| CPU request      | `250m`  | `SPIRE_WIZARD_CPU_REQUEST`   |
| CPU limit        | `1000m` | `SPIRE_WIZARD_CPU_LIMIT`     |

These defaults are sized for planning and apprentice fan-out and are
higher than the generic executor / sage tier.

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

**`Dockerfile.mayor`** — the mayor/operator image. Contains `spire`,
`bd`, `dolt`, `git`. Runs `k8s/entrypoint.sh` which initializes beads
state (cluster-as-truth installs use the in-cluster Dolt directly;
DoltHub is consulted only as a first-install seed) and starts
`spire mayor`.

**`Dockerfile.agent`** — the wizard-pod image. Contains everything in
the mayor image plus `claude` CLI, `gh`, `node`, `go`, `python`. The
default entrypoint is `spire` (so the pod `command:` is
`["spire", "execute", "<bead-id>", "--name", "<agent-name>"]`); no
shell wrapper is baked in.

## RBAC

The operator runs under a `spire-operator` ServiceAccount with a namespaced Role:

| Resource | Verbs |
|----------|-------|
| `wizardguilds`, `spireworkloads`, `spireconfigs` | get, list, watch, create, update, patch, delete |
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
| `role` | `worker` or `wizard` |
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
    wizardguild.yaml               — WizardGuild CRD schema
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

cmd/spire-sidecar/main.go         — familiar binary (retained for historical
                                     non-wizard use; not deployed into wizard pods)
Dockerfile.mayor                   — mayor/operator image
Dockerfile.agent                   — wizard-pod image (runs `spire execute`)
spire.yaml                         — this repo's own agent config
pkg/repoconfig/repoconfig.go      — spire.yaml parser + auto-detection
pkg/metrics/recorder.go            — agent_runs table writer
cmd/spire/metrics.go               — spire metrics command
migrations/agent_runs.sql          — agent_runs + golden_prompts DDL
```

## Deprecated: agent-entrypoint.sh / Model A

Earlier revisions of this document described a richer wizard pod
("Model A") with:

- a main container running `agent-entrypoint.sh` (bash-driven clone,
  seed, claim, Claude invocation, validate, push)
- a familiar sidecar at `:8080` for inbox polling, control commands,
  health probes, and heartbeats
- a shared `/comms` emptyDir for filesystem IPC between worker and
  familiar
- a `beads-seed` ConfigMap to initialize `.beads/`

That model is **removed from main** because it diverged from the code
path actually executed by `pkg/agent/backend_k8s.go`, which spawns a
single-container pod running `spire execute` directly. Only the
canonical wizard pod contract described above is promised on main.

Tracked under epic **spi-kjh9e** with design **spi-lm26c**.
