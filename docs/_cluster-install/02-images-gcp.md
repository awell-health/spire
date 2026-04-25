## 3. Push Spire images

The Spire helm chart pulls three container images: `spire-steward`,
`spire-agent`, and `dolt-sql-server`. Operators have two paths for the
Spire-owned images: pull from the Awell Health public registry, or
build and push their own to a private Artifact Registry repo. The
upstream `dolthub/dolt-sql-server` image is pulled from Docker Hub by
default in either path; mirror it into your own AR if you want pulls to
stay inside the VPC.

### (a) Pull from the Awell Health public registry (default)

Pinned image tags are resolved by the chart from `.Chart.AppVersion` at
`helm install` time, so you don't normally need to override anything in
`values.gke.yaml` for the public-registry path. The image references
the chart consumes are:

```
us-central1-docker.pkg.dev/awell-health/spire/spire-steward:${TAG}
us-central1-docker.pkg.dev/awell-health/spire/spire-agent:${TAG}
docker.io/dolthub/dolt-sql-server:<chart default>
```

`${TAG}` matches the chart `appVersion` (e.g. `v0.44.0`). To list what
is available in the public registry:

```bash
gcloud artifacts docker images list \
  us-central1-docker.pkg.dev/awell-health/spire
```

If you want the chart to pull the public images verbatim, leave
`values.gke.yaml` `images.steward.repository` /
`images.agent.repository` at their committed defaults but replace the
literal `PROJECT_ID` token with `awell-health` — the chart's region and
repo path (`us-central1-docker.pkg.dev/<project>/spire`) already match.

> TODO: verify against E1/E2 deliverable. The release pipeline today
> publishes to `ghcr.io/awell-health/spire-{steward,agent,builder}`
> (see `.github/workflows/release.yml`); the
> `us-central1-docker.pkg.dev/awell-health/spire/...` path above is the
> intended public Artifact Registry mirror per the cluster-install
> epic. Confirm the public AR is provisioned and populated before
> recommending this path as the default.

### (b) Build and push your own images

Use the project's CI workflow to publish images into your own Artifact
Registry repo on every git tag push. The committed workflow is
`.github/workflows/release-gar.yml` — it authenticates via Workload
Identity Federation, configures `gcloud auth configure-docker`, and
runs `docker/build-push-action` for `Dockerfile.steward` and
`Dockerfile.agent`. Set the following GitHub-level variables/secrets
in your fork before pushing a tag:

- `vars.GCP_REGION` — e.g. `us-central1`
- `vars.GCP_PROJECT` — your `${PROJECT_ID}`
- `secrets.GAR_WIF_PROVIDER` — Workload Identity Federation provider
- `secrets.GAR_SERVICE_ACCOUNT` — the publisher GSA email

> TODO: verify against E1/E2 deliverable. The committed workflow tags
> images as `us-central1-docker.pkg.dev/${PROJECT_ID}/spire/steward:${TAG}`
> (i.e. image name `steward`, not `spire-steward`). The chart's
> `values.gke.yaml` defaults reference `spire/spire-steward` /
> `spire/spire-agent`. Reconcile the naming in E1 before this section
> is canonical — either update the workflow output names or the chart
> `images.*.repository` defaults so a fresh `helm install` picks the
> CI-built images without per-environment `--set` overrides.

Before either CI or manual pushes can succeed, the Artifact Registry
repo must exist in your project:

```bash
gcloud artifacts repositories create spire \
  --repository-format=docker \
  --location=us-central1 \
  --description="Spire container images"
```

#### Manual fallback (build and push from your laptop)

Useful for one-off testing or when CI is unavailable. Run from the
repo root:

```bash
# Authenticate docker against Artifact Registry (one-time per machine)
gcloud auth configure-docker us-central1-docker.pkg.dev

# Build the two Spire images locally
TAG=v0.0.0-dev
docker build -f Dockerfile.steward -t spire-steward:${TAG} .
docker build -f Dockerfile.agent   -t spire-agent:${TAG}   .

# Tag for your project's Artifact Registry
docker tag spire-steward:${TAG} \
  us-central1-docker.pkg.dev/${PROJECT_ID}/spire/spire-steward:${TAG}
docker tag spire-agent:${TAG} \
  us-central1-docker.pkg.dev/${PROJECT_ID}/spire/spire-agent:${TAG}

# Push
docker push us-central1-docker.pkg.dev/${PROJECT_ID}/spire/spire-steward:${TAG}
docker push us-central1-docker.pkg.dev/${PROJECT_ID}/spire/spire-agent:${TAG}
```

The canonical chart-consumed reference shape is
`us-central1-docker.pkg.dev/${PROJECT_ID}/spire/spire:${TAG}` — the
two distinct steward/agent images above each follow that pattern with
their own image name. When you install the chart, override
`images.steward.repository` and `images.agent.repository` in
`values.gke.yaml` (or via `--set`) to point at your project's repo if
you used path (b).

