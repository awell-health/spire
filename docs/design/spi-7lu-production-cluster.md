# Design: Production Cluster (Helm) — spi-7lu

> Design bead spi-8n9km for epic spi-7lu.

## Context

Spire's Kubernetes layer is v3-aligned and functional: the Helm chart
deploys dolt, steward, operator, and agent pods. CRDs (SpireAgent,
SpireWorkload, SpireConfig) are clean and well-typed. The operator's
three controllers (BeadWatcher, WorkloadAssigner, AgentMonitor) handle
the full bead-to-pod lifecycle. The minikube demo script and cluster
deployment guide cover local and basic production setups.

Six gaps remain before the cluster story is production-ready. This
document designs each one.

---

## Gap 1: Bootstrap Job

**Problem.** A fresh Helm install requires the operator to attach to an
existing tower on DoltHub. Today this is a manual step — the admin must
exec into a pod or run `spire tower attach` out-of-band before the
cluster can sync beads.

**Design.**

Add a Helm **pre-install/pre-upgrade hook** Job that runs
`spire tower attach <dolthub-remote>`:

```yaml
# helm/spire/templates/bootstrap-job.yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: spire-bootstrap
  namespace: {{ .Values.namespace }}
  annotations:
    "helm.sh/hook": pre-install,pre-upgrade
    "helm.sh/hook-weight": "-5"        # run before other hooks
    "helm.sh/hook-delete-policy": hook-succeeded
spec:
  backoffLimit: 3
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: bootstrap
          image: {{ .Values.images.steward.repository }}:{{ .Values.images.steward.tag }}
          command: ["sh", "-c"]
          args:
            - |
              # If the beads DB already exists, skip bootstrap
              if [ -d /data/.beads ]; then
                echo "Beads DB already exists, skipping bootstrap"
                exit 0
              fi
              spire tower attach "$DOLTHUB_REMOTE"
          env:
            - name: DOLTHUB_REMOTE
              value: {{ .Values.dolthub.remote | quote }}
            - name: DOLT_REMOTE_USER
              valueFrom:
                secretKeyRef:
                  name: spire-credentials
                  key: DOLT_REMOTE_USER
            - name: DOLT_REMOTE_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: spire-credentials
                  key: DOLT_REMOTE_PASSWORD
          volumeMounts:
            - name: beads-data
              mountPath: /data
      volumes:
        - name: beads-data
          persistentVolumeClaim:
            claimName: spire-beads-data
```

**Key decisions:**

- **pre-install AND pre-upgrade**: On fresh install, creates the DB. On
  upgrade, skips if DB exists (idempotent).
- **hook-succeeded delete policy**: Completed Jobs are cleaned up
  automatically. Failed Jobs persist for debugging.
- **backoffLimit: 3**: Transient DoltHub connectivity issues get retried.
- **Shares the beads-data PVC**: The bootstrap Job writes into the same
  volume the dolt and steward pods use.
- **Idempotent guard**: Checks for `.beads` dir before running attach.

**Values additions:**

```yaml
bootstrap:
  enabled: true   # can disable for clusters that manage tower attach externally
```

**Validation:** After `helm install`, the dolt pod should start with a
populated `.beads/` directory. `kubectl logs job/spire-bootstrap` shows
either "attached to tower" or "skipping bootstrap".

---

## Gap 2: Operator Reads Repos Table

**Problem.** SpireAgent CRDs are currently defined explicitly in
`values.yaml`. When a new repo is registered in the tower via
`spire repo add`, the admin must manually add a corresponding agent
entry to the Helm values and run `helm upgrade`. This breaks the
"file work locally, push, cluster picks it up" workflow.

**Design.**

Add an **auto-derive mode** to the operator's BeadWatcher (or a new
lightweight controller, `RepoWatcher`) that reads the tower's `repos`
table and reconciles SpireAgent CRs.

### Flow

```
repos table (dolt)           RepoWatcher              SpireAgent CRs
┌──────────────┐         ┌─────────────────┐       ┌──────────────────┐
│ prefix: spi  │ ──────> │ Poll repos table│ ──>   │ spi-auto-agent   │
│ prefix: web  │         │ every cycle     │       │ web-auto-agent   │
│ prefix: api  │         │ (2m default)    │       │ api-auto-agent   │
└──────────────┘         └─────────────────┘       └──────────────────┘
                                │
                                │ Does NOT touch CRs
                                │ with label:
                                │   spire.awell.io/managed-by: helm
                                │ (those are explicit overrides)
```

### Reconciliation rules

