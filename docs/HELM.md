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

The chart renders the DoltHub-creds Secret itself from two inline values —
`dolthub.credsKeyId` (the key id shown by `dolt creds ls`) and
`dolthub.credsKeyValue` (the raw JWK file contents). Pass the JWK file
with `--set-file` so helm reads it from disk rather than trying to parse
it as a flag value.

```bash
# 1. Create a namespace.
kubectl create namespace spire

# 2. Install the chart — DoltHub Secret is rendered by the chart.
helm install spire helm/spire \
  --namespace spire \
  --set namespace=spire \
  --set createNamespace=false \
  --set beads.prefix=spi \
  --set images.steward.tag=vX.Y.Z \
  --set images.agent.tag=vX.Y.Z \
  --set dolthub.remoteUrl=my-org/my-tower \
  --set dolthub.credsKeyId=<keyid> \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<keyid>.jwk \
  --set anthropic.apiKey=$ANTHROPIC_API_KEY
```

If you prefer to manage secrets outside helm (sealed-secrets, external-secrets,
vault-injector), set `existingSecret` to the name of a pre-created Secret that
carries the same keys the chart would render. The chart will skip rendering
its own `spire-credentials` in that case.

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
helm install spire-a helm/spire \
  --namespace spire-a \
  --set namespace=spire-a \
  --set createNamespace=false \
  --set beads.prefix=a \
  --set images.steward.tag=vX.Y.Z \
  --set images.agent.tag=vX.Y.Z \
  --set dolthub.remoteUrl=my-org/tower-a \
  --set dolthub.credsKeyId=<keyid> \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<keyid>.jwk

# Release B — prefix "b", namespace spire-b.
kubectl create namespace spire-b
helm install spire-b helm/spire \
  --namespace spire-b \
  --set namespace=spire-b \
  --set createNamespace=false \
  --set beads.prefix=b \
  --set images.steward.tag=vX.Y.Z \
  --set images.agent.tag=vX.Y.Z \
  --set dolthub.remoteUrl=my-org/tower-b \
  --set dolthub.credsKeyId=<keyid> \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<keyid>.jwk
```

Because the CRDs are cluster-scoped and shipped in `helm/spire/crds/`,
only the first release installs them — subsequent releases reuse the
existing CRDs. `helm uninstall` does not remove CRDs (by design); you
must `kubectl delete crd` manually if you want them gone.

## Remotesapi SQL users

The cluster's dolt server exposes a remotesapi port (default `50051`)
so laptops/CI can `dolt clone/push/pull http://<host>:50051/<db>`
without going through DoltHub. Each client authenticates as a SQL user
on the cluster dolt.

The chart creates the primary `DOLT_REMOTE_USER` automatically via the
post-install `spire-dolt-provision` Job. For additional users (per-dev
logins, a scoped read-only role for CI, etc.), use
`dolt.additionalUsers` and pre-create a k8s Secret per entry:

```yaml
# my-values.yaml
dolt:
  additionalUsers:
    - name: dolt_remote_ci
      host: "%"
      existingSecret: dolt-remote-ci-credentials
      secretKey: password
      grants:
        - "ALL PRIVILEGES ON *.*"
    - name: analyst
      host: "%"
      existingSecret: dolt-analyst-credentials
      secretKey: password
      grants:
        - "SELECT ON *.*"
```

```bash
# Pre-create the Secrets before `helm install/upgrade`.
kubectl -n spire create secret generic dolt-remote-ci-credentials \
  --from-literal=password=$(openssl rand -base64 24 | tr -d /+= | head -c 24)
kubectl -n spire create secret generic dolt-analyst-credentials \
  --from-literal=password=$(openssl rand -base64 24 | tr -d /+= | head -c 24)

helm install spire helm/spire -n spire --values my-values.yaml
```

On install/upgrade the chart renders `spire-dolt-additional-users`, a
post-install/post-upgrade hook Job. It waits for dolt to be ready, then
runs idempotent `CREATE USER IF NOT EXISTS … ALTER USER … IDENTIFIED
BY … GRANT …` for every entry. Passwords are mounted from the named
Secrets at Pod-runtime via `valueFrom.secretKeyRef`, so the rendered
manifest contains Secret references but no plaintext.

Rotation is `kubectl patch secret <name>` followed by `helm upgrade`;
the Job re-runs and `ALTER USER` re-applies the new password. The
paired `CREATE USER IF NOT EXISTS` means initial provisioning is a
no-op on subsequent upgrades that don't add new entries.

Notes:

- **Single quotes are rejected** in `name`, `host`, `grants`, and the
  password itself. The chart refuses to render (and the Job exits
  non-zero at runtime for the password check) rather than generate
  quote-escaped SQL. Pick identifiers without `'`.
- **The referenced Secret must exist before the Job Pod schedules.**
  If it doesn't, the Pod stays in `CreateContainerConfigError` —
  inspect with `kubectl -n spire describe pod -l app.kubernetes.io/name=spire-dolt-additional-users`.
- **Inline passwords are deliberately unsupported.** There is no
  `password: "…"` field; every entry must use `existingSecret`.
- The Job does not delete users that were removed from the values list.
  Drop them from dolt by hand: `kubectl exec deploy/spire-dolt -c dolt
  -- dolt sql -q "DROP USER 'old_user'@'%'"`.

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
