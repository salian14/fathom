{{/*
Mirrors mcp/helm/templates/_helpers.tpl, so the four charts behave the same way.
*/}}
{{- define "fathom-eval.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fathom-eval.fullname" -}}
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
Deliberately not .Release.Namespace: every resource is pinned to namespace.name so it
lands correctly whether or not the operator remembered -n fathom-eval.
*/}}
{{- define "fathom-eval.namespace" -}}
{{- .Values.namespace.name | default .Release.Namespace -}}
{{- end -}}

{{- define "fathom-eval.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fathom-eval.labels" -}}
helm.sh/chart: {{ include "fathom-eval.chart" . }}
{{ include "fathom-eval.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: fathom
{{- end -}}

{{- define "fathom-eval.selectorLabels" -}}
app.kubernetes.io/name: {{ include "fathom-eval.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The read service. A separate name, separate labels and a separate ServiceAccount from the
harness, because it is a different thing with different privileges: the harness writes to
the cluster, this reads a file.
*/}}
{{- define "fathom-eval.serveFullname" -}}
{{- printf "%s-serve" (include "fathom-eval.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Component labels. Until the read service existed, every pod in this namespace carried
exactly `selectorLabels` and nothing else -- which was fine while they were all Jobs that
came and went. A long-running Deployment sharing those labels would be selected by any
Service built on them, and would make `make run-now`'s "is a run in flight?" guard true
forever. So all three workloads now say which one they are.
*/}}
{{- define "fathom-eval.serveSelectorLabels" -}}
{{ include "fathom-eval.selectorLabels" . }}
app.kubernetes.io/component: serve
{{- end -}}

{{- define "fathom-eval.serveLabels" -}}
{{ include "fathom-eval.labels" . }}
app.kubernetes.io/component: serve
{{- end -}}

{{- define "fathom-eval.serveServiceAccountName" -}}
{{- if .Values.serve.serviceAccount.create -}}
{{- default (include "fathom-eval.serveFullname" .) .Values.serve.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serve.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "fathom-eval.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "fathom-eval.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The environment shared by the run and judge CronJobs. Defined once so the two cannot
drift -- a judge pointed at a different database than the run wrote would silently
judge nothing.
*/}}
{{- define "fathom-eval.env" -}}
- name: FATHOM_EVAL_AGENT_URL
  value: {{ .Values.eval.agentUrl | quote }}
- name: FATHOM_EVAL_PROMETHEUS_URL
  value: {{ .Values.eval.prometheusUrl | quote }}
- name: FATHOM_EVAL_DB_PATH
  value: {{ .Values.eval.dbPath | quote }}
- name: FATHOM_EVAL_ITERATIONS
  value: {{ .Values.eval.iterations | quote }}
- name: FATHOM_EVAL_AGENT_MAX_ITERATIONS
  value: {{ .Values.eval.agentMaxIterations | quote }}
- name: FATHOM_EVAL_REQUEST_TIMEOUT
  value: {{ .Values.eval.requestTimeout | quote }}
- name: FATHOM_EVAL_TEARDOWN_TIMEOUT
  value: {{ .Values.eval.teardownTimeout | quote }}
- name: FATHOM_EVAL_LOG_LEVEL
  value: {{ .Values.eval.logLevel | quote }}
{{- with .Values.eval.fixtures }}
- name: FATHOM_EVAL_FIXTURES
  value: {{ . | quote }}
{{- end }}
- name: FATHOM_EVAL_JUDGE_URL
  value: {{ .Values.judge.url | quote }}
- name: FATHOM_EVAL_JUDGE_MODEL
  value: {{ .Values.judge.model | quote }}
- name: FATHOM_EVAL_JUDGE_SAMPLE
  value: {{ .Values.judge.sample | quote }}
- name: FATHOM_EVAL_JUDGE_TIMEOUT
  value: {{ .Values.judge.timeout | quote }}
- name: FATHOM_EVAL_TELEMETRY
  value: {{ .Values.otel.mode | quote }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .Values.otel.endpoint | quote }}
- name: OTEL_EXPORTER_OTLP_PROTOCOL
  value: grpc
- name: OTEL_TRACES_SAMPLER
  value: {{ .Values.otel.sampler | quote }}
- name: OTEL_RESOURCE_ATTRIBUTES
  value: "deployment.environment.name={{ .Values.otel.environment }}"
{{- with .Values.extraEnv }}
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end -}}
