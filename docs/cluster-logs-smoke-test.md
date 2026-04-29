# Cluster Logs — End-to-End Smoke Test

A reproducible manual procedure that proves the cluster log substrate
landed end-to-end: pods write artifacts, the exporter sidecar uploads
to GCS and writes manifest rows, and board / CLI / gateway all return
parity for a completed bead.

Run this:

- After every cluster-as-truth install (it is the acceptance test for
  log infrastructure).
- After any chart upgrade that touches `logStore.*`, `logExporter.*`,
  the `gcp.*` block, or the `pkg/agent/pod_builder.go` log mount.
- When the operator runbook ([cluster-logs-runbook.md](cluster-logs-runbook.md))
  troubleshooting steps don't isolate the problem — the smoke test
  forces every component into a known state.

The expected runtime is ~5 minutes once the cluster is healthy. Each
step has an explicit **PASS** condition; treat any deviation as a
test failure and consult the matching § of the runbook.

---

## 0. Prerequisites

You need:

- A cluster-as-truth Spire install with `logStore.backend=gcs` and
  `logExporter.enabled=true`.
- `kubectl` pointing at the cluster (`kubectl config current-context`
  resolves to your GKE cluster).
- `gsutil` / `gcloud` authenticated against the project that owns
  the log bucket.
- A laptop with `spire` ≥ the cluster's image tag, attached to the
  tower via `spire tower attach-cluster ...`.
- The bearer token for the gateway: `TOWER_TOKEN`.

Variables this doc assumes you've set:

```bash
export PROJECT_ID=<your-gcp-project>
export NAMESPACE=spire
export GATEWAY_HOST=<your-gateway-hostname>     # e.g. spire.example.com
export TOWER=<your-tower-name>                  # e.g. awell
export LOG_BUCKET=${PROJECT_ID}-spire-logs
export TOWER_TOKEN=<bearer-from-secret>
```

---

## 1. Pre-flight — confirm the substrate is wired

### 1.1 Helm values

```bash
helm get values spire -n ${NAMESPACE} \
  | yq '.logStore, .logExporter'
```

**PASS:** output shows `logStore.backend: gcs`,
`logStore.gcs.bucket: <your log bucket>`, and `logExporter.enabled: true`.

