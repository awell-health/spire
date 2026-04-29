# Cluster Install Runbook

Fresh GCP project to a working Spire tower with Spire Desktop attached, end-to-end. Follow these steps in order; each section assumes the previous succeeded.

**Audience:** an operator (engineer, SRE) standing up a Spire tower on GKE for the first time.

**Time estimate:** 60-90 minutes for first run.

## Cluster-as-truth (read first)

This runbook installs a **cluster-native** tower. The cluster-hosted
Dolt database accessed through the gateway is the canonical bead-graph
host. DoltHub serves as seed-only on first install and as a one-way
archive; it is not an active writable mirror. Desktop/laptop clients
attach via the gateway and route mutations through `/api/v1/*` over
HTTP. GCS backup is the required disaster-recovery path; the operator
runbook for restoring a tower from GCS lives at
[runbooks/gcs-restore.md](runbooks/gcs-restore.md).

Spire has three deployment modes: **local-native** (single-machine,
local filesystem), **cluster-native** (multi-user cluster,
gateway-fronted Dolt), and **attached-reserved** (reserved; not
implemented). Within cluster-native, individual clients attach in
**gateway mode** — this is `TowerConfig.Mode` / client routing, not a
fourth `DeploymentMode`. See [deployment-modes.md](deployment-modes.md)
for the server/client matrix.

## Table of contents

