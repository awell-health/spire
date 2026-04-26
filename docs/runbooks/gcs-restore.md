# GCS Dolt backup restore drill

Operator-facing runbook for restoring a cluster Dolt database from its GCS
backup into a blank PVC, then validating that the restored bead graph is
intact and that a freshly-deployed gateway can serve `/api/v1/*` against
it.

**Scope.** This drill applies only to **cluster-as-truth** (gateway-mode,
`deployment_mode: cluster-native`) deployments. It is not for
local-native or attached-reserved towers — those rely on different
disaster-recovery substrates.

**Drill, not production switchover.** Every step here runs in an
isolated test namespace. No production state is mutated. The drill is
the gate that turns a default-on backup into a validated DR posture; it
must be exercised before any production cluster-as-truth cutover and at
least quarterly thereafter.

The chart-level backup default (`backup.enabled=true`, fail-fast on
missing bucket / GCP auth) is owned by bead `spi-6az9s7` (commit
`2ba6f57`) and not re-documented here. This runbook documents only the
restore path.

---

## 1. Purpose & scope

The cluster Dolt database is the single write authority for the bead
graph in cluster-as-truth deployments. DoltHub is no longer a
bidirectional mirror; GCS is the canonical disaster-recovery substrate,
written by the daily `dolt backup sync` CronJob (`spire-dolt-backup`,
default schedule `0 3 * * *`).

Backup default-on means archives are accumulating. This runbook proves
that those archives can be restored into a usable, gateway-serviceable
cluster — i.e. that DR works, not just that backup runs.

This runbook does **not** cover:

- Restoring a corrupted production PVC in place (that is a different,
  higher-risk operation; consult dolt support before attempting).
- Re-seeding via DoltHub clone (DoltHub is archive-only in this
  topology — see §7).
- Kubernetes secret/credential recovery (k8s Secrets and GCP service
  accounts are recovered out-of-band, not from this backup).

---

## 2. Pre-restore checklist

Confirm every item before starting. Each line is load-bearing.

- **GCS credentials available** — either the same service-account JSON
  used by production (`gcp.serviceAccountJson`) or a Workload-Identity
  binding plus the externally-managed Secret pinned via
  `gcp.secretName`. The drill pod must be able to read
  `gs://<backup.gcs.bucket>/<backup.gcs.prefix>`.
- **Backup target values from production tower config** — the exact
  `backup.gcs.bucket`, `backup.gcs.prefix`, and `backup.remoteName`
  (default `gcs-backup`) values used by the production install. Read
  these from your production values overlay; they are the source URL
  for the restore.
- **Production database name** — `beads.database` (defaults to
  `beads.prefix`, conventionally `spi`). The restored DB must be
  created at the same name so the helm chart's dolt-init script
  recognises the PVC as already-populated and skips its first-boot
  DoltHub clone.
- **Production PVC size** — match `dolt.storage.size` from production
  values (chart default `5Gi`; production may be larger). Undersized
  drill PVCs will fill mid-restore.
- **Test namespace created and isolated** — a dedicated namespace
  (e.g. `spire-restore-drill`) with no production traffic routed at
  it. No production gateway or syncer must point at this namespace.
- **DoltHub bidirectional sync stays off post-restore** — verified
  before promoting the restored cluster (see §7). Do not begin this
  drill if you are not committed to keeping `syncer.enabled=false` on
  the restored install.

---

## 3. Restore steps

All commands run from a workstation with `kubectl` configured against
the cluster you intend to drill in. Variables used below:

```bash
export NAMESPACE=spire-restore-drill
export PROD_BUCKET=<your-prod-backup-bucket>            # backup.gcs.bucket
export PROD_PREFIX=<your-prod-backup-prefix>            # backup.gcs.prefix (may be empty)
export REMOTE_NAME=gcs-backup                           # backup.remoteName (chart default)
export DB_NAME=spi                                      # beads.database || beads.prefix
export PVC_SIZE=5Gi                                     # match production dolt.storage.size
export DOLT_IMAGE=dolthub/dolt-sql-server:latest        # match images.dolt in production
```

A helper script (`scripts/restore-gcs-drill.sh`) wraps steps 3.1–3.4 if
you prefer one-shot execution. The numbered steps below are the
authoritative reference.

### 3.1 Create the test namespace

```bash
kubectl create namespace "$NAMESPACE"
```

### 3.2 Create a blank PVC sized for the production DB

The PVC name `data-spire-dolt-0` matches what the chart's dolt
StatefulSet expects (`<volumeClaimTemplate-name>-<sts-name>-<ordinal>`),
so the eventual `helm install` in §3.5 binds to the pre-populated PVC
instead of provisioning a fresh empty one.

