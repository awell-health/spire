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
