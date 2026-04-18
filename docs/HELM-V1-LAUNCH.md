# Spire Helm v1 Launch Plan

This document is the step-by-step playbook for taking Spire from "chart
renders" to "end-to-end agent flow works, with backups and user
management, in your cluster." It's intended to be executable by a single
engineer or agent — each section has concrete commands and pass/fail
acceptance criteria.

**Related docs (read first if you are new):**
- [docs/HELM.md](HELM.md) — chart structure, values reference
- [docs/cluster-deployment.md](cluster-deployment.md) — background on
  the operator/steward architecture
- [docs/ARCHITECTURE.md](ARCHITECTURE.md) — data model, agent roles
- [docs/design/...](design/) — see the design bead `spi-yzcrn` for the
  Phase 1 pivot away from DoltHub-as-spine

## Architecture (one screen summary)

```
                        ┌───────────────────────────────┐
                        │ spire-dolt (StatefulSet)      │
                        │   :3306  mysql protocol       │
                        │   :50051 remotesapi endpoint  │
     laptop / CI ──────►│   PVC:  /var/lib/dolt/<db>    │──► GCS (nightly backup)
     (dolt remote       └───────────────────────────────┘
      add cluster …)           ▲             ▲
                                │ SQL        │ SQL
                 ┌──────────────┘             └───────────────┐
                 │                                            │
        ┌────────┴────────┐                         ┌─────────┴────────┐
        │ spire-steward    │                         │ spire-operator   │
        │ (Deployment)     │                         │ (Deployment)     │
        │   dispatch loop  │                         │   watches        │
        │   sidecar router │                         │   SpireAgent CRs │
        └──────────────────┘                         └─────────┬────────┘
                                                               ▼
                                                      ┌────────────────┐
                                                      │ agent pods     │
                                                      │ (wizard/       │
                                                      │  apprentice)   │
                                                      └────────────────┘
```

Phase 1 decision (see design bead `spi-yzcrn`): **the cluster's own dolt
server is the sync spine.** Laptops and CI reach it via `remotesapi`;
DoltHub is an optional archival target, not a hot-path dependency.

## Prerequisites

- A Kubernetes cluster you can admin (minikube for local, GKE/EKS/AKS
  for shared envs)
- `kubectl`, `helm` v3
- One of:
  - A DoltHub account with an existing tower repo (Use Case 1)
  - Or plans to create a fresh tower in the cluster (Use Case 2)
  - Or an existing laptop tower you want to migrate in (Use Case 3)
- A Dolt JWK credential for DoltHub HTTPS auth
  (from `~/.dolt/creds/<key-id>.jwk` after `dolt login`) — needed
  for Use Cases 1 and 3
- Optional: a GCS bucket + service-account JSON for backups

## Use Case 1 — Install against a pre-existing DoltHub tower (`awell/awell` style)

**You already have a DoltHub tower that other spire agents have been
writing to. The cluster joins as another participant.**

### Steps

1. **Decide release name and namespace.** For examples below:
   `release=spire-prod`, `namespace=spire-prod`.
2. **Prepare `values.local.yaml`** (gitignored, not checked in):
   ```yaml
   namespace: spire-prod
   createNamespace: false         # we pass --create-namespace on the cli
   dolthub:
     remoteUrl: awell/awell        # the pre-existing tower
     user: <dolthub-username>      # must match the JWK owner
     userName: <dolthub-username>  # user.name config, MUST match the account
     userEmail: <your-email>
     credsKeyId: <base32-key-id-from-creds-ls>
     # credsKeyValue is passed via --set-file on the install cmd
   anthropic:
     apiKey: "<sk-ant-api03-...>"  # steward-sidecar needs an API key
     oauthToken: "<sk-ant-oat01-...>"  # optional, agent pods can use this
   beads:
     prefix: spi                   # must match the prefix the tower was created with
     database: spi                 # local dolt db name (usually same as prefix)
   images:
     steward:
       repository: ghcr.io/awell-health/spire-steward
       tag: "v0.42.0"              # pin to a released version
       pullPolicy: IfNotPresent
     agent:
       repository: ghcr.io/awell-health/spire-agent
       tag: "v0.42.0"
   ```
3. **Create the target namespace and ensure the JWK file is on disk:**
   ```bash
   kubectl create namespace spire-prod
   ```
4. **Install:**
   ```bash
   helm install spire-prod helm/spire \
     --namespace spire-prod \
     -f values.local.yaml \
     --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<key-id>.jwk
   ```
