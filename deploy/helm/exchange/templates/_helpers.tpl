{{/*
Names and labels, in the shape `helm create` uses so tooling recognises them.
*/}}
{{- define "exchange.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "exchange.fullname" -}}
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

{{- define "exchange.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "exchange.labels" -}}
helm.sh/chart: {{ include "exchange.chart" . }}
app.kubernetes.io/name: {{ include "exchange.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "exchange.selectorLabels" -}}
app.kubernetes.io/name: {{ include "exchange.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "exchange.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "exchange.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "exchange.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/* Service name of a role: <fullname>-<role>. */}}
{{- define "exchange.roleService" -}}
{{- printf "%s-%s" (include "exchange.fullname" .root) .role -}}
{{- end -}}

{{/*
terminationGracePeriodSeconds: the shutdown plan's drain delay, the engine's
worst group (ten seconds, internal/trading/batch.go), the shared timeout, and
a margin.
*/}}
{{- define "exchange.terminationGrace" -}}
{{- add .Values.shutdown.drainDelaySeconds 10 .Values.shutdown.timeoutSeconds 5 -}}
{{- end -}}

{{/* The singleton roles: one replica, Recreate. */}}
{{- define "exchange.isSingleton" -}}
{{- if or (eq . "engine") (eq . "chain") (eq . "signer") -}}true{{- end -}}
{{- end -}}

{{/* Where the mounted secrets live inside a pod. */}}
{{- define "exchange.secretsDir" -}}/var/run/exchange{{- end -}}
