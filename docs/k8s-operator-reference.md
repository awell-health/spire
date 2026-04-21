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
1. List pods with labels `spire.agent=true` and `spire.agent.name={name}`
2. **Reap**: for each pod in `Succeeded` or `Failed` phase:
   - Remove the pod's bead ID from `agent.status.currentWork`
   - Delete the pod
3. **Create**: for each bead ID in `currentWork` with no existing pod:
   - Build the canonical wizard pod spec (see below)
   - Inject env vars, secrets, and volume mounts
   - Create the pod
4. **Clean**: for each pod whose bead ID is NOT in `currentWork`:
   - Delete the pod (orphan cleanup)
5. **Phase**: update `agent.status.phase` based on pod states:
   - No work → `Idle`
   - Any pod pending → `Provisioning`
   - All pods running → `Working`

## Canonical wizard pod contract

The wizard pod is the **canonical per-bead runtime** in Kubernetes. It is a
**single-container pod** (one main container, one init container) with
`restartPolicy: Never` — the pod is one-shot: it boots, runs the wizard's
formula lifecycle for a single bead, and exits. The operator (or steward's
k8s backend) reaps it on exit.

There is exactly one pod model on `main`. The prior richer model
(worker entrypoint script + familiar sidecar + `/comms` IPC volume) has
been removed — see [Deprecated: agent-entrypoint.sh / Model A](#deprecated-agent-entrypointsh--model-a) below.

### Pod spec

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: {agent-name}              # sanitized; timestamp-suffixed for uniqueness
  namespace: spire
  labels:
    spire.agent: "true"           # selector for network policies / listing
    spire.agent.name: {agent-name}
    spire.bead: {bead-id}
    spire.role: wizard            # wizard | executor | apprentice | sage
    spire.tower: {tower-name}
spec:
  restartPolicy: Never            # one-shot: pod exits, operator reaps
  priorityClassName: spire-agent-default

  volumes:
    - name: data                  # beads workspace + spire config
      emptyDir: {}
    - name: workspace             # git clone target for apprentice bundle production
      emptyDir: {}

  initContainers:
    - name: tower-attach
      image: {agent image}
      command:
        - spire
        - tower
        - attach-cluster
        - --data-dir=/data/{db}
        - --database={db}
        - --prefix={prefix}
        - --dolthub-remote={remote}
      volumeMounts:
        - name: data
          mountPath: /data

  containers:
    - name: agent
      image: {agent image}
      command:
        - spire
        - execute
        - {bead-id}
        - --name
        - {agent-name}
      env:
        # Tower / dolt wiring
        - name: DOLT_DATA_DIR
          value: /data
        - name: SPIRE_CONFIG_DIR
          value: /data/spire-config
        - name: BEADS_DOLT_SERVER_HOST
          value: spire-dolt.{namespace}.svc
        - name: BEADS_DOLT_SERVER_PORT
          value: "3307"

        # Identity
        - name: SPIRE_AGENT_NAME
          value: {agent-name}
        - name: SPIRE_BEAD_ID
          value: {bead-id}
        - name: SPIRE_TOWER
          value: {tower-name}
        - name: SPIRE_ROLE
          value: wizard

        # Observability (OTel)
        - name: OTEL_EXPORTER_OTLP_ENDPOINT
          value: http://spire-steward.{namespace}.svc:4317
        - name: OTEL_EXPORTER_OTLP_PROTOCOL
          value: grpc
        - name: OTEL_TRACES_EXPORTER
          value: otlp
        - name: OTEL_LOGS_EXPORTER
          value: otlp
        - name: OTEL_RESOURCE_ATTRIBUTES
          value: bead.id={bead-id},agent.name={agent-name},tower={tower-name}

        # Secrets
        - name: ANTHROPIC_API_KEY
          valueFrom:
            secretKeyRef:
              name: {credentials secret}
              key: ANTHROPIC_API_KEY_DEFAULT
        - name: GITHUB_TOKEN        # optional
          valueFrom:
            secretKeyRef:
              name: {credentials secret}
              key: GITHUB_TOKEN
              optional: true

      volumeMounts:
        - name: data
          mountPath: /data
        - name: workspace
          mountPath: /workspace

      resources:
        requests:
          memory: 1Gi              # SPIRE_WIZARD_MEMORY_REQUEST
          cpu: 250m                # SPIRE_WIZARD_CPU_REQUEST
        limits:
          memory: 2Gi              # SPIRE_WIZARD_MEMORY_LIMIT
          cpu: 1000m               # SPIRE_WIZARD_CPU_LIMIT
```

### Volumes

| Name        | Type     | Mount path   | Purpose                                                      |
|-------------|----------|--------------|--------------------------------------------------------------|
| `data`      | emptyDir | `/data`      | Beads workspace (dolt data dir) and spire config (`/data/spire-config`) |
| `workspace` | emptyDir | `/workspace` | Git clone target used when the wizard produces apprentice bundles |

No shared `/comms` volume exists. Wizard↔sidecar filesystem IPC is gone;
the wizard process is the whole runtime.

### Init container: `tower-attach`

A single init container named `tower-attach` runs
`spire tower attach-cluster` with:

- `--data-dir=/data/<db>` — dolt data directory under the shared `/data` volume
- `--database=<db>` — dolt database name
- `--prefix=<prefix>` — bead prefix for this tower
- `--dolthub-remote=<remote>` — DoltHub remote for sync

This replaces both the old operator-side `beads-seed` ConfigMap and the
`agent-entrypoint.sh` bootstrap flow. On exit, `/data` is primed with
beads state so the main container can open dolt immediately.

### Main container env

Required on the main (`agent`) container:

| Variable                   | Value / source                                               |
|----------------------------|--------------------------------------------------------------|
| `DOLT_DATA_DIR`            | `/data`                                                      |
| `SPIRE_CONFIG_DIR`         | `/data/spire-config`                                         |
| `BEADS_DOLT_SERVER_HOST`   | In-cluster dolt service (e.g. `spire-dolt.<ns>.svc`)         |
| `BEADS_DOLT_SERVER_PORT`   | `3307`                                                       |
| `SPIRE_AGENT_NAME`         | Agent identity                                               |
| `SPIRE_BEAD_ID`            | Bead the wizard will execute                                 |
| `SPIRE_TOWER`              | Tower name                                                   |
| `SPIRE_ROLE`               | `wizard`                                                     |
| `OTEL_*`                   | OTLP exporter endpoint, protocol, resource attrs (see spec)  |
| `ANTHROPIC_API_KEY`        | From `Secret` (key `ANTHROPIC_API_KEY_DEFAULT`)              |
| `GITHUB_TOKEN`             | From `Secret` (optional)                                     |

### Resource tier

Wizard pods have their own resource tier (distinct from apprentice and
sage tiers). Defaults:

| Field            | Default | Override env                 |
|------------------|---------|------------------------------|
| Memory request   | `1Gi`   | `SPIRE_WIZARD_MEMORY_REQUEST`|
| Memory limit     | `2Gi`   | `SPIRE_WIZARD_MEMORY_LIMIT`  |
| CPU request      | `250m`  | `SPIRE_WIZARD_CPU_REQUEST`   |
| CPU limit        | `1000m` | `SPIRE_WIZARD_CPU_LIMIT`     |

These defaults intentionally give the wizard enough headroom to plan and
dispatch apprentices without OOM-killing under parallel fan-out; they are
higher than the generic executor/sage tier.

### One-shot semantics

- `restartPolicy: Never` — k8s never restarts the pod. If the wizard
  crashes mid-formula, the steward observes the `Failed` phase, records
  the failure, and (per formula policy) may re-dispatch a fresh pod
  against the same bead.
- The pod succeeds (`Succeeded`) when `spire execute` exits 0, and fails
  (`Failed`) otherwise. Reap is driven by pod phase, not by any
  sidecar signal.
- There is no sidecar, no health probe, no long-running ancillary
  process. The pod's lifetime is exactly the lifetime of the wizard
  process.

## Deprecated: agent-entrypoint.sh / Model A

The **richer entrypoint model** — "Model A" — previously documented for
wizard pods has been **removed from main**. That model composed:

- A main container that ran `agent-entrypoint.sh` (a multi-phase bash
  script that cloned the repo, seeded `.beads/`, claimed the bead,
  invoked Claude, validated, and pushed).
- A **familiar sidecar** (`spire-sidecar`) at `:8080` providing
  `/healthz`, `/readyz`, `/status`, inbox polling, and control-channel
  command handling.
- A shared **`/comms`** emptyDir used as a filesystem IPC channel
  between the worker and the familiar (`inbox.json`, `control`,
  `result.json`, `heartbeat`, `steer`, etc.).
- A `beads-seed` ConfigMap that pre-staged `.beads/` layout for the
  worker to copy in.

**Why it was removed.** Model A diverged from the code path actually
executed by `pkg/agent/backend_k8s.go`. The backend spawns a single
container running `spire execute <bead> --name <name>` — no shell
entrypoint, no familiar, no `/comms`, no seed ConfigMap. Maintaining
two contracts (one documented, one implemented) caused operator,
helm, and test drift.

Starting with epic **spi-kjh9e** (design **spi-lm26c**), `main`
promises exactly one contract: the canonical wizard pod documented
above. References to `agent-entrypoint.sh`, the familiar sidecar, the
`/comms` volume, and Model A should be read as historical; they are
preserved only in dated design/plan archives under `docs/design/`,
`docs/plans/`, `docs/reviews/`, and `docs/superpowers/specs/`.

## CRD schemas

### WizardGuild

```
apiVersion: spire.awell.io/v1alpha1
kind: WizardGuild

spec:
  displayName:   string          # human-readable name
  mode:          enum(external, managed)  # REQUIRED
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

## Wizard process

The wizard pod's main container runs `spire execute <bead-id> --name <agent-name>`.
There is no shell entrypoint wrapper. The Go process drives the formula
lifecycle end-to-end: claim the bead, load the formula, execute each
step (planning, apprentice dispatch, review, merge), and exit.

Run outcome is reported through bead status (and, for the steward's own
bookkeeping, through the pod's terminal `Succeeded` / `Failed` phase).
There is no `/comms/result.json` file; a reviewer inspects the bead and
the pod logs.
