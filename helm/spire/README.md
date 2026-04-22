# Spire Helm Chart

Kubernetes chart for [Spire](https://github.com/awell-health/spire) — the AI
agent coordination hub. Installs the steward, operator, dolt SQL server,
syncer, and supporting resources (PriorityClasses, ResourceQuota, Secrets,
CRDs) into a single namespace.

The chart's `Chart.yaml` `version` / `appVersion` are rewritten by goreleaser
at release time; the in-git sentinel of `0.0.0` is intentional.

## Values reference

The authoritative per-parameter documentation lives alongside each value in
[`values.yaml`](values.yaml) as `## @param` comments, grouped by `## @section`
headers. The sections below summarize the contract for values that are
consumed by resources outside the chart (operator controllers, wizard pods)
and therefore have cross-component semantics worth documenting here.

### Guild repo cache storage (`cache.*`)

Deployment-time defaults for the per-guild repo-cache PVCs that the operator
reconciler materializes from each `WizardGuild.Spec.CacheSpec`. The operator
uses these values as the fallback when a guild does not specify its own
override.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `cache.storageClassName` | string | `""` | StorageClass for guild-owned repo-cache PVCs. Empty string uses the cluster's default StorageClass — consistent with `dolt.storage.storageClass` and `stewardStorage.storageClass`. Overridden per-guild via `WizardGuild.Spec.CacheSpec.StorageClassName`. |
| `cache.defaultSize` | string | `"10Gi"` | Default size for guild-owned repo-cache PVCs. Size up for monorepos with large working sets. Overridden per-guild via `WizardGuild.Spec.CacheSpec.Size`. |
| `cache.defaultAccessMode` | string | `"ReadOnlyMany"` | Default access mode for guild-owned repo-cache PVCs. Required to be RWX/ROX for cache fan-out: one refresh Job writes the mirror, many wizard pods mount it read-only. Override only if your CSI driver can't provide RWX/ROX and the guild runs one pod at a time. Overridden per-guild via `WizardGuild.Spec.CacheSpec.AccessMode`. |

Rationale for `ReadOnlyMany`: the cache is populated once per refresh cycle
by the operator-managed refresh Job (`git fetch` or `git clone --mirror`)
and then read by every wizard pod that derives a writable workspace from
it. `ReadWriteOnce` would serialize wizard pod scheduling onto a single
node and negate the point of having a shared cache.

Rationale for empty `storageClassName` default: Kubernetes treats an
explicit empty-string `storageClassName` on a PVC spec as "disable dynamic
provisioning" rather than "pick the default class." To keep a fresh install
working against the cluster's default StorageClass, the chart's
`spire.cachePVCSpec` helper omits the field entirely when this value is
empty, and only renders `storageClassName: <value>` when it is set.

#### Using the defaults from chart templates

Templates that need to render a guild-cache PVC spec MUST go through the
helpers defined in `templates/_helpers.tpl` so the chart has exactly one
source of truth for the PVC spec shape:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: example-repo-cache
  namespace: {{ .Values.namespace }}
spec:
  {{- include "spire.cachePVCSpec" . | nindent 2 }}
```

Finer-grained helpers are available when a consumer needs only one field:

- `spire.cacheDefaultStorageClassName` — raw `.Values.cache.storageClassName`
  (may be empty; callers omit the field when empty).
- `spire.cacheDefaultSize` — raw `.Values.cache.defaultSize`.
- `spire.cacheDefaultAccessMode` — raw `.Values.cache.defaultAccessMode`.

#### Consuming the defaults from the operator

The operator's cache reconciler (spi-myzn5) reads these values as the
fallback when a `WizardGuild.Spec.CacheSpec` field is unset. Wiring the
reconciler to these defaults is the reconciler task's responsibility and
is deliberately out of scope for this chart contract — the chart's
responsibility is to publish the values and the helper shape, not to
change operator runtime behavior.
