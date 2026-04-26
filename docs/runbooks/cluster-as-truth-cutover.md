# Cluster-as-truth cutover runbook

Operator procedure for migrating an **existing Awell tower** from the
DoltHub-backed/bidirectional-sync topology to **cluster-as-truth
gateway-mode**, where the cluster Dolt database is the single writer
and laptops attach through the gateway over HTTPS.

**Scope.** Applies to existing Awell towers being moved onto
gateway-mode. Does **not** apply to local-native deployments (see
[VISION-LOCAL.md](../VISION-LOCAL.md)) or to attached-reserved
deployments (see [VISION-ATTACHED.md](../VISION-ATTACHED.md)). For the
cluster install itself, see
[../cluster-install.md](../cluster-install.md); this runbook assumes
the cluster is already up and reachable.

**Single-writer invariant.** After cutover, the cluster Dolt database
is the only writer for the tower. Laptops mutate the graph only
through the gateway HTTPS API; direct Dolt push/pull/sync from
laptops is rejected at the CLI. DoltHub becomes archive-only (one-way
or disabled); no laptop or cluster path retains DoltHub write
credentials.

---

## Pre-flight invariants

Before starting, the operator must confirm:

- **Cluster gateway is reachable and healthy.** Confirm the gateway
  responds to a health probe and serves `/api/v1/tower`. The exact
  endpoint and credentials are documented in
  [../cluster-install.md](../cluster-install.md) — section 7
  ("Verify"); use that as the authoritative healthcheck source rather
  than inlining commands here.
- **GCS backup is configured and a recent backup exists for the
  target tower.** This is the canonical rollback path. Verify per the
  GCS restore drill at [./gcs-restore.md](./gcs-restore.md); the
  restore drill is delivered by sibling task spi-6az9s7's runbook
  section. Do **not** start cutover unless a recent successful backup
  is on hand.
- **Operator has cluster admin credentials and DoltHub admin
  credentials.** Cluster admin is needed to scale the syncer and edit
  secrets. DoltHub admin is needed to rotate the PAT and downgrade
  collaborator permissions. Both must be in hand before Step 1.

---

## Step 1 — Inventory legacy writers

Enumerate every host or process that can still write to the tower's
Dolt graph or to DoltHub.

```bash
# On every laptop/server known to attach to the legacy tower:
spire repo list
spire status
```

`spire repo list` shows registered repos and their resolved tower.
`spire status` reports running daemons/stewards on the host.

Also list:

- **DoltHub collaborators with write access** on the canonical tower
  repo. This is a manual DoltHub UI step: open the repo's Settings →
  Collaborators page and record every account whose role is anything
  other than read-only.
- **Cluster-side syncer pods or cronjobs** writing to DoltHub. Use
  the cluster install runbook ([../cluster-install.md](../cluster-install.md))
  to identify the syncer resource names; do not hand-roll selectors
  here.

Record the inventory before continuing. Every entry produced by this
step needs a corresponding action in Steps 2 through 5.

---

## Step 2 — Quiesce local writers

For every laptop or server identified in Step 1:

```bash
spire down       # stop the daemon (Dolt server keeps running)
spire shutdown   # fully stop daemon + Dolt
spire status     # verify nothing is left running
```

Confirm `spire status` reports no daemon, no steward, and no Dolt
process on the host. **Note the freeze timestamp.** From this moment
on, no laptop should produce any new commit on the legacy tower; any
later commit is evidence that a writer was missed in Step 1 and the
inventory must be redone.

---

## Step 3 — Stop cluster syncers

Disable every cluster-side bidirectional syncer that mutates DoltHub.
Concrete actions (scaling Deployments to 0, suspending CronJobs,
removing Helm values flags) live in the cluster install runbook —
follow [../cluster-install.md](../cluster-install.md) for the exact
resource names and namespaces.

Confirm:

- No syncer pod is running.
- No syncer CronJob is unsuspended.
- The cluster Dolt server continues to run.

If any syncer remains running, the dual-writer condition the cutover
exists to prevent is still active. Do not proceed.

---

## Step 4 — Revoke DoltHub write credentials

Once writers are quiesced, remove every credential that could
re-introduce a second writer.

- **Rotate or remove the DoltHub PAT used by laptops.** Known
  locations to clean on each laptop:
  - `~/.dolt/config.json` (the `user.creds` and remote stanzas).
  - OS keychain entries created by `dolt login` or by the laptop's
    Spire install.
  - `DOLTHUB_TOKEN` / `DOLT_REMOTE_PASSWORD` shell env vars in
    `~/.zshrc`, `~/.bashrc`, or process supervisors.
- **Rotate or remove the DoltHub PAT mounted into cluster syncer
  secrets.** The exact Secret names are the ones referenced by the
  syncer manifests in [../cluster-install.md](../cluster-install.md).
- **Downgrade DoltHub collaborators from write to read** on the
  canonical tower repo. Leave a single explicit archive-writer
  identity only if a one-way archive path is enabled (see Step 8).

