{{/*
Expand the name of the chart.
*/}}
{{- define "roxxy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "roxxy.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "roxxy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "roxxy.labels" -}}
helm.sh/chart: {{ include "roxxy.chart" . }}
{{ include "roxxy.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "roxxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "roxxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "roxxy.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "roxxy.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
CA certificate secret name
*/}}
{{- define "roxxy.caCertSecretName" -}}
{{- if .Values.caCert.existingSecret }}
{{- .Values.caCert.existingSecret }}
{{- else }}
{{- include "roxxy.fullname" . }}-ca-cert
{{- end }}
{{- end }}

{{/*
Cache PVC name
*/}}
{{- define "roxxy.cachePvcName" -}}
{{- if .Values.cache.existingClaim }}
{{- .Values.cache.existingClaim }}
{{- else }}
{{- include "roxxy.fullname" . }}-cache
{{- end }}
{{- end }}