5. **Wait for dolt + operator + steward pods ready** (clone from DoltHub
   takes a few minutes on first boot for a large tower):
   ```bash
   kubectl -n spire-prod rollout status statefulset/spire-dolt
   kubectl -n spire-prod rollout status deployment/spire-operator
   kubectl -n spire-prod rollout status deployment/spire-steward
   ```
6. **Provision the initial dolt user** (covered automatically by
   `spi-50788`'s post-install Job once merged; manual path below):
   ```bash
   kubectl -n spire-prod exec spire-dolt-0 -c dolt -- \
     dolt --host 127.0.0.1 --port 3306 --user root --no-tls -p "" \
     sql -q "CREATE USER IF NOT EXISTS 'dolt_remote'@'%' IDENTIFIED BY '<strong-password>'; GRANT ALL PRIVILEGES ON *.* TO 'dolt_remote'@'%'; FLUSH PRIVILEGES;"
   ```
7. **Verify the laptop can talk to the cluster** (port-forward for local
   clusters, Ingress for real ones):
   ```bash
   kubectl -n spire-prod port-forward svc/spire-dolt 50051:50051 &
   DOLT_REMOTE_PASSWORD=<strong-password> dolt clone --user=dolt_remote \
     http://localhost:50051/spi /tmp/cluster-spi
   ls /tmp/cluster-spi   # should have .dolt/
   ```

### Acceptance criteria (Use Case 1)

- [ ] `kubectl get pods -n spire-prod` shows `spire-dolt-0`,
      `spire-operator-*`, `spire-steward-*` all `Running` and ready
- [ ] Dolt-init logs contain `Imported credential: <key-public-id>`
      and show a successful clone of `awell/awell`
- [ ] `kubectl -n spire-prod exec spire-dolt-0 -- dolt --host 127.0.0.1 --port 3306 --user root --no-tls -p "" sql -q "USE spi; SELECT COUNT(*) FROM issues"` returns a row count > 0
- [ ] `dolt clone http://<remotesapi-host>:50051/spi` from an external
      machine succeeds with the provisioned user credentials
- [ ] Steward logs show `starting (backend=k8s, …)` with no repeated
      `store not initialized` or `SPIRE_AGENT_IMAGE env is required` errors
- [ ] Operator logs do not spam `store.GetSchedulableWork failed, skipping validation`

## Use Case 2 — Install with a fresh tower (new, not on DoltHub)

**You want a brand-new spire deployment from scratch. No pre-existing
data. The tower will be initialized in-cluster.**

A blank dolt database is not enough — spire needs:
- `project_id` and `prefix` in the `metadata` table
- The custom bead types (`attempt`, `design`, `message`, `recovery`,
  `review`, `step`) registered
- Empty `issues`, `dependencies`, `comments`, etc. tables created by
  `bd init`

### Two sub-paths

**2a. DoltHub-first (recommended for real orgs):**
  Create the tower on DoltHub first, then follow Use Case 1.
  ```bash
  # on your laptop
  spire tower create --name myorg --dolthub your-org/myorg --prefix my
  spire push        # push the fresh bootstrap to DoltHub
  ```
  Then install the chart per Use Case 1 with `dolthub.remoteUrl=your-org/myorg`.

**2b. Cluster-first (no DoltHub account needed):**
  Skip DoltHub entirely. The chart's `dolt-init` must run the full
  `spire tower create` equivalent in-cluster:
  - Initialize a dolt repo
  - Run `spire tower create` ritual (project_id generation, metadata
    population, custom bead types) — **currently NOT wired into the
    chart — TODO in a new bead**
  - Start sql-server with remotesapi
  - Laptop attaches via `spire tower attach http://<host>:50051/<db>`
    (requires bead `spi-ibq3g`)

### Gap for 2b

The `dolt-init` ConfigMap's init.sh has an "empty database" fallback
path that runs `dolt init`, but **does not invoke spire's
tower-creation ritual** (project_id, metadata.json, custom bead types).
Without that, the database is bd-incomplete and the steward will fail
on first query.

A new bead should be filed: *"Chart init.sh: wire spire tower-create
ritual for blank-tower install mode (Use Case 2b)."* Until then, Use
Case 2 requires going through DoltHub (path 2a).

### Acceptance criteria (Use Case 2)

- [ ] Same as Use Case 1, PLUS
- [ ] `USE <db>; SELECT value FROM metadata WHERE \`key\` = '_project_id'`
      returns a non-empty UUID
- [ ] `bd config get types.custom` returns the six required spire types
- [ ] `spire focus <any-bead-id>` on a freshly-filed bead does not error

## Use Case 3 — Install using a local laptop tower (e.g. `mlti`)

**You have a tower on your laptop you want to promote to being the
cluster's source of truth. The tower is small enough to move via a
single push.**

Two flavors:

**3a. Push your laptop tower to DoltHub, then install Use Case 1:**
  ```bash
  spire tower use mlti                # switch to the tower you want to promote
  spire tower set --dolthub your-org/mlti   # if not already pointing at a DoltHub remote
  spire push                          # push current state to DoltHub
  # then follow Use Case 1 with dolthub.remoteUrl=your-org/mlti
  ```
  This is the simplest path when you already have DoltHub.

**3b. Skip DoltHub — push laptop → cluster directly via remotesapi:**
  Install the chart with an empty dolt database (Use Case 2b path), then
  from your laptop:
  ```bash
  # Inside your laptop's mlti data dir
  cd $(dolt config --global --get "sqlserver.global.data_dir" 2>/dev/null || echo ~/.local/share/dolt)/mlti

  # Add the cluster as a dolt remote and push
  kubectl -n spire-prod port-forward svc/spire-dolt 50051:50051 &
  dolt remote add cluster http://localhost:50051/mlti
  DOLT_REMOTE_USER=dolt_remote DOLT_REMOTE_PASSWORD=<pwd> dolt push cluster main
  ```
  The cluster now has your mlti data. The laptop can continue to operate
  by `spire tower attach`-ing to the cluster's remotesapi (bead
  `spi-ibq3g` once merged).