After this step, the invariant must hold: **no credential anywhere
(laptops or cluster) can write to DoltHub**, except an explicit
archive-only identity if archive is configured.

---

## Step 5 — Local config cleanup before attach

For each laptop being onboarded to gateway-mode, prepare the local
config so `spire tower attach-cluster` will accept the gateway tower.

```bash
spire repo list   # re-confirm what's registered
spire tower list  # identify same-prefix or same-name local towers
```

For any tower whose `HubPrefix` or `Name` collides with the cluster
tower:

```bash
spire tower remove <name>
# or, for a registered repo whose prefix collides:
spire repo remove <prefix>
```

Then walk every repo CWD that previously resolved to the legacy
tower:

```bash
# Inside the repo:
spire status
```

Confirm `resolveBeadsDir()` no longer routes to the old `.beads/`
(the status output's tower / data-dir lines should be empty or
already point to the cluster gateway after attach).

This step is the operator-side preparation for the collision guard
landed by sibling task spi-6f6ky8: `attach-cluster` will refuse to
proceed if any local tower or instance can resolve the same prefix
or name as the cluster tower. Cleaning up here lets attach succeed.

---

## Step 6 — Attach laptops through gateway

For each laptop, run gateway-mode attach:

```bash
spire tower attach-cluster \
    --tower <cluster-tower-name> \
    --url   <https://gateway-host> \
    --token <archmage-scoped-bearer>
```

Flags are the existing `attach-cluster` flags — do not invent
alternatives. `--name <alias>` is optional and overrides the local
alias (defaults to `--tower`). The bearer token must be the
archmage-scoped token issued for this operator; identity propagation
on each call is delivered by sibling task spi-n6fk2h.

Verify:

```bash
spire status                    # tower should report gateway mode
bd list --json | head           # reads succeed against the cluster
```

If `spire status` does not report gateway mode, or `bd list` returns
errors, do not continue to Step 7 — the attach did not land cleanly.

---

## Step 7 — Validate writes go through gateway

From a freshly-attached laptop, run an end-to-end validation:

```bash
# 1. File a validation bead through the gateway.
spire file "cutover validation $(date -u +%FT%TZ)" -t chore -p 4
# Capture the returned bead ID, e.g. spi-xxxxxx
```

Confirm the bead landed on the cluster:

```bash
# Read it back from the laptop (this round-trips through the gateway).
bd show <id>
```

Then, on the cluster side, confirm the bead is present with the
**archmage identity** of the operator who filed it (this exercises
sibling task spi-n6fk2h's identity propagation through the gateway).
Use the cluster-side `bd show <id>` path documented in
[../cluster-install.md](../cluster-install.md) — typically a
`kubectl exec` into the dolt or steward pod.

**Negative test.** With every DoltHub write credential removed
(Step 4), attempt direct local Dolt sync from the laptop:

- If a direct-sync verb (`spire push`, `spire pull`, `spire sync`)
  is still wired in this version, it must **fail closed** with the
  gateway-mode rejection error from sibling task spi-hr3tcv:
  `tower X is gateway-mode; mutations route through <url>; direct
  Dolt sync is disabled`.
- If the verb has already been removed, document that result.

The intended outcome is the same either way: there is no path from a
gateway-mode laptop to a direct DoltHub or local-Dolt write.

Close the validation bead once round-trip and identity attribution
are confirmed:

```bash
bd update <id> --status closed
```

---

## Step 8 — Rollback

The only safe rollback path is a **GCS-restore** to a fresh Dolt
instance. There is no in-place "undo" of the cutover that does not
risk recreating dual-writer state.

- Restore the most recent GCS backup into a blank Dolt PVC, following
  [./gcs-restore.md](./gcs-restore.md). Do not duplicate that
  procedure here.
- If a one-way archive to DoltHub is desired post-rollback, enable it
  as an explicit archive-only identity. **Never** restore
  bidirectional sync.

> **Warning — never re-grant DoltHub write credentials to laptops as
> a rollback shortcut.** Doing so recreates the exact dual-writer
> condition this cutover exists to eliminate. If the cluster gateway
> is unavailable, the correct response is to restore from GCS, not to
> reopen DoltHub writes.

---

## Sign-off checklist

Before declaring the cutover complete, confirm every box:

- [ ] All laptops show **gateway mode** in `spire status`.
- [ ] No DoltHub PAT with **write scope** exists on any laptop or
      cluster secret.
- [ ] Cluster syncer is **scaled to 0** (Deployments) or
      **suspended** (CronJobs); no syncer pod is running.
- [ ] **GCS backup** ran successfully **after** the cutover and the
      backup object is present in the configured bucket.
- [ ] **Validation bead** round-tripped from laptop → gateway →
      cluster Dolt with the **correct archmage identity** recorded as
      author.
- [ ] **Negative direct-sync test** failed closed (or the direct-sync
      verb is no longer wired).
