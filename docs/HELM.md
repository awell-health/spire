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

## Fresh tower, no DoltHub

If you don't have a DoltHub account yet, install with an empty
`dolthub.remoteUrl`. The dolt pod's init container will run `dolt init`
locally instead of cloning, and the steward init container's
`--bootstrap-if-blank` flag (enabled by default in the chart) runs the
equivalent of `spire tower create` against the freshly-initialized
database — schema, `_project_id`, custom bead types — so the steward lands
ready to accept work.

```bash
kubectl create namespace spire
helm install spire helm/spire \
  --namespace spire \
  --set namespace=spire \
  --set createNamespace=false \
  --set beads.prefix=spi \
  --set images.steward.tag=vX.Y.Z \
  --set images.agent.tag=vX.Y.Z \
  --set dolthub.remoteUrl="" \
  --set anthropic.apiKey=$ANTHROPIC_API_KEY
```

Verify after install:

```bash
# Wait for steward to come up.
kubectl rollout status deploy/spire-steward -n spire --timeout=2m

# Bootstrap wrote _project_id into the tower metadata table.
kubectl exec -n spire spire-dolt-0 -c dolt -- \
  dolt --host 127.0.0.1 --port 3306 --user root --no-tls -p "" \
  sql -q "USE spi; SELECT value FROM metadata WHERE \`key\`='_project_id'"

# Custom bead types are registered.
kubectl exec -n spire spire-dolt-0 -c dolt -- \
  dolt --host 127.0.0.1 --port 3306 --user root --no-tls -p "" \
  sql -q "USE spi; SELECT name FROM custom_types"

# Steward workspace metadata points at the cluster dolt server.
kubectl exec -n spire deploy/spire-steward -c steward -- \
  cat /data/.beads/metadata.json
```

A restarted steward pod re-runs the init container but the
`--bootstrap-if-blank` guard detects the populated database and logs
`database "spi" already populated — skipping bootstrap`, so project_id
remains stable across restarts.

## Multi-tenant: multiple releases in one cluster

Spire is multi-tenant-safe: each release runs fully isolated in its own
namespace with its own dolt PVC, steward, operator, and bead prefix. No
state is shared except cluster-scoped CRDs (`spireagent`, `spireconfig`,
`spireworkload`).

Cluster-scoped RBAC is release-scoped: each release installs its own
ClusterRole and ClusterRoleBinding named `<release>-operator` (e.g.
`spire-a-operator`, `spire-b-operator`), so installing or uninstalling
one release never rebinds or removes another's permissions. The
ServiceAccount stays namespace-scoped and keeps the stable name
`spire-operator`.

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
must `kubectl delete crd` manually if you want them gone. CRDs are the
only cluster-scoped resources genuinely shared across releases; the
ClusterRole/ClusterRoleBinding are per-release (see above).

> **Upgrade note for installs predating this change:** earlier chart
> versions named the ClusterRole/ClusterRoleBinding `spire-operator`
> (un-prefixed). A `helm upgrade` across the boundary deletes the old
> object and creates a release-scoped one, so the operator briefly
> loses cluster-scoped permissions mid-upgrade. The risk window is
> seconds and the operator reconciles on its own — no manual action
> required for a single-release install.

## Remotesapi SQL users

The cluster's dolt server exposes a remotesapi port (default `50051`)
so laptops/CI can `dolt clone/push/pull http://<host>:50051/<db>`
without going through DoltHub. Each client authenticates as a SQL user
on the cluster dolt.

The chart creates the primary `DOLT_REMOTE_USER` automatically via the
post-install `spire-dolt-provision` Job. For additional users (per-dev
logins, a scoped read-only role for CI, a read-only auditor account,
etc.), declare them with `dolt.additionalUsers`. Each entry supplies
its password from either an operator-managed Secret (`passwordSecret`,
preferred) or an inline `password:` string that the chart materializes
into a per-release Secret (`spire-dolt-additional-users`) so the
rendered Job spec never carries plaintext.

```yaml
# my-values.yaml — two entries, one Secret-ref, one inline.
dolt:
  additionalUsers:
    - name: alice
      passwordSecret:
        name: spire-user-alice
        key: password            # default is "password"; specify if different
      grants:
        - "ALL ON spi.*"
    - name: readonly
      password: "plaintext-discouraged"   # dev/demo only — prefer passwordSecret
      grants:
        - "SELECT ON spi.*"
```

With the Secret-ref form, pre-create each referenced Secret before
`helm install/upgrade` — the Job Pod will fail with
`CreateContainerConfigError` if the Secret is missing:

```bash
kubectl -n spire create secret generic spire-user-alice \
  --from-literal=password=$(openssl rand -base64 24 | tr -d /+= | head -c 24)

helm install spire helm/spire -n spire --values my-values.yaml
```

With the inline form, the password goes into values and the chart
renders a Kubernetes Secret named `spire-dolt-additional-users` with
keys `addl-pw-<name>`. The Job's env reads from those keys — so the
password lands in a Secret (expected k8s handling), not in the Job's
PodSpec. Rotate an inline password by re-running `helm upgrade` with a
new value; rotate a Secret-ref password with `kubectl patch secret`
followed by `helm upgrade`. In both cases the Job's `ALTER USER` step
re-applies the new password on the next run. The paired
`CREATE USER IF NOT EXISTS` makes subsequent installs a no-op when no
entries have changed.

Notes:

- **`name` is validated at Helm render time against
  `^[a-zA-Z0-9_]{1,32}$`.** Values like `alice;DROP`, `bob spaces`, or
  64-char strings fail the render with a clear error — the Job
  manifest never reaches the cluster.
- **Single quotes are rejected** in `host`, `grants`, and the password
  itself. Helm render fails for host/grants and the Job exits non-zero
  at runtime for passwords rather than generating quote-escaped SQL.
- **Exactly one of `passwordSecret.name` or `password` must be set
  per entry** — render fails otherwise.
- **Operator-managed Secrets must exist before the Job Pod schedules.**
  If they don't, inspect with
  `kubectl -n spire describe pod -l app.kubernetes.io/name=spire-dolt-additional-users`.
- **Grant revocation on removal is NOT automatic.** Dropping an entry
  from `additionalUsers` on `helm upgrade` leaves the SQL user in
  place — drop it by hand:
  `kubectl exec statefulset/spire-dolt -- dolt sql -q "DROP USER 'old_user'@'%'"`.

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