1. **Create**: For each repo in the table that has no matching
   SpireAgent CR, create one with sensible defaults:
   - `name`: `{prefix}-auto-agent`
   - `mode`: `managed`
   - `prefixes`: `["{prefix}-"]`
   - `maxConcurrent`: from SpireConfig default (1 if unset)
   - `image`: from SpireConfig default agent image
   - `repo`: from repos table `url` column
   - `repoBranch`: from repos table `branch` column (default: `main`)
   - Label: `spire.awell.io/managed-by: repo-watcher`

2. **Update**: If a repo's URL or branch changes in the table, update
   the auto-derived CR. Only update CRs labeled
   `spire.awell.io/managed-by: repo-watcher`.

3. **Delete**: If a repo is removed from the table, delete the
   auto-derived CR (only if labeled `repo-watcher`).

4. **Override**: CRs created via Helm values get the label
   `spire.awell.io/managed-by: helm`. The RepoWatcher never touches
   these. This means explicit Helm values always win — admins can
   override image, resources, maxConcurrent, etc. for specific agents.

### SpireConfig additions

```yaml
spec:
  agentDefaults:
    image: ghcr.io/awell-health/spire-agent:latest
    maxConcurrent: 1
    resources:
      requests:
        memory: "256Mi"
        cpu: "100m"
      limits:
        memory: "1Gi"
        cpu: "500m"
  autoDerive:
    enabled: true   # false = only use explicit SpireAgent CRs
```

### Values additions

```yaml
operator:
  autoDerive:
    enabled: true
  agentDefaults:
    image: ""            # defaults to images.agent value
    maxConcurrent: 1
    resources: {}        # defaults to chart-level agent resources
```

