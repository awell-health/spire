# Cluster Logs — Operator Runbook

Audience: cluster operators installing or maintaining a cluster-as-truth
Spire tower on GKE. This document covers the durable read path —
how agent logs are persisted, who serves them, and how to fix the
failures operators actually hit.

The architectural decision lives in design bead **spi-7wzwk2** ("Persistent
cloud-native log export for cluster mode"). Read it once for the *why*;
this runbook is the *how*.

Companion documents:

- [cluster-logs-smoke-test.md](cluster-logs-smoke-test.md) — manual
  end-to-end verification you should run after install and after any
  log-substrate upgrade.
- [cluster-install.md § 10b](cluster-install.md#10b-log-retention-redaction-and-visibility) —
  retention/redaction/visibility policy details.
- [pkg/logartifact/README.md](../pkg/logartifact/README.md) — substrate
  internals (Identity, Manifest, Visibility, redaction).

---

## 1. Architecture in one diagram

```
                      pod (wizard | apprentice | sage | cleric | arbiter)
   ┌──────────────────────────────────────────────────────────────────────┐
   │                                                                      │
   │   container: agent                container: spire-log-exporter      │
   │   ─────────────────               ──────────────────────────────     │
   │   writes JSONL files     ──┐                                         │
   │   under SPIRE_LOG_ROOT     │   tails files (passive)                 │
   │   (/var/spire/logs)        ├──▶ emits structured JSON to stdout ─┐   │
   │                            │                                     │   │
   │                            └──▶ uploads finalized artifacts ──┐  │   │
   │                                 to logStore (local|GCS)       │  │   │
   │                                                               │  │   │
   │   shared volume: emptyDir name=spire-logs mount=/var/spire/logs  │   │
   └──────────────────────────────────────────────────┬───────────┬──┘
                                                      │           │
                                                      ▼           ▼
                              ┌────────────────────────┐   ┌────────────┐
                              │ logStore.gcs.bucket    │   │ Cloud      │
                              │   <prefix>/<tower>/    │   │ Logging    │
                              │   <bead>/<attempt>/    │   │ (live tail │
                              │   <run>/<agent>/...    │   │  + search) │
                              │   .jsonl objects       │   └────────────┘
                              └────────────┬───────────┘
                                           │ ObjectURI + identity
                                           ▼
                              ┌────────────────────────┐
                              │ tower (Dolt)           │
                              │   agent_log_artifacts  │
                              │   manifest table       │
                              └────────────┬───────────┘
                                           │
                                           ▼
                              ┌────────────────────────┐
                              │ gateway                │
                              │   /api/v1/beads/{id}/  │
                              │     logs[/...]         │
                              └────────────┬───────────┘
                                           │
                          ┌────────────────┼─────────────────┐
                          ▼                ▼                 ▼
                  Spire Desktop     spire CLI            board (TUI)
                  (board, logs)    (logs pretty)
```

Two facts to internalise:

1. **GCS holds the bytes; the manifest holds the index.** The gateway
   resolves bead → manifest rows → object URIs and either streams the
   bytes or renders them through the per-provider `pkg/board/logstream`
   adapter. Clients never talk to GCS, Cloud Logging, or pod
   filesystems directly.

2. **The exporter is passive.** It tails files, emits structured stdout,
   uploads completed artifacts, writes manifest rows. It does not own
   lifecycle, dispatch work, or touch any other table. A misbehaving
   exporter does *not* mark agent work failed — the manifest row gets
   `status=failed` and the agent's verdict is unchanged.

---

## 2. The three buckets — DO NOT REUSE

A cluster-as-truth deployment provisions **three distinct GCS buckets**.
The most common operator error is pointing two of them at the same
bucket; the chart can't catch it because Helm can't introspect GCS
lifecycle.

| Helm key                    | Purpose                                 | Lifetime              | Recommended class | Lifecycle rule |
|-----------------------------|-----------------------------------------|-----------------------|-------------------|----------------|
| `bundleStore.gcs.bucket`    | Apprentice → wizard git-bundle handoff  | minutes               | **Standard**      | none / very short |
| `backup.gcs.bucket`         | Dolt disaster-recovery backup target    | days–months           | **Nearline/Coldline** | delete after `backup.retentionDays` |
| `logStore.gcs.bucket`       | Durable agent log artifacts (this doc)  | weeks–months          | **Standard or Nearline** | delete after `logStore.retentionDays` (default 90d) |

**Why they cannot share:**

- Reusing the bundle bucket for logs hits **early-deletion fees** on
  Nearline/Coldline because bundles delete in minutes.
- Reusing the backup bucket loses **transcript replay**: backup
  retention is shorter and a lifecycle that suits Dolt snapshots will
  delete log artifacts before the forensic-replay window closes.
- Logs have **distinct IAM and redaction needs** (engineer/desktop/public
  scopes, redacted vs raw); bundles and backups are pure operational
  artefacts.

The chart fails the `helm install` render fast when `logStore.backend=gcs`
and either `logStore.gcs.bucket` is empty or no GCP auth path is
configured (`spire.validateLogStore` in `helm/spire/templates/_helpers.tpl`).

---

## 3. Bucket setup

Create the dedicated log bucket once, before the first install. The
chart does not create buckets — it only consumes existing ones.

```bash
PROJECT_ID=<your-gcp-project>
LOG_BUCKET=${PROJECT_ID}-spire-logs

gcloud storage buckets create gs://${LOG_BUCKET} \
  --project=${PROJECT_ID} \
  --location=us-central1 \
  --default-storage-class=STANDARD \
  --uniform-bucket-level-access
```

Apply a lifecycle rule matching the retention you want (the default
that values.yaml documents is 90 days):

```bash
cat > lifecycle.json <<EOF
{
  "lifecycle": {
    "rule": [{
      "action": {"type": "Delete"},
      "condition": {"age": 90}
    }]
  }
}
EOF

gsutil lifecycle set lifecycle.json gs://${LOG_BUCKET}
```

**Do not enable object versioning** on the log bucket. Log artifacts
are write-once; versioning inflates cost and serves no replay use case.

Repeat for the bundle bucket (`${PROJECT_ID}-spire-bundles`, Standard,
short retention) and the backup bucket (`${PROJECT_ID}-spire-backups`,
Nearline/Coldline, the retention you want for DR). See
[cluster-install.md § 4](cluster-install.md#4-provision-gcp-resources)
for the bundle and backup bucket setup.

---

## 4. IAM

### Single Workload Identity GSA

The chart's `gcp.*` block is the **single GCP credential path**. The
same GSA holds the bindings for every GCP-consuming feature
(`bundleStore`, `backup`, `logStore`). `spi-hzeyz9` deliberately did
*not* introduce a per-store credential field — keep it that way.

```bash
GSA=spire-tower@${PROJECT_ID}.iam.gserviceaccount.com
LOG_BUCKET=${PROJECT_ID}-spire-logs
BUNDLE_BUCKET=${PROJECT_ID}-spire-bundles
BACKUP_BUCKET=${PROJECT_ID}-spire-backups

# Grant the GSA object-admin on each bucket. The narrowest workable
# role today is roles/storage.objectAdmin — the gateway needs read,
# the operator-stamped exporter sidecar needs write, and the chart
# does not split them.
for B in ${LOG_BUCKET} ${BUNDLE_BUCKET} ${BACKUP_BUCKET}; do
  gcloud storage buckets add-iam-policy-binding gs://${B} \
    --member="serviceAccount:${GSA}" \
    --role=roles/storage.objectAdmin
done
```

### Workload Identity binding

Bind the Kubernetes ServiceAccounts that touch logs to the GSA. The
**operator** stamps log-exporter sidecars and the **gateway** reads
manifests + GCS objects:

```bash
NS=spire

for KSA in spire-operator spire-gateway; do
  gcloud iam service-accounts add-iam-policy-binding ${GSA} \
    --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:${PROJECT_ID}.svc.id.goog[${NS}/${KSA}]"

  kubectl annotate serviceaccount -n ${NS} ${KSA} \
    iam.gke.io/gcp-service-account=${GSA} --overwrite
done
```

The exporter sidecar inherits the agent pod's ServiceAccount, which the
operator stamps from the same GSA chain — no extra binding needed for
exporter sidecars themselves.

### Why the exporter and gateway need the same role

Today both reads and writes go through `roles/storage.objectAdmin`.
The runbook flags this so a future tightening (split read-only gateway
SA from write-only exporter SA) has a documented starting point — but
do not split them piecemeal. The chart's `gcp.*` field is one secret
ref; introducing two would be a deliberate chart change.

---

## 5. Helm values reference

The cluster-mode override block lives in
[`helm/spire/values.gke.yaml`](../helm/spire/values.gke.yaml). The
authoritative value documentation is in
[`helm/spire/values.yaml`](../helm/spire/values.yaml) under the
"Log artifact store" and "Log exporter" sections.

### Minimal cluster override

```yaml
# helm/spire/values.gke.yaml (excerpt — the chart's GKE overlay)

logStore:
  backend: gcs
  gcs:
    bucket: PROJECT_ID-spire-logs    # MUST pre-exist; MUST be dedicated
    prefix: ""                       # optional per-tower namespace
  retentionDays: 90                  # doc-only; enforced by gsutil lifecycle

logExporter:
  enabled: true                      # inject the spire-log-exporter sidecar
  # resources omitted → chart defaults: 10m/64Mi req, 200m/256Mi limit
```

### Authoritative key list

| Key                         | Default     | Purpose |
|-----------------------------|-------------|---------|
| `logStore.backend`          | `local`     | `local` writes under the wizard data dir; `gcs` routes through the cloud-native substrate. Cluster installs MUST flip to `gcs`. |
| `logStore.gcs.bucket`       | `""`        | Dedicated log bucket. Empty + `backend=gcs` fails the `helm` render. |
| `logStore.gcs.prefix`       | `""`        | Optional object-name prefix. Substrate appends `/<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/<provider>/<stream>.jsonl` after this prefix. |
| `logStore.retentionDays`    | `90`        | Documentation-only; enforced via `gsutil lifecycle set`. |
| `logExporter.enabled`       | `false`     | When true, injects the `spire-log-exporter` sidecar onto every wizard / apprentice / sage / cleric / arbiter pod. Cluster installs flip to `true` alongside `logStore.backend=gcs`. |
| `logExporter.resources.requests.cpu`    | `10m`   | Sidecar CPU request (mostly idle). |
| `logExporter.resources.requests.memory` | `64Mi`  | Sidecar memory request. |
| `logExporter.resources.limits.cpu`      | `200m`  | Sidecar CPU limit. |
| `logExporter.resources.limits.memory`   | `256Mi` | Sidecar memory limit. |

The exporter sidecar reuses the agent pod's image (it ships in the
same `spire-agent` binary). To pin a different version, set
`PodSpec.LogExporterImage` from your overlay; see
[`pkg/agent/pod_builder.go`](../pkg/agent/pod_builder.go) for the field.

### Pod-side env (rendered by the operator)

The operator's intent reconciler stamps these onto every pod it
builds. Operators normally don't set them by hand, but they're useful
when triaging:

| Env var                           | Source                          |
|-----------------------------------|---------------------------------|
| `SPIRE_LOG_ROOT`                  | Always `/var/spire/logs` (the `spire-logs` emptyDir mount). |
| `LOGSTORE_BACKEND`                | `logStore.backend` |
| `LOGSTORE_GCS_BUCKET`             | `logStore.gcs.bucket` (only when `backend=gcs`) |
| `LOGSTORE_GCS_PREFIX`             | `logStore.gcs.prefix` |
| `LOGSTORE_RETENTION_DAYS`         | `logStore.retentionDays` |
| `GOOGLE_APPLICATION_CREDENTIALS`  | `gcp.mountPath`/`gcp.keyName` (via the shared `gcp.*` Secret) |
| `SPIRE_TOWER`, `SPIRE_BEAD_ID`, `SPIRE_AGENT_NAME`, `SPIRE_ROLE`, `SPIRE_FORMULA_STEP`, `SPIRE_PROVIDER` | Stamped from `RunContext`; the exporter parses identity from the file path layout, not from env. |
| `SPIRE_LOG_EXPORTER_DRAIN_DEADLINE` | Optional; default `25s`. Pod TerminationGrace is 30s so SIGTERM → flush has 5s of headroom. |

### Object key schema

Provider transcripts:

```
gs://<bucket>/<prefix>/<tower>/<bead_id>/<attempt_id>/<run_id>/<agent_name>/<role>/<phase>/<provider>/<stream>.jsonl
```

Operational logs (no provider segment):

```
gs://<bucket>/<prefix>/<tower>/<bead_id>/<attempt_id>/<run_id>/<agent_name>/<role>/<phase>/operational.log
```

The exporter never invents identity from pod names, hostnames, or
wall-clock timestamps — the file path is the single source of truth.
Identity validation lives in `pkg/logartifact.Identity.Validate`.

---

## 6. Retention — three independent axes

| Axis             | Backed by                                | Owner                                  | Awell default |
|------------------|-------------------------------------------|----------------------------------------|---------------|
| Cloud Logging    | GKE log buckets                           | platform operator (gcloud)             | 30 days (GKE default) |
| GCS artifact     | `logStore.gcs.bucket`                     | bucket lifecycle rule (`gsutil`)       | 90 days |
| Tower manifest   | `agent_log_artifacts` table in Dolt       | `pkg/steward.LogArtifactCompactionPolicy` | PerBeadKeep=64, OlderThan=180 days |

The three axes are deliberately independent. The manifest's age cap
(180d) is longer than GCS retention (90d) so a missed manifest insert
(writer crashed mid-upload) does not leave a permanent gap. In steady
state, GCS lifecycle deletes the object first; a render against a
manifest row whose object is gone returns `410 Gone` and clients can
fall back to the manifest's `summary`/`tail` fields.

To raise Cloud Logging retention beyond 30 days:

```bash
gcloud logging buckets update _Default \
  --location=global --retention-days=90
```

To override manifest compaction (rare — the defaults are right for
Awell), patch `pkg/steward.LogArtifactCompactionPolicy` in code and
ship a steward image. There is no Helm value for this — it's a
deployment-time decision compiled into the daemon.

See [cluster-install.md § 10b](cluster-install.md#10b-log-retention-redaction-and-visibility)
for the policy rationale, the visibility/redaction model, and the
gateway scope header.

---

## 7. Troubleshooting

Each section names the failure mode in the words the operator will
hear from a stressed user, then gives diagnose-and-fix steps. The
order matches the failure-mode list in the spi-6k3953 acceptance
criteria.

### 7.1 Missing bucket (`helm install` render fails)

**Symptom:** the install command exits before any pod schedules:

```
Error: execution error at (spire/templates/_helpers.tpl:141:5):
logStore.backend=gcs requires logStore.gcs.bucket to be set...
```

**Cause:** `values.gke.yaml` (or your overlay) sets
`logStore.backend=gcs` but `logStore.gcs.bucket` is empty (or set to
the literal `PROJECT_ID-spire-logs` placeholder without
substitution).

**Fix:** set the real bucket name in your overlay or via `--set`:

```bash
helm upgrade --install spire helm/spire -n spire \
  -f helm/spire/values.gke.yaml \
  --set logStore.gcs.bucket=${PROJECT_ID}-spire-logs \
  --set-file gcp.serviceAccountJson=$HOME/.gcp/sa.json
```

Verify the bucket exists first: `gcloud storage buckets describe gs://${PROJECT_ID}-spire-logs`.

### 7.2 Missing IAM (uploads fail with 403)

**Symptom:** the exporter sidecar logs (`kubectl logs <pod>
-c spire-log-exporter`) show:

```
googleapi: Error 403: Caller does not have storage.objects.create access to bucket ...
```

The agent container completes normally, the bead transitions through
its formula, but the manifest table shows rows with `status=failed`
and the gateway returns an empty list.

**Cause:** the GSA that the operator's KSA is bound to does not have
`roles/storage.objectAdmin` on the log bucket — usually because the
operator added the role on the bundle bucket only.

**Where to look:**

```bash
# Confirm the KSA is annotated:
kubectl get sa -n spire spire-operator -o jsonpath='{.metadata.annotations.iam\.gke\.io/gcp-service-account}'

# Confirm the GSA has role on the LOG bucket:
gcloud storage buckets get-iam-policy gs://${PROJECT_ID}-spire-logs \
  --format=json | jq '.bindings[] | select(.role=="roles/storage.objectAdmin")'
```

**Fix:** add the binding (see § 4 above), then restart any pods that
were running when the failure happened — the in-flight artifacts
will not be retried automatically.

### 7.3 Exporter crashloop

**Symptom:** `kubectl get pods` shows the wizard or apprentice pod
with `2/2 Running` flipping to `1/2 CrashLoopBackOff`. The agent
container is healthy; the `spire-log-exporter` container is
restarting.

**Where to look:**

```bash
# Container-scoped logs from the failed exporter:
kubectl logs <pod> -c spire-log-exporter --previous

# Common terminal errors (sidecar exits 1 only on misconfiguration):
#   "config: SPIRE_LOG_ROOT env is required"
#   "config: SPIRE_TOWER env is required"
#   "dolt: ping <host>:<port>/<tower>: ..."
#   "store: NewGCS: bucket ...: storage: bucket doesn't exist"
```

**Causes and fixes:**

| Terminal error | Cause | Fix |
|----------------|-------|-----|
| `SPIRE_LOG_ROOT env is required` | Operator did not stamp the env. | Confirm the operator pod's image has the `LogExporterEnabled` plumbing (spi-k1cnof). Roll the operator: `kubectl rollout restart deploy/spire-operator -n spire`. |
| `SPIRE_TOWER env is required` | Same, but for tower. | Same fix. |
| `dolt: ping ...` | Dolt is not reachable from the pod. | Check `BEADS_DOLT_SERVER_HOST/PORT` env on the failing pod; confirm the `spire-dolt` Service resolves; check NetworkPolicy. |
| `bucket doesn't exist` | `LOGSTORE_GCS_BUCKET` points at a missing bucket. | Confirm `logStore.gcs.bucket` value matches a real bucket; create the bucket and re-roll. |

The exporter exits **0** for runtime upload/manifest failures — those
do *not* crashloop the sidecar. Crashloops are misconfiguration only,
which is why the table maps each terminal error to a specific cause.

### 7.4 Manifest row exists but GCS object 404 (`410 Gone`)

**Symptom:** `spire logs pretty <bead>` or a desktop client renders
some artifacts but one returns:

```
HTTP 410 Gone
{"error":"artifact bytes unavailable"}
```

**Cause:** the manifest row persists but the GCS object has been
removed. Two paths:

1. **Lifecycle policy** removed the object — expected behavior past
   `logStore.retentionDays`. The 410 is the right answer; clients
   should fall back to the manifest's `summary`/`tail` fields.
2. **Operator removed the object** by hand (e.g. cleaning up after a
   failed deploy). Avoid this — the manifest's compaction window
   (180d) is longer than GCS retention (90d) for exactly this reason.

**Where to look:**

```bash
# Pull the artifact ID from the gateway list:
curl -H "Authorization: Bearer $TOWER_TOKEN" \
  https://<gateway>/api/v1/beads/<bead-id>/logs | jq '.artifacts[].id'

# Confirm the manifest row in Dolt:
kubectl exec -it spire-dolt-0 -n spire -- \
  dolt sql -q "select id, status, object_uri, updated_at \
              from agent_log_artifacts where id = '<artifact-id>';"

# Confirm the object is missing in GCS:
gsutil stat <object_uri>
# CommandException: No URLs matched: <object_uri>
```

**Fix:** none — the bytes are gone. If the gap was unexpected
(within `retentionDays`), check the bucket's lifecycle policy:

```bash
gsutil lifecycle get gs://${PROJECT_ID}-spire-logs
```

A misconfigured rule (e.g. `age: 1`) deletes too aggressively. Set a
correct policy and the *next* artifact survives.

### 7.5 Artifact present but redacted or denied

**Symptom:** the gateway returns a 200 with redacted tokens
(`[REDACTED]`) where the operator expected raw transcript bytes, or
returns:

```
HTTP 403 Forbidden
{"error":"access denied for caller scope","visibility":"engineer_only"}
```

**Cause:** the caller is in a non-engineer scope. Every artifact has
a `visibility` value (`engineer_only`, `desktop_safe`, `public`) and
every request has a `scope` derived from the `X-Spire-Scope` header
(`engineer`, `desktop`, `public`). Default scope when no header is
sent is **desktop** — engineer-only artifacts return 403 because the
caller has not declared engineer scope.

The redactor (`pkg/logartifact/redact`) ALWAYS re-runs on read for
non-engineer scopes, regardless of what was stored. This is
defense-in-depth; it is hygiene, not a security boundary.

**Fix:**

- **Desktop / board rendering looks redacted:** that's correct.
  Desktop scope sees redacted bytes by design. If a user needs raw
  bytes, they should use `spire logs pretty` from a CLI session
  (the CLI sends `X-Spire-Scope: engineer`).
- **CLI returns 403:** confirm the CLI has the engineer scope
  header. `spire logs pretty` sets it automatically (see
  `cmd/spire/logs.go`'s `resolveLogSource`); a hand-built `curl` does
  not. Pass `-H "X-Spire-Scope: engineer"` for engineer-only access.
- **Operator wants to expose more by default:** that is a deliberate
  call site decision in the substrate (spi-cmy90h). Do not flip
  every artifact to `desktop_safe` to "fix" the policy — review the
  redactor's coverage instead.

See [cluster-install.md § 10b](cluster-install.md#10b-log-retention-redaction-and-visibility)
for the visibility model.

---

## 8. Local vs cluster behavior

| Concern                      | Local-native (`logStore.backend=local`)                  | Cluster-as-truth (`logStore.backend=gcs`)              |
|------------------------------|----------------------------------------------------------|---------------------------------------------------------|
| Where bytes live             | Wizard data dir (`~/.local/share/spire/wizards/...`)     | GCS bucket `logStore.gcs.bucket` |
| Where the manifest lives     | Local Dolt (`agent_log_artifacts` table)                 | Cluster Dolt (`agent_log_artifacts` table) |
| Who serves reads             | `spire logs` reads files directly + local manifest       | The gateway, via `/api/v1/beads/{id}/logs[/...]` |
| Who runs the exporter        | Nothing — local writes go straight through `pkg/logartifact.LocalStore` | Per-pod `spire-log-exporter` sidecar |
| What `spire logs pretty` does | Reads files under `wizards/<wizard-name>/` and renders through `pkg/board/logstream` | Resolves through the gateway and renders through the same adapters |
| What the board does          | Same path as `spire logs pretty` (LocalSource)           | GatewaySource hits the same gateway endpoints |
| What stays identical         | The provider adapters (`claude`, `codex`), the rendered output, the redactor, and the manifest schema. The CLI command surface is unchanged.|

Local-native is **first-class**. Cluster mode does not deprecate or
replace it — it adds a backend behind the same shared abstraction.

---

## 9. Cross-references

- Design: design bead **spi-7wzwk2** ("Persistent cloud-native log
  export for cluster mode") — the architectural decision and forecloses.
- Substrate: bead **spi-k1cnof** (passive cluster log exporter), bead
  **spi-j3r694** (gateway bead-log API), bead **spi-4jmbsc** (board
  and CLI consume artifacts), bead **spi-cmy90h** (retention,
  redaction, visibility).
- Manual verification: [cluster-logs-smoke-test.md](cluster-logs-smoke-test.md).
- Install path: [cluster-install.md](cluster-install.md), particularly
  §§ 4 (GCP resources) and 10b (retention/redaction/visibility).
- Substrate code: [`pkg/logartifact/README.md`](../pkg/logartifact/README.md),
  [`pkg/logexport/doc.go`](../pkg/logexport/doc.go).
- Live follow (separate work): bead **spi-bkha5x** — not yet wired.
  This runbook documents the post-completion read path only.
