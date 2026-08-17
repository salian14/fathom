{{/* Name helpers, standard chart boilerplate. Mirrors mcp/helm/templates/_helpers.tpl. */}}

{{- define "fathom-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fathom-agent.fullname" -}}
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
fathom-agent.namespace resolves the namespace every resource in this chart is
pinned to. Deliberately not .Release.Namespace: the agent belongs beside the MCP
server whether or not the operator remembered `-n fathom`. Falls back to the
release namespace only if namespace.name is emptied on purpose.
*/}}
{{- define "fathom-agent.namespace" -}}
{{- .Values.namespace.name | default .Release.Namespace -}}
{{- end -}}

{{- define "fathom-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fathom-agent.labels" -}}
helm.sh/chart: {{ include "fathom-agent.chart" . }}
{{ include "fathom-agent.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: fathom
{{- end -}}

{{- define "fathom-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "fathom-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "fathom-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "fathom-agent.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
