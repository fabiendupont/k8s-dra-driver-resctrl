{{- define "dra-cache-partition.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "dra-cache-partition.fullname" -}}
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

{{- define "dra-cache-partition.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "dra-cache-partition.labels" -}}
helm.sh/chart: {{ include "dra-cache-partition.chart" . }}
{{ include "dra-cache-partition.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "dra-cache-partition.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dra-cache-partition.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "dra-cache-partition.serviceAccountName" -}}
{{- if .Values.driver.serviceAccount.create }}
{{- default (include "dra-cache-partition.fullname" .) .Values.driver.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.driver.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "dra-cache-partition.image" -}}
{{- printf "%s:%s" .Values.driver.image.repository (default .Chart.AppVersion .Values.driver.image.tag) }}
{{- end }}
