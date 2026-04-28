{{/*
spire.secretName returns the name of the Secret holding all chart
credentials. When .Values.existingSecret is set the chart does not
render its own Secret and every consumer references the external
one by this name; otherwise it falls back to "<release>-credentials".
*/}}
{{- define "spire.secretName" -}}
{{- .Values.existingSecret | default (printf "%s-credentials" .Release.Name) -}}
{{- end -}}

{{/*
spire.database — canonical database / tower name. Every template that
needs this value (volume subpaths, --data-dir, attach-cluster --database,
dolt backup subdir, etc) MUST use this helper rather than repeating the
`beads.database | default beads.prefix` expression, so there is exactly
one place that decides what the database is called.
*/}}
{{- define "spire.database" -}}
{{ .Values.beads.database | default .Values.beads.prefix }}
{{- end -}}

{{/*
spire.dbDataDir — per-database dolt data dir (`<dataRoot>/<database>`).
Matches the laptop convention `$DOLT_DATA_DIR/<db>`, so setting
DOLT_DATA_DIR=<dataRoot> in-pod lets `BeadsDirForTower()` resolve a
tower's .beads/ without any persisted per-tower overrides. Used by
every `spire tower attach-cluster --data-dir=...` invocation.
*/}}
{{- define "spire.dbDataDir" -}}
{{ .Values.paths.dataRoot }}/{{ include "spire.database" . }}
{{- end -}}

{{/*
spire.beadsDir — the tower's `.beads/` workspace (`<dbDataDir>/.beads`).
This is where `attach-cluster` seeds the workspace and where the steward,
operator, and syncer read work state. Derived, not a knob.
*/}}
{{- define "spire.beadsDir" -}}
{{ include "spire.dbDataDir" . }}/.beads
{{- end -}}

{{/*
spire.configDir — SPIRE_CONFIG_DIR for every Spire-owned container
(init and main). Points at a subdir of the shared PVC so that the tower
config written by the init container's `attach-cluster` survives into
the main container, where the steward loads it via ListTowerConfigs().
Without this persistence, the init's ephemeral filesystem discards the
file and the main container reports "no tower configured".
*/}}
{{- define "spire.configDir" -}}
{{ .Values.paths.dataRoot }}/spire-config
{{- end -}}

{{/*
spire.gcpSecretName — resolved name of the Secret holding the shared
GCP service-account JSON. Three templates reference this (secret-gcp,
dolt, steward) so the helper is the single source of truth for the
name resolution rule: when `.Values.gcp.secretName` is set it wins
(externally-managed secret); otherwise the chart renders
`<release>-gcp-sa`. Returning the same name from all three templates
keeps the three features mounting the same Secret object.
*/}}
{{- define "spire.gcpSecretName" -}}
{{- .Values.gcp.secretName | default (printf "%s-gcp-sa" .Release.Name) -}}
{{- end -}}

{{/*
spire.gcpAuthConfigured — returns "true" when at least one GCP auth
path is configured for backup/bundleStore consumers. Two acceptable
paths today:
  1. `.Values.gcp.serviceAccountJson` non-empty — chart-managed Secret.
  2. `.Values.gcp.secretName` non-empty — externally-managed Secret
     (sealed-secrets, external-secrets-operator, Workload Identity
     placeholder, etc.). The consumer mounts this Secret by name; the
     chart does NOT render its contents.
Empty string when neither is set. Read by `spire.validateBackup` and by
the dolt/steward templates so the credential volume renders for both
auth shapes.
*/}}
{{- define "spire.gcpAuthConfigured" -}}
{{- if or .Values.gcp.serviceAccountJson .Values.gcp.secretName -}}true{{- end -}}
{{- end -}}