```bash
cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-spire-dolt-0
  labels:
    app.kubernetes.io/name: spire-dolt
    app.kubernetes.io/component: beads-storage
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: $PVC_SIZE
EOF
```

If your production install pinned a `dolt.storage.storageClass`, add
`storageClassName: <class>` to the spec above so the drill PVC uses the
same backing storage.

### 3.3 Stage GCS credentials in the test namespace

Two paths, mirroring production. Pick the one your production install
uses.

**Inline JSON path** (the same pattern as `--set-file
gcp.serviceAccountJson`):

```bash
kubectl create secret generic spire-gcp-sa \
  -n "$NAMESPACE" \
  --from-file=key.json=$HOME/.gcp/spire-sa.json
```

**External Secret / Workload-Identity path:** create the same Secret
name your production install references via `gcp.secretName`, populated
from your secret store (sealed-secrets, ESO, etc.). The data key must
be `key.json` (matches `gcp.keyName`).

### 3.4 Run a one-shot restore Pod

The Pod runs `dolt backup restore` against the GCS URL, writes a fresh
DB into the PVC, then exits. After it succeeds the PVC is the only
output; the Pod itself is disposable.

```bash
cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
apiVersion: v1
kind: Pod
metadata:
  name: dolt-restore
spec:
  restartPolicy: Never
  containers:
    - name: dolt
      image: $DOLT_IMAGE
      command: ["bash", "-c"]
      args:
        - |
          set -euo pipefail
          cd /var/lib/dolt
          if [ -d "${DB_NAME}/.dolt" ]; then
            echo "PVC already has ${DB_NAME}/.dolt — refusing to overwrite. Wipe the PVC and retry."
            exit 1
          fi
          BACKUP_URL="gs://${PROD_BUCKET}/${PROD_PREFIX}"
          echo "restoring \$BACKUP_URL into ${DB_NAME}"
          dolt backup restore "\$BACKUP_URL" "${DB_NAME}"
          cd "${DB_NAME}"
          echo "restored HEAD:"
          dolt log --oneline -n 1
      env:
        - name: DOLT_ROOT_PATH
          value: /var/lib/dolt
        - name: GOOGLE_APPLICATION_CREDENTIALS
          value: /var/secrets/gcp/key.json
      volumeMounts:
        - name: data
          mountPath: /var/lib/dolt
        - name: gcp-sa
          mountPath: /var/secrets/gcp
          readOnly: true
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data-spire-dolt-0
    - name: gcp-sa
      secret:
        secretName: spire-gcp-sa
        defaultMode: 0400
EOF

kubectl wait --for=condition=Ready pod/dolt-restore -n "$NAMESPACE" --timeout=120s || true
kubectl logs -n "$NAMESPACE" pod/dolt-restore -f
```

Pass criterion: the Pod terminates with `status.phase == Succeeded` and
the log ends with `restored HEAD: <hash> <message>`. Record `<hash>` —
§3.5 verifies it.

### 3.5 Verify the restored commit hash

```bash
kubectl get pod dolt-restore -n "$NAMESPACE" -o jsonpath='{.status.phase}{"\n"}'
# Expect: Succeeded

kubectl logs pod/dolt-restore -n "$NAMESPACE" | grep -A1 'restored HEAD:'
# Expect: a non-empty <hash> within RPO of production's latest commit.
```

To compare against production's current head (run from a workstation
with read access to the production cluster):

```bash
kubectl exec -n spire spire-dolt-0 -c dolt -- \
  bash -c "cd /var/lib/dolt/${DB_NAME} && dolt log --oneline -n 1"
```

The drill hash should match a commit that exists in the production
log. A drill hash that is *ahead* of production indicates a configuration
error — the backup is not from production.

Delete the restore Pod once verified; the PVC retains the data:

```bash
kubectl delete pod dolt-restore -n "$NAMESPACE"
```

### 3.6 Bring up a gateway pointed at the restored PVC

Install the chart into the test namespace, reusing the already-populated
PVC and disabling backup/syncer so the drill cluster cannot accidentally
write back to production GCS or DoltHub.