If `backend: local` or `enabled: false` appears, this is a local-mode
install and the rest of the test does not apply — see
[cluster-logs-runbook.md § 2](cluster-logs-runbook.md#2-the-three-buckets--do-not-reuse).

### 1.2 Bucket reachability

```bash
gsutil ls gs://${LOG_BUCKET}
```

**PASS:** the command returns either an empty list (fresh bucket) or
existing objects under `${prefix}/${TOWER}/`. A `BucketNotFoundException`
or 403 means setup is incomplete — go to runbook § 7.1 / § 7.2.

### 1.3 Gateway reachability

```bash
curl -sS -o /dev/null -w "%{http_code}\n" \
  -H "Authorization: Bearer ${TOWER_TOKEN}" \
  https://${GATEWAY_HOST}/api/v1/tower
```

**PASS:** HTTP `200`. `401` means the bearer token is wrong; `502/503`
means the gateway pod isn't ready.

### 1.4 Operator and gateway are on the post-spi-k1cnof image

```bash
kubectl describe deploy spire-operator spire-gateway -n ${NAMESPACE} \
  | grep -E 'Image:'
```

**PASS:** both deployments report image tags that include the log
exporter wiring (anything ≥ the chart version that landed
spi-k1cnof). If you're rolling forward, `helm history spire -n
${NAMESPACE}` should list the upgrade revision.

---

## 2. File a bead and summon a wizard

### 2.1 File the bead

```bash
SMOKE_BEAD=$(spire file "log smoke test" -t task -p 3 --json | jq -r '.id')
echo "Smoke bead: ${SMOKE_BEAD}"
```

**PASS:** a bead ID like `spi-XXXXXX` is printed. Record it; every
later step references this ID.

### 2.2 Summon a wizard

```bash
spire summon ${SMOKE_BEAD}
```

The smoke test deliberately uses a small task so the formula
completes in a few minutes. If you have a tower-specific "smoke"
formula that exits faster, use it.

**PASS:** the command returns without error. `spire status` shows
the wizard's pod scheduling under `${SMOKE_BEAD}`.

### 2.3 Confirm the wizard pod has the sidecar

```bash
WIZARD_POD=$(kubectl get pods -n ${NAMESPACE} \
  -l spire.bead=${SMOKE_BEAD},spire.role=wizard \
  -o jsonpath='{.items[0].metadata.name}')

kubectl get pod ${WIZARD_POD} -n ${NAMESPACE} \
  -o jsonpath='{range .spec.containers[*]}{.name}{"\n"}{end}'
```

**PASS:** the output contains both `agent` AND `spire-log-exporter`.
Just `agent` means `logExporter.enabled` is false or the operator
didn't see it — go to runbook § 7.3.

### 2.4 Watch the bead complete

```bash
spire focus ${SMOKE_BEAD}    # or watch in the board
```

**PASS:** the bead reaches `closed` (or whatever terminal state your
smoke task targets). If the bead errors before completion, the
exporter still flushes what it captured up to the failure — the rest
of the test still runs.

---

## 3. Verify the exporter ran

### 3.1 Sidecar logs show uploads

```bash
kubectl logs ${WIZARD_POD} -n ${NAMESPACE} -c spire-log-exporter --tail=200
```

**PASS:** the output contains structured JSON lines for the bead
(grep for `${SMOKE_BEAD}`), plus a final shutdown line of the form:

```
spire-log-exporter: shutdown stats finalized=N failed=0 files=M retries=0
```

`finalized` should be ≥ 1 and `failed` should be `0`. `failed > 0`
means at least one artifact didn't upload cleanly; go to runbook § 7.2
or § 7.3.

If the wizard launched apprentice / sage pods, repeat for each pod —
each role's exporter sidecar uploads its own artifacts:

```bash
kubectl get pods -n ${NAMESPACE} -l spire.bead=${SMOKE_BEAD} \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
```

### 3.2 Cloud Logging has structured stream events

```bash
gcloud logging read \
  "resource.type=k8s_container AND \
   resource.labels.namespace_name=${NAMESPACE} AND \
   jsonPayload.bead_id=\"${SMOKE_BEAD}\"" \
  --project=${PROJECT_ID} \
  --limit=5 --format=json \
  | jq '.[] | {time: .timestamp, role: .jsonPayload.role, agent: .jsonPayload.agent_name, stream: .jsonPayload.stream}'
```

**PASS:** at least one entry returned with the expected `role` /
`agent_name` / `stream` fields. Empty result means Cloud Logging is
not picking up the sidecar's stdout — usually a GKE log-collection
issue, not a Spire issue.

---

## 4. Verify GCS objects landed

```bash
gsutil ls -r "gs://${LOG_BUCKET}/**/${SMOKE_BEAD}/**" | head -20
```

**PASS:** the listing includes objects under
`<prefix>/<tower>/${SMOKE_BEAD}/<attempt>/<run>/<agent>/<role>/<phase>/...`
matching the schema documented in
[cluster-logs-runbook.md § 5](cluster-logs-runbook.md#object-key-schema).

Operational logs end in `operational.log`; provider transcripts end
in `<stream>.jsonl` (typically `transcript.jsonl`).

If the listing is empty:

- Confirm GCS uploads completed: `kubectl logs ${WIZARD_POD} -c
  spire-log-exporter | grep -i upload`
- Confirm the exporter wasn't killed before flush: TerminationGrace
  defaults to 30s and the drain deadline to 25s; if either was
  shortened, the exporter may exit before uploading. See
  `pkg/agent/pod_builder.go`'s `LogExporterDefaultTerminationGrace`.

---

## 5. Verify manifest rows match objects

### 5.1 Query the manifest table

```bash
kubectl exec -it spire-dolt-0 -n ${NAMESPACE} -- \
  dolt --user=root sql -q "
    SELECT id, agent_name, role, phase, provider, stream,
           sequence, byte_size, status, object_uri
    FROM agent_log_artifacts
    WHERE bead_id = '${SMOKE_BEAD}'
    ORDER BY agent_name, role, phase, sequence;
  "
```

**PASS:** one row per artifact you saw in `gsutil ls`, with
`status='finalized'` and a non-empty `object_uri`. The number of rows
matches the count of `.jsonl` / `.log` objects in GCS.

If rows have `status='writing'` or `status='failed'`:

- `writing` after the bead has closed → the agent crashed before
  the exporter saw EOF on the file. Acceptable on failed beads;
  unexpected on a clean smoke run.
- `failed` → the upload or manifest insert hit a retryable error
  and exhausted retries. Go to runbook § 7.2 / § 7.3.

### 5.2 Identity matches the object key

Pick one row's `object_uri` and confirm the file path encodes the
same identity:

```bash
# From the SQL query above, copy one object_uri value:
gsutil stat <object_uri>
```

**PASS:** the URI's path segments after `gs://${LOG_BUCKET}/` match
`<prefix>/${TOWER}/${SMOKE_BEAD}/<attempt>/<run>/<agent>/<role>/<phase>/(<provider>/)<stream>.jsonl`.
Any mismatch means the exporter is parsing a non-canonical path —
file an issue against `pkg/logexport`.

---

## 6. Verify gateway parity

### 6.1 List endpoint

```bash
curl -sS -H "Authorization: Bearer ${TOWER_TOKEN}" \
  "https://${GATEWAY_HOST}/api/v1/beads/${SMOKE_BEAD}/logs" \
  | jq '.artifacts | length, (.[0] | {id, role, stream, status, byte_size, links})'
```

**PASS:** the artifact count is non-zero and matches what step 5
returned. The first row carries identity fields, `status: "finalized"`,
and a `links.raw` URL like `/api/v1/beads/${SMOKE_BEAD}/logs/<id>/raw`.

If you want the headerised summary view instead (no pagination):

```bash
curl -sS -H "Authorization: Bearer ${TOWER_TOKEN}" \
  "https://${GATEWAY_HOST}/api/v1/beads/${SMOKE_BEAD}/logs/summary" \
  | jq '.artifacts | length'
```

### 6.2 Raw fetch matches GCS bytes

Pick one transcript's `id` from the list response and fetch raw:

```bash
ARTIFACT_ID=$(curl -sS -H "Authorization: Bearer ${TOWER_TOKEN}" \
  "https://${GATEWAY_HOST}/api/v1/beads/${SMOKE_BEAD}/logs" \
  | jq -r '.artifacts[] | select(.stream=="transcript") | .id' | head -1)

curl -sS -H "Authorization: Bearer ${TOWER_TOKEN}" \
       -H "X-Spire-Scope: engineer" \
  "https://${GATEWAY_HOST}/api/v1/beads/${SMOKE_BEAD}/logs/${ARTIFACT_ID}/raw" \
  > /tmp/gateway-raw.jsonl

# Compare against the bytes in GCS:
gsutil cat <object_uri-for-same-row> > /tmp/gcs-raw.jsonl

diff /tmp/gateway-raw.jsonl /tmp/gcs-raw.jsonl
```

**PASS:** `diff` exits 0 when scope is `engineer` and the artifact's
visibility is `engineer_only`. For other scope/visibility pairs the
gateway runs the redactor on read — that case is covered in step 6.4.

### 6.3 Pretty endpoint

```bash
curl -sS -H "Authorization: Bearer ${TOWER_TOKEN}" \
       -H "X-Spire-Scope: engineer" \
  "https://${GATEWAY_HOST}/api/v1/beads/${SMOKE_BEAD}/logs/${ARTIFACT_ID}/pretty" \
  | jq '.events | length, .events[0]'
```

**PASS:** `events` is non-empty and each entry has `kind`/`title`/`body`
fields. Empty events on a non-empty raw transcript means the provider
adapter (`pkg/board/logstream`) didn't recognize the format — that is
a substrate bug, not an operator bug.

### 6.4 Redaction is applied for non-engineer scopes

```bash
# Default scope (no header) → desktop. Engineer-only artifacts return 403:
curl -sS -o /dev/null -w "%{http_code}\n" \
  -H "Authorization: Bearer ${TOWER_TOKEN}" \
  "https://${GATEWAY_HOST}/api/v1/beads/${SMOKE_BEAD}/logs/${ARTIFACT_ID}/raw"
```

**PASS:** HTTP `403` if the artifact is `engineer_only` (the substrate's
default visibility for raw transcripts); HTTP `200` if it's
`desktop_safe` (the redactor will have run on the body). Either is
correct — the test is that the gateway honors the scope/visibility
matrix. See [cluster-logs-runbook.md § 7.5](cluster-logs-runbook.md#75-artifact-present-but-redacted-or-denied).

---

## 7. Verify CLI parity

```bash
spire logs pretty ${SMOKE_BEAD}
```

**PASS:** the command renders styled provider transcript lines
identical to what `${ARTIFACT_ID}/pretty` returned in step 6.3
(formatting differs because the CLI applies `pkg/board/logstream`
rendering locally, but the events themselves match).

The CLI sets `X-Spire-Scope: engineer` automatically for cluster-attach
mode, so an `engineer_only` artifact renders raw. If you see redacted
output here, your local CLI is not detecting the gateway tower — see
`spire tower list` and confirm the active tower has `is_gateway: true`.

For a specific provider:

```bash
spire logs pretty ${SMOKE_BEAD} --provider=claude
```

**PASS:** only Claude transcripts render.

---

## 8. Verify board parity

Open Spire Desktop (or the TUI board), switch to the tower's gateway
context, and navigate to `${SMOKE_BEAD}`.

**PASS:** the bead inspector shows:

- A populated **Logs** tab with the same artifact list the gateway
  returns.
- For each provider transcript row, opening the entry renders the
  styled events (Claude / Codex adapters from
  `pkg/board/logstream`).
- The header shows non-zero summary/tail counts.

Empty state on a bead that step 4–6 confirmed has artifacts means the
board is reading from a different source than the gateway — most
often, a stale local-mode tower in `spire tower list`. Switch to the
gateway tower with `spire tower use <name>`.

---

## 9. Cleanup

The smoke bead is harmless to leave behind, but if you want a clean
slate:

```bash
spire update ${SMOKE_BEAD} --status=closed
```

The artifacts in GCS will age out via the bucket lifecycle policy
(`logStore.retentionDays`, default 90 days). The manifest rows age
out via `pkg/steward.LogArtifactCompactionPolicy` (PerBeadKeep=64,
OlderThan=180 days). Manual deletion is unnecessary.

---

## 10. Failure → runbook map

| Smoke step that failed       | Most likely runbook section |
|------------------------------|------------------------------|
| 1.1 — wrong values           | runbook § 5 (Helm values) |
| 1.2 — bucket unreachable     | runbook § 7.1 (missing bucket) |
| 1.3 — gateway 401/502        | cluster-install.md § 12 (Troubleshooting), `Gateway has no external IP` / `/healthz returns 502` |
| 2.3 — sidecar absent         | runbook § 7.3 (exporter crashloop / not injected) |
| 3.1 — exporter shutdown stats `failed > 0` | runbook § 7.2 (missing IAM) |
| 4 — empty `gsutil ls`        | runbook § 7.2, § 7.3 |
| 5.1 — `status='failed'`      | runbook § 7.2 |
| 5.2 — identity mismatch      | substrate bug — file against `pkg/logexport` |
| 6.1 — empty list             | runbook § 7.2, § 7.4 (`410 Gone`) |
| 6.2 — diff non-zero          | runbook § 7.5 (redaction surprised the test) |
| 6.4 — wrong status code      | runbook § 7.5 (visibility/scope) |
| 7 — CLI redacted             | check `spire tower list` for the active tower's gateway flag |
| 8 — board empty              | confirm board's tower selection matches the gateway tower |

---

## Cross-references

- [cluster-logs-runbook.md](cluster-logs-runbook.md) — operator
  reference for setup, retention, and failure modes.
- [cluster-install.md § 10b](cluster-install.md#10b-log-retention-redaction-and-visibility) —
  retention/redaction/visibility policy.
- [pkg/logartifact/README.md](../pkg/logartifact/README.md) —
  substrate internals.