{{/*
spire.validateBackup — fail-fast validation for cluster-as-truth backup
config. Called from NOTES.txt so it runs unconditionally during
`helm template` and `helm install`/`helm upgrade`. Fires two checks
in sequence when `.Values.backup.enabled` is true:

  1. `.Values.backup.gcs.bucket` empty → fail. Without a bucket the
     chart would otherwise render `BACKUP_URL="gs:///"` in the dolt-init
     ConfigMap, which `dolt backup add` accepts but `dolt backup sync`
     fails on at runtime — the deployment looks healthy until the first
     backup attempt.
  2. No GCP auth path configured → fail. Either `gcp.serviceAccountJson`
     (inline JSON, materialized into the chart-rendered Secret) or
     `gcp.secretName` (externally-managed Secret name, used when an
     ESO/sealed-secrets/Workload-Identity flow injects creds) must be
     non-empty. Without one, the dolt pod can't authenticate to GCS.

Disposable/dev clusters that explicitly do not need disaster recovery
opt out via `--set backup.enabled=false`; that bypasses both checks.
The failure messages link to the install-ritual sections of
docs/cluster-deployment.md and k8s/DEPLOY.md so users can self-serve.
*/}}
{{- define "spire.validateBackup" -}}
{{- if .Values.backup.enabled -}}
  {{- if not .Values.backup.gcs.bucket -}}
{{- fail (printf "%s\n  - Set --set backup.gcs.bucket=<bucket-name> (the bucket MUST pre-exist).\n  - Or --set backup.enabled=false ONLY for disposable/dev clusters that do not need DR.\n  See docs/cluster-deployment.md (Backup bucket setup) and k8s/DEPLOY.md §1 (Prerequisites)." "backup.enabled=true requires backup.gcs.bucket — cluster-as-truth deployments use GCS as the disaster-recovery substrate.") -}}
  {{- end -}}
  {{- if not (include "spire.gcpAuthConfigured" .) -}}
{{- fail (printf "%s\n  - Inline JSON: --set-file gcp.serviceAccountJson=<path-to-sa.json>.\n  - External Secret: --set gcp.secretName=<existing-secret-name> (sealed-secrets, external-secrets-operator, or a Workload-Identity placeholder Secret).\n  - Or --set backup.enabled=false ONLY for disposable/dev clusters that do not need DR.\n  See docs/cluster-deployment.md (GCP auth) and k8s/DEPLOY.md §1 (Prerequisites)." "backup.enabled=true requires a GCP auth path — neither gcp.serviceAccountJson nor gcp.secretName is set.") -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
spire.validateLogStore — fail-fast validation for the cluster log
artifact store (design spi-7wzwk2). Called from NOTES.txt so it runs
unconditionally during `helm template` and `helm install`/`helm upgrade`,
matching the `spire.validateBackup` shape. Fires two checks in sequence
when `.Values.logStore.backend` is `gcs`:

  1. `.Values.logStore.gcs.bucket` empty → fail. Without a bucket the
     gateway and exporter would emit object URIs against an empty
     bucket name, which surfaces as opaque GCS errors at first write.
  2. No GCP auth path configured → fail. Either `gcp.serviceAccountJson`
     (inline JSON, materialized into the chart-rendered Secret) or
     `gcp.secretName` (externally-managed Secret name, used when an
     ESO/sealed-secrets/Workload-Identity flow injects creds) must be
     non-empty. The same path is reused for bundleStore and backup —
     this validator does NOT introduce a second auth model per the
     spi-hzeyz9 scope.

The default `logStore.backend=local` skips both checks; local-native
installs render no GCS-related templates and need no GCP auth.
*/}}
{{- define "spire.validateLogStore" -}}
{{- if eq .Values.logStore.backend "gcs" -}}
  {{- if not .Values.logStore.gcs.bucket -}}
{{- fail (printf "%s\n  - Set --set logStore.gcs.bucket=<bucket-name> (the bucket MUST pre-exist; use a DEDICATED log bucket separate from bundleStore.gcs.bucket and backup.gcs.bucket).\n  - Or --set logStore.backend=local to keep logs on the wizard data directory (local-native default).\n  See helm/spire/values.yaml (logStore section) and docs/cluster-deployment.md (Log artifact bucket setup)." "logStore.backend=gcs requires logStore.gcs.bucket — cluster log export uses GCS as the durable artifact substrate.") -}}
  {{- end -}}
  {{- if not (include "spire.gcpAuthConfigured" .) -}}
{{- fail (printf "%s\n  - Inline JSON: --set-file gcp.serviceAccountJson=<path-to-sa.json>.\n  - External Secret: --set gcp.secretName=<existing-secret-name> (sealed-secrets, external-secrets-operator, or a Workload-Identity placeholder Secret).\n  - Or --set logStore.backend=local to keep logs on the wizard data directory.\n  The same gcp.* path is reused by bundleStore and backup — there is no separate logStore credential field." "logStore.backend=gcs requires a GCP auth path — neither gcp.serviceAccountJson nor gcp.secretName is set.") -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
spire.logStoreEnv — LOGSTORE_BACKEND + LOGSTORE_GCS_BUCKET +
LOGSTORE_GCS_PREFIX + LOGSTORE_RETENTION_DAYS env entries for any
component that consumes the log artifact substrate. The pkg/logartifact
GCS backend reads these to construct a Store; the local backend reads
LOGSTORE_BACKEND alone and falls through to its data-directory default.

Emits nothing when `.Values.logStore.backend` is unset/empty so a
bare-bones install doesn't carry pointer-only env. When the backend is
`local` we still emit LOGSTORE_BACKEND=local so consumers don't have to
disambiguate "unset" from "explicitly local"; the GCS-specific keys are
omitted in that case.

Consumed by the gateway and operator deployments, and propagated onto
wizard / apprentice / sage pods via SPIRE_LOGSTORE_* env on the operator
(read by main.go and stamped onto every PodSpec).

The emitted block starts at column 0; callers indent with `nindent 12`
under the container's `env:` key.
*/}}
{{- define "spire.logStoreEnv" -}}
{{- if .Values.logStore.backend }}
- name: LOGSTORE_BACKEND
  value: {{ .Values.logStore.backend | quote }}
{{- if eq .Values.logStore.backend "gcs" }}
- name: LOGSTORE_GCS_BUCKET
  value: {{ .Values.logStore.gcs.bucket | quote }}