### Acceptance criteria (Use Case 3)

- [ ] Before push: `SELECT COUNT(*) FROM mlti.issues;` on the cluster
      returns 0
- [ ] After push: the same query returns the same count the laptop had
- [ ] `spire board` on the laptop (after re-attaching to cluster)
      displays the same beads as before the migration
- [ ] Laptop can file a new bead, and the cluster steward sees it
      (log line: `ready: N beads` on the next cycle)

## User management

### Who talks to the dolt server, and how

| Caller | Auth | Where creds live |
|---|---|---|
| Cluster pods (steward, operator, agents) | MySQL root, no password (default) | nowhere — local-pod connection |
| External `dolt remote` clients (laptop, CI) | MySQL user+password via Basic auth | `--user=<u>` + `DOLT_REMOTE_PASSWORD` env |
| DoltHub archival push | JWK + `dolt creds import`/`use` | `<release>-dolthub-creds` Secret, mounted `/var/lib/dolt/.dolt/creds/<id>.jwk` |

### Initial user (the first thing operators do after `helm install`)

Provisioned by bead `spi-50788`'s post-install Job (in flight). The Job
reads `DOLT_REMOTE_USER` / `DOLT_REMOTE_PASSWORD` from the
`<release>-credentials` Secret (rendered from `values.yaml`'s
`dolthub.user` / `dolthub.password`) and runs:

```sql
CREATE USER IF NOT EXISTS '${REMOTE_USER}'@'%' IDENTIFIED BY '${REMOTE_PASSWORD}';
GRANT ALL PRIVILEGES ON *.* TO '${REMOTE_USER}'@'%';
FLUSH PRIVILEGES;
```

Idempotent — re-running the Job after a password change should re-apply
(or `ALTER USER` should be preferred for rotation).

### Additional users

No chart-level UI yet. Operators use one of:

1. **One-off SQL:**
   ```bash
   kubectl -n spire-prod exec spire-dolt-0 -c dolt -- \
     dolt --host 127.0.0.1 --port 3306 --user root --no-tls -p "" \
     sql -q "CREATE USER 'alice'@'%' IDENTIFIED BY 'pw'; GRANT ALL ON spi.* TO 'alice'@'%';"
   ```
2. **SQL file in a ConfigMap + ad-hoc Job** that mirrors the
   `spi-50788` pattern (future work: a values block like
   `dolthub.additionalUsers: [...]` that renders an idempotent Job).

### Rotating credentials

Dolt's `SET PASSWORD` is not yet supported (see
https://docs.dolthub.com/sql-reference/server/access-management).
Work around with `ALTER USER 'u'@'%' IDENTIFIED BY 'new'` or
`DROP USER` + `CREATE USER`.

## Backups (GCS, 30-day retention)

Covered by bead `spi-ra5em` (in flight). Expected shape:

- Chart renders a `spire-dolt-backup` CronJob that `spire tower attach`
  to the cluster dolt on a schedule, then runs
  `dolt backup add gcs gs://<bucket>/<db> && dolt backup sync gcs`.
