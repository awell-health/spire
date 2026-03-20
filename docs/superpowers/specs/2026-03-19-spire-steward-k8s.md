# Spire Mayor — Kubernetes Work Coordinator

**Date**: 2026-03-19
**Status**: Draft

## Problem

Today, spire's work coordination relies on humans manually running `spire collect`,
`bd ready`, and `spire claim`. The daemon syncs Linear but doesn't assign work.
There's no automated layer that watches for ready work and dispatches it to agents.

Local agents (Claude Code sessions) are ephemeral — they start, do a task, and end.
Nobody is watching the queue and saying "this is ready, someone should pick it up."

## Design principle

**The mayor coordinates. Local agents execute.**

The mayor is a lightweight process running in Kubernetes that watches the beads
database for ready work and assigns it to agents via spire messaging. It never
clones a repo, never runs Claude, never writes code. It's a dispatcher.

Local agents are Claude Code sessions on developer machines. They already have
git access, the codebase, and the ability to write and test code. They receive
assignments through `spire collect` (via the SessionStart hook) and execute them
using the existing spire-work protocol.

## Architecture

```
┌─────────────────────────────────────┐
│  k8s: mayor pod (single replica)    │
│                                     │
│  - polls DoltHub for ready beads    │
│  - assigns work via spire send      │
│  - monitors progress (in_progress   │
│    beads that go stale)             │
│  - pushes state to DoltHub          │
│                                     │
│  Needs: ANTHROPIC_API_KEY (for AI   │
│  prioritization, optional),         │
│  DoltHub credentials, Linear API    │
│  key (optional)                     │
└──────────────┬──────────────────────┘
               │
               │ DoltHub (beads state)
               │
        ┌──────┼──────┐
        ▼      ▼      ▼
      local   local   local
      agent   agent   agent
      (dev    (dev    (CI)
      laptop) laptop)
```

## What the mayor does

### Core loop

```
every 2 minutes:
  1. bd dolt pull                          # sync latest state
  2. bd ready --json                       # find unblocked, unassigned work
  3. for each ready bead:
     a. select an agent (round-robin, or by label matching)
     b. spire send <agent> "claim <bead-id>" --ref <bead-id> --priority <p>
  4. check for stale in_progress beads     # claimed but no progress in N hours
     a. send reminder or reassign
  5. bd dolt push                          # push state changes
```

### Agent selection

Simple first pass: round-robin across registered agents. The mayor reads the
agent roster (`spire roster` / `bd list --label "agent"`) and cycles through them.

Future: label-based routing. Agents register with capabilities:
```bash
spire register frontend --context "React, Next.js, TypeScript"
spire register backend --context "Go, PostgreSQL, gRPC"
```

The mayor matches bead labels/prefixes to agent capabilities.

### Stale work detection

If a bead has been `in_progress` for longer than a configurable threshold
(default: 4 hours) with no comments or status changes, the mayor:
1. Sends a reminder to the assigned agent
2. After another threshold (default: 2 hours), unassigns and reassigns

### What the mayor does NOT do

- Clone repos or write code
- Run Claude or any LLM (except optionally for prioritization)
- Manage agent processes (start/stop/restart)
- Replace the daemon (Linear sync stays in the daemon)

## Agent side

No changes to local agent workflow. Agents already:
1. Run `spire collect` on session start (via SessionStart hook)
2. See messages like "claim spi-xyz"
3. Run `spire claim spi-xyz` / `spire focus spi-xyz`
4. Do the work
5. `bd close` / `bd dolt push`

The only new thing: agents should `spire register <name>` so the mayor knows
they exist. This could happen automatically in the SessionStart hook.

## Kubernetes manifests

### Namespace and secrets

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: spire
---
apiVersion: v1
kind: Secret
metadata:
  name: spire-credentials
  namespace: spire
type: Opaque
stringData:
  DOLT_REMOTE_USER: "<dolthub-username>"
  DOLT_REMOTE_PASSWORD: "<dolthub-token>"
  LINEAR_API_KEY: "<optional>"
  ANTHROPIC_API_KEY: "<optional, for AI prioritization>"
```

### Mayor Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: spire-mayor
  namespace: spire
spec:
  replicas: 1
  selector:
    matchLabels:
      app: spire-mayor
  template:
    metadata:
      labels:
        app: spire-mayor
    spec:
      containers:
        - name: mayor
          image: ghcr.io/awell-health/spire-mayor:latest
          envFrom:
            - secretRef:
                name: spire-credentials
          env:
            - name: MAYOR_INTERVAL
              value: "2m"
            - name: MAYOR_STALE_THRESHOLD
              value: "4h"
            - name: DOLTHUB_REMOTE
              value: "https://doltremoteapi.dolthub.com/awell/spire"
          resources:
            requests:
              cpu: 50m
              memory: 128Mi
            limits:
              cpu: 200m
              memory: 256Mi
```

