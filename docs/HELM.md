# Helm Install Guide

Spire ships a Helm chart at `helm/spire` that deploys the full
coordination stack — dolt, steward, operator, sidecar, syncer, and
SpireAgent CRDs — into a Kubernetes cluster.

## Prerequisites

- Kubernetes 1.27+ (minikube, kind, EKS, GKE, etc.)
- `helm` 3.12+
- `kubectl` pointed at the target cluster
- Container images published to a registry the cluster can pull from.
  The default is `ghcr.io/awell-health/spire-steward` /
  `ghcr.io/awell-health/spire-agent` — see the release pipeline for tags.
- A DoltHub remote (e.g. `my-org/my-tower`) and a local DoltHub JWK
  credential from `~/.dolt/creds/<keyid>.jwk`, created with
  `dolt creds new && dolt creds use <keyid>` on a workstation that's
  already authenticated to your DoltHub account.
- An Anthropic API key or OAuth token.

## Single-release install

```bash
# 1. Create a namespace.
kubectl create namespace spire

# 2. Create the DoltHub credentials secret.
kubectl -n spire create secret generic dolt-creds \
  --from-file="<keyid>.jwk=$HOME/.dolt/creds/<keyid>.jwk"

# 3. Install the chart.
helm install spire helm/spire \
  --namespace spire \
  --set namespace=spire \
  --set createNamespace=false \
  --set beads.prefix=spi \
  --set images.steward.tag=vX.Y.Z \
  --set images.agent.tag=vX.Y.Z \
  --set dolthub.remote=my-org/my-tower \
  --set dolthub.credsSecretName=dolt-creds \
  --set dolthub.keyId=<keyid> \
  --set anthropic.apiKey=$ANTHROPIC_API_KEY
```

The chart has two namespace-related values that both need to match the
`--namespace` flag:

- `namespace` — written into every resource's `metadata.namespace`.
- `createNamespace` — if `true`, the chart renders a `Namespace`
  resource. Set to `false` when you pre-create the namespace (as above)
  or when you use `helm install --create-namespace`.

## Multi-tenant: multiple releases in one cluster

Spire is multi-tenant-safe: each release runs fully isolated in its own
namespace with its own dolt PVC, steward, operator, and bead prefix. No
state is shared except cluster-scoped CRDs (`spireagent`, `spireconfig`,
`spireworkload`).

```bash
# Release A — prefix "a", namespace spire-a.
kubectl create namespace spire-a
kubectl -n spire-a create secret generic dolt-creds \
  --from-file="<keyid>.jwk=$HOME/.dolt/creds/<keyid>.jwk"
helm install spire-a helm/spire \
  --namespace spire-a \
  --set namespace=spire-a \
  --set createNamespace=false \
  --set beads.prefix=a \
  --set images.steward.tag=vX.Y.Z \
  --set images.agent.tag=vX.Y.Z \
  --set dolthub.remote=my-org/tower-a \
  --set dolthub.credsSecretName=dolt-creds \
  --set dolthub.keyId=<keyid>

# Release B — prefix "b", namespace spire-b.
kubectl create namespace spire-b
kubectl -n spire-b create secret generic dolt-creds \
  --from-file="<keyid>.jwk=$HOME/.dolt/creds/<keyid>.jwk"
helm install spire-b helm/spire \
  --namespace spire-b \
  --set namespace=spire-b \
  --set createNamespace=false \
  --set beads.prefix=b \
  --set images.steward.tag=vX.Y.Z \
  --set images.agent.tag=vX.Y.Z \
  --set dolthub.remote=my-org/tower-b \
  --set dolthub.credsSecretName=dolt-creds \
  --set dolthub.keyId=<keyid>
```

Because the CRDs are cluster-scoped and shipped in `helm/spire/crds/`,
only the first release installs them — subsequent releases reuse the
existing CRDs. `helm uninstall` does not remove CRDs (by design); you
must `kubectl delete crd` manually if you want them gone.

## Verifying isolation

```bash
# Pods in each release.
kubectl get pods -n spire-a
kubectl get pods -n spire-b

# Bead prefix differs per release.
kubectl exec -n spire-a deploy/spire-steward -c steward -- sh -c 'echo $BEADS_PREFIX'
# → a
kubectl exec -n spire-b deploy/spire-steward -c steward -- sh -c 'echo $BEADS_PREFIX'
# → b

# Each operator scopes to its own namespace.
kubectl get deploy/spire-operator -n spire-a \
  -o jsonpath='{.spec.template.spec.containers[0].args}'
# → [--namespace=spire-a ...]

# PVCs are per-namespace.
kubectl get pvc -n spire-a
kubectl get pvc -n spire-b
```

## Running the smoke test locally

The repo ships a scripted end-to-end check that installs both releases,
verifies isolation, and tears them down:

```bash
IMAGE_TAG=vX.Y.Z make smoke-test-helm
```

The script reads these optional environment variables:

| Var | Purpose |
|-----|---------|
| `IMAGE_TAG` | image tag for steward/agent (default `latest`) |
| `DOLTHUB_REMOTE_A` | DoltHub remote for release A |
| `DOLTHUB_REMOTE_B` | DoltHub remote for release B |
| `DOLT_CREDS_FILE` | path to a `.jwk` creds file |
| `DOLT_CREDS_KEY_ID` | key id (filename without `.jwk`) |
| `ANTHROPIC_API_KEY` | Anthropic API key |
| `KEEP_NAMESPACES` | set to `1` to skip teardown |

If the DoltHub / Anthropic vars are omitted, the chart installs with
empty values — the dolt pod still comes up, but the steward
init-container (`spire tower attach-cluster`) won't be able to clone
a real tower. For smoke-testing namespace isolation only, that's fine;
for a real install, provide all of them.

## Uninstall

```bash
helm uninstall spire-a -n spire-a && kubectl delete namespace spire-a
helm uninstall spire-b -n spire-b && kubectl delete namespace spire-b

# Optional: remove cluster-scoped CRDs (only do this when no other
# Spire release is still using them).
kubectl delete crd spireagents.spire.awellhealth.com
kubectl delete crd spireconfigs.spire.awellhealth.com
kubectl delete crd spireworkloads.spire.awellhealth.com
```
