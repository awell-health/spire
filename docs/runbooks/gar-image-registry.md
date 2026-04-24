# Artifact Registry: Building, Pushing, and Pulling Spire Images

Authoritative reference for how Spire container images flow from CI into
GKE. Covers the registry choice, one-time GCP setup, three pull-auth
strategies for GKE, a manual emergency push procedure, and verification
steps.

Placeholders (`<project>`, `<region>`, `<pool-id>`, `<provider-id>`,
`<github-org>`, `<github-repo>`, `<tag>`) appear throughout — substitute
your environment's values before running commands.

For the broader end-to-end deployment story, see the cluster deployment
runbook from epic #6. This document stands alone as the definitive
guide for image build / push / pull.

---

## 1. Registry choice

Spire publishes to **Google Artifact Registry (GAR)**, not the legacy
`gcr.io` registry. Artifact Registry is Google's recommended
going-forward registry; `gcr.io` is on the path to deprecation and
should not be used for new infrastructure.

### Path pattern

```
<region>-docker.pkg.dev/<project>/spire/<image>:<tag>
```

### Layout

One GAR repository named `spire`, in `docker` format, containing two
images:

| Image     | Built from           |
|-----------|----------------------|
| `steward` | `Dockerfile.steward` |
| `agent`   | `Dockerfile.agent`   |

Example fully-qualified image references:

```
us-central1-docker.pkg.dev/<project>/spire/steward:v0.44.0
us-central1-docker.pkg.dev/<project>/spire/agent:v0.44.0
```

Tags pushed by CI: the release tag (e.g. `v0.44.0`) and `latest`.

---

## 2. One-time GCP setup

Run these once per GCP project that will host the registry. You need
`roles/owner` or an equivalent combination of admin roles to execute
them.

### 2.1 Enable the Artifact Registry API

```bash
gcloud services enable artifactregistry.googleapis.com \
  --project <project>
```

### 2.2 Create the `spire` docker repository

```bash
gcloud artifacts repositories create spire \
  --repository-format=docker \
  --location=<region> \
  --description="Spire container images (steward, agent)" \
  --project <project>
```

### 2.3 Create a CI service account

This service account is what GitHub Actions impersonates to push
images. Grant it `roles/artifactregistry.writer` scoped to the `spire`
repository (not project-wide — least privilege).

```bash
gcloud iam service-accounts create spire-ci-pusher \
  --display-name="Spire CI image pusher" \
  --project <project>

gcloud artifacts repositories add-iam-policy-binding spire \
  --location=<region> \
  --member="serviceAccount:spire-ci-pusher@<project>.iam.gserviceaccount.com" \
  --role="roles/artifactregistry.writer" \
  --project <project>
```

### 2.4 Create a Workload Identity Federation pool and OIDC provider

Workload Identity Federation (WIF) lets GitHub Actions impersonate the
CI service account using short-lived OIDC tokens — no long-lived JSON
keys to rotate or leak.