### Container image

The mayor image needs:
- `spire` binary (for messaging: `spire send`, `spire register`)
- `bd` binary (for beads: `bd ready`, `bd dolt pull/push`, `bd list`)
- `dolt` binary (for the local database that syncs with DoltHub)
- No git, no node, no Claude

```dockerfile
FROM golang:1.22-alpine AS build
# ... build spire binary ...

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=build /spire /usr/local/bin/spire
COPY --from=dolt /usr/local/bin/dolt /usr/local/bin/dolt
# bd installed via: curl -sSL https://beads.sh/install | sh
COPY --from=bd /usr/local/bin/bd /usr/local/bin/bd

ENTRYPOINT ["spire", "mayor"]
```

Tiny image. No GPU, no large dependencies. Runs on the smallest node pool.

## New CLI: `spire mayor`

A new subcommand that runs the mayor loop. Designed to run in a container but
also usable locally for testing.

```bash
spire mayor                          # run the mayor loop
spire mayor --once                   # run one cycle and exit (for testing)
spire mayor --interval 5m            # custom poll interval
spire mayor --dry-run                # show what would be assigned, don't send
```

### Implementation sketch

```go
// cmd/spire/mayor.go
func cmdMayor(args []string) error {
    interval := 2 * time.Minute
    dryRun := false
    once := false
    // ... parse flags ...

    // Initialize: bd init if needed, set up DoltHub remote
    ensureMayorDB()

    for {
        if err := mayorCycle(dryRun); err != nil {
            log.Printf("mayor cycle error: %s", err)
        }
        if once {
            return nil
        }
        time.Sleep(interval)
    }
}

func mayorCycle(dryRun bool) error {
    // 1. Sync
    bd("dolt", "pull")

    // 2. Find ready work
    readyJSON, _ := bd("ready", "--json")
    var ready []Bead
    json.Unmarshal([]byte(readyJSON), &ready)

    // 3. Find available agents
    roster := loadRoster()

    // 4. Assign
    for _, bead := range ready {
        agent := selectAgent(roster, bead)
        if agent == "" {
            continue // no available agents
        }
        if dryRun {
            fmt.Printf("  [dry-run] would assign %s to %s\n", bead.ID, agent)
            continue
        }
        spire("send", agent, fmt.Sprintf("claim %s", bead.ID),
            "--ref", bead.ID, "--priority", bead.Priority)
    }

    // 5. Check stale
    checkStaleBeads(roster, dryRun)

    // 6. Push
    bd("dolt", "push")
    return nil
}
```

## Authentication and token management

### The mayor

The mayor itself only needs an Anthropic key if you want it to use AI for
prioritization or smart agent matching. Otherwise it's pure logic, no LLM calls.

### Agent tokens

Different work may need different Anthropic tokens — billing separation,
rate limit isolation, model access tiers. The mayor should be able to specify
which token an agent should use for a given task.

Token routing via beads labels or config:

```yaml
# In spire config or mayor config
tokens:
  default: "sk-ant-..."       # standard work
  heavy: "sk-ant-..."         # large context / opus tasks
  ci: "sk-ant-..."            # CI/automation budget
  customer-x: "sk-ant-..."    # customer-funded work

routing:
  - match: { priority: [0, 1] }
    token: heavy               # P0/P1 gets the good token
  - match: { prefix: "xserver-" }
    token: customer-x          # customer-specific billing
  - match: { label: "ci" }
    token: ci
  - default: default
```

The mayor includes the token name in the assignment message. The agent's
SessionStart hook (or the claim process) sets `ANTHROPIC_API_KEY` from
a local keyring or secrets manager before launching Claude.

For local dev: users are already logged in, token routing is optional.
For CI/headless: pass the token as a secret, use the Agent SDK.

### Token storage

Tokens should NOT be in DoltHub or beads. Options:
- **k8s Secrets** — mayor reads them, includes token name (not value) in messages
- **Local keyring** — agents resolve token names to values locally
- **Vault/SSM** — for production, the agent pod fetches from a secrets manager

The message says "use token: heavy". The agent knows how to resolve that name
to an actual key based on its own environment.

## Minikube testing plan

