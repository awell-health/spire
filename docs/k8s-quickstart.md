# Spire on Kubernetes — Quickstart

Get the full autonomous agent pipeline running on minikube in under 10 minutes.

## Prerequisites

- [minikube](https://minikube.sigs.k8s.io/docs/start/) installed and running
- [kubectl](https://kubernetes.io/docs/tasks/tools/) configured
- [Docker](https://docs.docker.com/get-docker/) installed
- DoltHub account with a beads remote (e.g., `awell/spire`)
- Anthropic API key

## One-command demo

```bash
./k8s/minikube-demo.sh
```

This script:
1. Starts minikube if not running
2. Builds the `spire-mayor:dev` image inside minikube
3. Creates the `spire` namespace
4. Applies all three CRDs (WizardGuild, SpireWorkload, SpireConfig)
5. Prompts for DoltHub credentials and creates the k8s Secret
6. Deploys the mayor
7. Applies example SpireConfig and registers you as an external agent
8. Waits for rollout

## Manual setup

### 1. Create the namespace and CRDs

```bash
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/crds/
```

### 2. Create secrets

```bash
kubectl create secret generic spire-credentials \
  --namespace spire \
  --from-literal=DOLT_REMOTE_USER="your-dolthub-user" \
  --from-literal=DOLT_REMOTE_PASSWORD="your-dolthub-token" \
  --from-literal=ANTHROPIC_API_KEY_DEFAULT="sk-ant-..." \
  --from-literal=GITHUB_TOKEN="ghp_..."
```

Or edit and apply `k8s/secrets.yaml`.

### 3. Apply SpireConfig

```bash
kubectl apply -f k8s/examples/config.yaml
```

Edit `spec.dolthub.remote` to point to your DoltHub remote.

### 4. Build and deploy the mayor

```bash
# If using minikube, point docker to its daemon first:
eval $(minikube docker-env)

# Build
docker build -f Dockerfile.mayor -t spire-mayor:dev .

# Deploy
kubectl apply -f k8s/mayor.yaml
```

### 5. Register agents

**External agent** (your local machine):

```bash
kubectl apply -f k8s/examples/agent-external.yaml
```

Or create your own:

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: WizardGuild
metadata:
  name: your-name
  namespace: spire
spec:
  mode: external
  prefixes: ["spi-", "open-"]
  maxConcurrent: 2
```

**Managed agent** (operator creates pods automatically):

```bash
# Build the agent image first
docker build -f Dockerfile.agent -t spire-agent:dev .

kubectl apply -f k8s/examples/agent-managed.yaml
```

Edit `spec.repo` and `spec.image` in the managed agent YAML.

### 6. File work and watch

```bash
# File a bead locally
spire file "Fix the auth token refresh" -t task -p 1
bd dolt push

# Watch the mayor pick it up
kubectl logs -n spire deploy/spire-mayor -f

# Watch workloads appear
kubectl get spireworkloads -n spire -w

# Watch agent pods get created (managed agents)
kubectl get pods -n spire -w

# Check agent status
kubectl get wizardguilds -n spire
```

## Verifying the pipeline

### Check the mayor is syncing

```bash
kubectl logs -n spire deploy/spire-mayor | grep "bead watcher"
```

You should see cycle logs with `totalReady` counts.

### Check workload assignment

```bash
kubectl get spireworkloads -n spire -o wide
```

Workloads should move from `Pending` → `Assigned` → `InProgress` → `Done`.

### Check managed agent pods

```bash
kubectl get pods -n spire -l spire.awell.io/managed=true
```

Each pod is named `{agent}-wizard-{bead-id}`. The pod has two containers:
- `worker` — runs the agent entrypoint
- `familiar` — polls inbox and serves health checks

### Read pod logs

```bash
# Worker logs
kubectl logs -n spire ci-worker-wizard-spi-a3f8 -c worker

# Familiar logs
kubectl logs -n spire ci-worker-wizard-spi-a3f8 -c familiar
```

### Check metrics

After agents have run:

```bash
spire metrics              # summary
spire metrics --model      # cost breakdown by model
spire metrics --bead spi-a3f8  # per-bead stats
```

## Tear down

```bash
# Delete everything in the spire namespace
kubectl delete namespace spire

# Or just stop the mayor
kubectl delete deploy spire-mayor -n spire
```

## Troubleshooting

**Mayor can't sync from DoltHub:**
Check credentials: `kubectl get secret spire-credentials -n spire -o yaml`. Verify `DOLT_REMOTE_USER` and `DOLT_REMOTE_PASSWORD` are set. Check mayor logs for "bd dolt pull failed".

**Workloads stuck in Pending:**
No available agent matches. Check that agent `spec.prefixes` includes the bead's prefix (e.g., `spi-`), and that `agent.status.currentWork` length is below `spec.maxConcurrent`.

**Agent pods not created:**
The agent must be `mode: managed`. Check `kubectl get wizardguilds -n spire -o yaml` for the agent's spec. Also verify `spec.image` is set and pullable.

**Worker fails immediately:**
Check `SPIRE_REPO_URL` — the worker needs to clone a repo. Check that `GITHUB_TOKEN` is set if the repo is private. Look at worker logs: `kubectl logs <pod> -c worker`.

**Familiar not ready:**
The familiar needs to successfully run `spire collect` at least once. It runs in `/data` where beads state lives. If `/data/.beads` doesn't exist yet (worker hasn't initialized), the familiar will retry.

**Stale workloads not being reassigned:**
Wait for the reassign threshold (default: 6h). Or reduce it: edit SpireConfig `spec.polling.reassignThreshold`.
