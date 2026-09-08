{{- define "dra-resctrl.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "dra-resctrl.fullname" -}}
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

{{- define "dra-resctrl.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "dra-resctrl.labels" -}}
helm.sh/chart: {{ include "dra-resctrl.chart" . }}
{{ include "dra-resctrl.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "dra-resctrl.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dra-resctrl.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "dra-resctrl.serviceAccountName" -}}
{{- if .Values.driver.serviceAccount.create }}
{{- default (include "dra-resctrl.fullname" .) .Values.driver.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.driver.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "dra-resctrl.image" -}}
{{- printf "%s:%s" .Values.driver.image.repository (default .Chart.AppVersion .Values.driver.image.tag) }}
{{- end }}
