# pkg/bundlestore

Storage for **git-bundle artifacts** produced by apprentices and consumed
by wizards during the submit / fetch flow.

## Why this package exists

An apprentice pod does not have push credentials for the project's git
remote — and even if it did, we don't want every apprentice's half-baked
commits hitting the shared remote. Instead:

1. The apprentice runs `git bundle create` on its feature branch.
2. It uploads the bundle via `BundleStore.Put`.
3. It writes the returned handle onto the task bead as a pointer.
4. The wizard reads the handle from the bead and calls `BundleStore.Get`
   to fetch the bundle into its staging workspace.

Dolt carries only the pointer (the opaque `Key`). The artifact lives in
whichever backend the tower is configured with.

## What this package owns

- The `BundleStore` interface: `Put`, `Get`, `Delete`, `List`, `Stat`.
- The `local` filesystem backend (`LocalStore`).
- The `Janitor`: a periodic retention sweep that deletes bundles whose
  task beads have been closed+sealed past the grace window, or which
  have orphaned files with no corresponding bead.
- Path-hygiene validation on `BeadID` / `AttemptID` (the triple-keyed
  uniqueness guarantee below).

## What this package does NOT own

- The apprentice `submit` command (spi-1fugj).
- The wizard fetch / merge flow (spi-rfee2).
- The bead-level metadata that records a bundle handle — that lives on
  whichever bead schema records apprentice completion.
- Role / RBAC concepts. The store deliberately has no `Role` field;
  authorization is a bead-level concern.
- The `pvc`, `http`, `gcs`, and `s3` backends. These are intentional
  follow-ups; the interface is shaped so they can be added without
  internal leaks (no `*os.File` in the public API).

## Interface contract

```go
type BundleHandle struct {
    BeadID string // task bead (not attempt bead)
    Key    string // store-opaque; callers MUST NOT parse
}

type PutRequest struct {
    BeadID        string // required
    AttemptID     string // required; disambiguates cleric-retries
    ApprenticeIdx int    // 0 for single apprentice; >0 for fan-out
}

type BundleStore interface {
    Put(ctx, req, bundle) (BundleHandle, error)
    Get(ctx, handle) (io.ReadCloser, error)
    Delete(ctx, handle) error
    List(ctx) ([]BundleHandle, error)
    Stat(ctx, handle) (BundleInfo, error)
}
```

### Triple-keyed uniqueness

The `(BeadID, AttemptID, ApprenticeIdx)` triple must uniquely identify a
submission. `Put` is **reject-on-duplicate**: two `Put`s with the same
triple return `ErrDuplicate`. Callers that want replace-on-submit (e.g.
an apprentice resubmitting after a local-build fix) must `Delete` the
old handle first.

Rationale: silent overwrite masks the "two apprentices collided on the
same slot" bug. Surface it at the storage layer so the caller has to
decide explicitly.

### Size limits

`Config.MaxBytes` caps individual bundle size (default 10 MB). `Put`
enforces the limit via `io.LimitReader(r, max+1)`; if the caller
supplies more than `max` bytes, `Put` returns `ErrTooLarge` and leaves
no partial artifact behind. Don't trust caller-declared sizes — the
limit is enforced on what actually streams in.

### Atomic writes

The `local` backend writes to a tmpfile in the target directory, fsyncs,
and renames into place. A crashed `Put` leaves only a `*.tmp` file that
`List` / `Stat` skip. The janitor's orphan path eventually reclaims them
if they get truly stranded.

### Path hygiene

`BeadID` and `AttemptID` are baked into filenames. Both must match
`^[a-z0-9-]{1,64}$` — anything with `/`, `..`, or null bytes is rejected
with `ErrInvalidRequest` before touching the filesystem. `Get` / `Delete`
also scrub handle keys for path-traversal attempts (handles round-trip
through bead metadata, which is ultimately user-influenced data).

## The Janitor

The janitor is the **correctness net** for bundle storage. In-process
`Delete` after merge is the optimization; the janitor guarantees that
crashes, timeouts, and orphaned state eventually get reclaimed.

Retention rules (5-minute default cadence):

| Condition                                   | Action                   |
|---------------------------------------------|--------------------------|
| bead closed + `sealed_at` set + age > 30m   | delete                   |
| bead not found + file mtime > 7d            | delete                   |
| anything else                               | keep                     |

The janitor takes a `BeadLookup` interface — it does **not** import
`pkg/store` directly. The composition layer (wherever the tower bootstrap
lives) wires in the real adapter. This keeps `pkg/bundlestore` free of a
dependency that could later produce an import cycle.

### `sealed_at` caveat

`sealed_at` populates once the wizard-seal bead (spi-rfee2) lands. Until
then, every closed bead has `SealedAt == time.Time{}`, so the sealed
branch is intentional dead code. The janitor does NOT fabricate a "seal
time" from `closed_at` or `updated_at` — we'd start reaping bundles that
downstream flows haven't had a chance to fetch yet. The orphan path
handles genuinely stranded bundles in the meantime.

## Backend matrix

