# Cluster Deployment Guide

This guide covers deploying Spire to Kubernetes for team and production use.

**Prerequisites:** Complete the [getting started guide](getting-started.md) first. The cluster adopts an existing tower — create your tower locally before deploying to k8s.

---

## Architecture overview

```
Developer laptop          DoltHub                 Kubernetes cluster
-----------------         -------                 ------------------
spire tower create ──>  remote ──────────────> syncer pod (pull)
spire push         ──>  remote
spire pull         <──  remote <────────────── syncer pod (push)
                                                      |
                                                 operator
                                                  ├── BeadWatcher
                                                  ├── WorkloadAssigner
                                                  └── AgentMonitor
                                                      |
                                                 wizard pods
                                                  ├── agent container
                                                  └── sidecar container
```

The cluster never creates a tower — it attaches to one you created locally. Developers file work locally, push to DoltHub, and the cluster picks it up.

---

## Quick start (minikube)

For local development and testing:

```bash
# Start minikube if not running
minikube start --memory=4096 --cpus=2

# Deploy Spire
./k8s/minikube-demo.sh
```

The demo script builds images, applies manifests, prompts for credentials, and starts the operator.

---

## Production deployment

### Step 1: Build and push images

```bash
# Set your registry
REGISTRY=ghcr.io/your-org

# Build steward image (operator + steward)
docker build -f Dockerfile.steward -t $REGISTRY/spire-steward:latest .
docker push $REGISTRY/spire-steward:latest

# Build agent image (wizard + sidecar + toolchains)
docker build -f Dockerfile.agent -t $REGISTRY/spire-agent:latest .
docker push $REGISTRY/spire-agent:latest
```

The agent image includes: Go, Node.js, Python, git, dolt, `spire`, `bd`, and the `claude` CLI.

### Step 2: Create the namespace and CRDs

```bash
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/crds/
```

### Step 3: Create secrets

```bash
kubectl create secret generic spire-credentials \
  --namespace spire \
  --from-literal=DOLT_REMOTE_USER="your-dolthub-user" \
  --from-literal=DOLT_REMOTE_PASSWORD="your-dolthub-token" \
  --from-literal=ANTHROPIC_API_KEY_DEFAULT="sk-ant-..." \
  --from-literal=GITHUB_TOKEN="ghp_..."
```

Or apply `k8s/secrets.yaml` after editing it:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: spire-credentials
  namespace: spire
type: Opaque
stringData:
  DOLT_REMOTE_USER: "your-dolthub-user"
  DOLT_REMOTE_PASSWORD: "your-dolthub-token"
  ANTHROPIC_API_KEY_DEFAULT: "sk-ant-..."
  GITHUB_TOKEN: "ghp_..."
```

### Step 4: Apply SpireConfig

Create `k8s/spire-config.yaml`:

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: SpireConfig
metadata:
  name: default
  namespace: spire
spec:
  dolthub:
    remote: your-org/your-tower    # DoltHub remote path
    credentialsSecret: spire-credentials
  polling:
    interval: 2m
    staleThreshold: 10m
    reassignThreshold: 30m
  tokens:
    default:
      secret: spire-credentials
      key: ANTHROPIC_API_KEY_DEFAULT
  defaultToken: default
```

```bash
kubectl apply -f k8s/spire-config.yaml
```

### Step 5: Deploy dolt

The dolt database is the shared state layer. Deploy it with a PersistentVolumeClaim:

```bash
kubectl apply -f k8s/beads-pvc.yaml
kubectl apply -f k8s/dolt.yaml
```

Wait for it to be ready:

```bash
kubectl rollout status deployment/spire-dolt -n spire
```

### Step 6: Deploy the operator and steward

```bash
kubectl apply -f k8s/operator.yaml
kubectl apply -f k8s/steward.yaml
```

The operator runs three concurrent control loops:
- **BeadWatcher**: watches `bd ready --json`, creates SpireWorkload CRs
- **WorkloadAssigner**: matches workloads to agents, handles capacity
- **AgentMonitor**: creates agent pods, monitors heartbeats, reaps completed pods

### Step 7: Register agents

Create SpireAgent CRDs for each repo you want agents to work on:

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: SpireAgent
metadata:
  name: my-repo-agent
  namespace: spire
spec:
  mode: managed              # operator creates pods for this agent
  image: ghcr.io/your-org/spire-agent:latest
  repo: https://github.com/your-org/my-repo.git
  repoBranch: main
  prefixes: ["myp-"]         # must match the prefix used in spire repo add
  maxConcurrent: 2           # max parallel wizards
  capabilities: ["implement"]
```

```bash
kubectl apply -f k8s/agents/my-repo-agent.yaml
```

Check agents are online:

```bash
kubectl get spireagents -n spire
```

### Step 8: Verify the pipeline

File a bead and push it:

```bash
# On your laptop
spire file "Test cluster deployment" -t task -p 2
spire push
```

Watch the cluster pick it up:

```bash
# Watch workloads appear
kubectl get spireworkloads -n spire -w

# Watch agent pods get created
kubectl get pods -n spire -w