- name: LOGSTORE_GCS_PREFIX
  value: {{ .Values.logStore.gcs.prefix | quote }}
- name: LOGSTORE_RETENTION_DAYS
  value: {{ .Values.logStore.retentionDays | quote }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
spire.logStoreNeedsGCP — returns "true" when the log store backend
needs the shared GCP credential mounted onto the consumer (gateway,
operator, or worker pod). Returns empty string otherwise.

Used by deployment templates to gate the GCP volume + mount + ADC env
without duplicating the `eq .Values.logStore.backend "gcs"` test.
Decoupled from `bundleStore.backend` because the two features can
opt-in independently — a tower might run with bundleStore=local and
logStore=gcs once spi-k1cnof ships local-bundle support.
*/}}
{{- define "spire.logStoreNeedsGCP" -}}
{{- if eq .Values.logStore.backend "gcs" -}}true{{- end -}}
{{- end -}}

{{/*
spire.gcpNeeded — returns "true" when ANY GCP-consuming feature is
enabled (bundleStore=gcs OR logStore=gcs). Empty string when none are.
Used by deployment templates that mount the shared `gcp-sa` Secret
volume so the volume rendering doesn't have to enumerate every feature
flag.

This helper composes existing per-feature gates rather than introducing
a new auth model — the gcp.* credential block is shared, so any one
GCS-using feature is enough to require the mount.
*/}}
{{- define "spire.gcpNeeded" -}}
{{- if or (eq .Values.bundleStore.backend "gcs") (eq .Values.logStore.backend "gcs") -}}true{{- end -}}
{{- end -}}

{{/*
spire.additionalUsersSecretName — name of the chart-managed Secret that
holds inline passwords (from `entry.password`) for dolt.additionalUsers.
Kept separate from `spire.secretName` so external-secret setups don't
need to know about inline additional-user passwords, and so this Secret
only renders when at least one inline password is present.
*/}}
{{- define "spire.additionalUsersSecretName" -}}
spire-dolt-additional-users
{{- end -}}