| Backend | Status      | Bead           | Notes |
|---------|-------------|----------------|-------|
| local   | ships today | spi-8qsmr      | Default for dev / single-tower mode. |
| gcs     | ships today | spi-iyykmx     | Multi-pod k8s deployments; ADC-only auth. |
| pvc     | follow-up   | tbd            | RWX PVC mounted into apprentice + wizard pods. Needs RWX provisioner in minikube. |
| http    | follow-up   | tbd            | Namespaced one-pod HTTP object server. Works on RWO storage classes. |
| s3      | follow-up   | tbd            | Parallel to gcs. |

The interface is designed so additional backends are drop-in: no
`*os.File` leaks, no filesystem-specific types in the public surface.

## Backends

### `local`

Filesystem-backed. Default for single-tower dev. Writes to
`$XDG_DATA_HOME/spire/bundles` unless `local_root` overrides.

```json
{
  "bundle_store": {
    "backend": "local",
    "local_root": "/var/lib/spire/bundles"
  }
}
```

### `gcs`

Google Cloud Storage-backed. Use this for multi-pod k8s deployments
where apprentice and wizard pods cannot share a PVC.

```json
{
  "bundle_store": {
    "backend": "gcs",
    "gcs": {
      "bucket": "my-tower-bundles",
      "prefix": "spire/bundles"
    }
  }
}
```

- **bucket** — required. The bucket MUST exist; the store does not
  create it. A missing bucket surfaces at tower startup with a
  `gsutil mb gs://<bucket>` hint.
- **prefix** — optional. Empty stores objects at the bucket root.
  Backend-internal: the prefix never appears in `BundleHandle.Key`.

#### Authentication — ADC only

The `gcs` backend uses Application Default Credentials. No credential
fields live in the tower config. The Go storage client picks up
credentials from exactly one of these sources, in order:

1. **GKE Workload Identity** — bind the wizard / apprentice KSA to a
   GSA with `roles/storage.objectAdmin` on the bucket. This is the
   supported production path; no secrets on disk.
2. **GOOGLE_APPLICATION_CREDENTIALS** — set on the pod to a JSON key
   file mounted as a secret. This is the documented minikube path.
3. **gcloud ADC** — `gcloud auth application-default login` locally.
   Used when running the tower directly on a developer workstation.

The storage client already does exponential backoff on 5xx / retriable
errors; the store does not add its own retry layer.

#### Operation mapping

| Op     | GCS primitive                                                 |
|--------|---------------------------------------------------------------|
| Put    | `Writer` with `Conditions.DoesNotExist=true` (412 → `ErrDuplicate`); `io.LimitReader(max+1)` for size enforcement |
| Get    | `Object.NewReader(ctx)`; `ErrObjectNotExist` → `ErrNotFound`  |
| Delete | `Object.Delete(ctx)`; `ErrObjectNotExist` → nil (idempotent)  |
| List   | `Bucket.Objects(ctx, &Query{Prefix})` with internal pagination; prefix stripped from each object name |
| Stat   | `Object.Attrs(ctx)` → `BundleInfo{Size, ModTime: Updated}`    |

#### Minikube wiring

For a real install, enable the chart's shared GCP credential:

```sh
helm install spire-smoke helm/spire \
  -n spire-smoke --create-namespace \
  -f k8s/values.smoke.yaml \
  --set-file gcp.serviceAccountJson=./spire-gcs-sa.json \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<keyId>.jwk
```

The chart renders a `<release>-gcp-sa` Secret from
`gcp.serviceAccountJson` and mounts it at `.Values.gcp.mountPath`
(default `/var/secrets/gcp`) with key `.Values.gcp.keyName` (default
`key.json`) on every pod that consumes GCS (steward + operator-built
wizard + apprentice). The same Secret is used by the dolt backup sync
CronJob when `backup.enabled=true` — one credential, two features.

`k8s-smoke-test/gcs-example.yaml` keeps the minimal hand-rolled shape
for ad-hoc testing outside the chart (pods mount a `spire-gcp-sa`
Secret at `/var/secrets/gcp/`). Both paths arrive at the same ADC env:
`GOOGLE_APPLICATION_CREDENTIALS=<mountPath>/<keyName>`.

## Expected operational ceiling

`List` must be cheap enough to run on the janitor cadence. The `local`
backend's filesystem walk is fine at 10s-of-thousands of bundles. When
the cloud backends land, they must implement `List` with internal
pagination — the store handle in the return value still surfaces as a
single slice to the caller, but the implementation should not issue a
single unbounded request to the underlying service.

## Config

```json
{
  "bundle_store": {
    "backend": "local",
    "local_root": "",
    "gcs": {
      "bucket": "",
      "prefix": ""
    },
    "max_bytes": 10485760,
    "janitor_interval": "5m"
  }
}
```

- `backend` — one of `"local"`, `"gcs"`. Empty defaults to `"local"`.
  An unknown backend fails tower startup with a diagnostic error.
- `local_root` — filesystem root for the local backend. Empty defaults
  to `$XDG_DATA_HOME/spire/bundles` (or `~/.local/share/spire/bundles`).
- `gcs.bucket` — required when `backend = "gcs"`. Must exist.
- `gcs.prefix` — optional object-name prefix. Empty stores at the root.
- `max_bytes` — bundle size cap in bytes. 0 defaults to 10 MiB.
- `janitor_interval` — duration string. 0 defaults to 5 minutes.
