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
{{- default (include "toolgate.fullname" .) .Values.serviceAccount.name -}}
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
