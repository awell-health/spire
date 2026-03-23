# Spire Cluster Deployment

## Prerequisites

- [minikube](https://minikube.sigs.k8s.io/) (or any k8s cluster)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [Docker](https://docs.docker.com/get-docker/)
- An Anthropic API key (for managed agents)

## Quick Start

```bash
# 1. Start minikube
minikube start

# 2. Build images and deploy everything
make deploy
```

That's it. This builds both images, loads them into minikube, applies all manifests, and restarts deployments.

## Step-by-Step

### 1. Start minikube

```bash
minikube start
```

### 2. Create secrets

Edit `k8s/secrets.yaml` with your credentials, then apply:

```bash
kubectl apply -f k8s/secrets.yaml
```

Required secrets:
- `ANTHROPIC_API_KEY_DEFAULT` — Anthropic API key for managed agents

Optional:
- `ANTHROPIC_API_KEY_HEAVY` — separate key for Opus-tier workloads (artificer)
- `LINEAR_API_KEY` — for Linear epic sync

### 3. Update the beads-seed ConfigMap

The `k8s/beads-seed.yaml` ConfigMap contains the project ID for the shared dolt database. If you're starting fresh, deploy first and then update it:

```bash
# After dolt is running, get the project ID:
kubectl exec deploy/spire-dolt -n spire -- \
  dolt sql -q "USE spi; SELECT value FROM metadata WHERE \`key\`='_project_id'" -r csv

# Update k8s/beads-seed.yaml with the returned project_id, then:
kubectl apply -f k8s/beads-seed.yaml
```

If connecting to an existing dolt database, the project ID in `beads-seed.yaml` must match.

### 4. Build and deploy

```bash
make deploy
```

Or step by step:

```bash
make build          # build Docker images
make load           # load into minikube
make apply          # kubectl apply -k k8s/
make restart        # restart deployments to pick up new images
```

### 5. Configure agents

Apply the SpireConfig (token routing, polling intervals):

```bash
kubectl apply -f k8s/examples/config.yaml
```

Register managed agents:

```bash
# Edit the example to match your repo/image:
kubectl apply -f k8s/examples/agent-managed.yaml
```

### 6. Verify

```bash
make status
```

Expected output:
- `spire-dolt` pod — Running
- `spire-operator` pod — Running
- `spire-steward` pod — Running (2 containers: steward + sidecar)
- SpireAgents — listed with phase Idle/Working

## Architecture

```
                    ┌─────────────┐
                    │  dolt (PVC) │  shared SQL database
                    └──────┬──────┘
                           │ port 3306
          ┌────────────────┼────────────────┐
          │                │                │
    ┌─────┴─────┐   ┌─────┴─────┐   ┌──────┴──────┐
    │  steward  │   │  operator │   │ wizard pods │
    │  (PVC)    │   │           │   │ (emptyDir)  │
    └───────────┘   └───────────┘   └─────────────┘
```

- **dolt**: Persistent database. All beads state lives here.
- **steward**: Finds ready work, routes to agents. PVC persists `.beads/` config across restarts.
- **operator**: Watches SpireAgent/SpireWorkload CRs, creates pods for managed agents.
- **wizard pods**: One-shot pods that execute tasks. `.beads/` seeded from ConfigMap.

All pods connect to dolt over SQL. No DoltHub remotes in the loop — deploy the optional `syncer.yaml` if you want DoltHub backup.

## Makefile Targets

| Target | What it does |
|--------|-------------|
| `make build` | Build both Docker images |
| `make build-steward` | Build steward image only |
| `make build-agent` | Build agent image only |
| `make deploy` | Full: build + load + apply + restart |
| `make steward` | Rebuild + load + restart steward only |
| `make agent` | Rebuild + load agent image only |
| `make operator` | Rebuild steward image + restart operator |
| `make apply` | `kubectl apply -k k8s/` |
| `make status` | Show pods, agents, workloads |
| `make logs` | Tail steward logs |
| `make logs-operator` | Tail operator logs |
| `make clean` | Delete the spire namespace |

## Iterating

After code changes, the fastest path depends on what you changed:

```bash
# Changed steward code or entrypoint:
make steward        # ~15s (Go compile + load + restart)

# Changed agent/artificer code or agent-entrypoint.sh:
make agent          # ~15s (Go compile + load)
# Then restart any running wizard pods or wait for next assignment

# Changed operator code:
make operator       # ~15s

# Changed k8s manifests:
make apply          # instant

# Changed everything:
make deploy         # ~30s
```

Docker layer caching means only the Go compile runs on code changes. Dependencies (Go modules, bd, dolt, Alpine packages, Claude CLI) are all cached.

## Optional: DoltHub Sync

To enable DoltHub backup/sync, deploy the syncer:

```bash
kubectl apply -f k8s/syncer.yaml
```

This runs `spire pull` + `spire push` on an interval (default 5m), keeping the cluster in sync with DoltHub. If not deployed, the cluster works entirely locally.

## Troubleshooting

**Pods stuck in Pending**: Check PVCs are bound: `kubectl get pvc -n spire`

**Steward logs show errors**: `make logs` — look for dolt connectivity or config issues.

**Wizard pods crash-looping**: Check the beads-seed ConfigMap has the correct project ID. Verify with:
```bash
kubectl exec deploy/spire-dolt -n spire -- \
  dolt sql -q "USE spi; SELECT value FROM metadata WHERE \`key\`='_project_id'" -r csv
```

**No work being assigned**: `make status` — check SpireAgents exist and aren't Offline. Check SpireWorkloads exist (bead bridge creates these from ready beads).