{{/*
spire.validateAdditionalUserName — asserts that `name` matches
`^[a-zA-Z0-9_]{1,32}$` and `fail`s the render otherwise. Runs at render
time so that a values.yaml with `name: "alice'; DROP USER"` or `name:
"bob spaces"` never produces a Job manifest. Call per entry during
range iteration.
Usage: {{- include "spire.validateAdditionalUserName" (list $i $user.name) -}}
*/}}
{{- define "spire.validateAdditionalUserName" -}}
{{- $i := index . 0 -}}
{{- $name := index . 1 -}}
{{- if not $name -}}
{{- fail (printf "dolt.additionalUsers[%d].name is required" $i) -}}
{{- end -}}
{{- if not (regexMatch "^[a-zA-Z0-9_]{1,32}$" $name) -}}
{{- fail (printf "dolt.additionalUsers[%d].name %q is invalid: must match ^[a-zA-Z0-9_]{1,32}$ (letters, digits, underscore; 1-32 chars)" $i $name) -}}
{{- end -}}
{{- end -}}

{{/*
spire.additionalUserPasswordRef — renders the `valueFrom.secretKeyRef`
block for one additionalUsers entry's password env var. Resolves the
Secret+key source in priority order:
  1. `entry.passwordSecret.name` set → operator-managed Secret, with key
     `entry.passwordSecret.key` (default "password").
  2. `entry.password` set → chart-managed Secret
     (`spire.additionalUsersSecretName`) with key "addl-pw-<name>".
  3. Neither set → `fail` the render.
The emitted block starts at column 0; callers must indent with
`nindent`. Inline passwords never appear in the rendered Job — only a
`secretKeyRef` pointing at the chart-managed Secret whose data is
base64-encoded from values.
Usage:
  {{- include "spire.additionalUserPasswordRef" (dict "user" $user "i" $i) | nindent 14 }}
*/}}
{{- define "spire.additionalUserPasswordRef" -}}
{{- $user := .user -}}
{{- $i := .i -}}
{{- if and $user.passwordSecret $user.passwordSecret.name -}}
valueFrom:
  secretKeyRef:
    name: {{ $user.passwordSecret.name }}
    key: {{ $user.passwordSecret.key | default "password" }}
{{- else if $user.password -}}
valueFrom:
  secretKeyRef:
    name: {{ include "spire.additionalUsersSecretName" . }}
    key: addl-pw-{{ $user.name }}
{{- else -}}
{{- fail (printf "dolt.additionalUsers[%d] (%q) must set either passwordSecret.name or password" $i $user.name) -}}
{{- end -}}
{{- end -}}

{{/*
spire.additionalUsersHasInline — returns "true" if at least one entry
in dolt.additionalUsers carries an inline `password` (i.e. something
the chart must render into a Secret). Empty string otherwise. Used to
gate rendering of the chart-managed inline-password Secret.
*/}}
{{- define "spire.additionalUsersHasInline" -}}
{{- $hit := "" -}}
{{- range .Values.dolt.additionalUsers -}}
{{- if and (not (and .passwordSecret .passwordSecret.name)) .password -}}
{{- $hit = "true" -}}
{{- end -}}
{{- end -}}
{{- $hit -}}
{{- end -}}

{{/*
spire.clickhouseDSN — in-cluster ClickHouse DSN for the chart-rendered
StatefulSet. Uses the native protocol port (9000) because the Go
clickhouse driver speaks native, not HTTP. The database path segment
resolves via `.Values.clickhouse.database` so operators can rename the
target DB without having to override this helper.
Only meaningful when `.Values.clickhouse.enabled=true`.
*/}}
{{- define "spire.clickhouseDSN" -}}
clickhouse://spire-clickhouse.{{ .Values.namespace }}.svc:{{ .Values.clickhouse.ports.native }}/{{ .Values.clickhouse.database }}
{{- end -}}