This runbook only shows the shape of the commands. The canonical
reference, kept current by Google, is
[`google-github-actions/auth`](https://github.com/google-github-actions/auth#setting-up-workload-identity-federation).
Follow that for the definitive setup; come back here for the Spire-
specific pieces (step 2.6).

```bash
gcloud iam workload-identity-pools create <pool-id> \
  --location=global \
  --display-name="GitHub Actions pool" \
  --project <project>

gcloud iam workload-identity-pools providers create-oidc <provider-id> \
  --location=global \
  --workload-identity-pool=<pool-id> \
  --display-name="GitHub OIDC provider" \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repository_owner=assertion.repository_owner" \
  --attribute-condition="assertion.repository_owner == '<github-org>'" \
  --project <project>
```

### 2.5 Bind the GitHub repo principal to the CI service account

Allow only the specific GitHub repository to impersonate the CI service
account:

```bash
gcloud iam service-accounts add-iam-policy-binding \
  spire-ci-pusher@<project>.iam.gserviceaccount.com \
  --role="roles/iam.workloadIdentityUser" \
  --member="principalSet://iam.googleapis.com/projects/<project-number>/locations/global/workloadIdentityPools/<pool-id>/attribute.repository/<github-org>/<github-repo>" \
  --project <project>
```

`<project-number>` is the numeric project number (not the
project ID). Get it with `gcloud projects describe <project>
--format='value(projectNumber)'`.

### 2.6 Configure GitHub repo variables and secrets

The CI workflow expects exactly these names. Set them under **Settings
→ Secrets and variables → Actions** in the GitHub repo.

Repository **variables** (non-sensitive):

| Name          | Value                                  |
|---------------|----------------------------------------|
| `GCP_PROJECT` | Your GCP project ID (e.g. `acme-prod`) |
| `GCP_REGION`  | GAR region (e.g. `us-central1`)        |

Repository **secrets** (sensitive):

| Name                  | Value                                                                                                         |
|-----------------------|---------------------------------------------------------------------------------------------------------------|
| `GAR_WIF_PROVIDER`    | Full resource name: `projects/<project-number>/locations/global/workloadIdentityPools/<pool-id>/providers/<provider-id>` |
| `GAR_SERVICE_ACCOUNT` | `spire-ci-pusher@<project>.iam.gserviceaccount.com`                                                           |

`GAR_WIF_PROVIDER` must be the full `projects/.../providers/...`
resource path, not just the provider ID. `google-github-actions/auth`
will reject a short name.

---

## 3. GKE pull authentication

Three options. Pick one per cluster based on where the cluster lives
and how strict your least-privilege stance is.

### Option A — Same-project cluster (recommended default)

**Use when:** the GKE cluster runs in the **same GCP project** as the
`spire` GAR repository.

Grant `roles/artifactregistry.reader` at the project level (or scoped
to the repository) to the **node default service account**. Every pod
on the cluster then pulls transparently — no per-pod config, no image
pull secrets.

```bash
# Find the node default SA (usually "<project-number>-compute@...")
gcloud iam service-accounts list --project <project>

gcloud artifacts repositories add-iam-policy-binding spire \
  --location=<region> \
  --member="serviceAccount:<node-default-sa-email>" \
  --role="roles/artifactregistry.reader" \
  --project <project>
```

No Kubernetes-side changes are required. `imagePullSecrets` should be
left empty in `values.yaml`.

**Trade-offs.** Simplest setup. The downside is that every pod on the
cluster, not just Spire pods, gets read access to the `spire` repo via
the node SA. Acceptable when the cluster is dedicated or
same-trust-domain; not acceptable for multi-tenant clusters.

### Option B — Workload Identity on the pod

**Use when:** the cluster runs in a **different GCP project** from the
registry, or when you want least-privilege (only the Spire pod, not
every pod on the node, can pull Spire images).

Bind a Kubernetes service account to a GCP service account that holds
`roles/artifactregistry.reader`, via GKE Workload Identity.

```bash
# 1. Create the GCP SA that will be used for pulls
gcloud iam service-accounts create spire-puller \
  --display-name="Spire image puller" \
  --project <project>

# 2. Grant reader on the GAR repo
gcloud artifacts repositories add-iam-policy-binding spire \
  --location=<region> \
  --member="serviceAccount:spire-puller@<project>.iam.gserviceaccount.com" \
  --role="roles/artifactregistry.reader" \
  --project <project>

# 3. Allow the Kubernetes SA to impersonate the GCP SA
gcloud iam service-accounts add-iam-policy-binding \
  spire-puller@<project>.iam.gserviceaccount.com \
  --role="roles/iam.workloadIdentityUser" \
  --member="serviceAccount:<cluster-project>.svc.id.goog[<k8s-namespace>/<k8s-sa-name>]" \
  --project <project>
```

Then annotate the Kubernetes service account used by Spire pods:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: <k8s-sa-name>
  namespace: <k8s-namespace>
  annotations:
    iam.gke.io/gcp-service-account: spire-puller@<project>.iam.gserviceaccount.com
```

The cluster must have Workload Identity enabled (cluster-level
`--workload-pool=<cluster-project>.svc.id.goog`) and the node pool
must have `GKE_METADATA` enabled.

**Trade-offs.** Tight least-privilege. More moving parts; requires
Workload Identity to be enabled cluster-wide.

### Option C — `imagePullSecret` from a service-account JSON key

**Use when:** the target is **not GKE** (e.g. self-managed Kubernetes,
EKS, AKS, kind/minikube used for smoke tests), or the cluster doesn't
have Workload Identity.

Create a service account key and load it into the cluster as a
`docker-registry` secret.

```bash
# 1. Create a JSON key for the puller SA
gcloud iam service-accounts keys create /tmp/spire-puller.json \
  --iam-account=spire-puller@<project>.iam.gserviceaccount.com \
  --project <project>

# 2. Create the docker-registry secret
kubectl create secret docker-registry spire-gar-pull \
  --namespace=<k8s-namespace> \
  --docker-server=<region>-docker.pkg.dev \
  --docker-username=_json_key \
  --docker-password="$(cat /tmp/spire-puller.json)" \
  --docker-email=unused@example.com

# 3. Shred the key file locally
shred -u /tmp/spire-puller.json 2>/dev/null || rm -f /tmp/spire-puller.json
```

Reference the secret from your overlay values:

```yaml
imagePullSecrets:
  - name: spire-gar-pull
```

**Trade-offs.** Works anywhere. Long-lived JSON keys are a rotation and
leakage risk — treat them as first-class secrets, rotate regularly,
and prefer A or B when available.

### Which option should I pick?

| Situation                                             | Pick     |
|-------------------------------------------------------|----------|
| GKE cluster in the same project as the registry      | A        |
| GKE cluster in a different project, or multi-tenant  | B        |
| Not GKE, or Workload Identity unavailable            | C        |

---

## 4. Manual emergency push

For use **only** when CI is unavailable and an image needs to be
pushed by hand. The operator must personally hold
`roles/artifactregistry.writer` on the `spire` repo. Should be rare.

```bash
# Authenticate the local docker client against GAR
gcloud auth configure-docker <region>-docker.pkg.dev

# Build and push the steward image
docker build -f Dockerfile.steward \
  -t <region>-docker.pkg.dev/<project>/spire/steward:<tag> .
docker push <region>-docker.pkg.dev/<project>/spire/steward:<tag>

# Build and push the agent image
docker build -f Dockerfile.agent \
  -t <region>-docker.pkg.dev/<project>/spire/agent:<tag> .
docker push <region>-docker.pkg.dev/<project>/spire/agent:<tag>
```

If you also want `:latest` to point at this build, retag and re-push:

```bash
docker tag \
  <region>-docker.pkg.dev/<project>/spire/steward:<tag> \
  <region>-docker.pkg.dev/<project>/spire/steward:latest
docker push <region>-docker.pkg.dev/<project>/spire/steward:latest
```

After a manual push, record what you did (tag, commit SHA, reason) and
re-run CI on the same tag once the outage is resolved so CI-signed
images become the source of truth again.

---

## 5. Verification

### 5.1 List images in the repo

```bash
gcloud artifacts docker images list \
  <region>-docker.pkg.dev/<project>/spire \
  --include-tags
```

You should see entries for `spire/steward` and `spire/agent` with the
expected tags.

### 5.2 Confirm a tag resolves to a digest

```bash
gcloud artifacts docker images describe \
  <region>-docker.pkg.dev/<project>/spire/steward:<tag>
```

### 5.3 Confirm the cluster can pull

From inside the cluster, run a throwaway pod that pulls the full
image reference and exits. If it succeeds, pull auth is correctly
wired for that namespace.

```bash
kubectl run pull-check \
  --namespace=<k8s-namespace> \
  --rm -it --restart=Never \
  --image=<region>-docker.pkg.dev/<project>/spire/steward:<tag> \
  --command -- /bin/true
```

A successful run prints `pod "pull-check" deleted` and exits zero.
`ErrImagePull` or `ImagePullBackOff` means pull auth is not set up
correctly for that namespace — re-check section 3.

If you chose option C, also pass `--overrides` to attach the pull
secret:

```bash
kubectl run pull-check \
  --namespace=<k8s-namespace> \
  --rm -it --restart=Never \
  --image=<region>-docker.pkg.dev/<project>/spire/steward:<tag> \
  --overrides='{"spec":{"imagePullSecrets":[{"name":"spire-gar-pull"}]}}' \
  --command -- /bin/true
```
