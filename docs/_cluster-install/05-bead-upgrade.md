## 10. First bead

This is the end-to-end smoke: file → claim → summon → close, all flowing through the cluster API rather than a local dolt instance. If every step here works, the tower is wired up correctly.

### 10.1 Register a repo

A bead has to belong to a repo. Register one with the cluster tower so we have somewhere to file work. From any directory:

```bash
spire repo add
```

Or point at a specific local clone with an explicit prefix:

```bash
spire repo add --prefix=demo /path/to/local/repo
```

Confirm the repo is registered against the cluster tower:

```bash
spire repo list
```

You should see your repo with prefix `demo` (or whatever you chose). Subsequent `spire file` calls from that working directory will issue IDs under that prefix.

### 10.2 File a task

```bash
spire file "Smoke: hello cluster" -t task -p 2
```

The CLI prints the issued ID — something like `demo-a3f8`. Bead IDs are the repo prefix plus a short random suffix; subtasks of an epic get hierarchical IDs (`demo-a3f8.1`).

View the bead to confirm it landed in the cluster's dolt:

```bash
bd show demo-a3f8
```

The `bd` commands run against the same dolt server the cluster tower writes to (because `spire repo add` configured this working directory to point at the cluster), so what you see here is the cluster's view, not a local copy.

### 10.3 Claim the bead

```bash
spire claim demo-a3f8
```

`spire claim` is atomic — it verifies the bead isn't closed or already claimed by another agent, then sets it to `in_progress`. A successful claim against the cluster tower means the gateway accepted the write and the executor's view of the bead is now `in_progress`.

### 10.4 Summon a wizard

```bash
spire summon demo-a3f8
```

This is where the cluster proves itself. `spire summon` enqueues an executor run for the bead; the executor running in-cluster picks it up, spawns an apprentice pod, runs the formula's phases, and writes results back through the gateway.

### 10.5 Watch progress

```bash
spire focus demo-a3f8
bd comments list demo-a3f8 --json
```

`spire focus` assembles full context: bead details, current step, related deps, recent comments, and messages. `bd comments list` is useful when you just want the raw timeline — the wizard, apprentice, and sage all post comments as they progress.

For deeper kubernetes-side visibility while the run is in flight:

```bash
kubectl -n spire get pods
kubectl -n spire logs -l app.kubernetes.io/name=spire-gateway --tail=100
```

You'll see the apprentice pod come up, run, and exit; the gateway logs will show the API calls landing.

### 10.6 Close the bead

When the wizard reports done (a `seal` comment will appear and the formula's terminal step will mark the bead ready to close):

```bash
bd update demo-a3f8 --status closed
```

### 10.7 Confirm round-trip

A clean round-trip means all of the following are true:

- `bd show demo-a3f8` reports `status: closed`
- The wizard's `seal` comment is visible in `bd comments list demo-a3f8 --json`
- The feature branch the apprentice produced has been merged into the cluster's working repo (check `git log` on the registered repo, or look at the branch in the remote you configured)
- Re-running `bd show demo-a3f8` against the cluster tower from a fresh directory still reflects `closed` — which proves the write went through the gateway, not just a local cache

If all four hold, file → claim → summon → close are flowing through the cluster API. The local dolt instance is uninvolved; the cluster is your tower.

---

## 11. Upgrade path

Upgrading the tower is a helm upgrade with a new image tag. The cluster does the rest: rolling deployments, persistent dolt state, durable BundleStore in GCS.

### 11.1 Pick the new image tag

You either bump the tag in `helm/spire/values.gke.yaml`:

```yaml
image:
  repository: us-central1-docker.pkg.dev/${PROJECT_ID}/spire/spire
  tag: ${NEW_TAG}
```

…or pass it on the command line for a one-shot upgrade without editing the values file:

```bash
helm upgrade --install spire helm/spire \
  -n spire \
  -f helm/spire/values.gke.yaml \
  --set image.tag=${NEW_TAG}
```

This is the same `helm upgrade --install` invocation as the first install (section 5); helm idempotently reconciles to the new tag.

### 11.2 Expected rollout

Helm patches the deployments; kubernetes performs a rolling update. You'll see:

- New `spire-gateway` pods come up alongside the old ones, then the old ones terminate
- The executor and other tower deployments roll the same way
- Spire Desktop briefly shows a "reconnecting" banner while the gateway pod cycles, then resumes

Persistent state (dolt's PVC, BundleStore in GCS) is unaffected by the rollout — only the image changes.

### 11.3 Verify post-upgrade

Wait for the rollout to finish:

```bash
kubectl -n spire rollout status deploy/spire-gateway
```

Then re-run the section 7 smoke checks to confirm the new version is healthy:

```bash
curl https://spire.example.com/healthz
curl -H "Authorization: Bearer $TOWER_TOKEN" https://spire.example.com/api/v1/tower
```

Both should return 200 with the expected payloads. If they don't, jump to section 12 (Troubleshooting).

### 11.4 Rollback

If the new version is bad, roll back to the previous helm revision:

```bash
helm history spire -n spire
helm rollback spire -n spire
```

`helm history` shows the revision list; `helm rollback` without a revision number reverts to the immediately previous one. This restores the prior image tag and any prior chart values; persistent state is again untouched.

### 11.5 Migrations

Dolt schema migrations run automatically on the new pod's startup — there is no separate migration step to invoke. The tower process applies any pending schema changes against its dolt server before accepting traffic. If you want to confirm migrations ran cleanly, watch the steward/gateway logs during rollout:

```bash
kubectl -n spire logs -l app.kubernetes.io/name=spire-gateway --tail=200
```

Look for migration log lines on startup; readiness flips after they complete.

### 11.6 Backup expectations

Two pieces of state need to survive an upgrade:

- **BundleStore in GCS** (`${PROJECT_ID}-spire-bundles`) — durable by design. GCS handles the redundancy; nothing to back up at the cluster layer.
- **Dolt state** — lives on the tower's persistent volume (PVC mounted into the dolt pod). It survives pod restarts and rolling upgrades, but it is *not* backed up off-cluster by this chart in v1.

Periodic snapshots and disaster-recovery for dolt are a non-goal for v1 of this runbook; see the follow-on epic for backup/restore tooling. For now, treat the cluster's PVC as the single source of truth and rely on the cluster's own storage class durability guarantees.