{{/*
spire.olapEnv — SPIRE_OLAP_BACKEND + SPIRE_CLICKHOUSE_DSN env entries,
conditional on `.Values.clickhouse.enabled`. Emits nothing when
ClickHouse is disabled so local-native installs keep their DuckDB
defaults (and steward/operator pods don't carry an env that would
force a connect to a service that isn't there).

Consumed by steward.yaml, operator.yaml, gateway-deployment.yaml, and
any other Spire-owned Deployment that opens OLAP. The gateway needs it
because GET /api/v1/metrics opens the OLAP backend; without these env
vars it falls back to the duckdb default and serves 503 from a
CGO-off agent image. The operator also projects the same two vars
onto every wizard pod it builds (see `pkg/agent/pod_builder.go` and
`operator/controllers/agent_monitor.go`) so apprentice/sage
subprocesses route their OLAP writes the same way.

The emitted block starts at column 0; callers indent with `nindent 12`
under the container's `env:` key.
*/}}
{{- define "spire.olapEnv" -}}
{{- if .Values.clickhouse.enabled }}
- name: SPIRE_OLAP_BACKEND
  value: "clickhouse"
- name: SPIRE_CLICKHOUSE_DSN
  value: {{ include "spire.clickhouseDSN" . | quote }}
{{- end }}
{{- end -}}

{{/*
spire.stewardCommonEnv — env block shared by both containers of the
steward Deployment (main steward and sidecar router). Emits the set
of variables that let each container resolve the tower's per-database
paths and reach the in-cluster dolt server. Main-only entries
(STEWARD_INTERVAL/BACKEND/METRICS_PORT, SPIRE_AGENT_IMAGE,
SPIRE_CREDENTIALS_SECRET) and conditional ones (GOOGLE_APPLICATION_CREDENTIALS
when bundleStore.backend=gcs) are layered in AFTER this include at the
call site — they must never leak into the sidecar via this partial.

Keeping the shared block here prevents the two env lists from drifting
apart, which previously caused the sidecar to fall back to /data/.beads
and fail with "no tower configured" when the canonical per-database
layout was `<dataRoot>/<database>/.beads`.

The partial emits `- name:`/`value:` list items starting at column 0;
callers indent with `nindent 12` under the container's `env:` key.
*/}}
{{/*
spire.gatewaySecretName — Secret name for the gateway's SPIRE_API_TOKEN.
Co-resident with the main spire Secret but separate so the token can be
rotated without re-rolling the whole release.
*/}}
{{- define "spire.gatewaySecretName" -}}
spire-gateway-auth
{{- end -}}

