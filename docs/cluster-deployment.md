# Cluster Deployment Guide

This guide covers deploying Spire to Kubernetes for team and production use.

**Prerequisites:** Complete the [getting started guide](getting-started.md) first. The cluster adopts an existing tower — create your tower locally before deploying to k8s.

---

## Deployment modes

Spire has three deployment modes: **local-native** (single-machine, local
filesystem), **cluster-native** (multi-user cluster, gateway-fronted Dolt),
and **attached-reserved** (reserved; not implemented). Within
cluster-native, individual clients attach in **gateway mode** — this is
`TowerConfig.Mode` / client routing, not a fourth `DeploymentMode`.

This guide describes cluster-native (cluster-as-truth) deployments.
Local-native towers may still use local filesystem, remotesapi, or
DoltHub as sync transport; that path is unchanged by cluster-as-truth.

---

## Architecture overview

```
Archmage laptop / Desktop          DoltHub             Kubernetes cluster
-------------------------          -------             ------------------
spire tower create  ─────>  remote (first-install seed) ──> dolt-init (clone)
spire tower attach-cluster  ────────────────────────────>  gateway pod (HTTP)
spire file / mutations      ─────── HTTPS POST ─────────>  /api/v1/* (gateway)
                                                                  |
                                                          cluster Dolt
                                                          (write authority)
                                                                  |
                                                           operator
                                                            ├── BeadWatcher
                                                            ├── WorkloadAssigner
                                                            └── AgentMonitor
                                                                  |
                                                           wizard pods (one-shot)
                                                            ├── init: tower-attach
                                                            ├── init: cache-bootstrap
                                                            └── agent container
                                                                (spire execute)

cluster Dolt  ─── one-way archive push ───>  DoltHub (archive only)
cluster Dolt  ─── dolt backup sync ───────>  GCS bucket (archive/DR)
```