## 4. Provision GCP resources

The tower needs four pieces of GCP infrastructure before `helm install`
will succeed: a target namespace, a GCS bucket for the BundleStore, a
Google Service Account (GSA) for Workload Identity, and the IAM
bindings that connect the GSA to the in-cluster Kubernetes
ServiceAccount (KSA) and to the bucket.

### Namespace

```bash
kubectl create namespace spire
```

The chart defaults to namespace `spire` (overridable via
`.Values.namespace`). Keep this name unless you have a hard reason to
diverge — it is referenced as a literal in several agent-side
breadcrumbs and in the runbook's later sections.

### GCS bucket for BundleStore

The steward writes review/merge bundles to GCS. Create a Standard-class
bucket in the same region as your cluster (avoid Nearline/Coldline —
bundles are short-lived and would incur early-deletion fees):

```bash
gsutil mb -l us-central1 -c STANDARD gs://${PROJECT_ID}-spire-bundles
# or, equivalently:
gcloud storage buckets create gs://${PROJECT_ID}-spire-bundles \
  --location=us-central1 \
  --default-storage-class=STANDARD
```

The bucket name is wired into `values.gke.yaml` at
`bundleStore.gcs.bucket`; the committed default is
`PROJECT_ID-spire-bundles` (literal token `PROJECT_ID`). Override with
`--set bundleStore.gcs.bucket=${PROJECT_ID}-spire-bundles` or edit your
own overlay.

### Google Service Account

```bash
gcloud iam service-accounts create spire-tower \
  --display-name="Spire tower runtime"
```

This produces the GSA `spire-tower@${PROJECT_ID}.iam.gserviceaccount.com`.
The tower's runtime KSA in-cluster is `spire/spire-tower`; the rest of
this section binds the two together.

### IAM bindings

Grant the GSA write access to the BundleStore bucket:

```bash
gcloud storage buckets add-iam-policy-binding \
  gs://${PROJECT_ID}-spire-bundles \
  --member="serviceAccount:spire-tower@${PROJECT_ID}.iam.gserviceaccount.com" \
  --role="roles/storage.objectAdmin"
```

If you went with path (b) above and the chart pulls images from your
own project's Artifact Registry, also grant pull access. Skip this
binding if you stayed on the Awell Health public registry — pulls from
that registry don't require project-level IAM in your project.

```bash
gcloud projects add-iam-policy-binding ${PROJECT_ID} \
  --member="serviceAccount:spire-tower@${PROJECT_ID}.iam.gserviceaccount.com" \
  --role="roles/artifactregistry.reader"
```

### Workload Identity binding (KSA ↔ GSA)

Bind the in-cluster KSA `spire/spire-tower` to the GSA via the
Workload Identity user role. This is what lets the tower pod present
itself to GCS as `spire-tower@...` without a static key file:

```bash
gcloud iam service-accounts add-iam-policy-binding \
  spire-tower@${PROJECT_ID}.iam.gserviceaccount.com \
  --role="roles/iam.workloadIdentityUser" \
  --member="serviceAccount:${PROJECT_ID}.svc.id.goog[spire/spire-tower]"
```

The KSA itself must carry the `iam.gke.io/gcp-service-account`
annotation pointing at the GSA email. The intent is that the helm
chart sets this annotation automatically when the GSA is provided in
`values.gke.yaml` (see the `gcp.*` block — e.g. `gcp.workloadIdentity.gsa`
or equivalent), so operators don't need to `kubectl annotate` by hand.

> TODO: verify against E1/E2 deliverable. As of this writing the
> committed chart in `helm/spire/` does NOT auto-annotate the KSA: the
> steward/operator workloads run under KSA `spire-operator` (see
> `helm/spire/templates/rbac.yaml`), and the `values.gke.yaml` header
> documents annotation as a manual `kubectl annotate serviceaccount`
> step. Until E1 lands the auto-annotation values key (and renames the
> KSA to `spire-tower` if that becomes canonical), apply the
> annotation explicitly:
>
> ```bash
> kubectl annotate serviceaccount -n spire spire-tower \
>   iam.gke.io/gcp-service-account=spire-tower@${PROJECT_ID}.iam.gserviceaccount.com
> ```
>
> The chart also currently enforces a non-empty
> `gcp.serviceAccountJson` whenever `bundleStore.backend=gcs` (see
> `helm/spire/templates/steward.yaml`). Pass a placeholder value via
> `--set-file` until that gate is loosened to allow a pure-Workload-
> Identity path; runtime credential resolution still goes through
> Workload Identity when the KSA is annotated.

With the namespace, bucket, GSA, and bindings in place, you have the
GCP-side prerequisites for the helm install in section 5.