```bash
# Drill values overlay — keep this file local, do not commit drill bearer tokens.
cat > /tmp/drill-values.yaml <<EOF
namespace: $NAMESPACE
createNamespace: false

beads:
  prefix: $DB_NAME
  database: $DB_NAME

# Chart's dolt-init detects the existing .dolt/ directory in the PVC
# and skips its first-boot DoltHub clone — see helm/spire/templates/dolt.yaml.

# Drill: do not run backup or sync against production targets.
backup:
  enabled: false
syncer:
  enabled: false

# Single-archmage gateway with a drill-only bearer token.
gateway:
  enabled: true
  apiPort: 3030
  webhookPort: 8080
  apiToken: $(openssl rand -hex 32)
  service:
    type: ClusterIP

# Minimal credentials — drill does not need DoltHub or Anthropic auth.
dolthub:
  remoteUrl: ""
  user: dolt_remote
  password: drill-only
github:
  token: ""
EOF

helm install spire-drill ./helm/spire \
  --namespace "$NAMESPACE" \
  -f /tmp/drill-values.yaml

kubectl rollout status statefulset/spire-dolt   -n "$NAMESPACE" --timeout=300s
kubectl rollout status deploy/spire-gateway     -n "$NAMESPACE" --timeout=300s
```

Pass criterion: every Deployment/StatefulSet reaches Ready within the
timeout. If `spire-dolt` fails to come up because the StatefulSet
provisioned a new PVC instead of binding the pre-existing
`data-spire-dolt-0`, the labels in §3.2 are wrong — recreate the PVC
with the labels shown.

> **Note.** Apprentice/operator/steward features are not exercised by
> this drill — only the gateway. The chart's other workloads may
> CrashLoop on missing credentials in the drill values; that is
> expected and not a drill failure.

---

## 4. Bead graph integrity validation

Run every check inside the drill's dolt pod. Each command has an
explicit pass criterion. Replace `<EXPECTED_*>` with the values you
recorded from production immediately before the drill.

### 4.1 Connect to the restored database

```bash
DOLT="kubectl exec -n $NAMESPACE spire-dolt-0 -c dolt -- \
  dolt --host 127.0.0.1 --port 3306 --user root --no-tls -p '' sql -q"
```

A trailing semicolon and `USE` clause is needed on each query because
`dolt sql -q` reuses no session state across invocations.

### 4.2 Per-table row counts (core beads schema)

```bash
$DOLT "USE $DB_NAME; SELECT 'issues'       AS table_name, COUNT(*) AS rows FROM issues
                  UNION ALL SELECT 'dependencies', COUNT(*) FROM dependencies
                  UNION ALL SELECT 'labels',       COUNT(*) FROM labels
                  UNION ALL SELECT 'comments',     COUNT(*) FROM comments
                  UNION ALL SELECT 'metadata',     COUNT(*) FROM metadata
                  UNION ALL SELECT 'repos',        COUNT(*) FROM repos
                  UNION ALL SELECT 'formulas',     COUNT(*) FROM formulas;"
```

Pass: each row count is non-zero and within rolling tolerance of the
pre-drill snapshot from production. Run the same query against
production immediately before kicking off the drill and store the
output as the comparison baseline. (`issues` and `dependencies` are
allowed to be slightly higher in production — that delta is the RPO
window. They must not be lower in the restore.)

### 4.3 Internal bead types are present

Internal beads (`message`, `step`, `attempt`, `review`) live in the same
`issues` table; the restore must preserve them.

```bash
$DOLT "USE $DB_NAME; SELECT issue_type, COUNT(*) AS n FROM issues
       WHERE issue_type IN ('message','step','attempt','review')
       GROUP BY issue_type ORDER BY issue_type;"
```

Pass: four rows, each with a non-zero count consistent with the
pre-drill snapshot. A missing `step` or `attempt` count indicates the
backup ran during a phase transition — verify by re-running after the
next backup window.

### 4.4 Spot-check three random bead IDs

Pick three known bead IDs from production (an epic, a recently-closed
task, a long-lived design bead) and confirm full content survives.

```bash
for ID in <epic-id> <task-id> <design-id>; do
  $DOLT "USE $DB_NAME; SELECT id, title, status, issue_type
         FROM issues WHERE id = '$ID';"
  $DOLT "USE $DB_NAME; SELECT COUNT(*) AS comments FROM comments
         WHERE issue_id = '$ID';"
  $DOLT "USE $DB_NAME; SELECT COUNT(*) AS deps FROM dependencies
         WHERE issue_id = '$ID' OR depends_on_id = '$ID';"
done
```

Pass: title/status/type match production for each ID; comment count and
dep count match within the RPO window.

### 4.5 Optional: full bead count via `bd` (if installed in the dolt pod)

The upstream dolt image does not ship `bd`. If you need a `bd`-side
sanity check, run from a workstation that has a `BEADS_DIR` configured
to point at the drill cluster (use the standard `spire tower
attach-cluster` flow with the drill gateway URL + token):

```bash
bd list --json | jq 'length'
```

This is informational, not a primary acceptance check — §4.2 is.

---

