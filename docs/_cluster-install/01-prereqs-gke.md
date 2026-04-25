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

### DoltHub tower repo

The cluster attaches to a tower hosted on DoltHub. Create a free DoltHub account at https://www.dolthub.com and create an empty data repository — see https://www.dolthub.com/data-repositories for the listing. Use the repo path `acmeco/spire-tower` (replace `acmeco` with your DoltHub org or username) when you configure the chart later.

You will also need a DoltHub credential keypair on the laptop you push from. Generate one if you do not already have it:

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
