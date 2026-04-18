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
