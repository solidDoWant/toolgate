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
"true" when the migration hook Job renders: a migration Secret or inline URL is set and the
gateway doesn't migrate on start. Empty otherwise.
*/}}
{{- define "toolgate.migrateJobEnabled" -}}
{{- if and (or .Values.config.database.migrationUrl .Values.config.database.migrationSecret) (not .Values.config.database.migrateOnStart) -}}true{{- end -}}
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

{{/*
The model cache claim: the existing claim of the embeddings server, or the
one the chart creates.
*/}}
{{- define "toolgate.modelClaim" -}}
{{- if .Values.embeddings.server.cache.existingClaim -}}
{{- .Values.embeddings.server.cache.existingClaim -}}
{{- else -}}
{{- printf "%s-models" (include "toolgate.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
"true" when the gateway scales the embeddings server to zero: the chart runs
the server and embeddings.server.scaleToZero.enabled is set. Empty otherwise.
Fails when the values can't work: an idle timeout outside 1m to 24h, or a
hand-set embeddings URL, which the gateway must never scale.
*/}}
{{- define "toolgate.embeddingsScaleToZero" -}}
{{- $server := .Values.embeddings.server -}}
{{- $scale := $server.scaleToZero | default dict -}}
{{- if and $server.enabled $scale.enabled -}}
{{- if .Values.config.toolSearchEmbeddings.url -}}
{{- fail "embeddings.server.scaleToZero.enabled can't be used with config.toolSearchEmbeddings.url: toolgate scales only the embeddings server the chart runs, so leave the URL empty" -}}
{{- end -}}
{{- $timeout := toString $scale.idleTimeout -}}
{{- if or (eq $timeout "") (not (regexMatch "^([0-9]+h)?([0-9]+m)?([0-9]+s)?$" $timeout)) -}}
{{- fail (printf "embeddings.server.scaleToZero.idleTimeout %q must be a duration such as 15m or 1h30m, in hours (h), minutes (m) and seconds (s)" $timeout) -}}
{{- end -}}
{{- $seconds := 0 -}}
{{- range $unit, $factor := dict "h" 3600 "m" 60 "s" 1 -}}
{{- with regexFind (printf "[0-9]+%s" $unit) $timeout -}}
{{- $seconds = add $seconds (mul (atoi (trimSuffix $unit .)) $factor) -}}
{{- end -}}
{{- end -}}
{{- if or (lt (int $seconds) 60) (gt (int $seconds) 86400) -}}
{{- fail (printf "embeddings.server.scaleToZero.idleTimeout %q must be from 1m to 24h" $timeout) -}}
{{- end -}}
true
{{- end -}}
{{- end -}}

{{/*
"true" when the gateway keeps volumes after an instance stops
(config.persistentVolumes.enabled). Empty otherwise. When it is true, the
ClusterRole gets the PersistentVolume and StorageClass rules, and the
admission policy lets the gateway change the volumes it labelled.
*/}}
{{- define "toolgate.persistentVolumes" -}}
{{- $volumes := .Values.config.persistentVolumes | default dict -}}
{{- if $volumes.enabled -}}
true
{{- end -}}
{{- end -}}

{{/* The name of the embeddings server's Deployment. */}}
{{- define "toolgate.embeddingsName" -}}
{{- printf "%s-embeddings" (include "toolgate.fullname" .) -}}
{{- end -}}
