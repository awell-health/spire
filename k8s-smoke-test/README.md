# k8s-smoke-test

Manual smoke-test scaffolding for Spire features that depend on
out-of-cluster resources. The YAMLs here are not applied by CI — they
exist so a human can round-trip a feature end-to-end against a real
cluster (usually minikube).

## `gcs-example.yaml` — GCS bundlestore backend (spi-iyykmx)

Round-trips an apprentice→wizard bundle through a real GCS bucket from
a minikube cluster. The ADC path inside the pod is
`/var/secrets/gcp/gcp-sa-key.json`, set via
`GOOGLE_APPLICATION_CREDENTIALS`.

### 0. Prerequisites

- A pre-existing GCS bucket. The store does NOT create one. If you need
  one:

      gsutil mb gs://my-tower-bundles

- A GCP service account with `roles/storage.objectAdmin` on that bucket
  (or `roles/storage.objectUser` if your org's IAM has the finer-grained
  role enabled).

### 1. Create the k8s secret from a GSA key

```sh
gcloud iam service-accounts keys create ./gcp-sa-key.json \
  --iam-account=spire-bundles@<PROJECT>.iam.gserviceaccount.com

kubectl create secret generic spire-gcp-sa \
  --from-file=gcp-sa-key.json=./gcp-sa-key.json \
  --namespace=spire

# Scrub the local copy once the secret is in the cluster.
rm ./gcp-sa-key.json
```

### 2. Point the tower config at gcs

In the tower's config:

```json
{
  "bundle_store": {
    "backend": "gcs",
    "gcs": {
      "bucket": "my-tower-bundles",
      "prefix": "smoke-test"
    }
  }
}
```

### 3. Apply the example pods

```sh
kubectl apply -f k8s-smoke-test/gcs-example.yaml -n spire
```

The two sidecar pods (`apprentice-gcs-smoke`, `wizard-gcs-smoke`) mount
the secret at `/var/secrets/gcp/` and export
`GOOGLE_APPLICATION_CREDENTIALS`. Their entrypoints are placeholders —
replace them with whatever submit / fetch invocation you want to
exercise.

### 4. Verify round-trip

After a submit:

```sh
gsutil ls gs://my-tower-bundles/smoke-test/
```

Should list a `<beadID>/<attemptID>-<idx>.bundle` object.

### Alternative: cluster-local fake-gcs-server

For credential-free smoke tests, deploy `fsouza/fake-gcs-server` as a
cluster-local service and point `storage.NewClient` at it via
`STORAGE_EMULATOR_HOST`. This keeps the test hermetic but does not
exercise the real ADC plumbing — useful for CI, less useful for
validating Workload Identity paths.
