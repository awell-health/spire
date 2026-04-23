# k8s-smoke-test

Manual smoke-test scaffolding for Spire features that depend on
out-of-cluster resources. The YAMLs here are not applied by CI — they
exist so a human can round-trip a feature end-to-end against a real
cluster (usually minikube).

## `gcs-example.yaml` — GCS bundlestore backend (spi-iyykmx)

Round-trips an apprentice→wizard bundle through a real GCS bucket from
a minikube cluster. The ADC path inside the pod is
`<gcp.mountPath>/<gcp.keyName>` (defaults: `/var/secrets/gcp/key.json`),
set via `GOOGLE_APPLICATION_CREDENTIALS`.

### 0. Prerequisites

- A pre-existing GCS bucket. The store does NOT create one. If you need
  one:

      gsutil mb gs://spire-awell

- A GCP service account with `roles/storage.objectAdmin` on that bucket
  (or `roles/storage.objectUser` if your org's IAM has the finer-grained
  role enabled).

### 1. Install the chart with the shared GCP credential

The chart provisions the GCP Secret (`<release>-gcp-sa`) from
`gcp.serviceAccountJson`. Pass the SA key file via `--set-file` so helm
base64-encodes it into the Secret at install time — no out-of-band
`kubectl create secret` needed.

```sh
gcloud iam service-accounts keys create ./spire-gcs-sa.json \
  --iam-account=spire-bundles@<PROJECT>.iam.gserviceaccount.com

helm install spire-smoke helm/spire \
  -n spire-smoke --create-namespace \
  -f k8s/values.smoke.yaml \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<keyId>.jwk \
  --set-file gcp.serviceAccountJson=./spire-gcs-sa.json

# Scrub the local copy once the secret is in the cluster.
rm ./spire-gcs-sa.json
```

`k8s/values.smoke.yaml` already sets `bundleStore.backend=gcs` plus
`bundleStore.gcs.bucket=spire-awell` and `bundleStore.gcs.prefix=smoke`.
The same `gcp.*` top-level block is consumed by the dolt backup sync
CronJob (when `backup.enabled=true`) — one shared Secret, two features.

### 2. Verify the tower config picked up the GCS selection

`spire tower attach-cluster` (run by the steward init container) writes
the `bundle_store` block into the tower config on the shared PVC:

```sh
kubectl -n spire-smoke exec deploy/spire-steward -c steward -- \
  cat /data/spire-config/towers/<database>.json | jq .bundle_store
```

Expect `{"backend":"gcs","gcs":{"bucket":"spire-awell","prefix":"smoke"}}`.

### 3. Verify the steward pod has the mount + env

```sh
kubectl -n spire-smoke exec deploy/spire-steward -c steward -- \
  printenv GOOGLE_APPLICATION_CREDENTIALS
# /var/secrets/gcp/key.json

kubectl -n spire-smoke exec deploy/spire-steward -c steward -- \
  ls -l /var/secrets/gcp/key.json
```

### 4. Dispatch a wizard and verify apprentice → GCS round-trip

File a bead and let the steward claim it; the operator then spawns a
wizard pod with the same `<release>-gcp-sa` Secret mounted. After an
apprentice submits:

```sh
gsutil ls gs://spire-awell/smoke/
# → <beadID>/<attemptID>-<idx>.bundle
```

### Alternative: cluster-local fake-gcs-server

For credential-free smoke tests, deploy `fsouza/fake-gcs-server` as a
cluster-local service and point `storage.NewClient` at it via
`STORAGE_EMULATOR_HOST`. This keeps the test hermetic but does not
exercise the real ADC plumbing — useful for CI, less useful for
validating Workload Identity paths.
