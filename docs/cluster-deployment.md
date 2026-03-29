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
                                                  └── familiar container
```

The cluster never creates a tower — it attaches to one you created locally. Developers file work locally, push to DoltHub, and the cluster picks it up.

---

## Helm chart

Spire ships a Helm chart at `helm/spire/`. The chart deploys:

- The Dolt SQL database (state layer)
- The steward (work coordinator)
- The operator (pod lifecycle manager)
- SpireConfig CRD (cluster singleton configuration)
- SpireAgent CRDs (per-repo agent definitions)

Today those `SpireAgent` definitions are still explicit in chart values.
The operator does not yet derive them automatically from the tower's
`repos` table.

**Requirements:**

- Helm 3.x: `brew install helm`
- A Kubernetes cluster (minikube, EKS, GKE, etc.)
- A DoltHub remote (create with `spire tower create`)

---

## Quick start (minikube)

For local development and testing:

```bash
# Start minikube if not running
minikube start --memory=4096 --cpus=2

# Install Spire with local images
helm install spire ./helm/spire \
  --namespace spire \
  --create-namespace \
  --set images.steward.repository=spire-steward \
  --set images.steward.tag=dev \
  --set images.steward.pullPolicy=IfNotPresent \
  --set images.agent.repository=spire-agent \
  --set images.agent.tag=dev \
  --set images.agent.pullPolicy=IfNotPresent \
  --set dolthub.remote=your-org/your-tower \
  --set dolthub.user=your-dolthub-user \
  --set dolthub.password=your-dolthub-token \
  --set anthropic.apiKey=sk-ant-...
```

To use the demo build script that builds images first:

```bash
./k8s/minikube-demo.sh
```

---

## Production deployment

### Step 1: Build and push images

```bash
REGISTRY=ghcr.io/your-org

docker build -f Dockerfile.steward -t $REGISTRY/spire-steward:latest .
docker push $REGISTRY/spire-steward:latest

docker build -f Dockerfile.agent -t $REGISTRY/spire-agent:latest .
docker push $REGISTRY/spire-agent:latest
```

The agent image includes: Go, Node.js, Python, git, dolt, `spire`, `bd`, and the `claude` CLI.

### Step 2: Create a values file

Create `my-values.yaml`:

```yaml
namespace: spire

images:
  steward:
    repository: ghcr.io/your-org/spire-steward
    tag: latest
  agent:
    repository: ghcr.io/your-org/spire-agent
    tag: latest

dolthub:
  remote: your-org/your-tower
  user: your-dolthub-user
  password: your-dolthub-token

anthropic:
  apiKey: sk-ant-...

github:
  token: ghp_...

beads:
  prefix: spi

# Define agents — one SpireAgent CRD per repo
agents:
  - name: my-repo-agent
    mode: managed
    repo: https://github.com/your-org/my-repo.git
    repoBranch: main
    prefixes: ["myp-"]
    maxConcurrent: 2
    capabilities: ["implement"]
    resources:
      requests:
        memory: "256Mi"
        cpu: "100m"
      limits:
        memory: "1Gi"
        cpu: "500m"
```

### Step 3: Install with Helm

```bash
helm install spire ./helm/spire \
  --namespace spire \
  --create-namespace \
  --values my-values.yaml
```

Watch the rollout:

```bash
kubectl rollout status deployment/spire-dolt -n spire
kubectl rollout status deployment/spire-steward -n spire
kubectl rollout status deployment/spire-operator -n spire
```

### Step 4: Verify the pipeline

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

## Upgrades

To upgrade to a new chart or image version:

```bash
helm upgrade spire ./helm/spire \
  --namespace spire \
  --values my-values.yaml \
  --set images.steward.tag=v0.2.0 \
  --set images.agent.tag=v0.2.0
```

To preview changes before applying:

```bash
helm upgrade spire ./helm/spire \
  --namespace spire \
  --values my-values.yaml \
  --dry-run
```

---

## Configuration reference

All configurable values are documented in `helm/spire/values.yaml`. Key values:

| Value | Default | Description |
|-------|---------|-------------|
| `namespace` | `spire` | Kubernetes namespace |
| `images.steward.repository` | `ghcr.io/awell-health/spire-steward` | Steward image |
| `images.agent.repository` | `ghcr.io/awell-health/spire-agent` | Agent image |
| `dolthub.remote` | `""` | DoltHub remote path (required) |
| `dolthub.user` | `""` | DoltHub username (required) |
| `dolthub.password` | `""` | DoltHub token (required) |
| `anthropic.apiKey` | `""` | Anthropic API key (required) |
| `github.token` | `""` | GitHub PAT (required for repo clone/push operations) |
| `beads.prefix` | `spi` | Hub bead prefix |
| `steward.interval` | `2m` | Steward sync interval |
| `spireConfig.polling.staleThreshold` | `4h` | Mark workload stale after this |
| `spireConfig.polling.reassignThreshold` | `6h` | Reassign stale workloads after this |
| `agents` | `[]` | List of SpireAgent definitions |
| `syncer.enabled` | `false` | Enable DoltHub sync CronJob |
| `syncer.schedule` | `*/2 * * * *` | CronJob schedule |

---

## Resources

### Storage

| Resource | Purpose | Default size |
|----------|---------|-------------|
| `spire-beads-data` | Dolt database storage | 5Gi |
| `spire-steward-data` | Steward state persistence | 1Gi |

Adjust sizes in `my-values.yaml`:

```yaml
dolt:
  storage:
    size: 20Gi
    storageClass: gp3  # optional: leave empty for cluster default

stewardStorage:
  size: 2Gi
```

### RBAC

The operator needs cluster-level access to manage pods and custom resources. The Helm chart creates a ServiceAccount, ClusterRole, and ClusterRoleBinding automatically.

If your cluster enforces namespace-scoped RBAC, patch the ClusterRole after installation to scope it to the `spire` namespace.

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
    staleThreshold: duration   # mark stale after this (default: 4h)
    reassignThreshold: duration # reassign after this (default: 6h)
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
kubectl logs -n spire spire-agent-my-agent-spi-a3f8 -c familiar
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

For continuous bidirectional sync without the daemon, enable the syncer CronJob:

```yaml
# in my-values.yaml
syncer:
  enabled: true
  schedule: "*/2 * * * *"
```

Then upgrade:

```bash
helm upgrade spire ./helm/spire --namespace spire --values my-values.yaml
```

The syncer CronJob runs `spire pull && spire push` on the configured interval.

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

Check the `reassignThreshold` in SpireConfig (configured via `spireConfig.polling.reassignThreshold` in values.yaml). Default is 6h — workloads aren't reassigned until then.

To force reassignment, delete the SpireWorkload CR:

```bash
kubectl delete spireworkload -n spire <workload-name>
```

The BeadWatcher will recreate it, and WorkloadAssigner will reassign it.

### Helm troubleshooting

List installed releases:

```bash
helm list -n spire
```

Check deployed values:

```bash
helm get values spire -n spire
```

Rollback to a previous release:

```bash
helm rollback spire -n spire
```

Uninstall (does not delete PVCs by default):

```bash
helm uninstall spire -n spire
# PVCs persist — delete manually if you want to wipe all state:
# kubectl delete pvc -n spire --all
```
