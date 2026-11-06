{{/* The release's full name, used for the gateway's objects. */}}
{{- define "toolgate.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
The name of cluster-scoped objects: the full name and the release namespace,
so two releases in different namespaces don't collide.
*/}}
{{- define "toolgate.clusterName" -}}
{{- printf "%s-%s" (include "toolgate.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "toolgate.serviceAccountName" -}}
{{- default (include "toolgate.fullname" .) .Values.config.serviceAccount.name -}}
{{- end -}}

{{/*
"true" when the migration hook Job renders: a migration Secret is set and the
gateway doesn't migrate on start. Empty otherwise.
*/}}
{{- define "toolgate.migrateJobEnabled" -}}
{{- if and .Values.config.database.migrationSecret (not .Values.config.database.migrateOnStart) -}}true{{- end -}}
{{- end -}}

{{- define "toolgate.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Labels of the chart's own objects. They use the managed-by value Helm sets,
not toolgate's, so the gateway's informers and orphan sweep never see them.
*/}}
{{- define "toolgate.labels" -}}
{{ include "toolgate.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/*
Applies tpl to every string in .map, in place, with .root as the context. An
"enabled" string becomes a bool and a "replicas" string an int, so these
fields can follow `config`.
*/}}
{{- define "toolgate.tplValues" -}}
{{- $root := .root -}}
{{- $map := .map -}}
{{- range $key, $value := $map -}}
  {{- if kindIs "string" $value -}}
    {{- $rendered := tpl $value $root -}}
    {{- if eq $key "enabled" -}}
      {{- $_ := set $map $key (eq $rendered "true") -}}
    {{- else if eq $key "replicas" -}}
      {{- $_ := set $map $key (atoi $rendered) -}}
    {{- else -}}
      {{- $_ := set $map $key $rendered -}}
    {{- end -}}
  {{- else if kindIs "map" $value -}}
    {{- include "toolgate.tplValues" (dict "map" $value "root" $root) -}}
  {{- else if kindIs "slice" $value -}}
    {{- $items := list -}}
    {{- range $item := $value -}}
      {{- if kindIs "string" $item -}}
        {{- $items = append $items (tpl $item $root) -}}
      {{- else -}}
        {{- if kindIs "map" $item -}}
          {{- include "toolgate.tplValues" (dict "map" $item "root" $root) -}}
        {{- end -}}
        {{- $items = append $items $item -}}
      {{- end -}}
    {{- end -}}
    {{- $_ := set $map $key $items -}}
  {{- end -}}
{{- end -}}
{{- end -}}