**Risk mitigation (from PLAN.md risk #3):** Both modes work during
transition. CRs created by Helm are labeled `managed-by: helm` and
never touched by the RepoWatcher. Auto-derived CRs are labeled
`managed-by: repo-watcher` and can be overridden by creating an
explicit Helm-managed CR with the same prefix.

**Implementation:** This is a new controller in `operator/controllers/repo_watcher.go`.
It reads from the dolt repos table using the same `bd` tooling the
BeadWatcher uses. Polling interval matches the existing `spec.polling.interval`.

---

## Gap 3: Image Version Alignment

**Problem.** Dockerfiles reference `latest` tags. In production this
means:
- No reproducibility — two deploys at different times get different
  images.
- No audit trail — can't tell which Spire version a pod ran.
- Build breakage when upstream images change.

**Design.**

### Versioning strategy

1. **Pin Spire images to release tags.** goreleaser already produces
   tagged images. The Helm chart `appVersion` should track the Spire
   release (e.g., `0.35.0`). Helm values default to:

   ```yaml
   images:
     steward:
       tag: "{{ .Chart.AppVersion }}"
     agent:
       tag: "{{ .Chart.AppVersion }}"
   ```

   The `_helpers.tpl` template provides an `imageTag` helper:

   ```
   {{- define "spire.imageTag" -}}
   {{ .Values.images.steward.tag | default .Chart.AppVersion }}
   {{- end }}
   ```

2. **Pin the Dolt image to a tested version.** Instead of `latest`,
   pin to the version validated in CI (e.g., `dolthub/dolt-sql-server:1.35.0`).
   Update in lockstep with Spire releases when Dolt compatibility is
   verified.

3. **Dockerfile ARGs for beads version.** The Dockerfiles should accept
   a `BEADS_VERSION` build arg:

   ```dockerfile
   ARG BEADS_VERSION=latest
   RUN curl -sL https://github.com/.../beads/releases/download/v${BEADS_VERSION}/... | tar xz
   ```

   goreleaser sets this to the current release tag at build time.

4. **Image pull policy.** Defaults change from `IfNotPresent` to:
   - `IfNotPresent` for tagged versions (immutable by convention)
   - `Always` only when tag is explicitly `latest` or `dev`

### Helm values changes

```yaml
images:
  steward:
    tag: ""  # empty = use Chart.AppVersion
  agent:
    tag: ""  # empty = use Chart.AppVersion
  dolt:
    tag: "1.35.0"  # pinned, tested version
```

### CI integration

The goreleaser config already builds and pushes images. Add:
- Tag images with both the semver and `latest`.
- Update `Chart.yaml` `appVersion` as part of the release process
  (the `/release` skill already tags versions).

---

## Gap 4: Syncer Pod Formalization

**Problem.** The syncer CronJob works but is disconnected from the
SpireConfig CR. It has no health reporting, its schedule isn't
configurable via the CR, and there's no way to know if syncs are
failing without checking CronJob logs manually.

**Design.**

### Move syncer config into SpireConfig CR

```yaml
# SpireConfig addition
spec:
  syncer:
    enabled: true
    schedule: "*/2 * * * *"
    mode: cronjob           # "cronjob" (current) or "sidecar" (future: runs in steward pod)
```

The Helm template reads from `spireConfig.syncer` values and renders
both the SpireConfig CR field and the CronJob spec.

### Health reporting

Add a `status.syncer` section to the SpireConfig CR:

```yaml
status:
  syncer:
    lastSuccessfulSync: "2026-04-10T17:30:00Z"
    lastAttemptedSync: "2026-04-10T17:32:00Z"
    lastError: ""
    consecutiveFailures: 0
```

The syncer script writes its outcome to a known location. The operator
reads this on each cycle and updates SpireConfig status. This surfaces
sync health in `kubectl get spireconfig default -o yaml` and enables
alerting.

### Syncer script improvements

The syncer currently runs `spire pull && spire push`. Enhance to:

```bash
#!/bin/sh
set -e
START=$(date +%s)

# Pull from DoltHub
if spire pull 2>/tmp/sync-error; then
  echo "pull succeeded"
else
  echo "pull failed: $(cat /tmp/sync-error)"
  echo '{"success":false,"error":"pull failed","timestamp":"'$(date -u +%FT%TZ)'"}' > /data/sync-status.json
  exit 1
fi

# Push to DoltHub
if spire push 2>/tmp/sync-error; then
  echo "push succeeded"
else
  echo "push failed: $(cat /tmp/sync-error)"
  echo '{"success":false,"error":"push failed","timestamp":"'$(date -u +%FT%TZ)'"}' > /data/sync-status.json
  exit 1
fi

END=$(date +%s)
echo '{"success":true,"duration_seconds":'$((END-START))',"timestamp":"'$(date -u +%FT%TZ)'"}' > /data/sync-status.json
```

### Alerting

The operator should log a warning when `consecutiveFailures >= 3`.
Future: emit a Kubernetes Event on the SpireConfig CR for external
alerting tools to pick up.

---

## Gap 5: End-to-End Cluster Smoke Test

**Problem.** There's no automated test that verifies the full cluster
pipeline: install chart → bootstrap tower → file bead → operator picks
it up → agent executes → bead closes. The minikube demo script exists
but isn't CI-integrated.

**Design.**

### Test script: `test/smoke/cluster-smoke.sh`

A self-contained script that:

1. **Setup**
   - Requires: `minikube`, `helm`, `kubectl`, `spire` CLI
   - Starts minikube (or uses existing)
   - Builds images: `docker build -f Dockerfile.mayor ...`,
     `docker build -f Dockerfile.agent ...`
   - Loads images into minikube: `minikube image load`

2. **Install**
   - `helm install spire ./helm/spire` with test values
   - Wait for bootstrap job to complete
   - Wait for all deployments to be ready (timeout: 5m)

3. **Verify bootstrap**
   - Check that SpireConfig exists
   - Check that dolt pod has `.beads/` directory

4. **File work**
   - From the host: `bd create "Smoke test task" -t task -p 2`
   - `spire push` (or let syncer handle it)

5. **Observe operator pickup**
   - Poll `kubectl get spireworkloads` until one appears (timeout: 3m)
   - Verify workload moves to `Assigned`

6. **Observe agent execution**
   - Poll for agent pod creation (timeout: 2m)
   - Wait for pod to complete (timeout: 10m)

7. **Verify completion**
   - `spire pull`
   - Check bead status is `closed` (or at least `in_progress` with a
     branch pushed)
   - Check SpireWorkload is `Done`
   - Check agent pod was reaped

8. **Teardown**
   - `helm uninstall spire`
   - Optionally stop minikube

### Exit codes

- **0**: All checks passed
- **1**: Setup failure (minikube, build, install)
- **2**: Pipeline failure (bead not picked up, pod not created)
- **3**: Execution failure (agent failed, bead not closed)

### CI integration

Add a GitHub Actions workflow `cluster-smoke.yml`:

```yaml
name: Cluster Smoke Test
on:
  push:
    paths:
      - 'helm/**'
      - 'operator/**'
      - 'k8s/**'
      - 'Dockerfile.*'
      - 'agent-entrypoint.sh'
  workflow_dispatch:

jobs:
  smoke:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@v4
      - uses: medyagh/setup-minikube@latest
      - run: ./test/smoke/cluster-smoke.sh
        env:
          DOLTHUB_REMOTE: ${{ secrets.SMOKE_DOLTHUB_REMOTE }}
          ANTHROPIC_API_KEY: ${{ secrets.SMOKE_ANTHROPIC_KEY }}
```

**Cost note:** The smoke test uses a real Anthropic API key. To limit
cost, the test task should be trivial (e.g., "add a comment to
README.md") and use `claude-haiku-4-5` as the model. Estimated cost per
run: < $0.10.

### Gating

The smoke test is not a PR gate (too slow, too expensive). It runs on
pushes to Helm/operator/k8s paths and on `workflow_dispatch` for
manual triggering before releases.

---

## Gap 6: Optional Ingress for Webhook Receiver

**Problem.** `spire serve` runs the webhook receiver for Linear
epic sync and (future) GitHub webhooks. In the cluster, this needs an
Ingress to be reachable from the internet.

**Design.**

### Helm template: `ingress.yaml`

```yaml
{{- if .Values.ingress.enabled }}
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: spire-webhook
  namespace: {{ .Values.namespace }}
  annotations:
    {{- toYaml .Values.ingress.annotations | nindent 4 }}
spec:
  {{- if .Values.ingress.className }}
  ingressClassName: {{ .Values.ingress.className }}
  {{- end }}
  {{- if .Values.ingress.tls }}
  tls:
    {{- range .Values.ingress.tls }}
    - hosts:
        {{- range .hosts }}
        - {{ . | quote }}
        {{- end }}
      secretName: {{ .secretName }}
    {{- end }}
  {{- end }}
  rules:
    - host: {{ .Values.ingress.host | quote }}
      http:
        paths:
          - path: /webhook
            pathType: Prefix
            backend:
              service:
                name: spire-webhook
                port:
                  number: 8080
{{- end }}
```

### Service for webhook receiver

```yaml
# helm/spire/templates/webhook-service.yaml
{{- if .Values.ingress.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: spire-webhook
  namespace: {{ .Values.namespace }}
spec:
  selector:
    app: spire-steward  # webhook receiver runs in the steward pod
  ports:
    - port: 8080
      targetPort: 8080
      protocol: TCP
{{- end }}
```

### Values additions

```yaml
ingress:
  enabled: false
  className: ""        # e.g., "nginx", "alb"
  host: ""             # e.g., "spire.example.com"
  annotations: {}
  # Example for AWS ALB:
  #   kubernetes.io/ingress.class: alb
  #   alb.ingress.kubernetes.io/scheme: internet-facing
  #   alb.ingress.kubernetes.io/target-type: ip
  # Example for nginx + cert-manager:
  #   cert-manager.io/cluster-issuer: letsencrypt-prod
  tls: []
  # - hosts:
  #     - spire.example.com
  #   secretName: spire-tls
```

### Webhook receiver in steward

The steward pod runs `spire serve --port 8080` as a sidecar or
additional process. The Helm template conditionally adds the container
and port when `ingress.enabled` is true:

```yaml
# In steward.yaml, conditional sidecar:
{{- if .Values.ingress.enabled }}
- name: webhook
  image: {{ .Values.images.steward.repository }}:{{ include "spire.imageTag" . }}
  command: ["spire", "serve", "--port", "8080"]
  ports:
    - containerPort: 8080
  livenessProbe:
    httpGet:
      path: /healthz
      port: 8080
    initialDelaySeconds: 5
    periodSeconds: 30
  env:
    - name: LINEAR_API_KEY
      valueFrom:
        secretKeyRef:
          name: spire-credentials
          key: LINEAR_API_KEY
          optional: true
{{- end }}
```

### Security considerations

- **TLS termination**: Handled by the Ingress controller (nginx, ALB,
  etc.), not by Spire.
- **Webhook signature verification**: Linear webhooks include an
  `X-Linear-Signature` header. `spire serve` should verify this using
  a shared secret stored in the `spire-credentials` Secret. (This is
  an implementation detail for the webhook handler, not the Helm chart.)
- **Path restriction**: Only `/webhook` is exposed. The Ingress rule
  uses `pathType: Prefix` for `/webhook` — no other steward endpoints
  are reachable from outside the cluster.

---

## Implementation Order

These gaps have minimal interdependencies. Suggested order based on
value and risk:

```
1. Image version alignment  (low risk, high hygiene value, unblocks others)
2. Bootstrap job            (enables fresh installs, Helm hook only)
3. Syncer formalization     (incremental improvement to existing CronJob)
4. Ingress for webhooks     (new templates, no changes to existing ones)
5. Operator reads repos     (new controller, medium risk — risk #3 in PLAN.md)
6. E2E smoke test           (depends on 1-4 being stable, CI integration)
```

Each gap is independently implementable as a separate task under the
epic. No gap requires another to be complete first, though the smoke
test is most valuable after the others are done.

## Subtask Breakdown

| # | Title | Type | Priority | Depends on |
|---|-------|------|----------|------------|
| 1 | Pin image versions and add `_helpers.tpl` tag logic | task | P1 | — |
| 2 | Bootstrap Job Helm hook (`spire tower attach`) | task | P1 | — |
| 3 | Syncer health reporting and SpireConfig integration | task | P2 | — |
| 4 | Ingress + webhook Service templates | task | P2 | — |
| 5 | RepoWatcher controller (auto-derive SpireAgent from repos table) | task | P2 | — |
| 6 | Cluster smoke test script + CI workflow | task | P2 | 1-4 ideally |