# Check operator logs
kubectl logs -n spire deploy/spire-operator -f
```

Workloads move: `Pending` → `Assigned` → `InProgress` → `Done`.

---

## Resources

### Storage

| Resource | Purpose | Default size |
|----------|---------|-------------|
| `beads-pvc` | Dolt database storage | 10Gi |
| `steward-pvc` | Steward state persistence | 1Gi |

Adjust sizes in `k8s/beads-pvc.yaml` and `k8s/steward-pvc.yaml` before deploying.

### Resource requests

Agent pods run LLM inference, so they need network access but minimal CPU/memory for the sidecar process. The Claude API does the heavy lifting remotely.

Recommended resource requests:

```yaml
resources:
  requests:
    memory: "256Mi"
    cpu: "100m"
  limits:
    memory: "1Gi"
    cpu: "500m"
```

Set these in the SpireAgent CRD under `spec.resources`.

### RBAC

The operator needs cluster-level access to manage pods and custom resources. The `k8s/operator.yaml` includes the necessary ServiceAccount, ClusterRole, and ClusterRoleBinding.

If your cluster enforces namespace-scoped RBAC, edit these to scope to the `spire` namespace only.

---

## CRD reference

### SpireAgent

Represents an entity that can do work.

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: SpireAgent
spec:
  mode: managed | external     # "managed" = operator creates pods; "external" = your process
  image: string                # container image (managed only)
  repo: string                 # git repo URL to clone (managed only)
  repoBranch: string           # branch to clone (default: main)
  prefixes: [string]           # bead prefixes this agent can handle
  maxConcurrent: int           # max simultaneous workloads
  token: string                # token name from SpireConfig (default: "default")
  capabilities: [string]       # what this agent can do (default: ["implement"])
  resources:                   # k8s resource requests/limits
    requests:
      memory: string
      cpu: string
    limits:
      memory: string
      cpu: string
```

### SpireWorkload

Represents a bead assignment. Created by the operator, consumed by the assigner.

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: SpireWorkload
spec:
  beadId: string               # bead ID (e.g., "myp-a3f8")
  priority: int                # from bead priority (0=critical, 4=low)
  beadType: string             # task | bug | feature | epic | chore
```

Status fields (set by operator):

| Field | Description |
|-------|-------------|
| `phase` | `Pending` → `Assigned` → `InProgress` → `Done` / `Stale` / `Failed` |
| `assignedAgent` | Name of the SpireAgent handling this workload |
| `startTime` | When the pod started |
| `completionTime` | When the pod finished |

### SpireConfig

Cluster singleton for global configuration.

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: SpireConfig
spec:
  dolthub:
    remote: string             # DoltHub remote (org/repo-name)
    credentialsSecret: string  # k8s Secret name
  polling:
    interval: duration         # steward cycle interval (default: 2m)
    staleThreshold: duration   # mark stale after this (default: 10m)
    reassignThreshold: duration # reassign after this (default: 30m)
  tokens:
    <name>:
      secret: string           # k8s Secret name
      key: string              # key in the Secret
  defaultToken: string         # which token to use by default
```

---

## Monitoring

### Check operator health

```bash
kubectl logs -n spire deploy/spire-operator | grep -E "cycle|error|bead"
```

The operator logs cycle summaries: `totalReady`, `assigned`, `inProgress`, `done`.

### Check agent pods

```bash
# All agent pods
kubectl get pods -n spire -l spire.awell.io/managed=true

# Logs for a specific wizard
kubectl logs -n spire spire-agent-my-agent-spi-a3f8 -c wizard

# Sidecar logs
kubectl logs -n spire spire-agent-my-agent-spi-a3f8 -c sidecar
```

### Check workload status

```bash
kubectl get spireworkloads -n spire -o wide
```

### Metrics

From your local machine:

```bash
spire pull            # sync latest results
spire metrics         # summary
spire metrics --model # cost breakdown
```

---

## Syncer (optional)

For continuous bidirectional sync without the daemon:

```bash
kubectl apply -f k8s/syncer.yaml
```

The syncer CronJob runs `spire pull && spire push` on the configured interval. Use it when you want the cluster to auto-sync with DoltHub without relying on the daemon's sync cycle.

---

## Troubleshooting

### Operator can't sync from DoltHub

```bash
kubectl logs -n spire deploy/spire-operator | grep "pull failed"
```

Check credentials:

```bash
kubectl get secret spire-credentials -n spire -o jsonpath='{.data}' | \
  jq 'keys'    # verify expected keys are present
```

### Workloads stuck in Pending

No available agent matches the workload. Check:

1. Agent `spec.prefixes` includes the bead's prefix
2. Agent `status.phase` is `Idle` (not `Stale` or `Offline`)
3. Agent `status.currentWork` length is below `spec.maxConcurrent`

```bash
kubectl get spireagents -n spire -o yaml | grep -A5 "status:"
```

### Agent pods not being created

The agent must have `spec.mode: managed`. Check:

```bash
kubectl get spireagents -n spire -o jsonpath='{.items[*].spec.mode}'
```

Also verify `spec.image` is set and pullable by the cluster.

### Worker pod fails immediately

Check worker logs:

```bash
kubectl logs -n spire <pod-name> -c wizard
```

Common causes:
- `GITHUB_TOKEN` not set or lacks `repo` scope
- Repo URL unreachable from inside the cluster
- Bead already claimed by another agent

### Stale workloads not reassigned

Check the `reassignThreshold` in SpireConfig. Default is 30m — workloads aren't reassigned until then.

To force reassignment, delete the SpireWorkload CR:

```bash
kubectl delete spireworkload -n spire <workload-name>
```

The BeadWatcher will recreate it, and WorkloadAssigner will reassign it.
