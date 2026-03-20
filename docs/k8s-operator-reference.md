# Spire Operator Reference

Detailed reference for the Spire Kubernetes operator — flags, controller behavior, pod lifecycle, and CRD specifications.

## Operator flags

The operator binary (`operator/main.go`) accepts:

| Flag | Default | Description |
|------|---------|-------------|
| `--namespace` | `spire` | Namespace to watch for CRDs and create pods in |
| `--interval` | `2m` | Poll interval for all three controllers |
| `--stale-threshold` | `4h` | Time before marking an assigned workload as stale |
| `--reassign-after` | `6h` | Time before unassigning a stale workload for re-matching |
| `--offline-timeout` | `30m` | Time before marking an external agent as offline |
| `--mayor-image` | `ghcr.io/awell-health/spire-mayor:latest` | Default image for managed agent pods |

Standard controller-runtime zap logging flags are also available (`--zap-log-level`, `--zap-devel`, etc.).

## Controller details

### BeadWatcher

**Purpose**: Sync beads from DoltHub into SpireWorkload CRs.

**Cycle** (every `--interval`):

1. `bd dolt pull` — sync latest beads state from DoltHub
2. `bd ready --json` — find beads with all dependencies satisfied
3. For each ready bead not already tracked as a SpireWorkload:
   - Extract prefix from bead ID (e.g., `spi-a3f8` → `spi-`)
   - Create SpireWorkload CR with `status.phase = Pending`
4. `bd list --status=closed --json` — find closed beads
5. For each closed bead that has a tracked SpireWorkload:
   - Set `status.phase = Done`, `status.completedAt = now`
6. `bd dolt push` — push any state changes

**Key behavior**: The watcher is idempotent — if a SpireWorkload already exists for a bead, it skips creation. Closed beads are detected and marked Done even if the worker didn't close them (e.g., human closed a bead manually).

### WorkloadAssigner

**Purpose**: Match pending workloads to available agents.

**Cycle** (every `--interval`):

1. List all SpireWorkloads in namespace
2. Partition by phase:
   - `Pending` or empty → add to pending queue
   - `Assigned`, `InProgress`, `Stale` → check staleness
3. Sort pending queue by priority (lower number = more urgent)
4. For each pending workload, find a matching agent:
   - Skip agents with `status.phase = Offline`
   - Skip agents at capacity (`len(currentWork) >= maxConcurrent`)
   - If both agent and workload have prefixes, require at least one match
   - Take the first match (not globally optimal, but simple and fast)
5. Assign: send message via `spire send`, update workload and agent status

**Staleness timeline**:

```
Assignment
    |
    |--- 4h (staleThreshold) ---> Mark phase=Stale, send reminder message
    |
    |--- 6h (reassignThreshold) -> Unassign, return to Pending for re-matching
```

After reassignment, the workload's `attempts` counter increments. The agent's `currentWork` is NOT cleaned by the assigner — that's the AgentMonitor's job (via pod reaping).

### AgentMonitor

**Purpose**: Track agent health and manage pod lifecycle.

**Cycle** (every `--interval`):

For **external** agents:
- Parse `status.lastSeen` timestamp
- If older than `--offline-timeout`, set `status.phase = Offline`
- If never seen, set Offline with "Never seen" message

For **managed** agents:
1. List pods with labels `spire.awell.io/agent={name}` and `spire.awell.io/managed=true`
2. **Reap**: for each pod in `Succeeded` or `Failed` phase:
   - Remove the pod's bead ID from `agent.status.currentWork`
   - Delete the pod
3. **Create**: for each bead ID in `currentWork` with no existing pod:
   - Build pod spec with worker + sidecar containers
   - Inject env vars, secrets, volume mounts
   - Create the pod
4. **Clean**: for each pod whose bead ID is NOT in `currentWork`:
   - Delete the pod (orphan cleanup)
5. **Phase**: update `agent.status.phase` based on pod states:
   - No work → `Idle`
   - Any pod pending → `Provisioning`
   - All pods running → `Working`

## Pod specification

The operator generates this pod spec for each workload assignment:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: spire-agent-{agent}-{bead}   # max 63 chars
  namespace: spire
  labels:
    spire.awell.io/agent: {agent-name}
    spire.awell.io/bead: {bead-id}
    spire.awell.io/managed: "true"
    app.kubernetes.io/name: spire-agent
spec:
  restartPolicy: Never
  volumes:
    - name: comms
      emptyDir: {}
    - name: workspace
      emptyDir: {}
    - name: data
      emptyDir: {}
  containers:
    - name: worker
      image: {agent.spec.image or default}
      command: ["/usr/local/bin/agent-entrypoint.sh"]
      workingDir: /workspace
      env:
        - name: SPIRE_AGENT_NAME
          value: {agent.name}
        - name: SPIRE_BEAD_ID
          value: {bead-id}
        - name: SPIRE_REPO_URL
          value: {agent.spec.repo}
        - name: SPIRE_REPO_BRANCH
          value: {agent.spec.repoBranch or "main"}
        - name: SPIRE_COMMS_DIR
          value: /comms
        - name: SPIRE_WORKSPACE_DIR
          value: /workspace
        - name: SPIRE_STATE_DIR
          value: /data
        - name: DOLTHUB_REMOTE
          value: {config.spec.dolthub.remote}
        - name: DOLT_REMOTE_USER
          valueFrom:
            secretKeyRef: {credentialsSecret}/DOLT_REMOTE_USER
        - name: DOLT_REMOTE_PASSWORD
          valueFrom:
            secretKeyRef: {credentialsSecret}/DOLT_REMOTE_PASSWORD
        - name: ANTHROPIC_API_KEY
          valueFrom:
            secretKeyRef: {resolved token secret/key}
        - name: GITHUB_TOKEN       # optional
          valueFrom:
            secretKeyRef: {credentialsSecret}/GITHUB_TOKEN
      volumeMounts:
        - name: comms
          mountPath: /comms
        - name: workspace
          mountPath: /workspace
        - name: data
          mountPath: /data
      resources: {from agent.spec.resources}

    - name: sidecar
      image: {same as worker}
      command:
        - spire-sidecar
        - --comms-dir=/comms
        - --poll-interval=10s
        - --port=8080
        - --agent-name={agent.name}
      workingDir: /data
      volumeMounts:
        - name: comms
          mountPath: /comms
        - name: data
          mountPath: /data
      readinessProbe:
        httpGet:
          path: /readyz
          port: 8080
        initialDelaySeconds: 5
        periodSeconds: 10
      livenessProbe:
        httpGet:
          path: /healthz
          port: 8080
        initialDelaySeconds: 10
        periodSeconds: 30
