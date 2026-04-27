# k8s v1.0 cluster-as-truth punch list

> **Status**: snapshot from the first GKE deploy attempt 2026-04-27.
> Captures every gap discovered while bringing up
> `spire-tower-dev.awellhealth.com` on Autopilot in `awell-turtle-pond`
> against `awell/awell` DoltHub.
>
> Filed as a doc rather than beads because the bead graph is in flux —
> laptop `awell` tower vs cluster `spi` tower vs DoltHub `awell/awell`
> have not been reconciled, and bead IDs are still drifting. Once the
> cluster-as-truth migration completes, the items below should be
> opened directly in the cluster's `spi` DB.
>
> Read top-to-bottom or jump via the table. Each issue carries:
> *Symptom*, *Root cause*, *Fix*, *Workaround in place* (if any).

---

## 0. Direction recap

- Cluster Dolt + gateway are the canonical bead-graph host.
- DoltHub is seed-only on first install + (optional) one-way archive.
- Laptops/desktops attach via the gateway and never `dolt push` to DoltHub.
- GCS is the disaster-recovery substrate via `backup.*` block.
- Multi-archmage support exists at the gateway with per-bearer auth.

---

## 1. Punch list summary

Severity is impact on shipping a usable v1.0 desktop UX over a GKE
gateway.