## 5. Gateway smoke test (test namespace ONLY — never against prod)

> **WARNING.** Every command in this section is destructive enough to
> matter (the mutation in §5.5 inserts a row in the `comments` table).
> Run them only against the **drill** gateway in `$NAMESPACE`. Do not
> point this script at the production gateway URL.

### 5.1 Port-forward the drill gateway

```bash
kubectl port-forward -n "$NAMESPACE" svc/spire-gateway 3030:3030 &
PF_PID=$!
trap "kill $PF_PID 2>/dev/null || true" EXIT

# Bearer token came from the drill values overlay in §3.6.
export API_TOKEN=$(grep '^  apiToken:' /tmp/drill-values.yaml | awk '{print $2}')
export GATEWAY=http://127.0.0.1:3030
```

### 5.2 Tower endpoint

```bash
curl -fsS -H "Authorization: Bearer $API_TOKEN" "$GATEWAY/api/v1/tower" | jq .
```

Pass: HTTP 200, JSON body with `name`, `prefix`, `database`,
`deploy_mode` (`cluster-native` for a cluster-as-truth restore),
`dolt_url`, `archmage`, `version`. The fields are populated by
`pkg/gateway/gateway.go::handleTower` from the resolved tower config —
empty values mean the chart's gateway init container did not pick up
the restored PVC.

### 5.3 Bead list

```bash
curl -fsS -H "Authorization: Bearer $API_TOKEN" "$GATEWAY/api/v1/beads" \
  | jq 'length'
```

Pass: a non-zero integer matching the `issues`-table count from §4.2
within the limit/cursor pagination window applied by `handleBeads`.

### 5.4 Bead read

Use one of the spot-check IDs from §4.4.

```bash
curl -fsS -H "Authorization: Bearer $API_TOKEN" \
  "$GATEWAY/api/v1/beads/<known-id>" | jq '{id, title, status, issue_type}'
```

Pass: the body matches what `dolt sql` returned for the same ID in §4.4.

### 5.5 Non-destructive mutation: append a drill comment

`POST /api/v1/beads/{id}/comments` is the safest mutation surface — it
appends a row to the `comments` append-only table and never modifies
existing data. We deliberately avoid `POST /api/v1/beads/{id}/ready` or
`/summon` because those trigger executor side-effects.

> **Pick a disposable bead.** Create a fresh task in the drill cluster
> for this purpose, or pick a closed/obsolete bead whose comment thread
> nobody reads. Do **not** comment-spam an active production bead via
> the drill — even though the drill DB is separate, the discipline is
> what keeps DR drills from accidentally polluting prod records when
> the wrong gateway URL is used.

```bash
DRILL_BEAD=<closed-or-disposable-bead-id>

curl -fsS -X POST \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"text":"restore-drill validation comment — safe to delete"}' \
  "$GATEWAY/api/v1/beads/$DRILL_BEAD/comments"
```

Pass: HTTP 201 with `{"id":"<uuid>"}`. Verify the row landed:

```bash
$DOLT "USE $DB_NAME; SELECT id, author, LEFT(text, 40) AS preview
       FROM comments WHERE issue_id = '$DRILL_BEAD'
       ORDER BY created_at DESC LIMIT 1;"
```

Pass: the most recent comment row has the drill text. Tear-down in §8
deletes the entire test namespace (and therefore the comment) — no
manual cleanup needed.

---

## 6. RPO / RTO

Fill in the operator-recorded values during each drill run. The
fixed-by-config values come from production helm overlay; the measured
values come from this drill.

| Metric | Value | Source |
|---|---|---|
| **RPO (recovery point objective)** | _operator-configured; document yours here_ | Equal to the production `backup.schedule` cadence. Chart default is `0 3 * * *` (daily 03:00 UTC), so RPO ≤ 24h on the default. Tighten by overriding `backup.schedule` to a sub-daily cron if your tower's mutation rate warrants it. |
| **RTO (recovery time objective)** | _drill-measured; record total wall-clock from §3.1 → §5.5 here_ | Wall-clock from creating the test namespace to a passing mutation smoke test. |
| **Backup completion lag** | _operator-recorded_ | Time between the most recent commit on production dolt HEAD and the most recent commit replicated to the GCS backup. Read from `gsutil ls -l gs://<bucket>/<prefix>/` (latest object timestamp) vs. `dolt log --oneline -n 1` on production. |

### Not covered by this backup / restore path

- **In-flight mutations between the last backup and the incident.**
  Anything written to the cluster Dolt after the last `dolt backup
  sync` ran is lost on restore. Tightening RPO requires shortening
  `backup.schedule`, not changing the restore procedure.