For cluster-native production towers, the cluster-hosted Dolt database
accessed through the gateway is the canonical bead-graph host. DoltHub
serves as seed-only on first install and as a one-way archive; it is
not an active writable mirror. Desktop/laptop clients attach via the
gateway and route mutations through `/api/v1/*` over HTTP. GCS backup
is the canonical archive/DR substrate (see [Backup bucket
setup](#backup-bucket-setup) and [Archive and disaster
recovery](#archive-and-disaster-recovery)).

Bidirectional cluster ↔ DoltHub sync was removed because both sides
writing produced non-fast-forward push rejections and merge conflicts
that silently diverged the two stores (witnessed 2026-04-26).
Cluster-as-truth makes the cluster the single writer; DoltHub receives
one-way archive pushes only.

---

## Helm chart

Spire ships a Helm chart at `helm/spire/`. The chart deploys:

- The Dolt SQL database (state layer)
- The steward (work coordinator)
- The operator (pod lifecycle manager)
- SpireConfig CRD (cluster singleton configuration)
- WizardGuild CRDs (per-repo agent definitions)

Today those `WizardGuild` definitions are still explicit in chart values.
The operator does not yet derive them automatically from the tower's
`repos` table.

**Requirements:**

- Helm 3.x: `brew install helm`
- A Kubernetes cluster (minikube, EKS, GKE, etc.)
- A DoltHub remote (create with `spire tower create`) — used as the **first-install seed** in cluster-as-truth deployments. The dolt-init container clones from it on first boot; the cluster Dolt is the write authority thereafter and DoltHub is not a bidirectional mirror.
- A **GCS backup bucket** for disaster recovery — see [Backup bucket setup](#backup-bucket-setup) below. `backup.enabled=true` is the chart default; install will fail fast without a bucket.
- A **GCS log bucket** when running with `logStore.backend=gcs` — see [Log artifact bucket setup](#log-artifact-bucket-setup) below. Separate from the backup and bundle buckets because the lifecycle / storage class / access rules differ.
- A **GCP service-account JSON** with `roles/storage.objectAdmin` on the backup bucket (and on the bundle bucket if `bundleStore.backend=gcs`, and on the log bucket if `logStore.backend=gcs`), OR a Workload-Identity binding plus an external Secret pinned via `gcp.secretName`. See [GCP auth](#gcp-auth) below.

---

## Backup bucket setup

Cluster-as-truth deployments use GCS as the disaster-recovery substrate. Backup is **on by default** in the chart (`backup.enabled=true`); the install fails fast if the bucket or auth path is missing. This section is a required pre-install step, not an optional appendix.

### 1. Create the bucket

```bash
PROJECT_ID=<your-gcp-project>
BACKUP_BUCKET=$PROJECT_ID-spire-backups

gcloud storage buckets create gs://$BACKUP_BUCKET \
  --project=$PROJECT_ID \
  --location=us-central1 \
  --default-storage-class=NEARLINE     # Nearline/Coldline suit dolt backups
```

Use a separate bucket from `bundleStore.gcs.bucket` — backup objects sit in Nearline/Coldline; bundles are short-lived and need Standard.

### 2. Configure retention

The chart records `backup.retentionDays` for documentation only. Apply a GCS lifecycle rule out-of-band so old backups age out automatically:

```bash
cat > /tmp/lifecycle.json <<EOF
{ "lifecycle": { "rule": [
  { "action": { "type": "Delete" },
    "condition": { "age": 30 } }
] } }
EOF
gsutil lifecycle set /tmp/lifecycle.json gs://$BACKUP_BUCKET
```

### 3. Wire the bucket into your values overlay

```yaml
backup:
  enabled: true                    # chart default; opt out only for disposable/dev
  gcs:
    bucket: $PROJECT_ID-spire-backups
    prefix: prod                   # optional per-tower namespace inside the bucket
```

### Opt-out (disposable/dev clusters only)

Disposable/dev clusters that explicitly do not need DR can opt out:

```yaml
backup:
  enabled: false
```

This is the only documented reason to disable backup. Production installs MUST keep it on.

### Production readiness

Backup default-on means the CronJob runs once a day and archives to GCS. **It does not prove the restore path is healthy.** Production cutover is gated on the restore drill (bead `spi-i7k1ag.3`); do not announce DR readiness based on backup-enabled alone.

---

## Log artifact bucket setup

Cluster-as-truth deployments persist agent logs and provider transcripts in a dedicated GCS bucket so the gateway can serve them through `/api/v1/beads/{id}/logs` after the emitting pod is gone (design `spi-7wzwk2`). Local-native installs skip this and keep using the wizard data directory; cluster installs that want gateway-served logs flip `logStore.backend=gcs`.

The bucket is **separate from `bundleStore.gcs.bucket` and `backup.gcs.bucket`** and the three are not interchangeable:

- `bundleStore` — Standard storage class; bundles deleted within minutes of merge. Reusing the log bucket would trigger early-deletion fees on Nearline/Coldline.
- `backup` — Nearline/Coldline; daily Dolt DR snapshots with a lifecycle rule deleting after `backup.retentionDays`. Reusing for logs would either delete transcripts too early or block backup compaction.
- `logStore` — Standard or Nearline depending on retention; logs have provider-redaction and access-control rules that don't apply to bundles or DR snapshots.

The chart's `spire.validateLogStore` helper fails the render fast when `logStore.backend=gcs` is set without `logStore.gcs.bucket` or a GCP auth path.

### 1. Create the bucket

```bash
PROJECT_ID=<your-gcp-project>
LOG_BUCKET=$PROJECT_ID-spire-logs

gcloud storage buckets create gs://$LOG_BUCKET \
  --project=$PROJECT_ID \
  --location=us-central1 \
  --default-storage-class=STANDARD     # Standard for short retention; Nearline for 30+ days
```

Do NOT enable object versioning — log artifacts are write-once and versioning would inflate cost without serving any forensic-replay use case.

### 2. Configure IAM

The same Workload Identity GSA already bound to `spire-operator` (see [GCP auth](#gcp-auth) below) needs `roles/storage.objectAdmin` on the **log** bucket:

```bash
gcloud storage buckets add-iam-policy-binding gs://$LOG_BUCKET \
  --member "serviceAccount:<gsa>@$PROJECT_ID.iam.gserviceaccount.com" \
  --role roles/storage.objectAdmin
```

The gateway needs read access to serve transcripts; the operator-stamped exporter (once `spi-k1cnof` ships) needs write access to upload artifacts. **Do not create a second GSA for log access** — the chart's `gcp.*` block is the single auth path; spi-hzeyz9 deliberately does not introduce a per-store credential field.

### 3. Configure retention

The chart records `logStore.retentionDays` for documentation only. Apply a GCS lifecycle rule out-of-band so old artifacts age out automatically:

```bash
cat > /tmp/log-lifecycle.json <<EOF
{ "lifecycle": { "rule": [
  { "action": { "type": "Delete" },
    "condition": { "age": 90 } }
] } }
EOF
gsutil lifecycle set /tmp/log-lifecycle.json gs://$LOG_BUCKET
```

90 days is the chart default — pick what matches your forensic-replay window. Coldline retrieval is not free, so default to Standard or Nearline unless you intend to never read transcripts back through the gateway.

### 4. Wire the bucket into your values overlay

```yaml
logStore:
  backend: gcs
  gcs:
    bucket: $PROJECT_ID-spire-logs
    prefix: prod                       # optional per-tower namespace
  retentionDays: 90                    # doc-only; matches the lifecycle rule above
```

The same `gcp.*` block configured for backup/bundleStore is reused — no additional credential field is required.

### Opt-out (local-native installs)

Local-native and dev clusters that don't need cluster log export keep the chart default:

```yaml
logStore:
  backend: local                       # chart default
```

Local mode keeps writing under the wizard data directory (`~/.local/share/spire/wizards`); `spire logs` and `spire logs pretty` continue to work unchanged.

---

## GCP auth

The dolt backup CronJob and the BundleStore GCS backend share one
service-account credential mounted via the chart's `gcp.*` block. Two
auth paths are supported and the chart's `spire.validateBackup` helper
fails the render at install time if neither is configured while
`backup.enabled=true`:

1. **Inline JSON** (`gcp.serviceAccountJson`) — pass with `--set-file
   gcp.serviceAccountJson=<path-to-sa.json>`. Helm base64-encodes the
   file into a chart-rendered Secret named `<release>-gcp-sa`. Use this
   for minikube and any environment where you control the SA key file.

2. **External Secret** (`gcp.secretName`) — set
   `--set gcp.secretName=<existing-secret-name>` and provision the
   Secret out-of-band via sealed-secrets, external-secrets-operator, or
   a Workload-Identity placeholder Secret. Use this for GKE in
   production where SA keys are managed by an external secret store and
   the runtime identity comes through Workload Identity.

The dolt pod mounts the resolved Secret at `gcp.mountPath` and exports
`GOOGLE_APPLICATION_CREDENTIALS` so `dolt backup sync` can authenticate
to GCS at the configured cron cadence.

---

## Quick start (minikube)

For local development and testing. Two paths:

- **Disposable cluster (no DR needed):** opt out of backup with `--set
  backup.enabled=false`. This is the only documented reason to disable
  backup; production installs MUST keep it on.
- **Cluster-as-truth dogfood:** keep the chart default
  `backup.enabled=true` and supply a real GCS bucket + SA JSON.

```bash
# Start minikube if not running
minikube start --memory=4096 --cpus=2

# Install Spire with local images (disposable-cluster opt-out)
helm install spire ./helm/spire \
  --namespace spire \
  --create-namespace \
  --set images.steward.repository=spire-steward \
  --set images.steward.tag=dev \
  --set images.steward.pullPolicy=IfNotPresent \
  --set images.agent.repository=spire-agent \
  --set images.agent.tag=dev \
  --set images.agent.pullPolicy=IfNotPresent \
  --set dolthub.remoteUrl=your-org/your-tower \
  --set dolthub.credsKeyId=<keyid> \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<keyid>.jwk \
  --set dolthub.userName=your-dolthub-username \
  --set dolthub.user=dolt_remote \
  --set dolthub.password=<strong-remotesapi-password> \
  --set anthropic.apiKey=sk-ant-... \
  --set backup.enabled=false
```

For a backup-enabled minikube install, follow [Backup bucket
setup](#backup-bucket-setup) first, then add `--set
backup.gcs.bucket=<bucket>` and `--set-file
gcp.serviceAccountJson=<path>` to the command above (and drop the
`--set backup.enabled=false` line).

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
  # DoltHub tower — first-install seed only in cluster-as-truth installs.
  # The dolt-init container clones from it on first boot; thereafter the
  # cluster Dolt is the write authority and DoltHub is not a bidirectional
  # mirror.
  remoteUrl: your-org/your-tower
  credsKeyId: <base32-key-id>       # from `dolt creds ls`
  credsKeyValue: ""                 # pass via --set-file, never inline in git
  userName: your-dolthub-username   # MUST match the account that owns the JWK
  userEmail: you@example.com

  # Cluster remotesapi SQL login. Auto-provisioned by the post-install Job
  # (see `dolt.provisionRemoteUser.enabled`). External `dolt remote`
  # clients authenticate with these against the cluster's :50051 endpoint.
  user: dolt_remote
  password: <strong-remotesapi-password>

anthropic:
  # One of apiKey or oauthToken is required.
  apiKey: sk-ant-api03-...
  oauthToken: ""                    # sk-ant-oat01-... (from `claude setup-token` on a Max plan)

github:
  token: ghp_...

beads:
  prefix: spi
  database: spi

# Cluster-as-truth disaster-recovery (chart default backup.enabled=true).
# helm install will fail fast without bucket+auth — see Backup bucket setup
# above for the bucket creation + GCP auth steps.
backup:
  enabled: true
  gcs:
    bucket: your-org-spire-backups
    prefix: prod

# Define guilds — one WizardGuild CR per repo. `name` is the guild shortname
# and prefixes every wizard pod spawned under it (`<name>-wizard-<bead>`).
guilds:
  - name: main
    mode: managed
    repo: https://github.com/your-org/my-repo.git
    repoBranch: main
    prefixes: ["myp-"]
    maxConcurrent: 2
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
  --values my-values.yaml \
  --set-file gcp.serviceAccountJson=$HOME/.gcp/spire-sa.json
```

`gcp.serviceAccountJson` is required when `backup.enabled=true` (the
chart default). Pass it with `--set-file` so the SA JSON never lands in
your values file. To use Workload Identity / external-secrets instead,
swap `--set-file gcp.serviceAccountJson=...` for `--set
gcp.secretName=<existing-secret>`. See [GCP auth](#gcp-auth) above.

Watch the rollout:

```bash
kubectl rollout status deployment/spire-dolt -n spire
kubectl rollout status deployment/spire-steward -n spire
kubectl rollout status deployment/spire-operator -n spire
```

### Step 4: Verify the pipeline

Attach the laptop/Desktop to the cluster gateway, then file a bead.
In cluster-attach (gateway) mode, all bead mutations route through the
gateway over HTTP. Do not run direct `dolt push`/`pull` against the
cluster store — these will be rejected. Use `spire tower attach-cluster`
to attach, then operate normally; the client transparently tunnels
writes to the gateway.

```bash
# On your laptop, after `spire tower attach-cluster --tower <name> --url <gateway-url> --token <bearer>`
spire file "Test cluster deployment" -t task -p 2
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

## Additional SQL users

The cluster's dolt server exposes a remotesapi port (default `50051`)
so laptops and CI can `dolt clone/push/pull` against it directly
instead of going through DoltHub. The chart auto-provisions one
`dolt_remote` user (`dolthub.user` / `dolthub.password`) for that
remotesapi flow. When a real team wants per-user credentials — a
scoped read-only role for CI, per-dev logins with independent
rotation, a read-only auditor account — declare them via
`dolt.additionalUsers`:

```yaml
# my-values.yaml
dolt:
  additionalUsers:
    # Secret-ref form (preferred) — password lives in a Secret you
    # manage out-of-band (sealed-secrets, ESO, vault-injector, etc.).
    - name: alice
      passwordSecret:
        name: spire-user-alice
        key: password            # default "password"
      grants:
        - "ALL ON spi.*"
    # Inline form — password comes from values. The chart materializes
    # it into a chart-managed Secret (`spire-dolt-additional-users`,
    # key `addl-pw-<name>`) so the rendered Job spec never carries
    # plaintext. Use for demos/dev; prefer passwordSecret in prod.
    - name: readonly
      password: "plaintext-discouraged"
      grants:
        - "SELECT ON spi.*"
```

With Secret-ref entries, pre-create each Secret before `helm install`
or the Job Pod fails with `CreateContainerConfigError`:

```bash
kubectl -n spire create secret generic spire-user-alice \
  --from-literal=password=$(openssl rand -base64 24 | tr -d /+= | head -c 24)
```

On install/upgrade the chart renders `spire-dolt-additional-users`, a
post-install/post-upgrade hook Job that waits for dolt to be ready and
runs idempotent `CREATE USER IF NOT EXISTS … ALTER USER … IDENTIFIED
BY … GRANT …` for every entry. Rotation is re-running `helm upgrade`
with the new password (inline) or `kubectl patch secret` + `helm
upgrade` (Secret-ref); the Job's `ALTER USER` re-applies the password
on the next run.

Constraints to be aware of:

- **`name`** is validated at Helm render time against
  `^[a-zA-Z0-9_]{1,32}$`. Anything else (`alice;DROP`, `bob spaces`,
  64-char strings) fails the render before a manifest reaches the
  cluster.
- **Single quotes are rejected** in `host`, `grants`, and passwords.
  `host`/`grants` fail the Helm render; passwords trip a runtime check
  in the Job and exit non-zero with a clear message.
- **Exactly one password source per entry.** Either `passwordSecret`
  or `password` must be set — neither or both fails the render.
- **Grant revocation on removal is not automatic.** Dropping an entry
  from `additionalUsers` on `helm upgrade` leaves the SQL user in
  place. Drop it by hand:
  `kubectl exec statefulset/spire-dolt -- dolt sql -q "DROP USER 'old_user'@'%'"`.

Connect with a provisioned user from a laptop (port-forward first):

```bash
kubectl -n spire port-forward svc/spire-dolt 50051:50051
dolt clone --user=alice http://localhost:50051/spi /tmp/alice-clone
```

See `docs/HELM.md` for the full schema reference.

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
| `dolthub.remoteUrl` | `""` | DoltHub tower path (`org/tower`). First-install seed only in cluster-as-truth installs; the dolt-init container clones from it on first boot. Not a bidirectional mirror. |
| `dolthub.credsKeyId` | `""` | Dolt key ID — basename of the JWK file; shown by `dolt creds ls`. Required. |
| `dolthub.credsKeyValue` | `""` | Raw JWK JSON contents. Pass via `--set-file`; never inline. Required. |
| `dolthub.userName` | `""` | Dolt CLI `user.name`. MUST match the DoltHub account that owns the JWK. |
| `dolthub.userEmail` | `""` | Dolt CLI `user.email`. |
| `dolthub.user` | `""` | Cluster remotesapi SQL username (provisioned by post-install Job). |
| `dolthub.password` | `""` | Cluster remotesapi SQL password. |
| `anthropic.apiKey` | `""` | Anthropic classic API key. Use this or `oauthToken`. |
| `anthropic.oauthToken` | `""` | Anthropic OAuth token (Max subscription, from `claude setup-token`). |
| `github.token` | `""` | GitHub PAT (required for repo clone/push operations). |
| `beads.prefix` | `spi` | Hub bead prefix. |
| `beads.database` | `""` | Local dolt database name. Defaults to `beads.prefix`. |
| `steward.interval` | `2m` | Steward sync interval |
| `spireConfig.polling.staleThreshold` | `4h` | Mark workload stale after this |
| `spireConfig.polling.reassignThreshold` | `6h` | Reassign stale workloads after this |
| `agents` | `[]` | List of WizardGuild definitions |
| `syncer.enabled` | `false` | DEPRECATED. Bidirectional DoltHub sync is disabled in cluster-as-truth installs (the runtime Sync is a no-op even when this is true). Set to true only for a one-shot first-install seed. Use `backup.*` for archive/DR. |
| `backup.enabled` | `true` | Render the GCS backup CronJob (canonical archive/DR substrate). Helm fails fast if true without bucket+auth. |

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

### WizardGuild

Represents an entity that can do work.

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: WizardGuild
spec:
  mode: managed | external     # "managed" = operator creates pods; "external" = your process
  image: string                # container image (managed only)
  repo: string                 # git repo URL to clone (managed only)
  repoBranch: string           # branch to clone (default: main)
  prefixes: [string]           # bead prefixes this agent can handle
  maxConcurrent: int           # max simultaneous workloads
  token: string                # token name from SpireConfig (default: "default")
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
| `assignedAgent` | Name of the WizardGuild handling this workload |
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

# Logs for a specific wizard (main container is named `agent`)
kubectl logs -n spire my-agent-wizard-spi-a3f8 -c agent

# Init container logs (tower-attach bootstrap)
kubectl logs -n spire my-agent-wizard-spi-a3f8 -c tower-attach
```

The wizard pod is single-container (`agent`) with two init containers
(`tower-attach`, then `cache-bootstrap`). `cache-bootstrap` materializes
the writable workspace from the guild-owned read-only cache PVC (see
[cluster-repo-cache.md](cluster-repo-cache.md)); it is the canonical
cluster-native substrate path — the older `repo-bootstrap` origin-clone
init container is retired (spi-gvrfv). There is no familiar sidecar or
`/comms` volume in wizard pods — see
[k8s-operator-reference.md](k8s-operator-reference.md#deprecated-agent-entrypointsh--model-a).

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

## Archive and disaster recovery

Cluster-as-truth deployments treat the cluster Dolt database as the
single write authority. GCS backup is the canonical disaster-recovery
path for cluster-as-truth deployments. Backup is default-on and
fail-fast; restore-from-GCS is the documented recovery procedure (see
[`docs/runbooks/gcs-restore.md`](runbooks/gcs-restore.md)).

Bidirectional cluster ↔ DoltHub sync is **disabled by default** in the
chart (`syncer.enabled=false`) because running it concurrently with
cluster writes recreates the non-fast-forward / merge-conflict
divergence class fixed in epic `spi-i7k1ag` (witnessed 2026-04-26).
The runtime Sync method itself is a no-op now, so even an opt-in
`syncer.enabled=true` install will not perform DOLT_PULL/DOLT_PUSH
against DoltHub on its loop — the template is preserved only so a
deliberate one-shot first-install seed scenario can still be expressed
in values. DoltHub receives one-way archive pushes only; it is not
read back as truth.

The canonical archive/DR substrate is GCS backup, configured via the
chart's `backup.*` block:

```yaml
# in my-values.yaml — chart default backup.enabled=true; helm fails fast
# without bucket+auth (see Backup bucket setup above).
backup:
  enabled: true
  schedule: "0 3 * * *"   # daily 03:00 UTC
  remoteName: gcs-backup
  gcs:
    bucket: <your-backup-bucket>
    prefix: prod
```

The dolt-init container registers the `dolt backup` remote on first
boot and the `spire-dolt-backup` CronJob runs `dolt backup sync` on the
configured cadence. Restore is exercised by a separate runbook —
[`docs/runbooks/gcs-restore.md`](runbooks/gcs-restore.md) — because a
backup that has never been restored is unproven, not unwritten. **Do
NOT re-enable bidirectional DoltHub sync after a restore;** the
post-restore guard checks in that runbook list the exact verification
an operator must perform before promoting the restored cluster.

---

## Troubleshooting

### Backup CronJob not landing objects in GCS

```bash
kubectl get cronjob spire-dolt-backup -n spire
kubectl logs -n spire job/<latest-backup-job-name>
```

Check credentials:

```bash
kubectl get secret -n spire | grep gcp-sa    # chart-rendered <release>-gcp-sa Secret
kubectl describe pod -n spire -l app.kubernetes.io/name=spire-dolt | grep -A2 GOOGLE_APPLICATION_CREDENTIALS
```

If the bucket is missing or the SA lacks `roles/storage.objectAdmin`,
`dolt backup sync` exits non-zero and the CronJob's last `Job` is in
`Failed` state. Re-check [Backup bucket setup](#backup-bucket-setup)
and [GCP auth](#gcp-auth).

### Workloads stuck in Pending

No available agent matches the workload. Check:

1. Agent `spec.prefixes` includes the bead's prefix
2. Agent `status.phase` is `Idle` (not `Stale` or `Offline`)
3. Agent `status.currentWork` length is below `spec.maxConcurrent`

```bash
kubectl get wizardguilds -n spire -o yaml | grep -A5 "status:"
```

### Agent pods not being created

The agent must have `spec.mode: managed`. Check:

```bash
kubectl get wizardguilds -n spire -o jsonpath='{.items[*].spec.mode}'
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