| # | Sev | Where | One-liner |
|---|---|---|---|
| 1 | P1 | Helm chart — `gateway-ingress.yaml` | Ingress needs the legacy `kubernetes.io/ingress.class` annotation for GKE Autopilot |
| 2 | P1 | Helm chart — `gateway-ingress.yaml` | Ingress needs an explicit `defaultBackend` (cluster default-backend NEG can be missing) |
| 3 | P1 | Helm chart — `_helpers.tpl` | `clickhouse.enabled=true` does not plumb `SPIRE_OLAP_BACKEND` / `SPIRE_CLICKHOUSE_DSN` env to pods |
| 4 | P1 | Code — `pkg/gateway/gateway.go` | `metricsReaderFactory` uses legacy `olap.Open(path)`, ignores `SPIRE_OLAP_BACKEND` entirely |
| 5 | P1 | Code — `pkg/board/roster.go` | `RosterFromClusterRegistry` shells out to `kubectl` from inside the gateway pod (no `kubectl` in image) |
| 6 | P1 | Code — `pkg/agent/registry.go` (suspected) | Gateway pod accumulates `[dolt]` zombie subprocesses; some endpoints (`/board`, `/beads`, `/messages`) hang under load |
| 7 | P2 | Helm chart — `gateway-deployment.yaml` | Gateway pod start is 3–5 min cold because emptyDir forces a full `attach-cluster` bootstrap on every restart |
| 8 | P2 | Code — `cmd/spire/main.go` build | `var version = "dev"` is the build default, so `decideVersionAction` runs migrations on every restart |
| 9 | P2 | Helm chart — `dolt-backup-cronjob.yaml` (or image build) | Dolt-backup CronJob's pod is in `ImagePullBackOff` |
| 10 | P2 | Code — `pkg/store/dispatch.go` | Gateway-mode tower with same prefix as a local tower silently loses CWD-resolution; `spire tower use` doesn't override |
| 11 | P2 | Code — `cmd/spire/tower_cluster.go` | `attach-cluster` does not refuse on prefix collision with an existing local tower (related to #10) |
| 12 | P2 | Code — `pkg/dolt/sync` (or wherever `spire push` paths live) | Nothing prevents a laptop in gateway-mode from running `spire push`/auto-push to DoltHub |
| 13 | P2 | Code — `cmd/spire/tower.go` (`towerListCmd`) | `spire tower list` shows `kind=dolthub remote=local` for gateway-mode towers |
| 14 | P1 | Code — gateway `/api/v1/*` mutation handlers | Per-call archmage attribution missing on bead create / comment / message paths (partially threaded for `from`) |
| 15 | P3 | Helm chart — `values.yaml` | `backup.enabled` defaults to false; for cluster-as-truth it should be true (or already is — verify) |

Items 1–3 are the blockers that prevent a clean install from completing
without manual `kubectl` patches. Items 4–6 are why the desktop doesn't
work end-to-end against a clean install.

---

## 2. Per-issue detail

### 1. Ingress chart needs legacy `kubernetes.io/ingress.class` annotation for GKE Autopilot

**Symptom**: `helm install` with `gateway.ingress.enabled=true,
className=gce` produces an Ingress that the GLBC controller never
processes. After 25+ min: no `Address`, no forwarding rules, no NEGs,
no events on the Ingress at all.

**Root cause**: GKE Autopilot does not ship pre-populated `IngressClass`
CRs (`kubectl get ingressclass` returns `No resources found`). Without
an `IngressClass` matching `gce`, the controller falls back to the
legacy annotation `kubernetes.io/ingress.class: gce` — which the chart
does not emit. The chart only sets `spec.ingressClassName`.

**Fix**: In `helm/spire/templates/gateway-ingress.yaml`, when
`ingress.className` is set, emit it as both `spec.ingressClassName` AND
the legacy `kubernetes.io/ingress.class` annotation. Setting both is
harmless on classic GKE and required on Autopilot.

**Workaround in place**:

```bash
kubectl annotate ingress spire-gateway -n spire \
  kubernetes.io/ingress.class=gce --overwrite
```

Pinned via `gateway.ingress.annotations` in
`k8s/values.gke.local.yaml` so subsequent `helm upgrade` calls keep
it.

---

### 2. Ingress needs explicit `defaultBackend`

**Symptom**: After workaround #1, the GLBC controller starts syncing
but errors with:

> `Error syncing to GCP: googleapi: Error 404: The resource
> 'projects/awell-turtle-pond/zones/us-east5-a/networkEndpointGroups/k8s1-…-kube-system-default-http-backend-80-…'
> was not found, notFound`

The cluster's `kube-system/default-http-backend` NEG is missing in the
zone the controller expects. Fresh Autopilot cluster — likely a zone
mismatch from the default-backend pod scheduling. Cannot fix
kube-system from the user side: Autopilot blocks
`kubectl delete pod -n kube-system` and `kubectl rollout restart
deployment -n kube-system` with `GKE Warden authz [denied by
managed-namespaces-limitation]`.

**Root cause**: Our Ingress has no `spec.defaultBackend`, so the
controller falls through to the cluster's default. When that default
backend's NEG isn't where the controller expects, the entire Ingress
fails to sync — even paths that have explicit backends.

**Fix**: In `helm/spire/templates/gateway-ingress.yaml`, set
`spec.defaultBackend` pointing at the gateway service:

```yaml
spec:
  defaultBackend:
    service:
      name: {{ $serviceName }}
      port:
        name: api
```

Either unconditional or behind
`gateway.ingress.defaultBackend.enabled` (default true). Removes the
dependency on cluster default-backend.

**Workaround in place**:

```bash
kubectl patch ingress spire-gateway -n spire --type=merge -p \
  '{"spec":{"defaultBackend":{"service":{"name":"spire-gateway","port":{"name":"api"}}}}}'
```

This sticks across helm upgrades (the chart doesn't manage
`defaultBackend`), but the chart should set it natively.

---

### 3. `clickhouse.enabled=true` does not plumb env vars

**Symptom**: After flipping `clickhouse.enabled: true` and helm
upgrading, the ClickHouse StatefulSet renders correctly and is
reachable from the gateway pod. But the pod's environment shows
neither `SPIRE_OLAP_BACKEND` nor `SPIRE_CLICKHOUSE_DSN`:

```bash
$ kubectl exec -n spire deploy/spire-gateway -c gateway -- env | grep -iE 'olap|clickhouse'
(empty)
```

**Root cause**: `values.yaml`'s comment claims `clickhouse.enabled=true`
makes "the steward/operator Deployments receive `SPIRE_OLAP_BACKEND` +
`SPIRE_CLICKHOUSE_DSN`". The comment is aspirational — the templates
do not implement it. The `spire.stewardCommonEnv` helper has no OLAP
entries.

**Fix**: In `helm/spire/templates/_helpers.tpl::spire.stewardCommonEnv`:

```yaml
{{- if .Values.clickhouse.enabled }}
- name: SPIRE_OLAP_BACKEND
  value: clickhouse
- name: SPIRE_CLICKHOUSE_DSN
  value: "clickhouse://default@spire-clickhouse.{{ .Values.namespace }}.svc:{{ .Values.clickhouse.ports.native }}/{{ .Values.clickhouse.database }}"
{{- end }}
```

All consumers (`steward`, `operator`, `gateway`) include the helper, so
plumbing happens once. Wizard pods built via `pkg/agent/pod_builder.go`
also need to inherit these — verify when chart fix lands.

**Workaround in place**:

```bash
kubectl set env deploy/spire-gateway deploy/spire-operator deploy/spire-steward -n spire \
  SPIRE_OLAP_BACKEND=clickhouse \
  SPIRE_CLICKHOUSE_DSN="clickhouse://default@spire-clickhouse.spire.svc:9000/spire"
```

Lost on next `helm upgrade`. Re-apply each time.

---

### 4. Gateway `/api/v1/metrics` uses legacy `olap.Open` — bypasses env entirely

**Symptom**: Even after #3 is worked around and the env vars are set on
the pod, `/api/v1/metrics` returns:

> `HTTP 503 — metrics: OLAP unavailable (olap open
> /root/.local/share/spire/spi/analytics.db: sql: unknown driver
> "duckdb" (forgotten import?))`

The handler ignores `SPIRE_OLAP_BACKEND=clickhouse` and tries DuckDB,
which isn't compiled in (CGO_ENABLED=0).

**Root cause**: `pkg/gateway/gateway.go:1036`:

```go
metricsReaderFactory = func() (report.Reader, func(), error) {
    ...
    adb, err := olap.Open(tc.OLAPPath())
    ...
}
```

`olap.Open(path)` is the legacy CGO-only DuckDB-only entry point. The
modern `olap.OpenStore(Config)` would consult `SPIRE_OLAP_BACKEND` and
route to ClickHouse. From `pkg/olap/factory.go`:

> "The legacy path-based entry point Open(path string) (*DB, error) is
> kept unchanged in db.go for backward compatibility with existing
> callers (cmd/spire/trace.go, cmd/spire/metrics.go, pkg/steward/
> daemon.go — all currently off-limits for this task)."

The gateway is on the same legacy path. The chart fix in #3 is
necessary but not sufficient; the gateway code must migrate too.

**Fix**: Migrate `metricsReaderFactory` to use `olap.OpenStore`:

```go
adb, err := olap.OpenStore(olap.Config{
    Backend: os.Getenv("SPIRE_OLAP_BACKEND"),
    DSN:     os.Getenv("SPIRE_CLICKHOUSE_DSN"),
    Path:    tc.OLAPPath(),
})
```

Same migration is needed in `cmd/spire/metrics.go`,
`pkg/steward/daemon.go`. `cmd/spire/trace.go` already reads the env
vars at line 963.

**Workaround**: None possible without code change. Spire Desktop's
metrics view shows "Failed to load metrics — 503" indefinitely.

---

### 5. Gateway `/api/v1/roster` shells out to `kubectl` from inside the pod

**Symptom**: `/api/v1/roster` returns:

> `HTTP 500 {"error":"exit status 1"}`

Spire Desktop's roster view shows no agents.

**Root cause**: `pkg/board/roster.go:141`
`RosterFromClusterRegistry`:

```go
cmd := exec.Command("kubectl", "get", "pods", "-n", "spire",
    "-l", "spire.awell.io/managed=true",
    "-o", "json")
out, err := cmd.Output()
```

The gateway image (Dockerfile.steward) does not include `kubectl` —
no reason to. `exec.Command` fails with "exit status 1" because the
binary isn't on `$PATH`. The function's docstring assumes a laptop
caller in cluster-native mode (laptop-attached). It was never adapted
to run as the cluster-side gateway.

**Fix**: Use the Go k8s client with in-cluster config instead of
shelling out. `controller-runtime/pkg/client` or `client-go`. RBAC
already permits the operator KSA to list pods + WizardGuilds; the
gateway shares that KSA. No `kubectl` binary needed.

Same pattern check needed for any other `exec.Command("kubectl"`
sites — `grep -rn 'exec.Command("kubectl"' pkg/`.

**Workaround**: None without code change. Roster view stays empty.

---

### 6. Gateway pod accumulates `[dolt]` zombies; `/board`, `/beads`, `/messages` hang under load

**Symptom**: After a few requests, dolt-touching read endpoints
(`/board`, `/beads`, `/messages`) hang and eventually time out.
`/api/v1/tower` (no SQL) keeps responding 200. Inside the pod
`ps -ef` shows multiple `[dolt]` zombie processes accumulating.

**Root cause** (suspected, not fully proven): the gateway shells out to
`bd` or `dolt` subprocesses for some store operations and those
subprocess exits aren't reaped by `spire serve` running as PID 1.
Eventually subprocess accumulation either hits a fork limit or
correlates with a connection-pool exhaustion against the dolt server
(possibly tower/registry refresh paths, which may also shell out).

**Fix**: Two angles need investigation:

1. Find the call sites that produce `[dolt]` subprocesses inside
   `spire serve` and either reap them (`os/exec.Cmd.Wait` everywhere)
   or replace with in-process calls (the dolt server is reachable via
   in-process SQL client — no subprocess needed).
2. Make sure the gateway's HTTP handlers use bounded contexts for
   their store calls and don't share connection pools with anything
   that can deadlock.

**Workaround**: Restart the gateway pod (`kubectl rollout restart
deploy/spire-gateway`) — buys time before zombies pile up again.

---

### 7. Gateway pod startup is 3–5 min cold

**Symptom**: Every new gateway pod (helm upgrade, `kubectl set env`,
autoscale) takes 3–5 min to become Ready. Init container hangs at
`[attach-cluster] ensuring Spire custom bead types`. During that
window the desktop reconnects through the cutover.

**Root cause**: `helm/spire/templates/gateway-deployment.yaml` mounts
`/data` as `emptyDir`:

```yaml
volumes:
  - name: data
    emptyDir: {}
```

Each new pod inherits a blank `/data` and re-runs the full
`attach-cluster` bootstrap. The "ensuring custom bead types" step fans
out into many `bd --sandbox config get types.custom` subprocess calls,
each with its own startup cost. Steward and operator are fast on
restart because their PVC keeps `/data/<db>/.beads` populated.

**Fix options**:

1. Make the gateway lazy-attach on first request rather than at pod
   start (the gateway only needs `TowerConfig`, not the full
   `.beads/` workspace). Most of the bootstrap is dead weight for the
   gateway use case.
2. Cache the custom-bead-types check via a ConfigMap-mounted file
   the steward writes once.
3. Reduce subprocess fan-out — batch the custom-types check into one
   query.

(1) is the architectural fix: the gateway is a thin HTTP shim, not a
tower.

**Workaround**: None — wait for pod to come up.

---

### 8. `var version = "dev"` makes every restart run migrations

**Symptom**: Even with PVC-backed steward/operator pods, every
restart runs the full migration ritual (custom bead types, schema
checks, etc.). Combines with #7 to make every gateway restart slow
*every* time.

**Root cause**: `cmd/spire/main.go:11` declares
`var version = "dev"`. Our Dockerfiles build with plain `go build` (no
`-ldflags "-X main.version=..."`). So the binary always thinks it's a
dev build. Per `cmd/spire/version_check.go::decideVersionAction`:

> *"Dev/empty binary version — can't do anything meaningful. Run
> migrations (safe, idempotent) but don't write garbage to config."*

Versioned binaries with `stored == binary` get
`skipMigrations: true` and restart fast.

**Fix**: Add `ARG SPIRE_VERSION=dev` to the Dockerfiles and pass it
via `-ldflags`:

```dockerfile
ARG SPIRE_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-X main.version=${SPIRE_VERSION}" \
    -o /out/spire ./cmd/spire/
```

Then `docker build --build-arg SPIRE_VERSION=v0.48.0 ...`.

**Workaround in place** (chart-level only, not committed): Dockerfiles
patched locally to accept the build arg; images rebuilt with
`SPIRE_VERSION=v0.48.0` and pushed under the `latest` tag. Subsequent
restarts will skip migrations once the version is stored. Commit the
Dockerfile change.

---

### 9. Dolt-backup CronJob's pod is in `ImagePullBackOff`

**Symptom**: `kubectl get pods -n spire`:

```
spire-dolt-backup-29620980-fqg9b   0/1   ImagePullBackOff
```

Backup CronJob never runs.

**Root cause**: Not investigated yet. Likely the CronJob's container
image references a tag/repo that isn't resolvable in this cluster
(possibly an arch mismatch as we hit on the spire images, or a
ghcr.io tag that doesn't exist for `Chart.AppVersion=0.0.0`).

**Fix**: `kubectl describe pod spire-dolt-backup-... -n spire` for
the actual error, then either rebuild the backup image into Artifact
Registry or override the chart's image reference.

**Workaround**: None — backups silently not running.

---

### 10. Gateway-mode tower loses to same-prefix local tower in CWD resolution

**Symptom**: After `spire tower attach-cluster --tower spi --url ... --token ...`,
mutations from the desktop CWD (anywhere under a same-prefix local
tower's repo) go to the local tower not the gateway. Even
`spire tower use spi` does not override CWD-resolution.

**Root cause**: `pkg/store/dispatch.go::isGatewayMode()` reads
`activeTower()` → `config.ResolveTowerConfig()`. CWD-mapped tower
wins; gateway-mode is only entered if the CWD-resolved tower IS the
gateway one.

**Fix**: pairs with #11 — refusing the parallel tower at attach time
makes the routing bug impossible to hit. Optionally also: a
session-scoped `--tower` flag that wins over CWD for explicit
invocations.

**Workaround**: Run mutations from `/tmp` (no CWD-mapped tower).

---

### 11. `attach-cluster` does not refuse on prefix collision

**Symptom**: Pairs with #10. Today, `spire tower attach-cluster
--tower spi ...` silently creates a parallel tower when the laptop
already has a tower with `prefix=spi`. The user has no signal that
anything is wrong — until mutations go to the wrong tower.

**Root cause**: `cmd/spire/tower_cluster.go::cmdTowerAttachClusterGateway`
does not check for prefix collision before saving config.

**Fix**: Detect collision, return a clear error with a copy-pasteable
remove command:

> `a tower with prefix "spi" already exists locally ("awell"); remove
> it first with 'spire tower remove awell' then re-attach`

No conversion path needed. Local cache is disposable.

**Workaround**: `spire tower remove <existing>` before `attach-cluster`.

---

### 12. Gateway-mode tower has no `spire push` gate

**Symptom**: A laptop in gateway mode can still run `spire push`
against the underlying local DB — direct DoltHub mutation that
contradicts cluster-as-truth.

**Root cause**: `cmd/spire/push.go` (and any auto-push paths in
`pkg/dolt`, `pkg/store`, `pkg/syncer`) does not consult the active
tower's `Mode` before pushing.

**Fix**: At the entry of any push path:

```go
if t, ok := isGatewayMode(); ok {
    return fmt.Errorf("tower %q is gateway-mode; mutations route through %s — direct push is disabled", t.Name, t.URL)
}
```

Plus a sweep of `pkg/dolt`, `pkg/store`, `pkg/syncer` for indirect
push paths to apply the same gate.

**Workaround**: Operational discipline — do not run `spire push` on
gateway-mode towers.

---

### 13. `spire tower list` mislabels gateway-mode towers

**Symptom**:

```
~ spi    spi    (no database)    dolthub    local
```

`kind=dolthub`, `remote=local` — but the tower is `TowerModeGateway`
with a URL and a keychain-stashed bearer token.

**Root cause**: `cmd/spire/tower.go::towerListCmd` formatter doesn't
know about `TowerModeGateway`.

**Fix**: Teach the formatter. Suggested:
`kind=gateway, remote=<URL>`.

**Workaround**: Identify gateway-mode towers by reading
`~/.spire/towers/<name>.json` and looking for `"Mode": "gateway"`.

---

### 14. Per-call archmage attribution missing on mutation handlers

**Symptom**: Every desktop mutation lands as either the cluster's
static `tower.Archmage.Name` or unattributed. With multiple desktops
attached, all mutations look like the same person.

**Root cause**: `pkg/gateway/gateway.go` mutation handlers — bead
create, comment, message — do not read the calling archmage from
either request body or headers. Partial: `sendMessage` does honour
`X-Archmage-Name` via `IdentityFromContext` and overrides `body.From`.
The rest do not.

**Fix**: Thread `IdentityFromContext` through every mutation handler:

- `createBead`: pass to `store.CreateBead.Author` (already a field;
  matches the message handler)
- `postBeadComment`: pass to `commentsAddFunc` (currently hardcodes
  the author via `Actor()`)
- Anywhere else mutations land

Same gatewayclient side: `cmd/spire/...` callers must send the local
archmage identity as `X-Archmage-Name` / `X-Archmage-Email` headers
(or per-archmage bearer tokens — also acceptable).

**Workaround in place**: Set a single static archmage on the cluster
tower (`spire tower set --archmage-name JB --archmage-email
jbb@jbb.dev` in *both* steward and gateway pods — gateway uses
emptyDir so it forgets on restart). Until per-call attribution lands,
all mutations show as `JB`.

---

### 15. `backup.enabled` default

Status: per recent commits (`2ba6f57 feat(spi-6az9s7): default
backup.enabled=true with fail-fast validation`), this should already
be defaulted to true with a `spire.validateBackup` helper. Verify on
the current chart and remove from the punch list if confirmed.

---

## 3. Runtime workarounds applied to the GKE cluster

The cluster at `spire-tower-dev.awellhealth.com` has these manual
patches on top of the helm release. They will be lost on a fresh
install or full reinstall — recreate them or land the chart fixes
above.

```bash
# --- Ingress legacy class annotation (#1) ---
kubectl annotate ingress spire-gateway -n spire \
  kubernetes.io/ingress.class=gce --overwrite
# (also pinned via gateway.ingress.annotations in values.gke.local.yaml)

# --- Ingress explicit defaultBackend (#2) ---
kubectl patch ingress spire-gateway -n spire --type=merge -p \
  '{"spec":{"defaultBackend":{"service":{"name":"spire-gateway","port":{"name":"api"}}}}}'

# --- OLAP env vars (#3) ---
kubectl set env deploy/spire-gateway deploy/spire-operator deploy/spire-steward -n spire \
  SPIRE_OLAP_BACKEND=clickhouse \
  SPIRE_CLICKHOUSE_DSN=clickhouse://default@spire-clickhouse.spire.svc:9000/spire

# --- Always-pull images (compensates for repeated rebuilds at the same tag) ---
helm upgrade ... --set images.steward.pullPolicy=Always --set images.agent.pullPolicy=Always

# --- Static archmage (#14) ---
kubectl exec -n spire deploy/spire-steward -c steward -- \
  spire tower set --tower spi --archmage-name "JB" --archmage-email "jbb@jbb.dev"
kubectl exec -n spire deploy/spire-gateway -c gateway -- \
  spire tower set --tower spi --archmage-name "JB" --archmage-email "jbb@jbb.dev"
```

---

## 4. Cluster state at end of session

- Cluster: `spire` (Autopilot) in `us-east5`, project `awell-turtle-pond`
- Namespace: `spire`, Helm release `spire`
- Tower: `spi`, deploy_mode `cluster-native`, archmage `JB`, dolt_url `awell/awell`, **8077 beads** seeded from DoltHub
- Repos table: `oo`, `spd`, `spi` registered
- Images: `us-east5-docker.pkg.dev/awell-turtle-pond/spire/spire-{steward,agent}:latest` (built with `SPIRE_VERSION=v0.48.0` baked via ldflags)
- ClickHouse StatefulSet: enabled, Running, reachable from gateway pod
- Ingress IP: `35.244.252.196`, ManagedCertificate Active for `spire-tower-dev.awellhealth.com`
- Gateway bearer token: stashed in `k8s/values.gke.local.yaml` (gitignored, `*.local.*`)
- Workload Identity GSA binding: `spire-awell@awell-turtle-pond.iam.gserviceaccount.com` ↔ `spire/spire-steward` (KSA-side annotation pending; chart uses `spire-operator` KSA, not `spire-steward` — needs reconcile)
- Outstanding: dolt-backup CronJob image pull, slow gateway init on every restart

## 5. Architectural questions raised

1. **Should the gateway carry a `.beads/` workspace at all?** It only
   serves `/api/v1/*` HTTP, talking to dolt. `attach-cluster` for the
   gateway pod feels like dead weight inherited from the steward
   pattern.

2. **Should `RosterFromClusterRegistry` live in the gateway?** It
   shells out to kubectl — an obvious hint it was designed for laptops,
   not in-pod use. Either move it to a sidecar, an operator endpoint,
   or rewrite to use the in-cluster k8s client.

3. **What's the migration plan from laptop awell tower → cluster as
   truth?** Today the laptop's awell tower is bidirectional with
   DoltHub; once we cutover, the cluster needs to be the single
   writer. Bead IDs collide between laptop awell and DoltHub awell/awell
   when wizards mint IDs concurrently — see the cluster's
   `spi-i7k1ag` having a different title than the local
   `spi-i7k1ag`.

4. **GCS Workload Identity vs SA-JSON dual gate.** The chart still
   requires `gcp.serviceAccountJson` non-empty when
   `bundleStore.backend=gcs`, even on Workload Identity clusters.
   Loosening this gate to allow `gcp.secretName` alone is partially
   landed (per the chart's `spire.validateBackup` helper accepting
   either) but `templates/steward.yaml` may still require the JSON.
   Verify and document.

## 6. Next steps (suggested order)

Land in this order — each unblocks the next:

1. Dockerfile change for `SPIRE_VERSION` ldflags (#8) — already
   patched locally, just commit
2. Chart fix #1 + #2 (Ingress annotation + defaultBackend)
3. Chart fix #3 (OLAP env plumbing)
4. Code fix #4 (gateway metrics → OpenStore)
5. Code fix #5 (roster → in-cluster k8s client)
6. Code fix #6 (gateway subprocess audit)
7. Code fix #14 (archmage threading) — the desktop UX is degraded but
   not broken until this lands
8. Code fix #11 + #12 (attach-cluster refuse + push gate)
9. Cosmetic #13 (tower list display)
10. Verify #15 (backup default)
11. Investigate #9 (dolt-backup ImagePullBackOff)

After 1–6 land and ship in a versioned image, a clean `helm install`
on a fresh GKE Autopilot cluster should produce a working desktop
experience without manual `kubectl` patches.

---

Last updated: 2026-04-27 (first GKE deploy attempt)