- GCS lifecycle rule enforces 30-day retention on the bucket.
- Values:
  ```yaml
  backup:
    enabled: true
    schedule: "0 */6 * * *"    # every 6 hours
    remoteUrl: gs://my-spire-backups/spire-prod
    credentialsSecret: gcs-sa-key    # externally pre-created Secret with a key.json
  ```

### Acceptance criteria (backups)

- [ ] `kubectl -n spire-prod get cronjobs` shows `spire-dolt-backup`
- [ ] After one successful run, the GCS bucket contains a dolt-backup
      manifest and nbs chunks
- [ ] Bucket lifecycle policy shows 30-day delete rule

## Example pod — drive a bead end-to-end

Once the cluster is up, prove the full control loop works by running
one bead through the cluster.

```bash
# 1. Laptop: file a trivially small task bead against the cluster's tower
spire file "Smoke test: create file k8s-smoke.md saying 'hello from the cluster'" -t task -p 2
#   → prints e.g. spi-abc12

# 2. Set it ready and push to DoltHub (or direct to cluster via 3b path)
spire ready spi-abc12
spire push      # Use Case 1: bounces through DoltHub
#                 OR for Use Case 3b: `dolt push cluster main`

# 3. Cluster steward pulls / sees it on next cycle (≤ syncer.interval).
#    Watch for the SpireWorkload CR:
kubectl -n spire-prod get spireworkloads -w

# 4. Operator assigns to an agent pod:
kubectl -n spire-prod get pods -w | grep wizard

# 5. Wizard runs the task-default formula, creates a PR.
#    Agent log lives at the agent pod's stdout:
kubectl -n spire-prod logs -f <wizard-pod>
```

### Acceptance criteria (example pod)

- [ ] A freshly-filed bead transitions `open → ready → in_progress → closed`
      without manual intervention
- [ ] A `spire-agent` pod spawns, finishes, and is reaped within the
      steward's shutdown threshold (15m default)
- [ ] The work product (a PR or a closed bead with a commit reference)
      exists

## Overall v1 acceptance criteria (the go/no-go checklist)

- [ ] **Install:** chart installs cleanly on minikube with all three
      Use Cases above reaching their per-case acceptance criteria
- [ ] **Auth:** external `dolt clone/push/pull` against the cluster
      remotesapi succeeds with the provisioned user; root still works
      in-cluster for operator/steward
- [ ] **Backups:** one automated GCS backup has landed and is visible
      in the bucket; 30-day lifecycle rule is in place
- [ ] **End-to-end:** one smoke-test bead has made it all the way
      through laptop → cluster → agent pod → closed bead
- [ ] **Operability:**
  - No pod is in `CrashLoopBackOff` for more than 60 seconds after
    install
  - `kubectl -n <ns> logs` for every component contains no `ERROR`
    lines except known-benign startup messages
  - `helm uninstall` cleanly removes everything except PVCs (which is
    intentional — reinstall resumes from the data)
- [ ] **Docs:** this file + `docs/HELM.md` + `docs/cluster-deployment.md`
      are consistent; README links to this launch plan

## Known open work (block v1 until resolved)

| Bead | Scope | Why it blocks v1 |
|---|---|---|
| `spi-50788` | Auto-provision remotesapi SQL user via post-install Job | Manual SQL post-install is a regression from a "one-command install" goal |
| `spi-ra5em` | GCS backup CronJob with 30-day retention | Non-negotiable per Archmage |
| `spi-ibq3g` | Laptop `spire tower attach` supports cluster URLs (`http://host:50051/db`) | Without this, Use Case 1 and 3 can't close the laptop↔cluster loop |
| `spi-doylg` | Fix TOCTOU race in `pkg/steward/daemon_runner.go` Run() | Correctness bug in sync loop |
| `spi-2p022` | Tests for `pkg/steward/daemon_runner.go` | Concurrency-heavy code with zero coverage |
| `spi-ct6o8` | Tests for `pkg/gateway` | HTTP surface with zero coverage |
| (new) | Chart init.sh: wire `spire tower create` ritual for Use Case 2b | Blocks the "no DoltHub, fresh tower" path |
| (new) | Chart values for `additionalUsers: [...]` rendering SQL Job | Operators need a declarative path to add users |

## Running this plan

Each Use Case is independently testable. A good sequence for a single
engineer / agent:

1. Validate Use Case 1 (easiest — existing data, one helm install)
2. Validate Use Case 3a (push laptop → DoltHub, reuse Use Case 1 install)
3. Validate backups
4. Validate example pod flow
5. Validate Use Case 2a (fresh DoltHub tower)
6. File gaps for 2b / 3b / additional users as future-work beads

When all acceptance criteria above are checked, the chart is v1-ready.