- **Gateway pod state.** The gateway is stateless; it materialises
  `.beads/` from the PVC on each start. No gateway-local state is
  backed up because there is none worth recovering.
- **Kubernetes Secrets and credentials.** GCP service accounts,
  DoltHub JWKs, GitHub PATs, Anthropic tokens — all recovered from
  k8s Secrets / external secret stores, not from this backup.
- **Steward-side ephemeral session state.** The steward's `/data`
  PVC holds workflow molecule scratchpads and dispatch tracking that
  reconstruct themselves from dolt on next reconcile. Not covered;
  not load-bearing.

---

## 7. Post-restore: do **not** re-enable bidirectional DoltHub sync

This is the most important paragraph of the runbook. The cluster-as-truth
topology depends on a single writer; turning DoltHub bidirectional sync
back on after a restore re-creates the divergence-and-merge-conflict
class of bug the topology was designed to remove (epic [spi-i7k1ag]
context — non-fast-forward push errors observed on 2026-04-26 are the
canonical example).

Before promoting a restored cluster from drill to production, an
operator MUST verify each of the following:

- [ ] **`TowerConfig.Mode` (`deployment_mode`) remains `cluster-native`.**
  The `/api/v1/tower` smoke test in §5.2 already returns this; treat
  any other value as a critical failure.
- [ ] **DoltHub remote is absent or one-way archive only.** Inspect the
  restored DB's remotes:
  ```bash
  $DOLT "USE $DB_NAME; SELECT name, url, fetch_specs FROM dolt_remotes;"
  ```
  An `origin` remote is fine for archive pushes, but no automation
  must `dolt fetch` or `dolt pull` from it on a loop.
- [ ] **`syncer.enabled=false`.** The drill values overlay sets this
  explicitly; confirm the production overlay does too.
  ```bash
  helm get values spire -n spire | grep -A2 '^syncer:'
  ```
  Expect `enabled: false`. If you find `enabled: true`, do not
  promote — the syncer will recreate divergence within minutes (see
  bead `spi-q5q6s6` for the disable-bidirectional-syncer scope).
- [ ] **Gateway-mode rejection of direct local Dolt mutation is
  intact.** Confirm by attempting a direct `spire push` from a laptop
  attached to the restored cluster — it must fail closed (sibling bead
  `spi-6f6ky8` / `spi-hr3tcv`). If `spire push` succeeds against the
  restored cluster, the gateway-mode guard is missing or bypassed; do
  not promote.

If any of these checks fail, the restore is not production-ready. Pull
the drill plug, file a bead, and do not flip traffic.

---

## 8. Drill cadence and tear-down

### Cadence

- **Quarterly minimum** — exercise this drill at least once per
  quarter on a live production-shaped backup.
- **After any backup configuration change** — including
  `backup.schedule`, `backup.gcs.bucket`/`prefix`, `backup.remoteName`,
  GCP credential rotation, or chart upgrades that touch the
  `helm/spire/templates/dolt-backup-cronjob.yaml` or `dolt.yaml`
  files. Configuration drift between backup-write and backup-restore
  is the most common drill failure.
- **After any major Dolt version bump** — the on-disk format is
  generally compatible across patch versions, but a major bump may
  require a follow-up `dolt migrate` step that this runbook does not
  cover. Check dolt release notes before upgrading the chart's
  `images.dolt.tag`.

### Tear-down

```bash
helm uninstall spire-drill -n "$NAMESPACE"
kubectl delete namespace "$NAMESPACE"
```

The namespace deletion reaps the PVC, the gateway Secret, and the drill
comment from §5.5. Production state is unaffected.

---

## References

- Backup default-on (sibling bead): [`spi-6az9s7`](../cluster-deployment.md#backup-bucket-setup) — chart-side default flip and fail-fast validation. Commit `2ba6f57`.
- Cluster deployment guide: [`docs/cluster-deployment.md`](../cluster-deployment.md) — backup bucket setup is a required pre-install step.
- K8s deployment runbook: [`k8s/DEPLOY.md`](../../k8s/DEPLOY.md#11-backup-bucket-cluster-as-truth-dr) — Track A/B install, backup bucket, GCP auth.
- Cluster-as-truth epic: [`spi-i7k1ag`] — DoltHub archive-only topology and the disable-bidirectional-syncer roadmap.
- Internal bead types: [`docs/INTERNAL-BEADS.md`](../INTERNAL-BEADS.md) — `message`/`step`/`attempt`/`review` definitions used in §4.3.
- Helper script: [`scripts/restore-gcs-drill.sh`](../../scripts/restore-gcs-drill.sh) — wraps §3.1–§3.4.
