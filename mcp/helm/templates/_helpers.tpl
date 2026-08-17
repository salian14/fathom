{{/* Name helpers, standard chart boilerplate. */}}

{{- define "fathom-mcp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fathom-mcp.fullname" -}}
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
fathom-mcp.namespace resolves the namespace every resource in this chart is
pinned to. Deliberately not .Release.Namespace: the server belongs in its own
namespace whether or not the operator remembered `-n fathom`. Falls back to the
release namespace only if namespace.name is emptied on purpose.
*/}}
{{- define "fathom-mcp.namespace" -}}
{{- .Values.namespace.name | default .Release.Namespace -}}
{{- end -}}

{{- define "fathom-mcp.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fathom-mcp.labels" -}}
helm.sh/chart: {{ include "fathom-mcp.chart" . }}
{{ include "fathom-mcp.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: fathom
{{- end -}}

{{- define "fathom-mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "fathom-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "fathom-mcp.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "fathom-mcp.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
fathom-mcp.rbacRules renders the policy rules for the Role or ClusterRole.

This is the lockstep the design depends on: the same k8s.allowedResources list
that becomes FATHOM_MCP_ALLOWED_RESOURCES in the Deployment becomes the RBAC
here. Entries arrive as "resource" or "resource.group" and are regrouped by API
group, because that is the shape a PolicyRule wants.

Verbs are get and list, and nothing else. A watch would let a tool hold a
long-lived stream open; the three tools are request/response and have no use
for one. Widening this beyond read verbs is the decision that ends Phase 1's
safety property, so it is not parameterised -- changing it means editing this
template on purpose.
*/}}
{{- define "fathom-mcp.rbacRules" -}}
{{- $byGroup := dict -}}
{{- range $entry := .Values.k8s.allowedResources -}}
  {{- /* splitn returns map[string]string, so a missing "_1" reads back as ""
         -- which is exactly the core API group. */ -}}
  {{- $parts := splitn "." 2 (lower (trim $entry)) -}}
  {{- $group := index $parts "_1" -}}
  {{- $existing := index $byGroup $group | default (list) -}}
  {{- $_ := set $byGroup $group (append $existing (index $parts "_0")) -}}
{{- end -}}
{{- range $group := keys $byGroup | sortAlpha }}
- apiGroups:
    - {{ $group | quote }}
  resources:
{{- range $resource := index $byGroup $group | uniq | sortAlpha }}
    - {{ $resource | quote }}
{{- end }}
  verbs:
    - get
    - list
{{- end }}
{{- if has "pods" .Values.k8s.allowedResources }}
# k8s_log reads the pods/log subresource. Granted alongside pods rather than
# listed in allowedResources, because it is not a resource type the tools can be
# asked for directly.
- apiGroups:
    - ""
  resources:
    - pods/log
  verbs:
    - get
{{- end }}
{{- end -}}
