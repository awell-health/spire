# Spire Cluster Deployment Runbook

Canonical install path for Spire on Kubernetes. Two tracks share one chart:

- **Track A — Minikube** (local dev / runbook validation)
- **Track B — GKE** (production)

Architecture: see [docs/k8s-architecture.md](../docs/k8s-architecture.md). This doc is the *operator's* runbook — exact commands, in order.

## Direction (read first)

- The cluster is the source of truth for the bead graph. Laptops/desktops attach via the gateway and **never push to DoltHub directly**.
- DoltHub seeds the cluster on first install (`dolt clone https://doltremoteapi.dolthub.com/<org>/<tower>`) and may receive periodic one-way archival pushes. **It is not a writable mirror.**
- GCS is the canonical disaster-recovery substrate via the chart's `backup.*` block.
- Multi-archmage support exists at the gateway. Each desktop attaches with its own bearer token.

There are open v1.0 gaps that affect the desktop UX — see [§9 Known issues](#9-known-issues--workarounds). Land those before announcing the deployment GA.

---

## 1. Prerequisites

Universal:

- **kubectl** (matching the cluster's k8s version)
- **helm** ≥ 3.10
- **docker** (for image builds; minikube only)
- **A DoltHub credential** with read access to your tower's repo (initial seed; this is the only time DoltHub HTTPS auth is required from the cluster)
- **An Anthropic OAuth token** (Max subscription) or **API key** for managed agents
- **A GCS bundle bucket** for apprentice→wizard git-bundle artifacts (`bundleStore.gcs.bucket`)
- **A GCS dolt-backup bucket** for the cluster-as-truth disaster-recovery substrate (`backup.gcs.bucket`). The chart defaults `backup.enabled=true` and Helm will fail the render if the bucket is missing; opting out is **only** appropriate for disposable/dev clusters that explicitly do not need DR. See [§1.1 Backup bucket](#11-backup-bucket-cluster-as-truth-dr) below.
- **A GCP service-account JSON** with `roles/storage.objectAdmin` on BOTH buckets above. Workload-Identity GKE installs may instead pin `gcp.secretName` to an externally-managed Secret — both auth paths are accepted by the chart's `spire.validateBackup` helper.
- **A GitHub fine-grained PAT** with `contents:write` on the workspace repo (wizard pods use this to push merge commits)

Track-specific:

- **Minikube**: minikube CLI, ~12 GB free disk, ~10 GB RAM
- **GKE**: a project with Workload Identity enabled, Artifact Registry repo for images, an Ingress-capable LB or `cert-manager`

### 1.1 Backup bucket (cluster-as-truth DR)

In the cluster-as-truth topology the cluster Dolt database is the only writable copy of the bead graph — DoltHub is not a bidirectional mirror. GCS is the disaster-recovery substrate and the chart defaults `backup.enabled=true`. The dolt-init script registers a `dolt backup` remote on first boot and a CronJob runs `dolt backup sync` on the configured cadence (default daily 03:00 UTC).

Before `helm install`:

```bash
PROJECT_ID=<your-gcp-project>
BACKUP_BUCKET=$PROJECT_ID-spire-backups

# 1. Create the bucket (Nearline/Coldline storage class — backups are
#    cold data; do NOT reuse the bundle bucket which needs Standard).
gcloud storage buckets create gs://$BACKUP_BUCKET \
  --project=$PROJECT_ID \
  --location=us-central1 \
  --default-storage-class=NEARLINE

# 2. Apply a lifecycle rule so old backups age out — `backup.retentionDays`
#    in values.yaml is documentation-only; this is what enforces it.
cat > /tmp/lifecycle.json <<EOF
{ "lifecycle": { "rule": [
  { "action": { "type": "Delete" },
    "condition": { "age": 30 } }
] } }
EOF
gsutil lifecycle set /tmp/lifecycle.json gs://$BACKUP_BUCKET

# 3. Grant the Spire service account roles/storage.objectAdmin on the bucket
#    (or set up Workload Identity — see §4.2 for the GKE flow).
gcloud storage buckets add-iam-policy-binding gs://$BACKUP_BUCKET \
  --member="serviceAccount:<sa>@$PROJECT_ID.iam.gserviceaccount.com" \
  --role=roles/storage.objectAdmin
```

Then wire `backup.gcs.bucket` into your local values overlay (see §2). `helm install` fails fast at template time if the bucket or auth path is missing — the failure points back here.

**Backup-enabled is not the same as DR-ready.** Production cutover is gated on exercising the restore drill — see [§11 Disaster recovery](#11-disaster-recovery) and [`docs/runbooks/gcs-restore.md`](../docs/runbooks/gcs-restore.md) for the runnable procedure. Treat the chart's backup default as "archival is happening" not "we know we can recover."

To opt out (disposable/dev cluster only), set `--set backup.enabled=false`. This is the only documented reason to disable backup; production installs MUST keep it on.

---

## 2. Local secrets file

Create a gitignored values overlay holding your secrets. The repo's `.gitignore` matches `*.local.*`, so name the file `k8s/values.<env>.local.yaml`.

```yaml
# k8s/values.minikube.local.yaml — example for the minikube track
namespace: spire
createNamespace: false   # let `helm install --create-namespace` own it

# Tower identity — must match what the DoltHub repo declares
beads:
  prefix: spi
  database: spi

# DoltHub seed (clone-only auth; pull/push disabled in cluster-as-truth model)
dolthub:
  remoteUrl: awell/awell
  user: <dolt-remote-user>             # cluster-internal SQL user (provisioned by post-install Job)
  password: <dolt-remote-password>     # any non-empty string; rotate per release
  credsKeyId: <jwk-id>                 # basename of $HOME/.dolt/creds/<jwk-id>.jwk
  userName: <dolthub-account>          # MUST match the JWK's owning DoltHub account
  userEmail: <dolthub-account-email>

# Anthropic — provide one of: oauthToken (Max) or apiKey
anthropic:
  oauthToken: sk-ant-oat01-...
  # apiKey: sk-ant-api03-...

# (optional) OpenAI / codex
openai:
  apiKey: ""

# GitHub PAT — wizard pods push merge commits via url.insteadOf rewrite
github:
  token: ""   # supply via --set github.token=... at install time, not in this file

# Dev images (minikube only — GKE pulls from your registry, see §4)
images:
  steward:
    repository: spire-steward
    tag: dev
    pullPolicy: Never
  agent:
    repository: spire-agent
    tag: dev
    pullPolicy: Never

# Single guild — adjust repo, prefixes, concurrency for your workload
guilds:
  - name: main
    mode: managed
    repo: https://github.com/<org>/<workspace-repo>.git
    repoBranch: main
    prefixes: ["spi-"]
    maxConcurrent: 2

steward:
  maxConcurrent: 2
  dispatchedTimeout: "5m"

# BundleStore — apprentice + wizard run in different pods, must share via GCS
bundleStore:
  backend: gcs
  gcs:
    bucket: <your-bundle-bucket>
    prefix: minikube                    # per-env namespace in the bucket

# Cluster-as-truth: syncer disabled (chart default), backup enabled.
# Bidirectional DoltHub sync is no longer supported in this topology;
# the runtime cluster Sync is itself a no-op even when enabled=true.
# Set true ONLY for a deliberate one-shot first-install seed. GCS
# backup (`backup.*` below) is the archive/DR path.
syncer:
  enabled: false

backup:
  enabled: true
  schedule: "0 3 * * *"
  remoteName: gcs-backup
  gcs:
    bucket: <your-backup-bucket>
    prefix: minikube
  retentionDays: 30        # documentation-only; enforce via GCS lifecycle rule

# Spire Desktop gateway — bearer-token auth; minikube uses NodePort + port-forward
gateway:
  enabled: true
  apiPort: 3030
  webhookPort: 8080
  apiToken: <generated-bearer>          # `openssl rand -hex 32`
  service:
    type: NodePort                      # GKE overlay flips this to ClusterIP behind Ingress
    nodePort: 30030
```

Generate the API token with `openssl rand -hex 32`. Treat it like a password.

---

## 3. Track A — Minikube install

### 3.1 Start the cluster

```bash
minikube delete -p spire    # only if a stale profile exists
minikube start -p spire
kubectl config use-context spire
```

### 3.2 Build images into minikube's docker daemon

`eval $(minikube docker-env)` is critical here — `minikube image load` has been observed to silently leave stale images in place when the tag points at a pre-existing image id.

```bash
cd /path/to/spire
eval $(minikube -p spire docker-env)
docker build -f Dockerfile.steward -t spire-steward:dev .
docker build -f Dockerfile.agent   -t spire-agent:dev   .

docker images | grep -E 'spire-(steward|agent):dev'
# Expect 2 lines, CREATED within the last few minutes
```

### 3.3 Apply CRDs

```bash
kubectl apply -f helm/spire/crds/
```

(Helm install also applies CRDs once, but explicit is safer for upgrades — `helm upgrade` does not update CRDs.)

### 3.4 Helm install

```bash
helm upgrade --install spire helm/spire -n spire --create-namespace \
  -f k8s/values.minikube.local.yaml \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<jwk-id>.jwk \
  --set-file gcp.serviceAccountJson=$HOME/Downloads/spire-gcs-sa.json \
  --set github.token=$GITHUB_PAT
```

The two `--set-file` flags inject file contents at install time so no out-of-band `kubectl create secret` step is needed.

### 3.5 Wait for rollouts

```bash
kubectl rollout status statefulset/spire-dolt -n spire --timeout=300s
kubectl rollout status deploy/spire-steward    -n spire --timeout=300s
kubectl rollout status deploy/spire-operator   -n spire --timeout=300s
kubectl rollout status deploy/spire-gateway    -n spire --timeout=300s
```

Dolt usually takes the longest because the dolt-init container pulls the entire DoltHub repo (~1 GB clone for `awell/awell`). Tail it if you want progress:

```bash
kubectl logs spire-dolt-0 -n spire -c dolt-init -f
```

---

## 4. Track B — GKE install

Differences from §3:

| Topic | Minikube | GKE |
|---|---|---|
| Images | Built into minikube docker (`pullPolicy: Never`) | Mirrored to Artifact Registry; chart resolves tag from `Chart.AppVersion` |
| Service exposure | `NodePort` + `kubectl port-forward` | `ClusterIP` + GKE Ingress + Google-managed cert |
| Storage class | default (`standard`) | `premium-rwo` for dolt; `standard-rwo` for cache (RWX-capable CSI) |
| GCP credential | Mounted JSON via `--set-file` | Workload Identity (KSA bound to GSA); `--set-file` placeholder still required until chart loosens the gate |
| Backup bucket | Optional (dev) | Required (production retention policy) |
| values overlay | `k8s/values.minikube.local.yaml` | `helm/spire/values.gke.yaml` + per-env `*.gke.local.yaml` |

### 4.1 Image push

```bash
PROJECT_ID=<your-gcp-project>
REGION=us-central1
REPO=spire

gcloud artifacts repositories create $REPO \
  --repository-format=docker --location=$REGION \
  --project=$PROJECT_ID

gcloud auth configure-docker ${REGION}-docker.pkg.dev

# From the spire repo root:
docker build -f Dockerfile.steward -t ${REGION}-docker.pkg.dev/$PROJECT_ID/$REPO/spire-steward:$TAG .
docker build -f Dockerfile.agent   -t ${REGION}-docker.pkg.dev/$PROJECT_ID/$REPO/spire-agent:$TAG   .
docker push ${REGION}-docker.pkg.dev/$PROJECT_ID/$REPO/spire-steward:$TAG
docker push ${REGION}-docker.pkg.dev/$PROJECT_ID/$REPO/spire-agent:$TAG
```

`$TAG` should match `Chart.AppVersion` if you want chart-default tag resolution to work.

### 4.2 Workload Identity

```bash
PROJECT_ID=<your-gcp-project>
GSA=spire-steward@$PROJECT_ID.iam.gserviceaccount.com

gcloud iam service-accounts create spire-steward --project $PROJECT_ID
gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member "serviceAccount:$GSA" \
  --role roles/storage.objectAdmin

gcloud iam service-accounts add-iam-policy-binding $GSA \
  --role roles/iam.workloadIdentityUser \
  --member "serviceAccount:$PROJECT_ID.svc.id.goog[spire/spire-steward]"

# After helm install creates the KSA:
kubectl annotate serviceaccount -n spire spire-steward \
  iam.gke.io/gcp-service-account=$GSA
```

### 4.3 Per-env values overlay

```yaml
# k8s/values.gke.local.yaml
namespace: spire
createNamespace: false

beads:
  prefix: spi
  database: spi

dolthub:
  remoteUrl: <org>/<tower>
  user: <dolt-remote-user>
  password: <dolt-remote-password>
  credsKeyId: <jwk-id>
  userName: <dolthub-account>
  userEmail: <dolthub-account-email>

guilds:
  - name: main
    mode: managed
    repo: https://github.com/<org>/<workspace-repo>.git
    repoBranch: main
    prefixes: ["spi-"]
    maxConcurrent: 4
    cache:
      size: 20Gi
      storageClassName: standard-rwo

bundleStore:
  backend: gcs
  gcs:
    bucket: $PROJECT_ID-spire-bundles
    prefix: prod

backup:
  enabled: true
  gcs:
    bucket: $PROJECT_ID-spire-backups
    prefix: prod

syncer:
  enabled: false   # cluster-as-truth — see §0 Direction

gateway:
  apiToken: <generated-bearer>
  ingress:
    enabled: true
    className: gce
    host: spire.<your-domain>
    managedCert:
      enabled: true
    backendConfig:
      enabled: true
      http2: true
```

### 4.4 Helm install

```bash
helm upgrade --install spire helm/spire -n spire --create-namespace \
  -f helm/spire/values.gke.yaml \
  -f k8s/values.gke.local.yaml \
  --set images.steward.repository=${REGION}-docker.pkg.dev/$PROJECT_ID/$REPO/spire-steward \
  --set images.agent.repository=${REGION}-docker.pkg.dev/$PROJECT_ID/$REPO/spire-agent \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<jwk-id>.jwk \
  --set-file gcp.serviceAccountJson=/dev/null \
  --set anthropic.oauthToken="$ANTHROPIC_OAUTH" \
  --set github.token="$GITHUB_PAT"
```

(`/dev/null` works as a placeholder for `gcp.serviceAccountJson` until the chart's `required` gate is loosened — Workload Identity does the actual auth at runtime.)

### 4.5 DNS + cert

After Ingress reconciles:

```bash
kubectl get ingress -n spire
# Note the ADDRESS; create an A record at <your-domain> pointing at it
# ManagedCertificate moves to Active in 5–60 min after DNS propagates
kubectl get managedcertificate -n spire
```

---

## 5. Verify the bootstrap (both tracks)

All checks should pass on a fresh install.

### 5.1 Tower config

```bash
kubectl exec -n spire deploy/spire-steward -c steward -- \
  cat /data/spire-config/towers/<tower-name>.json
```

Expect (formatted): `name`, `project_id` (UUID from DoltHub seed), `hub_prefix`, `dolthub_remote`, `database`, `bundle_store={gcs, <bucket>, <prefix>}`, `deployment_mode: cluster-native`.

### 5.2 DoltHub clone landed

```bash
kubectl exec -n spire spire-dolt-0 -c dolt -- \
  dolt --host 127.0.0.1 --port 3306 --user root --no-tls -p "" sql -q \
  "SHOW DATABASES; USE <database>; SELECT COUNT(*) AS bead_count FROM issues;"
```

Expect a non-zero `bead_count` matching what your DoltHub tower has.

### 5.3 Repos table

```bash
kubectl exec -n spire spire-dolt-0 -c dolt -- \
  dolt --host 127.0.0.1 --port 3306 --user root --no-tls -p "" sql -q \
  "USE <database>; SELECT prefix, repo_url, branch FROM repos;"
```

Expect one row per registered repo.

### 5.4 Cache PVC + refresh job

```bash
kubectl get pvc  -n spire -l spire.awell.io/cache-role=pvc
kubectl get jobs -n spire -l spire.awell.io/cache-role=refresh-job
```

Expect at least one Bound PVC named `<guild>-repo-cache` and at least one Job in Complete or Running state.

### 5.5 Pods all Ready

```bash
kubectl get pods -n spire -o wide
```

Expect (Running, Ready):

- `spire-dolt-0` (1/1)
- `spire-steward-*` (2/2 — steward + sidecar)
- `spire-operator-*` (1/1)
- `spire-gateway-*` (1/1 minikube; 2/2 if `gateway.replicas=2`)
- `spire-syncer-*` (1/1, only if `syncer.enabled=true` — left disabled by default in cluster-as-truth installs; the runtime cluster Sync is a no-op even when this pod runs)

### 5.6 Gateway health

```bash
# Minikube — open a port-forward in a long-running terminal
kubectl port-forward svc/spire-gateway -n spire 3030:3030 8080:8080 &

# Both tracks
curl -s -o /dev/null -w "healthz: HTTP %{http_code}\n" http://127.0.0.1:3030/healthz
curl -s -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:3030/api/v1/tower
```

Expect `healthz: HTTP 200` and a JSON tower payload with `deploy_mode: cluster-native`.

For GKE, replace `127.0.0.1:3030` with `https://<ingress-host>` once DNS + cert are live.

---

## 6. Cluster-attach from the desktop

This is the per-archmage handshake. Each desktop runs it once.

### 6.1 Get the gateway URL + token

- **Minikube**: gateway URL = `http://127.0.0.1:3030` while the port-forward is open.
- **GKE**: gateway URL = `https://<ingress-host>`.
- Token: the `gateway.apiToken` value from your local values overlay.

### 6.2 Attach

> **Heads up — until spi-zz2ve9 lands**, attach-cluster will silently create a parallel tower if your laptop already has a tower with the same prefix. **Remove the existing tower first**, then attach. See [§9](#9-known-issues--workarounds).

```bash
spire tower remove <existing-tower-name>     # ONLY if a same-prefix local tower exists

spire tower attach-cluster \
  --tower <tower-name> \
  --url <gateway-url> \
  --token <bearer>
```

Expected output: `Tower attached via gateway: <name>`, prefix, archmage, URL, local alias.

### 6.3 Register the desktop

```bash
spire register spire-desktop-<archmage-handle>
```

Until per-call archmage threading lands (spi-1h1ucq), every mutation from this desktop is attributed to the cluster tower's static archmage. Set that on the cluster side once via:

```bash
kubectl exec -n spire deploy/spire-steward -c steward -- \
  spire tower set --tower <tower-name> --archmage-name "<Name>" --archmage-email "<email>"
kubectl exec -n spire deploy/spire-gateway -c gateway -- \
  spire tower set --tower <tower-name> --archmage-name "<Name>" --archmage-email "<email>"
```

(Both pods needed: each holds its own tower config; gateway pods use emptyDir, lose archmage on restart until the value is persisted upstream.)

### 6.4 Verify the routing

The CWD-resolution bug (spi-n6fk2h) currently means `spire register` and `spire file` route through whatever tower CWD maps to, not the gateway. Until that's fixed:

```bash
cd /tmp                                 # avoid CWD-resolution to a same-prefix local tower
spire register spire-desktop-<handle>
# Confirm the bead landed in cluster dolt (not in the laptop's local DB)
```

---

## 7. End-to-end smoke test

Validate the full pipeline by filing a trivial bead, watching the cluster pick it up, and seeing the wizard close it.

### 7.1 File a smoke bead

```bash
cd /tmp
spire file "Cluster smoke test $(date +%Y%m%d-%H%M)" -t task -p 4 --prefix <prefix>
# → spi-xxxxxx
```

Note the bead id.

### 7.2 Mark ready

```bash
spire ready spi-xxxxxx
```

The steward dispatches a wizard within ~1 min (interval = 10 s by default but emission rate is throttled).

### 7.3 Watch the wizard pod

```bash
kubectl get pods -n spire -l spire.bead=spi-xxxxxx -w
```

Init containers: `tower-attach` → `cache-bootstrap` → main `agent`. Each takes 10–60 s on first run.

### 7.4 Watch the bead close

```bash
end=$(( $(date +%s) + 1800 ))
while [ $(date +%s) -lt $end ]; do
  STATUS=$(spire focus spi-xxxxxx 2>/dev/null | grep -E '^Status:' | awk '{print $2}')
  echo "[$(date +%H:%M:%S)] $STATUS"
  [ "$STATUS" = "closed" ] && break
  sleep 30
done
```

### 7.5 Verify the commit on the workspace repo

For tasks that produce code changes:

```bash
git -C /path/to/workspace fetch origin
git -C /path/to/workspace log origin/main --oneline | grep spi-xxxxxx | head
```

A `feat(spi-xxxxxx): ...` commit on `origin/main` confirms the wizard's merge phase landed.

---

## 8. Tearing down

### 8.1 Minikube

```bash
helm uninstall spire -n spire
kubectl delete namespace spire           # cleans up PVCs + Secrets
minikube delete -p spire                  # full cluster removal
```

### 8.2 GKE

```bash
helm uninstall spire -n spire             # CRDs persist by helm convention
kubectl delete crd \
  spireconfigs.spire.awell.io \
  spireworkloads.spire.awell.io \
  wizardguilds.spire.awell.io
kubectl delete namespace spire
```

GCS buckets, Workload Identity bindings, and Artifact Registry images are retained intentionally — delete out-of-band when fully decommissioning.

### 8.3 Desktop side

```bash
spire tower remove <gateway-tower-name>   # also deletes the keychain entry
```

---

## 9. Known issues & workarounds

All filed under epic [spi-i7k1ag — DoltHub becomes archive-only](https://). Severity column is the impact on this runbook today.

| Bead | Pri | Impact | Workaround |
|---|---|---|---|
| **spi-zz2ve9** | P1 | `attach-cluster` silently duplicates same-prefix towers | Remove existing local tower before attach (§6.2) |
| **spi-n6fk2h** | P1 | Gateway-mode write routing loses to CWD-resolved tower | Run mutations from a non-tower dir (`cd /tmp`) until landed |
| **spi-1h1ucq** | P1 | `/api/v1/*` mutations don't carry per-call archmage identity | Set a default archmage on the cluster tower (§6.3) |
| **spi-6f6ky8** | P1 | Nothing prevents a laptop in gateway-mode from running `spire push` | Operational discipline — don't run `spire push` against a gateway-mode tower |
| **spi-hr3tcv** | P2 | Auto-push call sites in pkg/dolt, pkg/store, pkg/syncer not gated by mode | Same as above — discipline + spi-6f6ky8 |
| **spi-43q7hp** | P2 | `spire tower list` shows `kind=dolthub remote=local` for gateway-mode towers | Cosmetic only; identify gateway-mode towers by the `Mode=gateway` line in `~/.spire/towers/<name>.json` |

When a bead closes, update the corresponding row above and remove the workaround text.

---

## 10. Iterating

After code changes (minikube only):

```bash
# Steward / sidecar code
eval $(minikube -p spire docker-env)
docker build -f Dockerfile.steward -t spire-steward:dev .
kubectl rollout restart deploy/spire-steward -n spire

# Agent / wizard code
eval $(minikube -p spire docker-env)
docker build -f Dockerfile.agent -t spire-agent:dev .
# Wizard pods are ephemeral — fresh ones pull the new image automatically

# Operator code (uses steward image)
docker build -f Dockerfile.steward -t spire-steward:dev .
kubectl rollout restart deploy/spire-operator -n spire

# Chart values
helm upgrade spire helm/spire -n spire \
  -f k8s/values.minikube.local.yaml \
  --reuse-values --set <key>=<value>
```

Docker layer caching means only the Go compile runs on code changes (~15 s).

---

## 11. Disaster recovery

GCS is the canonical disaster-recovery substrate for cluster-as-truth
deployments. The chart defaults `backup.enabled=true` (see [§1.1](#11-backup-bucket-cluster-as-truth-dr))
and the daily `spire-dolt-backup` CronJob writes a full Dolt backup to
`gs://<backup.gcs.bucket>/<backup.gcs.prefix>` via `dolt backup sync`.

Restore is a separate runbook because the drill is what proves DR works
— a default-on backup that has never been restored is unproven, not
unwritten. The full procedure (test-namespace setup, blank PVC, GCS
clone, bead-graph integrity validation, gateway smoke tests, RPO/RTO
recording, post-restore guard checks) lives at:

- [`docs/runbooks/gcs-restore.md`](../docs/runbooks/gcs-restore.md) —
  copy-pasteable operator runbook, eight numbered sections.
- [`scripts/restore-gcs-drill.sh`](../scripts/restore-gcs-drill.sh) —
  bash helper wrapping the namespace + PVC + restore-Pod steps.
  Supports `--dry-run`.

> **Warning — do NOT re-enable bidirectional DoltHub sync after a
> restore.** Cluster-as-truth requires a single writer. Turning the
> bidirectional cluster syncer back on (`syncer.enabled=true` with the
> pre-cluster-as-truth `DOLT_PULL`/`DOLT_PUSH` loop) re-creates the
> divergence-and-merge-conflict class of bug observed on 2026-04-26
> and the whole epic [`spi-i7k1ag`] was filed to remove. The restore
> runbook section §7 lists the exact post-restore guard checks an
> operator must verify before promoting the restored cluster.

Run the drill at least quarterly and after any backup configuration
change. The drill produces no production impact when executed against
a test namespace.

---

## 12. References

- Chart values reference: [helm/spire/values.yaml](../helm/spire/values.yaml) — every parameter is documented inline as `## @param`
- GKE overlay: [helm/spire/values.gke.yaml](../helm/spire/values.gke.yaml)
- Architecture: [docs/k8s-architecture.md](../docs/k8s-architecture.md)
- Operator reference: [docs/k8s-operator-reference.md](../docs/k8s-operator-reference.md)
- Cluster-attach design: [docs/attached-mode.md](../docs/attached-mode.md), [docs/VISION-ATTACHED.md](../docs/VISION-ATTACHED.md)
- Vision (per deployment mode): [docs/VISION-CLUSTER.md](../docs/VISION-CLUSTER.md), [docs/VISION-LOCAL.md](../docs/VISION-LOCAL.md)
