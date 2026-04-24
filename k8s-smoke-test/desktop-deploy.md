# Spire Desktop — cluster deploy runbook

Steps to deploy the `spire serve` gateway into the `spire-smoke` cluster
and wire the desktop app to it. Written overnight 2026-04-23 for the
archmage to follow in the morning.

## Prerequisites

- `minikube -p spire` running (verified green before handoff)
- `kubectl` context `spire` active (`kubectl config current-context`)
- `spire-smoke` namespace existing
- `helm` installed
- `spire-steward:dev` and `spire-agent:dev` images already in the
  minikube docker daemon (ship baseline)

## 1 — Deploy gateway to cluster

The Helm chart now has a `gateway` block (values.yaml §HTTP API gateway).
`values.smoke.yaml` sets `gateway.enabled: true` with a NodePort Service
on port 30030.

```bash
cd /Users/jb/awell/spire

# Build updated steward image (includes gateway code)
eval $(minikube -p spire docker-env)
docker build -f Dockerfile.steward -t spire-steward:dev .

# Helm upgrade — gateway container appears in the steward pod, Service added
helm upgrade spire-smoke helm/spire -n spire-smoke \
  -f k8s/values.smoke.yaml \
  --set-file gcp.serviceAccountJson=$HOME/Downloads/spire-gcs-sa.json \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<keyId>.jwk

# (If dolthub creds aren't needed because the smoke tower doesn't sync,
# pass a dummy: --set-file dolthub.credsKeyValue=/dev/null)

# Wait for rollout
kubectl -n spire-smoke rollout status deploy/spire-steward --timeout=120s

# Verify 3 containers now running
kubectl -n spire-smoke get pod -l app.kubernetes.io/name=spire-steward \
  -o jsonpath='{.items[0].spec.containers[*].name}'
# Expect: steward gateway sidecar  (or steward sidecar gateway)
```

## 2 — Verify gateway endpoint in-cluster

```bash
# Service check
kubectl -n spire-smoke get svc spire-gateway
# Expect: NodePort, 3030:30030/TCP,8080:xxxxx/TCP

# Port-forward (alternative to NodePort)
kubectl -n spire-smoke port-forward svc/spire-gateway 3030:3030 &
PF_PID=$!

# Hit the API
curl -sS http://localhost:3030/healthz
curl -sS http://localhost:3030/api/v1/beads | jq '. | length'
curl -sS http://localhost:3030/api/v1/roster | jq .

# Via NodePort (minikube IP)
MINIKUBE_IP=$(minikube -p spire ip)
curl -sS http://$MINIKUBE_IP:30030/healthz

kill $PF_PID 2>/dev/null
```

## 3 — Point desktop app at the cluster

**Option A — Local `spire serve` (default, matches dev flow):**

The Electron main process spawns `spire serve --api-port 3030` against
the laptop's own dolt server (port 3307). No cluster involvement.
Useful for working with the `spi` tower which runs locally.

**Option B — Cluster gateway (for `smk` tower):**

Set an env var when launching the desktop app:

```bash
cd /Users/jb/awell/spire-desktop
SPIRE_API_URL=http://$(minikube -p spire ip):30030 \
  yarn dev
```

Or in a terminal that port-forwards:

```bash
# Terminal 1
kubectl -n spire-smoke port-forward svc/spire-gateway 3030:3030

# Terminal 2
SPIRE_API_URL=http://localhost:3030 yarn dev
```

The Electron main process should detect `SPIRE_API_URL` and skip
spawning its own `spire serve` when set. (Check electron/main.ts —
the initial scaffold has the spawn logic; the overnight conversion
subagent was asked to honour SPIRE_API_URL.)

## 4 — Rollback

```bash
cd /Users/jb/awell/spire
git checkout helm/spire/values.yaml \
              helm/spire/templates/steward.yaml \
              helm/spire/templates/gateway.yaml \
              helm/spire/templates/_helpers.tpl \
              k8s/values.smoke.yaml

# If the gateway template already rendered and you want to drop the
# Service + gateway container without full rollback:
helm upgrade spire-smoke helm/spire -n spire-smoke \
  -f k8s/values.smoke.yaml \
  --set gateway.enabled=false \
  --set-file gcp.serviceAccountJson=$HOME/Downloads/spire-gcs-sa.json
```

## Known edges

1. **Gateway dataDir** — the container sets `BEADS_DIR` to the same
   per-database path as the steward (via `spire.stewardCommonEnv`).
   `store.Ensure()` picks that up. If you see "no tower configured",
   the init container probably didn't complete — check steward pod
   logs for the `tower-attach` initContainer.

2. **Dev mode auth** — `apiToken: ""` means no Bearer check. Fine on
   minikube behind the laptop's firewall. Before ever exposing the
   NodePort outside, set `gateway.apiToken` via --set or --set-file
   and rebuild the desktop client to send the header.

3. **Pod lifecycle** — the gateway shares the steward pod. Any roll
   (image bump, env change) restarts all three containers together.
   Acceptable for smoke; future work is a separate Deployment so the
   desktop doesn't get disconnected when the steward restarts.

4. **Board handler** — `/api/v1/board` returns `result.Columns.ToJSON(nil)`,
   grouping beads by pipeline phase (design/plan/implement/review/merge),
   NOT by status. If the desktop wants a status-grouped board, it needs
   to group client-side from `/api/v1/beads`.

5. **No owner field** — `store.Bead` omits Owner from the JSON
   projection used by `/api/v1/beads` and `/api/v1/board`. Agent
   assignment has to be derived from the `agent:<name>` label.
   `/api/v1/roster` has `bead_id` per agent — invert that map on the
   client to get "which agent owns this bead".