```bash
# 1. Start minikube
minikube start

# 2. Build the mayor image locally
eval $(minikube docker-env)
docker build -t spire-mayor:dev -f Dockerfile.mayor .

# 3. Create secrets
kubectl create namespace spire
kubectl create secret generic spire-credentials \
  --namespace spire \
  --from-literal=DOLT_REMOTE_USER=$DOLT_REMOTE_USER \
  --from-literal=DOLT_REMOTE_PASSWORD=$DOLT_REMOTE_PASSWORD

# 4. Deploy
kubectl apply -f k8s/mayor.yaml

# 5. Test: create a bead locally, watch the mayor assign it
spire file "Test mayor assignment" -t task -p 2
bd dolt push
kubectl logs -n spire deploy/spire-mayor -f

# 6. Check your inbox
spire collect
```

## Operator architecture

The mayor is really an operator. Instead of a dumb poll loop, it watches Custom
Resources that declare the desired state of work and agents, and reconciles.

### Custom Resource Definitions

**SpireAgent** — declares an agent that can do work:

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: SpireAgent
metadata:
  name: jb-frontend
  namespace: spire
spec:
  # Identity
  displayName: "JB's frontend agent"
  capabilities: ["React", "TypeScript", "Next.js"]
  prefixes: ["open-", "web-"]          # what beads it can work on

  # Execution
  mode: external                        # external = local dev machine
  # mode: managed                       # managed = operator creates a pod
  token: default                        # which Anthropic token to use

  # For managed mode only:
  # image: ghcr.io/awell-health/claude-agent:latest
  # repo: https://github.com/awell-health/open-orchestration
  # resources:
  #   requests: { cpu: 100m, memory: 512Mi }

status:
  registered: true
  lastSeen: "2026-03-19T14:30:00Z"     # last spire collect or heartbeat
  currentWork: "open-xyz"               # bead currently claimed
  phase: Idle                           # Idle | Working | Stale | Offline
```

**SpireWorkload** — a bead assignment, created by the operator when work is ready:

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: SpireWorkload
metadata:
  name: open-xyz
  namespace: spire
spec:
  beadId: "open-xyz"
  priority: 1
  type: task
  prefixes: ["open-"]                   # which agent pool can handle it
  token: heavy                          # override token for this work

status:
  phase: Assigned                       # Pending | Assigned | InProgress | Done | Stale
  assignedTo: jb-frontend
  assignedAt: "2026-03-19T14:31:00Z"
  lastProgress: "2026-03-19T14:35:00Z"
```

**SpireConfig** — singleton, cluster-wide configuration:

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: SpireConfig
metadata:
  name: default
  namespace: spire
spec:
  dolthub:
    remote: "https://doltremoteapi.dolthub.com/awell/spire"
    credentialsSecret: spire-credentials
  polling:
    interval: 2m
    staleThreshold: 4h
    reassignThreshold: 6h
  tokens:
    default: { secret: spire-credentials, key: ANTHROPIC_API_KEY_DEFAULT }
    heavy: { secret: spire-credentials, key: ANTHROPIC_API_KEY_HEAVY }
    ci: { secret: spire-credentials, key: ANTHROPIC_API_KEY_CI }
  routing:
    - match: { priority: [0, 1] }
      token: heavy
    - match: { label: "ci" }
      token: ci
    - default: default
```

### Operator reconciliation loops

The operator runs three controllers:

**1. Bead watcher** (every `polling.interval`):
```
bd dolt pull
ready = bd ready --json
for each ready bead not already a SpireWorkload:
    create SpireWorkload CR (phase: Pending)
for each SpireWorkload where bead is now closed:
    update phase → Done
```

**2. Workload assigner** (watches SpireWorkload):
```
on SpireWorkload with phase=Pending:
    find SpireAgents that match prefixes + are Idle + are registered
    pick one (round-robin, or least-recently-assigned)
    spire send <agent> "claim <bead-id>" --ref <bead-id>
    update SpireWorkload: phase=Assigned, assignedTo=<agent>

on SpireWorkload with phase=Assigned and age > staleThreshold:
    send reminder to agent
    if age > reassignThreshold:
        unassign, set phase=Pending (goes back into the pool)
```

**3. Agent monitor** (watches SpireAgent):
```
on SpireAgent:
    if lastSeen older than 30min → phase=Offline
    if mode=managed and phase=Idle and pending work exists:
        create Pod from agent spec (headless claude execution)
    if mode=managed and phase=Done:
        delete Pod