```

## CRD schemas

### SpireAgent

```
apiVersion: spire.awell.io/v1alpha1
kind: SpireAgent

spec:
  displayName:   string          # human-readable name
  mode:          enum(external, managed)  # REQUIRED
  capabilities:  []string        # skills (informational)
  prefixes:      []string        # bead prefixes to match (e.g., ["spi-"])
  token:         string          # Anthropic token name (default: "default")
  maxConcurrent: int             # max workloads (default: 1)
  image:         string          # container image (managed only)
  repo:          string          # git clone URL (managed only)
  repoBranch:    string          # branch to clone (default: "main")
  resources:                     # k8s resources (managed only)
    requests: {cpu, memory}
    limits:   {cpu, memory}

status:
  phase:          enum(Idle, Working, Stale, Offline, Provisioning)
  registered:     bool
  lastSeen:       datetime       # heartbeat timestamp (external only)
  currentWork:    []string       # bead IDs currently assigned
  completedCount: int            # lifetime completed count
  podName:        string         # pod name (managed only)
  message:        string         # human-readable status
```

### SpireWorkload

```
apiVersion: spire.awell.io/v1alpha1
kind: SpireWorkload

spec:
  beadId:    string    # REQUIRED
  title:     string
  priority:  int       # 0=critical, 4=nice-to-have
  type:      string    # task, bug, feature, epic, chore
  prefixes:  []string  # for agent matching
  token:     string    # override token for this workload

status:
  phase:        enum(Pending, Assigned, InProgress, Done, Stale, Failed)
  assignedTo:   string     # agent name
  assignedAt:   datetime
  startedAt:    datetime
  completedAt:  datetime
  lastProgress: datetime
  attempts:     int        # assignment count
  message:      string
```

### SpireConfig

```
apiVersion: spire.awell.io/v1alpha1
kind: SpireConfig

spec:
  dolthub:
    remote:            string   # DoltHub remote URL
    credentialsSecret: string   # k8s Secret name

  polling:
    interval:           duration  # default: "2m"
    staleThreshold:     duration  # default: "4h"
    reassignThreshold:  duration  # default: "6h"

  tokens:                        # map of token name → secret ref
    default:
      secret: spire-credentials
      key: ANTHROPIC_API_KEY_DEFAULT
    heavy:
      secret: spire-credentials
      key: ANTHROPIC_API_KEY_HEAVY

  defaultToken: string           # which token when agent doesn't specify

  routing:                       # future: priority-based token routing
    - match: {priority: "0"}
      token: heavy

status:
  lastSync:      datetime
  beadCount:     int
  agentCount:    int
  workloadCount: int
  message:       string
```

## Worker entrypoint reference

`agent-entrypoint.sh` environment variables:

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `SPIRE_AGENT_NAME` | Yes | — | Agent identity |
| `SPIRE_BEAD_ID` | No | from inbox | Bead to work on |
| `SPIRE_REPO_URL` | Yes* | — | Repo to clone (*unless workspace pre-populated) |
| `SPIRE_REPO_BRANCH` | No | `main` | Branch for clone and base |
| `SPIRE_COMMS_DIR` | No | `/comms` | Shared comms directory |
| `SPIRE_WORKSPACE_DIR` | No | `/workspace` | Git workspace |
| `SPIRE_STATE_DIR` | No | `/data` | Beads state directory |
| `SPIRE_AGENT_PREFIX` | No | `wrk` | Beads prefix for agent's state repo |
| `SPIRE_GIT_EMAIL` | No | `{name}@spire.local` | Git author email |
| `SPIRE_AGENT_CMD` | No | `claude --print` | Custom agent command |
| `DOLTHUB_REMOTE` | No | — | DoltHub remote for state sync |
| `ANTHROPIC_API_KEY` | Yes | — | Claude API key |
| `GITHUB_TOKEN` | No | — | For private repos and `gh` CLI |

### result.json schema

Written by the entrypoint on every exit:

```json
{
  "agentName": "ci-worker",
  "beadId": "spi-a3f8",
  "result": "success",          // success|test_failure|timeout|stopped|error
  "summary": "validated and pushed branch feat/spi-a3f8",
  "branch": "feat/spi-a3f8",
  "commit": "abc1234...",
  "startedAt": "2026-03-19T19:00:00Z",
  "completedAt": "2026-03-19T19:25:00Z",
  "model": "claude-sonnet-4-6",
  "maxTurns": 50,
  "timeout": "30m",
  "testsPassed": true
}
```
