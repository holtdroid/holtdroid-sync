{{- define "sync.job" -}}
{{- $job := required "job is required: slack, s3, github, webview or chromium" .Values.job -}}
{{- if not (has $job (list "slack" "s3" "github" "webview" "chromium")) -}}
{{- fail (printf "job must be slack, s3, github, webview or chromium, got %q" $job) -}}
{{- end -}}
{{- printf "holtdroid-%s-sync" $job -}}
{{- end -}}

{{- define "sync.fullname" -}}
{{- .Values.fullnameOverride | default .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sync.selector" -}}
app.kubernetes.io/name: {{ include "sync.job" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "sync.labels" -}}
{{ include "sync.selector" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}