{{/*
spire.name — chart name, truncated to the 63-char k8s label limit and
trimmed of trailing dashes. Used as the canonical `app.kubernetes.io/name`
value on resources whose selector is release-agnostic (ingress,
managed-cert, backend-config), and as the chart-identifier component of
`spire.fullname`.
*/}}
{{- define "spire.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
spire.fullname — fully-qualified release-scoped name. Standard helm
scaffolding: when the release name already contains the chart name (e.g.
`helm install spire ./spire`) we emit just the release name; otherwise
we join `<release>-<chart>` so two releases of this chart into one
namespace don't collide. Callers append their own suffix
(`"-gateway"`, `"-gateway-cert"`) so sibling resources stay grouped
under one prefix — the derivation MUST match between the Ingress, the
ManagedCertificate, and the BackendConfig or GKE won't wire them up.
*/}}
{{- define "spire.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
spire.chart — chart name + version for the `helm.sh/chart` label.
Replaces `+` with `_` so pre-release suffixes stay label-safe.
*/}}
{{- define "spire.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
spire.labels — standard `app.kubernetes.io/*` label block. Callers emit
these under `metadata.labels` so `kubectl -l app.kubernetes.io/part-of=spire`
filters hit every chart-owned resource. Existing pre-split templates
(gateway.yaml, dolt.yaml, steward.yaml) hardcode a narrower label set
tied to pod selectors; this helper is for new resources (ingress,
managed-cert, backend-config) that don't double as selectors.
*/}}
{{- define "spire.labels" -}}
helm.sh/chart: {{ include "spire.chart" . }}
{{ include "spire.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: spire
{{- end -}}

{{/*
spire.selectorLabels — subset of `spire.labels` safe for use inside a
Deployment/Service selector (values never change across upgrades so
selectors don't go stale). Kept separate from `spire.labels` so the
broader label block can grow without invalidating existing selectors.
*/}}
{{- define "spire.selectorLabels" -}}
app.kubernetes.io/name: {{ include "spire.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "spire.stewardCommonEnv" -}}
- name: BEADS_DIR
  value: {{ include "spire.beadsDir" . | quote }}
- name: DOLT_DATA_DIR
  value: {{ .Values.paths.dataRoot | quote }}
- name: SPIRE_CONFIG_DIR
  value: {{ include "spire.configDir" . | quote }}
- name: BEADS_PREFIX
  value: {{ .Values.beads.prefix | quote }}
- name: DOLT_HOST
  value: "spire-dolt.{{ .Values.namespace }}.svc"
- name: DOLT_PORT
  value: {{ .Values.dolt.port | quote }}
- name: BEADS_DOLT_SERVER_HOST
  value: "spire-dolt.{{ .Values.namespace }}.svc"
- name: BEADS_DOLT_SERVER_PORT
  value: {{ .Values.dolt.port | quote }}
{{- end -}}

{{/*
spire.cacheDefaultStorageClassName — deployment-time default StorageClass
for guild-owned repo-cache PVCs. Reads from `.Values.cache.storageClassName`.
Empty string means "use the cluster default StorageClass" — callers that
render PVC specs MUST omit the `storageClassName` field entirely when this
returns empty, because Kubernetes treats an explicit empty-string
storageClassName as "disable dynamic provisioning" rather than "pick the
default". See `spire.cachePVCSpec` for the correct rendering pattern.
*/}}
{{- define "spire.cacheDefaultStorageClassName" -}}
{{ .Values.cache.storageClassName }}
{{- end -}}

{{/*
spire.cacheDefaultSize — deployment-time default size for guild-owned
repo-cache PVCs. Reads from `.Values.cache.defaultSize`. Used by the
operator cache reconciler (spi-myzn5) as the fallback when a
WizardGuild's `CacheSpec.Size` is unset.
*/}}
{{- define "spire.cacheDefaultSize" -}}
{{ .Values.cache.defaultSize }}
{{- end -}}

{{/*
spire.cacheDefaultAccessMode — deployment-time default access mode for
guild-owned repo-cache PVCs. Reads from `.Values.cache.defaultAccessMode`.
Defaults to `ReadOnlyMany` because guild caches are populated by one
refresh Job and fanned out read-only to many wizard pods. Callers that
can't satisfy RWX/ROX should override per-guild via `CacheSpec.AccessMode`
rather than flipping the chart-wide default.
*/}}
{{- define "spire.cacheDefaultAccessMode" -}}
{{ .Values.cache.defaultAccessMode }}
{{- end -}}

{{/*
spire.cachePVCSpec — renders the PersistentVolumeClaim `spec:` body for
a guild-owned repo cache, using `.Values.cache.*` as defaults. This is
the single source of truth for the shape of a guild-cache PVC spec:
future templates that render cache PVCs (e.g. a per-guild cache PVC
template wired up by spi-myzn5 once the operator reconciler needs a
Helm-rendered variant) MUST go through this helper rather than
hand-rolling accessModes / resources / storageClassName.

The emitted block is the contents of `spec:` (NOT including the `spec:`
key itself), so callers indent with `nindent` underneath their own
`spec:`:

  spec:
    {{- include "spire.cachePVCSpec" . | nindent 4 }}

Empty `storageClassName` is deliberately NOT rendered — Kubernetes
treats an explicit empty-string value as "disable dynamic provisioning"
rather than "use the cluster default", so the field is omitted when the
default is unset. Set `.Values.cache.storageClassName` to pin a specific
CSI class (EFS, Filestore, Azure Files, etc.) that supports the chosen
access mode.
*/}}
{{- define "spire.cachePVCSpec" -}}
accessModes:
  - {{ include "spire.cacheDefaultAccessMode" . }}
resources:
  requests:
    storage: {{ include "spire.cacheDefaultSize" . }}
{{- $sc := include "spire.cacheDefaultStorageClassName" . -}}
{{- if $sc }}
storageClassName: {{ $sc }}
{{- end }}
{{- end -}}