1. [Prerequisites](#1-prerequisites)
2. [Create the GKE cluster](#2-create-the-gke-cluster)
3. [Push Spire images](#3-push-spire-images)
4. [Provision GCP resources](#4-provision-gcp-resources)
5. [First helm install](#5-first-helm-install)
6. [Set up DNS and TLS](#6-set-up-dns-and-tls)
7. [Verify](#7-verify)
8. [Attach the CLI](#8-attach-the-cli)
9. [Attach the Desktop](#9-attach-the-desktop)
10. [First bead](#10-first-bead)
10b. [Log retention, redaction, and visibility](#10b-log-retention-redaction-and-visibility)
11. [Upgrade path](#11-upgrade-path)
12. [Troubleshooting](#12-troubleshooting)

---

## Awell GKE log setup checklist

Dense one-liner-per-step checklist for the cluster log path. Detail
for every item lives in [cluster-logs-runbook.md](cluster-logs-runbook.md);
verification lives in [cluster-logs-smoke-test.md](cluster-logs-smoke-test.md).

- [ ] Create three **distinct** GCS buckets — bundle / backup / log — with the right storage classes (Standard / Nearline-or-Coldline / Standard-or-Nearline). [Runbook § 2](cluster-logs-runbook.md#2-the-three-buckets--do-not-reuse).
- [ ] Apply a GCS lifecycle rule on the log bucket matching `logStore.retentionDays` (default 90 days). [Runbook § 3](cluster-logs-runbook.md#3-bucket-setup).
- [ ] Bind the Workload Identity GSA with `roles/storage.objectAdmin` on **all three** buckets — not just the bundle bucket. [Runbook § 4](cluster-logs-runbook.md#4-iam).
- [ ] Annotate the `spire-operator` and `spire-gateway` KSAs with the GSA email so the operator can stamp pods and the gateway can read GCS. [Runbook § 4](cluster-logs-runbook.md#workload-identity-binding).
- [ ] In your overlay, set `logStore.backend=gcs`, `logStore.gcs.bucket=<your log bucket>`, `logStore.gcs.prefix=<optional per-tower>`, and `logExporter.enabled=true`. [Runbook § 5](cluster-logs-runbook.md#5-helm-values-reference).
- [ ] Confirm `gcp.serviceAccountJson` is supplied at install time (or `gcp.secretName` references an external Secret) — every GCP-consuming feature reuses the same credential. [§ 4 of this doc](#4-provision-gcp-resources).
- [ ] Raise Cloud Logging retention on `_Default` if your team relies on it for forensic search beyond 30 days (`gcloud logging buckets update _Default --location=global --retention-days=N`). [Runbook § 6](cluster-logs-runbook.md#6-retention--three-independent-axes).
- [ ] Run `helm upgrade --install` and watch the rollout — render is fail-fast on missing log bucket / GCP auth. [§ 5 of this doc](#5-first-helm-install).
- [ ] Run [cluster-logs-smoke-test.md](cluster-logs-smoke-test.md) end-to-end against a real bead. PASS = sidecar uploads + manifest rows + gateway list + CLI pretty + board parity.
- [ ] Bookmark [cluster-logs-runbook.md § 7](cluster-logs-runbook.md#7-troubleshooting) for missing-bucket / missing-IAM / exporter-crash / 410-Gone / redacted-or-denied recoveries.

---

## 1. Prerequisites

This runbook assumes a clean GCP project. Set a shell variable for the project ID — every command below references it.

```bash
export PROJECT_ID=<your-gcp-project-id>
```

### gcloud CLI

Install the Google Cloud SDK (https://cloud.google.com/sdk/docs/install), then authenticate and pin the active project:

```bash
gcloud auth login
gcloud auth application-default login
gcloud config set project ${PROJECT_ID}
```

### kubectl + GKE auth plugin

`kubectl` talks to GKE through the `gke-gcloud-auth-plugin` binary. Both must be on PATH before `gcloud container clusters get-credentials` will produce a working kubeconfig.

```bash
gcloud components install kubectl gke-gcloud-auth-plugin
```

If you installed `kubectl` from another source (Homebrew, `apt`, etc.) you still need the auth plugin:

```bash
gcloud components install gke-gcloud-auth-plugin
```

Verify both are reachable:

```bash
kubectl version --client
gke-gcloud-auth-plugin --version
```

### Enable required GCP APIs

Spire's GKE deployment uses GKE itself, Workload Identity (via IAM Credentials), Artifact Registry for images, GCS for the bundle store, and Certificate Manager for the managed TLS cert.

```bash
gcloud services enable \
  container.googleapis.com \
  iamcredentials.googleapis.com \
  artifactregistry.googleapis.com \
  storage.googleapis.com \
  certificatemanager.googleapis.com \
  --project=${PROJECT_ID}
```

### DoltHub tower repo (first-install seed)

The cluster's first install seeds its bead graph from a tower hosted on
DoltHub. After the first-install seed clone, the cluster Dolt is the
write authority and DoltHub is no longer a bidirectional mirror — it
either receives one-way archive pushes or is left untouched. Create a
free DoltHub account at https://www.dolthub.com and create an empty
data repository — see https://www.dolthub.com/data-repositories for the
listing. Use the repo path `acmeco/spire-tower` (replace `acmeco` with
your DoltHub org or username) when you configure the chart later.

You will also need a DoltHub credential keypair on the laptop you
generate it from. The cluster's dolt-init container uses these to clone
on first boot. Generate one if you do not already have it:

```bash
dolt creds new
dolt creds use <keyid>
dolt creds add <keyid>     # uploads the public half to DoltHub
```

### Anthropic credentials

The wizard pods need a Claude credential. Either an Anthropic API key or a Claude OAuth subscription token works — pick one.

```bash
export ANTHROPIC_API_KEY=sk-ant-...
```

Keep this value out of git. The Helm install passes it via `--set` or an external secret store; do not commit it to `values.gke.yaml`.

### GitHub PAT for wizard push

Each wizard pushes its feature branch to the repo's `git_remote` (typically GitHub). Create a fine-grained or classic personal access token with the `repo` scope at https://github.com/settings/tokens, scoped to the repos the cluster will run beads against.

```bash
export GITHUB_PAT=ghp_...
```

This token is supplied to the chart at install time. Like the Anthropic key, do not commit it.

## 2. Create the GKE cluster

The cluster needs Workload Identity enabled at create time so the steward pod can reach GCS via its Kubernetes ServiceAccount instead of a long-lived JSON key.

### Autopilot (recommended for v1)

Autopilot manages node sizing for you and turns Workload Identity on by default. Most operators should start here:

```bash
gcloud container clusters create-auto spire-cluster \
  --project=${PROJECT_ID} \
  --region=us-central1 \
  --workload-pool=${PROJECT_ID}.svc.id.goog
```

### Standard (if you need node-pool control)

If you want explicit control over node sizing, create a Standard cluster with a single autoscaling node pool:

```bash
gcloud container clusters create spire-cluster \
  --project=${PROJECT_ID} \
  --region=us-central1 \
  --workload-pool=${PROJECT_ID}.svc.id.goog \
  --release-channel=regular \
  --machine-type=e2-standard-4 \
  --num-nodes=1 \
  --enable-autoscaling \
  --min-nodes=1 \
  --max-nodes=5 \
  --workload-metadata=GKE_METADATA
```

`--workload-pool` is mandatory. Without it the chart's Workload Identity bindings (set up in section 4) have no effect and the steward pod will fail to reach GCS.

### Networking

Both commands above use the default VPC, which is fine for v1 — the ingress in section 6 fronts the cluster with a managed TLS cert and a public IP. For tighter network posture, you can reach for a private cluster (`--enable-private-nodes`, `--enable-private-endpoint`, master authorized networks) as a follow-on; that path is intentionally out of scope for this runbook so the first install stays simple.

### Populate kubeconfig

Fetch credentials so `kubectl` targets the new cluster:

```bash
gcloud container clusters get-credentials spire-cluster \
  --project=${PROJECT_ID} \
  --region=us-central1
```

### Verify

Nodes should be `Ready` (Autopilot may show a system-managed node pool with a generated name; that is expected):

```bash
kubectl get nodes
```

Create the namespace the chart will install into. Doing this now keeps the IAM bindings in section 4 self-contained:

```bash
kubectl create namespace spire
```

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

Section 2 already created the `spire` namespace as part of the cluster
verification step. If you are running section 4 in a fresh shell or are
unsure, the idempotent re-apply below will create the namespace if it
is missing and quietly succeed if it already exists:

```bash
kubectl create namespace spire \
  --dry-run=client -o yaml | kubectl apply -f -
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

## 5. First helm install

The chart lives in this repo at `helm/spire/`. The GKE-leaning overlay
ships at `helm/spire/values.gke.yaml` — it complements (does not
replace) `helm/spire/values.yaml`, so both render together when you
pass `-f helm/spire/values.gke.yaml`. Sections 1–4 of this runbook
already created the GKE cluster, the Artifact Registry images, the
DoltHub tower repo, the GCS bundle bucket, and the Workload Identity
binding from the `spire-tower` KSA to
`spire-tower@${PROJECT_ID}.iam.gserviceaccount.com`. This step turns
all of that into running pods.

### Canonical install command

This same invocation handles both the first install and every
subsequent upgrade — it is idempotent. Only the credential overrides
(below) and the image tag change between runs.

```bash
helm upgrade --install spire helm/spire \
  -n spire --create-namespace \
  -f helm/spire/values.gke.yaml
```

A bare invocation will fail the chart's own `required` gates: the GCS
bundle bucket must be set, the GCP credential placeholder needs to be
overridden, and DoltHub credentials must reach the Secret one way or
another. Add the override flags below before running.

### Required overrides

The bead description listed the override paths under a `secrets.*` /
`tower.*` shorthand for brevity; the real chart uses the keys below.
Pass these on the same `helm upgrade --install` line as the canonical
command above.

```bash
helm upgrade --install spire helm/spire \
  -n spire --create-namespace \
  -f helm/spire/values.gke.yaml \
  --set-string anthropic.apiKey="$ANTHROPIC_API_KEY" \
  --set-string github.token="$GITHUB_TOKEN" \
  --set-string dolthub.remoteUrl=acmeco/spire-tower \
  --set-string dolthub.user="$DOLTHUB_USER" \
  --set-string dolthub.password="$DOLTHUB_PASSWORD" \
  --set-string dolthub.credsKeyId="$DOLTHUB_KEY_ID" \
  --set-file   dolthub.credsKeyValue="$HOME/.dolt/creds/${DOLTHUB_KEY_ID}.jwk" \
  --set-string bundleStore.gcs.bucket=${PROJECT_ID}-spire-bundles \
  --set-file   gcp.serviceAccountJson=/dev/null \
  --set-string gateway.apiToken="$TOWER_TOKEN" \
  --set        gateway.ingress.enabled=true \
  --set-string gateway.ingress.host=spire.example.com
```

What each override does and where it lands:

- `anthropic.apiKey` — Anthropic classic API key. Rendered as
  `ANTHROPIC_API_KEY_DEFAULT` in the `spire-credentials` Secret. The
  steward sidecar's hand-rolled HTTP client reads this; wizard pods
  read it via `pkg/agent/pod_builder.go`'s AuthSlot routing. If you
  use a Claude subscription token instead, set
  `anthropic.subscriptionToken` (rendered as
  `ANTHROPIC_SUBSCRIPTION_TOKEN`).
- `github.token` — GitHub PAT used by wizard pods to clone, push, and
  open PRs. Rendered as `GITHUB_TOKEN`.
- `dolthub.remoteUrl` — the DoltHub repo used as the **first-install
  seed** for this tower's beads (e.g. `acmeco/spire-tower`). The
  dolt-init container clones from it on first boot. In cluster-as-truth
  installs it is no longer a writable mirror — `syncer.enabled` defaults
  to `false` and the runtime cluster Sync is a no-op even when set
  true. GCS backup (`backup.*`) is the canonical archive/DR path.
- `dolthub.user` / `dolthub.password` — username and password for the
  cluster-side `remotesapi` SQL user that the post-install
  `spire-dolt-provision` Job creates. Laptops use these when they
  `dolt clone --user=<here>` against the in-cluster dolt server. The
  password must not contain a single quote (`'`); the provisioning
  Job rejects such passwords to avoid SQL/shell quoting pitfalls.
- `dolthub.credsKeyId` / `dolthub.credsKeyValue` — the Dolt key ID
  (`base32` string from `dolt creds ls`) and the raw JWK JSON from
  `~/.dolt/creds/<id>.jwk`. These authenticate HTTPS clone/pull/push
  to DoltHub itself (separate from the cluster-side remotesapi user
  above). Pass the JWK with `--set-file` so the JSON file's contents
  are embedded into the `<release>-dolthub-creds` Secret without
  shell-escaping pitfalls.
- `bundleStore.gcs.bucket` — the bucket created in section 4. The
  chart `required`s this whenever `bundleStore.backend=gcs` (which is
  the default in the GKE overlay). The Workload Identity binding from
  section 4 is what authorises the steward pod to read/write the
  bucket; the chart-level GCP credential below is a render-time
  placeholder, not a runtime credential.
- `gcp.serviceAccountJson` — `templates/steward.yaml` `required`s a
  non-empty value when `bundleStore.backend=gcs`, even on a
  pure-Workload-Identity install. Pass `/dev/null` (or any empty
  file) via `--set-file` to satisfy the gate; the runtime credential
  resolution still goes through Workload Identity because the
  `spire-tower` KSA is bound to the GSA. (TODO: verify whether the
  chart has since grown a `--no-gcp-key` opt-out — this gate is
  tracked for removal in the cluster-attach epic chain.)
- `gateway.apiToken` — Bearer token the gateway validates for every
  `/api/v1/*` request. The chart materialises this into the
  `spire-gateway-auth` Secret and the gateway container reads it via
  envFrom. If left empty, the gateway boots in dev mode (no auth) —
  fine behind a port-forward, **never** acceptable behind an Ingress.
  Generate with `openssl rand -base64 32` and store the value
  alongside whatever distributes credentials to operators.
- `gateway.ingress.enabled=true` and `gateway.ingress.host` — turn on
  the GKE Ingress (off by default in the overlay) and set the
  external hostname. The overlay already defaults
  `gateway.ingress.className=gce`, `managedCert.enabled=true`,
  `backendConfig.enabled=true`, and `backendConfig.http2=true`, so
  these two flags are usually all you need.

### Workload Identity is wired outside the chart

There is no `serviceAccount.googleServiceAccount` value on this
chart. The header of `helm/spire/values.gke.yaml` is the source of
truth: bind the `spire-tower` KSA in the `spire` namespace to
`spire-tower@${PROJECT_ID}.iam.gserviceaccount.com` with `gcloud iam
service-accounts add-iam-policy-binding` and `kubectl annotate
serviceaccount`, then the chart picks it up at runtime. Section 4
already covers the gcloud sequence; if you skipped it, go back —
otherwise the steward pod will land in `CrashLoopBackOff` with 403s
on every GCS call. (TODO: verify against the chart's eventual
"native WI" path — at that point this dance and the `gcp.json`
placeholder both become unnecessary.)

### Watch the rollout

The chart renders three workload deployments under
`-n spire`: `spire-steward` (singleton, on the PVC), `spire-gateway`
(scaled to `replicas: 2` in the GKE overlay, each on its own
emptyDir-backed `.beads/` workspace), and `spire-operator`. Watch
each:

```bash
kubectl -n spire rollout status deploy/spire-gateway --timeout=5m
kubectl -n spire rollout status deploy/spire-steward --timeout=5m
kubectl -n spire rollout status deploy/spire-operator --timeout=5m
```

Dolt comes up as a StatefulSet (`spire-dolt`); track it separately:

```bash
kubectl -n spire rollout status statefulset/spire-dolt --timeout=5m
```

If `spire-gateway` rolls out before `spire-dolt` is serving, its
`tower-attach` initContainer will block until the dolt SQL server
answers. That's expected — the chart sets `--dolt-wait=300s` so
first-install timing isn't a concern.

### Upgrades

Re-run the same `helm upgrade --install` invocation with the new
image tag (e.g. when a release of
`us-central1-docker.pkg.dev/${PROJECT_ID}/spire/spire:${TAG}` lands
in your Artifact Registry). The chart already pins workload tags to
`.Chart.AppVersion`, so updating the chart version is sufficient for
a coordinated bump; pass `--set images.steward.tag=${TAG}` and
`--set images.agent.tag=${TAG}` only when you need to override the
chart's pin to a hotfix tag. Section 11 covers the upgrade flow in
more detail.

## 6. Set up DNS and TLS

The GKE overlay turns on a **GKE Ingress + Google-managed
ManagedCertificate** (the chart's "Strategy A"). It does **not** use
Gateway API / `HTTPRoute` — verify that against the live templates
(`gateway-ingress.yaml`, `gateway-managedcert.yaml`) before debugging
TLS issues, because the resource names below assume the Ingress
strategy. (TODO: verify whether the chart has grown a
`GoogleClusterIssuer` / `CertificateMap` path; if so, the names in
this section need to be updated.)

### Get the Ingress external IP

The `gateway.ingress.host` value you passed at install (e.g.
`spire.example.com`) becomes the host on the Ingress. GCLB allocates
a public IP and writes it into `status.loadBalancer.ingress[0].ip`.
The Ingress is named `spire-gateway` (it shares its name with the
Service for backend wiring; see `gateway-ingress.yaml`).

```bash
kubectl -n spire get ingress spire-gateway \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
echo
```

Allow a few minutes after `helm install` for GCLB to provision the
load balancer and surface the IP. If the field is empty for more
than 5 minutes, `describe` the Ingress and look for events from the
`loadbalancer-controller`:

```bash
kubectl -n spire describe ingress spire-gateway
```

### Create the DNS records

Point `spire.example.com` at that IP at your DNS registrar. An A
record is required; an AAAA record is optional and only useful if
your registrar supports IPv6 LB IPs (GKE Ingress is IPv4-only by
default).

```bash
# At your DNS provider — example only, syntax varies.
# Type: A     Name: spire.example.com     Value: <ingress-ip>     TTL: 300
# Type: AAAA  Name: spire.example.com     Value: <ipv6-if-any>    TTL: 300
```

### Verify DNS propagation

```bash
dig +short spire.example.com
```

The output must match the Ingress IP you just looked up. If it
doesn't, wait for TTL to expire on whatever stale record you had,
then re-check. The managed certificate **will not** advance past
`Provisioning` until DNS resolves to the GCLB IP.

### Watch the managed certificate provision

The overlay defaults the ManagedCertificate name to
`spire-gateway-cert` (see `gateway.ingress.managedCert.name` in
`values.gke.yaml`). The Ingress wires it in via the
`networking.gke.io/managed-certificates` annotation.

```bash
kubectl -n spire get managedcertificate spire-gateway-cert -o yaml
kubectl -n spire describe managedcertificate spire-gateway-cert
```

Watch for `status.certificateStatus` to move
`Provisioning → Active`. Per-domain status under
`status.domainStatus[].status` should likewise reach `Active`.

Expected duration: **10–30 minutes** after DNS resolves. Google
re-checks DNS on its own cadence and only kicks the cert issuance
once it sees a matching A record from its prober. If the cert is
still `Provisioning` after 45 minutes:

- Re-confirm `dig +short spire.example.com` resolves to the Ingress IP.
- Check `status.domainStatus[].reason` for `FailedNotVisible` or
  `FailedCaaChecking`.
- Look in `kubectl describe managedcertificate spire-gateway-cert`
  Events for any `FailedToCreate` / `FailedToBind` records.

Section 12 (Troubleshooting) catalogues the more common failure
modes.

## 7. Verify

Once DNS resolves and the managed certificate is `Active`, walk the
checks below in order. Each one isolates a different layer (pods,
GCLB, gateway HTTP, gateway auth) so a failure points at exactly
one component.

### Pods

```bash
kubectl -n spire get pods
```

Every pod should be `Running` and have all containers `Ready` (e.g.
`2/2` for `spire-steward`, `1/1` for the rest). A pod stuck in
`Init:0/1` for more than a minute usually means the `tower-attach`
initContainer is still waiting on dolt — `kubectl logs` it to
confirm. A pod in `CrashLoopBackOff` after that is a real failure;
jump to section 12.

### Ingress / GCLB

```bash
kubectl -n spire get ingress spire-gateway
```

`ADDRESS` should be populated and match the IP your A record points
at. (The `kubectl get gateway` form referenced in some earlier docs
is for Gateway API resources; this chart ships an Ingress, so
`get ingress` is the right verb.)

You can also verify GCLB sees the backend as healthy:

```bash
kubectl -n spire describe ingress spire-gateway | grep -A2 'Backends:'
```

A backend reported `UNHEALTHY` here usually means the BackendConfig
health check (`/healthz` on port `3030`, the `gateway.apiPort`) is
failing — confirm the gateway pods themselves answer it (next
check).

### Gateway HTTP — public, unauthenticated

```bash
curl -fsS https://spire.example.com/healthz
```

Expect `HTTP/2 200` and a small JSON body with the gateway version.
This proves DNS, TLS termination at the GCLB, the GKE Ingress
routing, and the gateway pod's `/healthz` handler — in one shot.
TLS errors here mean the managed cert is not yet `Active`; 502s
mean GCLB cannot reach the backend (BackendConfig / health check
mismatch); 404s mean the Ingress routes did not render (re-check
`gateway.ingress.enabled=true`).

### Gateway HTTP — authenticated

The `/api/v1/tower` route returns tower metadata to authenticated
callers. Use the same `$TOWER_TOKEN` you passed to
`gateway.apiToken` at install:

```bash
curl -fsS -H "Authorization: Bearer $TOWER_TOKEN" \
  https://spire.example.com/api/v1/tower
```

Expect a JSON document containing the tower's `id`, `prefix`,
`database`, and `dolthubRemote`. A `401` means the token does not
match what the gateway validates (re-set `gateway.apiToken` and
re-run `helm upgrade`); a `403` means the gateway's auth path
matched but rejected the request body — usually a mismatched audience
on a JWT, which the v1 token model does not use, so re-check that
you are passing the bare bearer string and not a JWT.

### Retrieving the tower token

If you forgot or never persisted the value you passed to
`gateway.apiToken`, retrieve it from the chart-rendered Secret:

```bash
kubectl -n spire get secret spire-gateway-auth \
  -o jsonpath='{.data.SPIRE_API_TOKEN}' | base64 -d
echo
```

The Secret name is `spire-gateway-auth` (chart default; defined by
the `spire.gatewaySecretName` helper) and the key inside it is
`SPIRE_API_TOKEN`. (TODO: verify that the chart still surfaces this
via `helm get notes spire -n spire` — the NOTES.txt template is
slated to print the retrieval command in a later epic.) Note that
the chart does **not** auto-generate a token: if you installed
without `--set-string gateway.apiToken=...`, the Secret value is
empty and the gateway is running in dev mode (no auth). Re-run
`helm upgrade` with a real token before exposing the Ingress to
anything beyond a port-forward.

### Backup landed in GCS

Cluster-as-truth installs make GCS backup the canonical archive/DR
substrate (the chart defaults `backup.enabled=true`; see the values
overlay in §5). Verify the dolt-backup CronJob is configured and that
the first scheduled run lands an object in the bucket — backup-enabled
is not the same as DR-ready, but a missing CronJob or empty bucket
means the install never wired backup up at all.

```bash
# CronJob exists and is scheduled
kubectl -n spire get cronjob spire-dolt-backup

# After the first scheduled run (or `kubectl create job --from=cronjob/...`
# to trigger one immediately), confirm the bucket has objects:
gsutil ls gs://${PROJECT_ID}-spire-backups/prod/
```

If `kubectl get cronjob` returns `NotFound`, the chart did not render
the backup resources — re-check `backup.enabled=true` in your values
overlay. If the Job runs but the bucket is empty, jump to "Backup
landed in GCS" in §12 to debug GCP auth.

The full restore drill (proving DR works end-to-end) lives in
[`docs/runbooks/gcs-restore.md`](runbooks/gcs-restore.md) and is run
out-of-band, not as part of first install.

With these checks green, the tower is up and ready for the CLI
attach in section 8.

## 8. Attach the CLI

The cluster is now serving the gateway at `https://spire.example.com`.
Point your local `spire` CLI at it so every bead/message op tunnels through
the gateway instead of going to a local Dolt server.

### Get the bearer token

The token lives in the `spire-gateway-auth` Secret that the chart created
when you set `gateway.apiToken` (you should have copied it during §7 —
this is the same value):

```bash
TOWER_TOKEN=$(kubectl -n spire get secret spire-gateway-auth \
  -o jsonpath='{.data.SPIRE_API_TOKEN}' | base64 -d)
```

> **Design note on `helm get notes`.** The design called for the token to
> be surfaced via `helm get notes spire -n spire`. The current
> `helm/spire` chart's `NOTES.txt` does not print the gateway token (it
> covers dolt/steward/operator readiness instead), so the kubectl path
> above is the canonical retrieval. Treat `helm get notes spire -n spire`
> as informational only.

### Attach

```bash
spire tower attach-cluster \
  --url=https://spire.example.com \
  --tower=spire-tower \
  --token=$TOWER_TOKEN
```

> **Design note on flags.** The design called this
> `spire tower attach-cluster --url=... --token=...`. The implementation
> additionally requires `--tower=<name>` so the CLI can verify the
> remote tower's identity (`GET /api/v1/tower`) matches the name you
> expect — preventing a typo'd URL from silently attaching you to
> someone else's tower. Use the tower name you set when creating the
> Helm release; in this runbook that is `spire-tower`.

What this does:

1. Calls `GET /api/v1/tower` on the gateway with the bearer token, and
   confirms the returned name equals `--tower`.
2. Persists the token in the OS keychain under service `spire-tower`,
   account `<tower-name>-token` (macOS Keychain via `security
   add-generic-password`; Linux secret-service via `secret-tool`).
3. Writes a gateway-mode `TowerConfig` to your local Spire config
   directory (`mode: gateway`, `url`, `token_ref`) and marks the tower
   active.

Subsequent `spire` commands (`spire file`, `spire claim`, `spire
collect`, etc.) detect the gateway-mode tower and route requests over
HTTPS instead of touching a local dolt server.

Optional: pass `--name=<alias>` to attach the cluster under a local
alias different from the remote tower name (useful when you have a
local-mode tower that already shares the name).

### Verify

```bash
spire tower list
```

Expected output (the active gateway-mode tower is marked with `*`):

```
  NAME             PREFIX   DATABASE             KIND       REMOTE
  ----             ------   --------             ----       ------
* spire-tower      spi      spire-tower          gateway    https://spire.example.com

  * = active tower
```

> **Design note on the verify verb.** The design suggested `spire status`
> with output `attached: cluster (https://spire.example.com)`. As
> implemented, `spire status` prints services (dolt, daemon, steward),
> agents, and the work queue — it does not print the active tower URL.
> Use `spire tower list` (above) to confirm the attachment; the `KIND`
> column shows `gateway` and the `REMOTE` column shows the cluster URL.
> If you also want to confirm the gateway is reachable, hit
> `https://spire.example.com/healthz` directly with `curl`.

A quick functional probe — file a throwaway bead through the CLI and
confirm it lands on the cluster's dolt server:

```bash
spire file "smoke test from cluster CLI" -t task -p 4
kubectl -n spire exec deploy/spire-dolt -- \
  dolt sql -q "SELECT id, title FROM \`spire-tower\`.issues ORDER BY created_at DESC LIMIT 1"
```

If the bead you just filed appears in the dolt query, the gateway and
the CLI are wired end-to-end.

### Detach or switch back

To stop using the cluster from this laptop:

```bash
# Detach the gateway-mode tower entirely (removes local config + keychain entry).
spire tower remove spire-tower
```

> **Design note on the detach verb.** The design called for
> `spire tower attach-cluster --remove` and `spire tower attach-local`.
> Neither flag landed as named — `attach-cluster` has no `--remove`
> mode, and there is no `attach-local` verb. The flow is:
>
> - **Detach this tower:** `spire tower remove <name>` — removes the
>   tower config and clears the keychain entry. For gateway-mode towers
>   no database is dropped (there is no local dolt database to drop);
>   for local-mode towers `tower remove` also drops the database, so
>   read the confirmation prompt before typing the tower name.
> - **Switch back to a local-mode tower without removing the cluster
>   attachment:** keep both towers configured and use
>   `spire tower use <local-tower-name>` to flip which one is active.
>   `spire tower list` shows them both.
> - **Re-attach later:** re-run `spire tower attach-cluster` with the
>   same flags. Keychain writes are idempotent; the local config is
>   recreated.

---

## 9. Attach the Desktop

Spire Desktop is the GUI companion to the CLI: a board view, a per-bead
panel, and an agent roster. It is the E5 deliverable from the
production-cluster epic. The first-run flow lets an archmage point the
desktop at the same gateway you just attached the CLI to.

### Where to get it

Spire Desktop lives in a separate repository (`spire-desktop`) and is
not part of the `awell-health/spire` release artifacts. Until the
desktop has its own published release artifacts, build it from source
following the README in that repo. (When desktop builds are published,
the link will go on the project's GitHub releases page; this runbook
will be updated with a direct URL.)

### First-run flow

Launch the desktop. On a clean install with no tower configured, the
welcome screen offers two paths:

- **Use a local tower** — points the desktop at a `spire serve`
  instance running on the laptop. Not what we want here.
- **Attach to cluster** — collects a gateway URL and a bearer token,
  then connects over HTTPS. Pick this.

Walkthrough of the **Attach to cluster** dialog:

1. **URL** — paste `https://spire.example.com` (the same URL you used
   for the CLI). The desktop probes `GET /healthz` before continuing
   and surfaces a clear error if the cert is not yet ready or the host
   does not resolve.
2. **Token** — paste the bearer token from the `spire-gateway-auth`
   Secret (the same `$TOWER_TOKEN` value you used for the CLI).
3. **Connect** — the desktop calls `GET /api/v1/tower` with the bearer
   token, confirms the returned tower name, and saves both pieces.

On success, the desktop transitions to the board view and starts polling
`/api/v1/beads` and `/api/v1/roster`. The first paint should show the
beads currently in the cluster's tower database.

### Where the token is stored

The desktop persists the bearer token in the same OS-native secret
store the CLI uses:

- **macOS:** Keychain, under service `spire-tower`, account
  `<tower-name>-token` (visible in **Keychain Access → login →
  Passwords**).
- **Linux:** secret-service (libsecret), via the
  `org.freedesktop.secrets` D-Bus API (visible in `seahorse` /
  GNOME Passwords under the same service+account labels).

The URL itself, plus a `TokenRef` pointing into the keychain entry,
goes into the desktop's config file (`~/Library/Application
Support/spire-desktop/` on macOS; `~/.config/spire-desktop/` on Linux).
The token never lives in the config file — only the reference does, so
backing up the config directory does not exfiltrate the token.

### Reconnect behaviour

When the gateway pod rolls (helm upgrade, node drain, etc.), in-flight
HTTPS requests fail and websocket subscriptions drop. The desktop's
gateway client treats this as a transient condition: it shows a brief
**Reconnecting…** banner across the top of the window, retries with
backoff, and clears the banner on the first successful poll against
the new gateway pod. No user action is required for routine rolls.

If the banner persists for more than ~30 seconds, that is a real
problem (gateway crash-looping, ingress misconfigured, token revoked) —
check `kubectl -n spire get pods -l app.kubernetes.io/name=spire-gateway`
and `kubectl -n spire logs deploy/spire-gateway --tail=100`. See §12
for the troubleshooting matrix.

### One cluster at a time

Spire Desktop in v1 holds exactly one tower attachment. To point the
desktop at a different cluster, open **Settings → Tower** and either
**Detach** (clears the current attachment and returns to the welcome
screen) or **Attach to cluster** again with new credentials (overwrites
in place).

> Multi-tower attach — switching between several clusters from a single
> desktop window without re-pasting credentials — is explicitly out of
> scope for v1. The CLI already supports multiple towers
> (`spire tower list`, `spire tower use <name>`), so an archmage who
> needs to operate against several clusters can do so from the
> terminal; the desktop will gain multi-tower support after v1 ships.

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

## 10b. Log retention, redaction, and visibility

Spire stores three artifact classes for cluster execution: agent transcripts, wizard operational logs, and live-stream events surfaced through Cloud Logging. Each is governed by a distinct retention axis with a distinct owner. **Do not try to collapse them into one policy** — they have different storage classes, access surfaces, and lifecycle assumptions. See [docs/design/spi-7wzwk2.md](#) (the bead's PRD) and `pkg/logartifact/README.md` for the design rationale.

### Three retention axes

| Axis | Backed by | Configured via | Awell default |
|------|-----------|----------------|---------------|
| **Cloud Logging** retention | GKE log buckets | Cloud Logging log-bucket retention (`gcloud logging buckets update _Default`) | **30 days** (matches GKE default; cite `cloud.google.com/logging/docs/buckets`) |
| **GCS artifact** retention | The dedicated log bucket from `logStore.gcs.bucket` | A GCS lifecycle rule applied out-of-band: `gsutil lifecycle set lifecycle.json gs://<log-bucket>` | **90 days** (the value `logStore.retentionDays` in `values.yaml` documents) |
| **Tower manifest** retention | `agent_log_artifacts` table in Dolt | `pkg/steward.LogArtifactCompactionPolicy` (compiled into the daemon) | PerBeadKeep=64, OlderThan=180 days |

Each owner is responsible for its own axis:

- **Cloud Logging retention.** The platform operator sets this via the GKE/Cloud Logging console. Spire never reaches in. If you need longer live-search history, raise the bucket retention there — the manifest and GCS bucket are unaffected.
- **GCS artifact retention.** Configure a bucket lifecycle rule on `logStore.gcs.bucket` that deletes objects older than your chosen retention. Spire does NOT reconcile bucket lifecycle policies (Helm cannot natively do so, and we keep parity with `bundleStore` and `backup` which take the same approach). Example `lifecycle.json`:
  ```json
  {
    "lifecycle": {
      "rule": [{
        "action": {"type": "Delete"},
        "condition": {"age": 90}
      }]
    }
  }
  ```
  Apply with: `gsutil lifecycle set lifecycle.json gs://<your-log-bucket>`.
- **Tower manifest retention.** The steward daemon runs `logartifact.CompactManifests` hourly with the policy compiled into `pkg/steward.LogArtifactCompactionPolicy`. The default keeps the 64 most-recent manifest rows per bead and prunes anything past 180 days of `updated_at`. Compaction prunes rows ONLY — the byte store is independent.

### Why the manifest age cap is longer than GCS retention

In steady state, GCS lifecycle deletes objects after 90 days; the manifest's 180-day cap is a safety net so a missed manifest insert (a writer that crashed before stamping a row) doesn't leave a permanent gap. A render that hits a manifest row whose object has been deleted by the lifecycle rule returns `ErrNotFound`; the gateway can fall back to the manifest's summary/tail fields.

### Visibility and redaction

Every log artifact carries a `visibility` class set at upload time. The substrate (spi-cmy90h) makes broader exposure than `engineer_only` an explicit decision at the call site:

| Visibility | Redaction at upload | Redaction at render | Who can read |
|------------|---------------------|----------------------|--------------|
| `engineer_only` (default) | None — raw bytes preserved | Engineer scope: none. Other scopes refused. | Engineer scope only |
| `desktop_safe` | Current redactor applied | Re-applied (defense in depth) | Engineer + Desktop |
| `public` | Current redactor applied | Re-applied | Engineer + Desktop + Public |

The redactor (`pkg/logartifact/redact`) masks high-confidence credential shapes: provider API keys (Anthropic, OpenAI), AWS/GCP credentials, GitHub tokens, JWTs, `Authorization: Bearer` headers, generic `api_key=` / `password=` assignments, and `BEGIN PRIVATE KEY` blocks. The mask token is the literal `[REDACTED]`.

**The redactor is hygiene, not a security boundary.** A determined adversary can phrase a credential in a way the patterns won't catch; the right control is "don't put secrets in logs in the first place." The redactor catches the obvious cases when that contract slips — it is the last filter, not the first.

The render layer always re-applies the current redactor on read for non-engineer scopes, regardless of what was stored. If the pattern set improves after an artifact is uploaded, the new patterns apply on the next read without rewriting storage. The redactor is versioned (`CurrentRedactionVersion`); the manifest records the version that ran at upload and the render layer reports both stored and runtime versions in its response metadata.

### Operator checklist

Before flipping `logStore.backend=gcs` for a tower that will serve transcripts to non-engineering surfaces:

1. Create a dedicated log bucket. **Do not reuse `bundleStore.gcs.bucket` or `backup.gcs.bucket`** — their lifecycle, storage-class, and access rules are incompatible.
2. Apply a lifecycle policy on the log bucket matching the retention you want (`gsutil lifecycle set ...`).
3. Confirm the Workload Identity GSA has `roles/storage.objectAdmin` on the new bucket; both the gateway (read) and operator-stamped exporter sidecars (write) reuse the same GSA.
4. Confirm Cloud Logging retention on the GKE cluster's log buckets matches your live-debug window; raise it explicitly if your team relies on Cloud Logging for forensic search rather than gateway reads.
5. Treat the redactor as defense, not the policy. Continue to redact secrets at the source: avoid logging tokens, scrub environment dumps, never `cat` credential files into stderr.

### Gateway log API surface

Bead-scoped logs are served exclusively through the gateway. Desktop, CLI, and board clients never touch GCS, Cloud Logging, or pod filesystems directly:

- `GET /api/v1/beads/{id}/logs` — list manifest rows for a bead. Returns `{ artifacts: [...], next_cursor: "..." }`. Each artifact row carries identity (attempt, run, agent, role, phase, provider, stream), `byte_size`, `checksum`, `status`, `visibility`, `redaction_version`, optional `summary`/`tail`, plus a `links` block with `raw` and (for transcripts) `pretty` URLs.
- `GET /api/v1/beads/{id}/logs/summary` — same shape as the list, no pagination, intended for board headers that want the full bounded summary set in one request.
- `GET /api/v1/beads/{id}/logs/{artifact_id}/raw` — streams the artifact's stored bytes through the gateway. Engineer scope reading engineer-only artifacts gets verbatim bytes; every other path runs the bytes through the runtime redactor.
- `GET /api/v1/beads/{id}/logs/{artifact_id}/pretty` — fetches the artifact and runs it through the provider-specific `pkg/board/logstream` adapter (Claude / Codex), returning canonical `{ events: [...] }` JSON for clients that want structured rendering rather than raw transcript bytes. Stream must be `transcript`.

Pagination is opaque: clients pass the `next_cursor` value back via `?cursor=...` and `?limit=N` (default 50, capped at 200). The cursor reserves a `byte_offset` field so live-follow (spi-bkha5x) can resume mid-artifact later without changing the wire format.

Caller scope is sourced from the `X-Spire-Scope` header today (`engineer`, `desktop`, `public`); absent the header, requests default to `desktop` so engineer-only artifacts are not exposed by accident. Auth itself reuses the gateway's existing bearer-token middleware — there is no separate log-API auth.

Manifest-only rows (status `writing`, no bytes yet) appear in the list; their `raw`/`pretty` requests return `409 not yet available`. Manifest rows whose bytes have been removed by GCS lifecycle policy return `410 Gone`.

---

## 11. Upgrade path

Upgrading the tower is a helm upgrade with a new image tag. The cluster does the rest: rolling deployments, persistent dolt state, durable BundleStore in GCS.

### 11.1 Pick the new image tag

You either bump the per-component tags in `helm/spire/values.gke.yaml`:

```yaml
images:
  steward:
    repository: us-central1-docker.pkg.dev/${PROJECT_ID}/spire/spire-steward
    tag: ${NEW_TAG}
  agent:
    repository: us-central1-docker.pkg.dev/${PROJECT_ID}/spire/spire-agent
    tag: ${NEW_TAG}
```

…or pass them on the command line for a one-shot upgrade without editing the values file:

```bash
helm upgrade --install spire helm/spire \
  -n spire \
  -f helm/spire/values.gke.yaml \
  --set images.steward.tag=${NEW_TAG} \
  --set images.agent.tag=${NEW_TAG}
```

The chart uses per-component image keys (`images.steward.*` and
`images.agent.*`); a singular `image.tag` override is silently ignored,
so make sure to set both. This is the same `helm upgrade --install`
invocation as the first install (section 5); helm idempotently
reconciles to the new tags.

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
- **Dolt state** — lives on the tower's persistent volume (PVC mounted into the dolt pod). It survives pod restarts and rolling upgrades, and is backed up off-cluster to GCS by the chart's `spire-dolt-backup` CronJob (chart default `backup.enabled=true`).

GCS backup is the canonical disaster-recovery path for cluster-as-truth
deployments. Backup is default-on and fail-fast; restore-from-GCS is
the documented recovery procedure. The full restore drill lives at
[`docs/runbooks/gcs-restore.md`](runbooks/gcs-restore.md). Treat the
cluster's PVC as the single writer for the bead graph (DoltHub is
seed-only/archive-only and is not read back as truth).

## 12. Troubleshooting

Each subsection below maps a real failure mode hit during E1-E5 development to a
diagnose-and-fix recipe. Symptoms are what an operator actually sees; diagnose
commands narrow the cause; fixes are copy-pasteable.

If you hit something not covered here, dump the offending pod's events and
logs first — `kubectl describe pod <name> -n spire` and `kubectl logs <name>
-n spire --all-containers --previous` — then file an issue against the runbook
with the symptom you saw and the cause you eventually found.

### Wizard / steward / gateway pod can't read or write GCS (Workload Identity not bound)

**Symptom:** the affected pod logs show one of:

```
googleapi: Error 401: Anonymous caller does not have storage.objects.create access ...
googleapi: Error 403: Caller does not have storage.objects.list access to bucket ...
google: could not find default credentials. See https://cloud.google.com/docs/authentication ...
```

`kubectl logs deploy/spire-steward -n spire -c steward` (or the wizard pod's
`agent` container) is the place these surface most often.

**Likely cause:** the Kubernetes ServiceAccount used by the pod is not bound
to the Google Service Account that holds `roles/storage.objectAdmin` on the
bundle bucket. Either the `iam.gke.io/gcp-service-account` annotation is
missing on the KSA, or the GSA's IAM policy never got the
`roles/iam.workloadIdentityUser` member that lets the KSA impersonate it.

**Diagnose:**

```bash
# Confirm the KSA carries the WI annotation
kubectl get sa spire-tower -n spire -o jsonpath='{.metadata.annotations.iam\.gke\.io/gcp-service-account}'
# Expect: spire-tower@${PROJECT_ID}.iam.gserviceaccount.com

# Confirm the GSA's IAM policy lists the KSA as a workloadIdentityUser
gcloud iam service-accounts get-iam-policy \
  spire-tower@${PROJECT_ID}.iam.gserviceaccount.com \
  --project=${PROJECT_ID} \
  --format='table(bindings.role,bindings.members)'
# Expect a binding "roles/iam.workloadIdentityUser" with member
# "serviceAccount:${PROJECT_ID}.svc.id.goog[spire/spire-tower]"

# From inside the pod, prove the metadata server is returning a token for
# the expected GSA.
kubectl exec deploy/spire-steward -n spire -c steward -- \
  curl -sS -H "Metadata-Flavor: Google" \
  http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/email
# Expect: spire-tower@${PROJECT_ID}.iam.gserviceaccount.com
```

If the email comes back as the node default service account
(`<projectnumber>-compute@developer.gserviceaccount.com`), Workload Identity
isn't taking effect for this pod — the KSA annotation, the node-pool
`GKE_METADATA` setting, or the cluster-level `--workload-pool` is wrong.

**Fix:**

```bash
# Bind the KSA → GSA (one-time per namespace/SA)
gcloud iam service-accounts add-iam-policy-binding \
  spire-tower@${PROJECT_ID}.iam.gserviceaccount.com \
  --role=roles/iam.workloadIdentityUser \
  --member="serviceAccount:${PROJECT_ID}.svc.id.goog[spire/spire-tower]" \
  --project=${PROJECT_ID}

# Annotate the KSA to declare which GSA to impersonate
kubectl annotate serviceaccount spire-tower -n spire \
  iam.gke.io/gcp-service-account=spire-tower@${PROJECT_ID}.iam.gserviceaccount.com \
  --overwrite

# Rotate pods so they pick up the fresh token
kubectl rollout restart deploy/spire-steward deploy/spire-gateway -n spire
```

If the cluster itself isn't WI-enabled, recreate it with
`--workload-pool=${PROJECT_ID}.svc.id.goog` (cluster-level) and the node pool
with `--workload-metadata=GKE_METADATA`.

### ManagedCertificate stuck in `Provisioning`

**Symptom:** `kubectl describe managedcertificate spire-gateway-cert -n spire`
keeps showing `Status: Provisioning` (sometimes for hours) and `curl
https://spire.<domain>/healthz` returns a TLS error or connection reset.

**Likely cause:** Google's managed-cert controller cannot complete the
HTTP-01 challenge because the gateway's external IP is not yet live in DNS,
or DNS still points somewhere else. The cert sits in `Provisioning` until DNS
resolves to the Ingress/Gateway IP.

**Diagnose:**

```bash
# Inspect the cert resource for the live status reason
kubectl describe managedcertificate spire-gateway-cert -n spire

# Resolve DNS from outside the cluster
dig +short spire.<domain>

# Get the actual Ingress / Gateway external IP
kubectl get ingress -n spire
# or, on Gateway API:
kubectl get gateway -n spire -o jsonpath='{.items[0].status.addresses[*].value}'

# Reserved static IP (if you used one)
gcloud compute addresses describe spire-gateway --global \
  --format='value(address)' --project=<project>
```

The IP returned by `dig` MUST equal the IP on the Ingress/Gateway. If they
differ, DNS hasn't propagated or the A record points at the wrong target.

**Fix:**

```bash
# Update the A record at your DNS provider to the gateway external IP, then
# wait for propagation (usually <5min, occasionally up to TTL).

# Once dig matches the gateway IP, the cert typically goes Active within
# 15-60 minutes. To prod the controller along, recreate the cert:
kubectl delete managedcertificate spire-gateway-cert -n spire
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml
```

If the cert refuses to leave `Provisioning` after an hour with DNS pointing
correctly, check `kubectl describe managedcertificate ...` for events
mentioning `FailedNotVisible` — that's Google's way of saying the
HTTP-01 challenge can't reach `/.well-known/acme-challenge/...`. Confirm the
Ingress accepts HTTP on port 80 (managed certs require port 80 reachable for
the challenge).

### Gateway has no external IP / Ingress addr is empty

**Symptom:** `kubectl get gateway -n spire` shows no `ADDRESS`, or
`kubectl get ingress -n spire` shows `<none>` under `ADDRESS` indefinitely.
Pods are healthy but unreachable from outside the cluster.

**Likely cause:** the Gateway API addon is disabled on the cluster, the
Gateway API CRDs (`gateway.networking.k8s.io`) aren't installed, or the
GKE Gateway controller can't reconcile the resource (often missing
`gke-l7-global-external-managed` GatewayClass).

**Diagnose:**

```bash
# Confirm the Gateway API CRDs are installed
kubectl get crds | grep gateway.networking.k8s.io
# Expect: gateways, gatewayclasses, httproutes at minimum

# Confirm the GKE GatewayClasses exist
kubectl get gatewayclasses
# Expect at least gke-l7-global-external-managed (or gke-l7-rilb for internal)

# Confirm the GKE gateway controller pods are running
kubectl get pods -n gke-system -l k8s-app=gke-l7-gateway-controller
# (namespace and label may differ on older clusters; on newer it's
# `kube-system` with `app=gke-gateway-controller`)

# Look at events on the Gateway resource itself
kubectl describe gateway spire -n spire
```

If the GatewayClass is missing or the controller pods aren't running, the
addon isn't installed.

**Fix:**

```bash
# Enable the Gateway API addon on the cluster (one-time)
gcloud container clusters update <cluster-name> \
  --gateway-api=standard \
  --location=<zone-or-region> \
  --project=<project>

# Re-apply your Helm release; the Gateway resource should pick up an IP
# within 1-2 minutes of the controller becoming ready.
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml
kubectl get gateway -n spire -w
```

If you'd rather use classic Ingress, set
`gateway.ingress.className=gce` in your overlay and the chart provisions a
GCE Ingress instead of a Gateway resource — covered under [Set up DNS and TLS](#6-set-up-dns-and-tls).

### Image pull errors (`ErrImagePull` / `ImagePullBackOff`)

**Symptom:** `kubectl get pods -n spire` shows pods stuck in
`ErrImagePull` or `ImagePullBackOff`. `kubectl describe pod <name> -n spire`
shows events like:

```
Failed to pull image "us-central1-docker.pkg.dev/<project>/spire/agent:latest":
  rpc error: code = NotFound desc = ... was not found
Failed to pull image "us-central1-docker.pkg.dev/<project>/spire/agent:latest":
  rpc error: code = Unauthenticated desc = ... unauthorized to access this resource
```

**Likely cause:**

1. The Artifact Registry repository wasn't created (`NotFound`), or
2. The image was pushed to a different region than the cluster pulls from
   (`NotFound` on a region mismatch), or
3. The cluster's nodes / pod ServiceAccount doesn't have
   `roles/artifactregistry.reader` on the GAR repo (`Unauthenticated`).

**Diagnose:**

```bash
# Confirm the repo exists in the expected region
gcloud artifacts repositories list --project=<project> \
  --format='table(name,format,location)'

# Confirm the image+tag is actually present
gcloud artifacts docker images list \
  <region>-docker.pkg.dev/<project>/spire \
  --include-tags

# Inspect the pod's actual image reference
kubectl get pod <name> -n spire -o jsonpath='{.spec.containers[*].image}'
# Compare region/project against the gcloud listing

# If pull auth is the issue, dump the pod events
kubectl describe pod <name> -n spire | grep -A5 -i "pull\|unauth"
```

**Fix:**

```bash
# (a) Repo missing — create it once per project (see runbook section 3)
gcloud artifacts repositories create spire \
  --repository-format=docker \
  --location=<region> \
  --project=<project>

# (b) Region mismatch — re-push in the cluster's region or update
# images.{steward,agent}.repository in your overlay to point at where the
# images actually live, then helm upgrade.

# (c) Auth missing on a same-project GKE cluster — grant the node SA reader
gcloud artifacts repositories add-iam-policy-binding spire \
  --location=<region> \
  --member="serviceAccount:<node-default-sa>" \
  --role=roles/artifactregistry.reader \
  --project=<project>
kubectl rollout restart deploy/spire-steward deploy/spire-operator -n spire
```

For cross-project clusters or non-GKE, see
[runbooks/gar-image-registry.md](runbooks/gar-image-registry.md) sections 3B
and 3C.

### `helm install` times out waiting for resources

**Symptom:** `helm install spire ...` hangs and eventually fails with:

```
Error: INSTALLATION FAILED: timed out waiting for the condition
```

`kubectl get pods -n spire` shows one or more pods stuck in `Pending` or
`ContainerCreating`.

**Likely cause:** the cluster has insufficient capacity to schedule the
requested pods (CPU/memory requests exceed available node allocatable),
or a PVC can't bind because no StorageClass / no volumes available.

**Diagnose:**

```bash
# Check pending pods cluster-wide
kubectl get pods -A | grep -i pending

# See WHY a specific pod is pending — events at the bottom
kubectl describe pod <pending-pod> -n spire | tail -30
# Common phrases: "0/N nodes are available: insufficient cpu",
#                 "insufficient memory", "had untolerated taint",
#                 "pod has unbound immediate PersistentVolumeClaims"

# Inspect node capacity vs allocations
kubectl top nodes
kubectl describe nodes | grep -A4 "Allocated resources"

# PVC status
kubectl get pvc -n spire
# If any are Pending, describe to see why
kubectl describe pvc <pvc-name> -n spire
```

**Fix:**

```bash
# (a) Add a node pool or scale up the existing one
gcloud container node-pools create extra \
  --cluster=<cluster-name> \
  --machine-type=e2-standard-4 \
  --num-nodes=2 \
  --location=<zone-or-region> \
  --project=<project>

# (b) Lower request floors via overlay if the defaults are over-eager,
# then helm upgrade. Don't lower below documented minimums for steward
# and dolt — they need real RAM headroom to run.

# (c) For unbound PVCs, set a default StorageClass on the cluster or
# specify storageClass: standard-rwo in the overlay's dolt.storage block.
kubectl patch storageclass standard-rwo \
  -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'
```

### Dolt clone fails with auth error / 403 (first-install seed)

**Symptom:** the dolt-init container's first-install clone from DoltHub
fails:

```
fatal: authentication failed for https://doltremoteapi.dolthub.com/<org>/<repo>
```

DoltHub auth is only exercised at first-install seed time in
cluster-as-truth installs (the bidirectional syncer is disabled and the
runtime cluster Sync is a no-op); ongoing operation does not require
DoltHub credentials.

**Likely cause:** the `doltCreds` Secret holds a key that's no longer valid
on DoltHub, the `dolthub.userName` configured on the cluster doesn't match
the account that owns the JWK, or the DoltHub repo's Collaborators list
doesn't include the account.

**Diagnose:**

```bash
# Confirm the Secret keys are present
kubectl get secret spire-dolthub-creds -n spire -o jsonpath='{.data}' | jq 'keys'
# Expect: ["creds-key-id", "creds-key-value", "user-email", "user-name"]

# Read the configured username (decode base64)
kubectl get secret spire-dolthub-creds -n spire \
  -o jsonpath='{.data.user-name}' | base64 -d

# Read the key ID and confirm it matches what `dolt creds ls` shows on the
# laptop the JWK was minted on
kubectl get secret spire-dolthub-creds -n spire \
  -o jsonpath='{.data.creds-key-id}' | base64 -d

# Test push from inside the dolt pod (cleanest way to isolate auth)
kubectl exec -n spire deploy/spire-dolt -- \
  dolt push origin main 2>&1 | head -20
```

Visit DoltHub → repo settings → Collaborators and confirm the configured
account is listed with write access.

**Fix:**

```bash
# Rotate the JWK on the laptop that owns the DoltHub account
dolt creds new
dolt creds use <new-key-id>
# (Push the new public key to DoltHub via the web UI, then test from laptop)

# Update the cluster Secret to the new key ID + JWK
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set dolthub.credsKeyId=<new-key-id> \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<new-key-id>.jwk

# Restart pods that hold a stale dolt config in memory
kubectl rollout restart deploy/spire-dolt deploy/spire-steward -n spire
```

If the DoltHub account is correct but `403` persists, double-check
`dolthub.userName` in your overlay — the Dolt CLI's `user.name` MUST match
the DoltHub account that owns the JWK, or the remote rejects the push.

### Wizard pod stuck `Pending` (Anthropic API key invalid / rate-limited)

**Symptom:** `kubectl get pods -n spire` shows `<guild>-wizard-<bead>` pods
created by the operator but never reaching `Running`, OR they reach
`Running` and immediately exit with an authentication error visible in
`kubectl logs`.

**Likely cause:** the Anthropic API token configured in `SpireConfig.tokens`
is missing, malformed, or rate-limited. The operator places the pod
successfully but the agent container exits during `claude` startup.

**Diagnose:**

```bash
# Operator logs show the assignment but no progress
kubectl logs -n spire deploy/spire-operator | grep -i "wizard\|assigned\|spawn" | tail -20

# Examine the wizard pod itself
kubectl describe pod -n spire -l spire.awell.io/role=wizard | tail -40

# Pull the agent container logs (or last-terminated logs if it crashed)
WIZARD=$(kubectl get pod -n spire -l spire.awell.io/role=wizard -o name | head -1)
kubectl logs $WIZARD -n spire -c agent --previous 2>/dev/null \
  || kubectl logs $WIZARD -n spire -c agent

# Watch for these strings:
#   "401 Unauthorized"     -> bad/expired token
#   "429 Too Many Requests" or "rate_limit_exceeded" -> rate-limited
#   "invalid x-api-key"    -> wrong header / token format
#   "OAuth token expired"  -> need to re-run `claude setup-token`

# Verify the Secret holding the token resolves
kubectl get secret -n spire | grep -i 'anthropic\|token'
kubectl get secret <secret-name> -n spire -o jsonpath='{.data.api-key}' \
  | base64 -d | head -c 20
# Should start with "sk-ant-api03-" (classic) or "sk-ant-oat01-" (OAuth)
```

**Fix:**

```bash
# (a) Rotate the API key
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set anthropic.apiKey=sk-ant-api03-...

# (b) Switch to OAuth (Max plan) if you've been hitting rate limits
claude setup-token   # on a Max-subscribed account, copy the sk-ant-oat01- token
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set anthropic.oauthToken=sk-ant-oat01-...

# (c) Force the operator to re-create stuck pods after rotating the secret
kubectl delete pod -n spire -l spire.awell.io/role=wizard
```

If pods keep cycling through `429`, lower
`spec.maxConcurrent` on the WizardGuild to throttle wizard creation, or
upgrade your Anthropic plan.

### `/healthz` returns 502 (gateway routes but pods aren't Ready)

**Symptom:** `curl https://spire.<domain>/healthz` returns:

```
HTTP/2 502
```

…or a generic Google "backend service unavailable" page. The DNS resolves
and the TLS handshake succeeds, so the Gateway is in the path — the failure
is past the L7.

**Likely cause:** the gateway pods aren't passing their liveness/readiness
probes, so the load balancer's backend service has no healthy endpoints to
forward to.

**Diagnose:**

```bash
# Confirm gateway pods exist and check their Ready column
kubectl get pods -n spire -l app.kubernetes.io/name=spire-gateway

# Inspect probe failures on the pods
kubectl describe pod -n spire -l app.kubernetes.io/name=spire-gateway | tail -40

# Hit /healthz from inside a gateway pod to confirm the app itself is fine
kubectl exec -n spire deploy/spire-gateway -- \
  curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:8080/healthz
# Expect: 200

# If the pod-local hit succeeds but external 502s persist, check the
# BackendConfig / Service health-check config matches the app's path/port
kubectl get backendconfig -n spire -o yaml
kubectl describe service -n spire spire-gateway
```

**Fix:**

```bash
# (a) Probe failure inside the pod — read the gateway logs and fix the
# underlying error (most often a missing tower-attach secret or wrong
# BEADS_DIR; see "Wizard / steward / gateway pod can't read or write GCS"
# above for the WI variant).
kubectl logs deploy/spire-gateway -n spire -c gateway --tail=100

# (b) Health-check path mismatch — ensure the BackendConfig's
# healthCheckPath matches what the gateway serves
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set gateway.ingress.backendConfig.healthCheckPath=/healthz

# (c) After fixing, force a fresh probe cycle
kubectl rollout restart deploy/spire-gateway -n spire
kubectl rollout status deploy/spire-gateway -n spire --timeout=120s
```

If pods are Ready but the LB still returns 502, give Google ~60s to refresh
its backend health view, then retry. Persistent 502s with all pods Ready
usually indicate a port mismatch between the Service and the BackendConfig.

### `spire tower attach-cluster` returns 401 Unauthorized

**Symptom:** the laptop CLI run:

```
$ spire tower attach-cluster --url=https://spire.<domain> --token=<token>
Error: 401 Unauthorized
```

The gateway is reachable (no TLS or 502), but the bearer check rejects.

**Likely cause:** the token sent by the CLI does not match what the gateway
was configured with via `gateway.apiToken`. Either the laptop is using a
stale value, the chart was installed without setting one (defaults to
empty / dev-mode), or the gateway pod is still running with the old
ConfigMap.

**Diagnose:**

```bash
# Confirm what the gateway pod thinks the token is
kubectl get secret -n spire | grep -i gateway
kubectl get secret <gateway-secret> -n spire -o jsonpath='{.data.api-token}' | base64 -d

# Hit the gateway with the same header to isolate CLI vs server
curl -i -H "Authorization: Bearer <token>" https://spire.<domain>/api/v1/tower
# 200 -> token is fine, the CLI is using a stale value
# 401 -> token mismatch, fix at the cluster
```

**Fix:**

```bash
# Reset the gateway token via helm overlay
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set gateway.apiToken=<new-strong-token>
kubectl rollout restart deploy/spire-gateway -n spire

# Update the laptop with the new token
spire tower attach-cluster --url=https://spire.<domain> --token=<new-strong-token>
```

### Steward pod CrashLoopBackOff during init (`tower-attach`)

**Symptom:** `kubectl get pods -n spire` shows the steward pod stuck in
`Init:CrashLoopBackOff`. `kubectl describe` reveals the failing init
container is `tower-attach`.

**Likely cause:** the init container failed to clone the DoltHub tower
into the shared PVC, usually because the JWK is invalid or the named
DoltHub repo doesn't exist for the configured account.

**Diagnose:**

```bash
# Read the init container logs (each restart wipes them, so use --previous
# if the pod is in CrashLoopBackOff)
STEWARD=$(kubectl get pod -n spire -l app.kubernetes.io/name=spire-steward -o name | head -1)
kubectl logs $STEWARD -n spire -c tower-attach --previous

# Common log lines:
#   "fatal: repository '...' not found"        -> remoteUrl wrong
#   "fatal: authentication failed"             -> creds wrong (see Dolt push)
#   "fatal: ambiguous argument"                -> JWK matches but the
#                                                  account lacks read access
```

**Fix:** once the underlying DoltHub auth/URL is corrected, delete the
pod so the init container retries with fresh values:

```bash
kubectl delete pod -n spire $STEWARD
kubectl get pod -n spire -l app.kubernetes.io/name=spire-steward -w
```

### Cluster mutations not visible on the laptop

**Symptom:** wizards complete and close beads inside the cluster
(visible via `kubectl logs deploy/spire-steward`), but
`spire focus`/`spire grok` on the laptop don't show the new state.

**Expected behavior in cluster-as-truth installs:** this is by design.
The cluster Dolt database is the write authority and DoltHub is no
longer a bidirectional mirror — laptops do **not** see cluster state by
running `spire pull`. They see it by attaching to the gateway
(`spire tower attach-cluster`) and routing reads/mutations through
`/api/v1/*`. The bidirectional syncer (`syncer.enabled`) is disabled by
default and the runtime cluster Sync is a no-op even when set true.

**Diagnose / fix:**

```bash
# Confirm the laptop is attached to the cluster tower and resolves to it
spire tower list
spire focus <bead-id>      # should reach the cluster via gateway HTTP

# If the laptop has a same-prefix local tower that wins CWD resolution,
# remove or rename it and re-attach to the cluster (see spi-43q7hp /
# spi-i7k1ag.2 cutover runbook).
```

For long-term archive/DR (laptop-side replica is NOT the model), the
canonical substrate is GCS backup — see [Backup
landed in GCS](#backup-landed-in-gcs) below.

### Backup landed in GCS

**Symptom:** verifying that the dolt-backup CronJob is actually
producing objects in `gs://<backup.gcs.bucket>/<backup.gcs.prefix>`.

**Diagnose:**

```bash
# CronJob and most-recent Job status
kubectl get cronjob spire-dolt-backup -n spire
kubectl get jobs -n spire -l app.kubernetes.io/name=spire-dolt-backup

# Logs from the latest backup run
LATEST=$(kubectl get jobs -n spire -l app.kubernetes.io/name=spire-dolt-backup \
  --sort-by=.metadata.creationTimestamp -o name | tail -1)
kubectl logs -n spire $LATEST

# List bucket contents (should grow on each successful run)
gsutil ls gs://<backup.gcs.bucket>/<backup.gcs.prefix>/
```

**Fix:** if the Job exits non-zero, check that the `<release>-gcp-sa`
Secret is mounted on the dolt pod and that the SA has
`roles/storage.objectAdmin` on the bucket. Restore is exercised by a
separate runbook ([`docs/runbooks/gcs-restore.md`](runbooks/gcs-restore.md))
— the first-install verification here only proves backup is landing,
not that DR is healthy.

### First wizard pod runs but never produces any apprentice work

**Symptom:** the wizard pod is `Running`, `kubectl logs` shows it claimed
the bead, but no apprentice pods get created and the wizard eventually
exits without progress.

**Likely cause:** the WizardGuild's `prefixes` doesn't include the bead's
prefix, OR `bundleStore` is misconfigured so the apprentice bundle never
makes it back from the worktree to the cluster.

**Diagnose:**

```bash
# Confirm the guild's prefix list covers the bead
kubectl get wizardguild -n spire -o yaml | grep -A2 prefixes

# Check the wizard's recent log for bundle-store errors
WIZARD=$(kubectl get pod -n spire -l spire.awell.io/role=wizard -o name | head -1)
kubectl logs $WIZARD -n spire -c agent | grep -iE "bundle|gcs|store"

# If using GCS, list the bucket — the apprentice bundle should appear here
gsutil ls gs://<bucket>/<prefix>/
```

**Fix:**

```bash
# Update the guild's prefix list
kubectl edit wizardguild <name> -n spire   # add the bead's prefix to spec.prefixes

# Or, if bundle-store auth is the issue, verify Workload Identity (above)
# and confirm the bucket exists and the GSA has objectAdmin on it.
```

### Helm install partially failed; rerun shows "release: already exists"

**Symptom:** the first `helm install` errored out (network blip, validation
failure), and re-running gives:

```
Error: INSTALLATION FAILED: cannot re-use a name that is still in use
```

**Likely cause:** Helm tracked the failed install as a release in
`pending-install` state. It's recoverable — Helm wants you to either retry
the upgrade path or roll back.

**Diagnose:**

```bash
helm list -n spire --all
# Look for STATUS=pending-install or failed
helm history spire -n spire
```

**Fix:**

```bash
# Easiest: switch to upgrade --install (idempotent) for the retry
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml ...

# If that still fails, uninstall and reinstall (PVCs survive by default)
helm uninstall spire -n spire
helm install spire helm/spire -n spire -f values.gke.yaml ...
```

If you want to clean state too, delete the PVCs after the uninstall —
but note that this wipes the on-cluster dolt database, which is the
single writer in cluster-as-truth deployments. The GCS backup
(`spire-dolt-backup` CronJob, see §7 "Backup landed in GCS" and
[`docs/runbooks/gcs-restore.md`](runbooks/gcs-restore.md)) is the
canonical recovery path; DoltHub is archive-only and not the
authoritative restore source.