```

### External vs managed agents

Two modes, same CRD:

**External (mode: external):**
- Agent is a human's local Claude session
- Operator only sends messages and monitors progress
- The SpireAgent CR is just a registration — "I exist, I can do this work"
- Created by `spire register` (which could `kubectl apply` under the hood)
- lastSeen updated by `spire collect` (heartbeat)

**Managed (mode: managed):**
- Operator creates and manages pods
- Pod runs claude headlessly via Agent SDK
- Operator handles full lifecycle: create pod → inject work → collect results → delete pod
- This is the "CI agent" path — no human in the loop

You'd start with external-only. Managed comes later when you want fully
autonomous agents.

### Directory structure

```
spire/
├── cmd/
│   └── spire/
│       └── mayor.go          # can still run standalone for local testing
├── operator/
│   ├── api/
│   │   └── v1alpha1/
│   │       ├── spireagent_types.go
│   │       ├── spireworkload_types.go
│   │       └── spireconfig_types.go
│   ├── controllers/
│   │   ├── bead_watcher.go
│   │   ├── workload_assigner.go
│   │   └── agent_monitor.go
│   ├── Dockerfile
│   └── main.go               # operator entrypoint (controller-runtime)
├── k8s/
│   ├── crds/                  # generated from types
│   ├── rbac/
│   ├── deployment.yaml
│   └── kustomization.yaml
```

Built with `controller-runtime` (kubebuilder). The operator binary includes
the spire and bd functionality directly (no shelling out in production — the
standalone `spire mayor` command is for local testing).

### Scaling path

```
Phase 1: spire mayor (standalone binary, dumb poll loop)
         → works in minikube, no CRDs, no operator
         → external agents only

Phase 2: CRDs + operator (SpireAgent, SpireWorkload, SpireConfig)
         → declarative agent management
         → still external agents only
         → kubectl apply to register agents

Phase 3: managed agents (mode: managed)
         → operator creates pods for headless work
         → Agent SDK for execution
         → full autonomy
```

Phase 1 is a weekend. Phase 2 is a week. Phase 3 depends on the Agent SDK
maturity and how much you trust headless agents.

## Implementation order

### Phase 1: `spire mayor` standalone (minikube demo)

1. **`spire mayor` command** — core poll loop (ready → assign → monitor stale)
2. **`spire roster`** — list registered agents (already partially exists)
3. **Dockerfile.mayor** — minimal image: spire + bd + dolt
4. **k8s manifests** — Deployment, Secret, Namespace (plain YAML, no CRDs)
5. **Minikube test** — create bead locally, watch mayor assign it

### Phase 2: CRDs + operator

6. **CRD types** — SpireAgent, SpireWorkload, SpireConfig (kubebuilder scaffold)
7. **Bead watcher controller** — syncs beads → SpireWorkload CRs
8. **Workload assigner controller** — matches SpireWorkloads to SpireAgents
9. **Agent monitor controller** — tracks heartbeats, detects stale/offline
10. **`spire register --k8s`** — creates SpireAgent CR from local machine

### Phase 3: managed agents

11. **Pod lifecycle** — operator creates/deletes pods for managed SpireAgents
12. **Agent SDK integration** — headless claude execution in pods
13. **Token injection** — operator mounts the right secret per-workload
14. **Result collection** — operator reads commit/push output, updates CR status

Phase 1 is a weekend. Phase 2 is a week. Phase 3 depends on Agent SDK maturity.

## Open questions

- **Agent heartbeat**: How does the mayor know an agent is alive? Options:
  periodic `spire send mayor "alive"` from agents, or just rely on stale detection.
  In Phase 2, the SpireAgent CR status.lastSeen is updated by `spire collect`.
- **Multi-hub**: If multiple DoltHub databases exist, does the mayor watch all of
  them or one? Start with one. SpireConfig can specify multiple databases later.
- **Scaling**: Single replica is fine for now. If the poll interval is 2min and
  cycle takes <10s, there's no concurrency concern. Shard by database later if needed.
- **Linear integration**: The daemon currently handles Linear sync. Should the
  mayor absorb the daemon's role? Probably yes, eventually — they're both poll loops.
- **Operator framework**: kubebuilder vs operator-sdk vs raw controller-runtime.
  kubebuilder is the standard choice for Go operators. Generates CRD manifests
  from Go types, scaffolds controllers, handles leader election.
- **Agent trust**: Managed agents run code autonomously. What guardrails? Options:
  read-only git access (can only push to feature branches), PR-only workflow
  (agents create PRs, humans merge), or full trust with audit logging.
