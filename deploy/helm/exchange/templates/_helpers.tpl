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

{{/*
The singleton roles: one replica, Recreate. deployment.yaml refuses more.

engine, chain and signer are here because each owns one piece of external
state that exactly one process may hold -- a database advisory lock, the
scanner cursor and the hot-wallet nonce, the keystore.

admin is here for a different reason and a worse failure. A webhook signing
secret is shown exactly once (docs/webhooks.md), and the back-office UI
carries it across the post-create redirect in a stash that lives in one
process's memory (internal/admin/ui_webhooks.go). A second admin process
serving that redirect finds nothing there, and the secret is stored encrypted
with nothing able to produce it again: it is gone, while the page renders as
though nothing happened. The Recreate strategy matters as much as the replica
count for this one -- RollingUpdate runs two admin pods for a few seconds on
every upgrade, which is the same bug through a narrower window.
*/}}
{{- define "exchange.isSingleton" -}}
{{- if or (eq . "engine") (eq . "chain") (eq . "signer") (eq . "admin") -}}true{{- end -}}
{{- end -}}

{{/*
Roles that answer as soon as the process is up, so a startup probe would only
postpone the first readiness check. Every other role opens a database pool,
joins NATS or waits on the engine before /readyz can pass, and wants the
longer startup grace.

This is deliberately its own list rather than "whatever is not a singleton".
They are two different questions, and answering one with the other is exactly
how the admin role silently acquired a startup probe the moment it was added
to exchange.isSingleton -- caught by deploy/helm/helm_test.go, which asserts
that api and admin answer at once.
*/}}
{{- define "exchange.answersAtOnce" -}}
{{- if or (eq . "api") (eq . "admin") -}}true{{- end -}}
{{- end -}}

{{/* Where the mounted secrets live inside a pod. */}}
{{- define "exchange.secretsDir" -}}/var/run/exchange{{- end -}}

{{/*
Non-secret environment for every role: the ConfigMap's data. Also rendered
inline into the dev bootstrap hook, which runs before the ConfigMap exists.
Secrets never go here: the roles read them from mounted files through the
*_FILE variables (deployment.yaml).
*/}}
{{- define "exchange.configData" -}}
OPS_ADDR: ":9100"
JWT_JWKS_URL: "http://{{ include "exchange.roleService" (dict "root" . "role" "api") }}:8080/.well-known/jwks.json"
SHUTDOWN_DRAIN_DELAY: "{{ .Values.shutdown.drainDelaySeconds }}s"
SHUTDOWN_TIMEOUT: "{{ .Values.shutdown.timeoutSeconds }}s"
{{- if .Values.dev.enabled }}
REDIS_ADDR: "{{ include "exchange.fullname" . }}-redis:6379"
ETH_RPC_URL: "http://{{ include "exchange.fullname" . }}-anvil:8545"
{{- end }}
{{- range $k, $v := .Values.config }}
{{- if and $.Values.dev.enabled (or (eq $k "REDIS_ADDR") (eq $k "ETH_RPC_URL")) }}
{{- else if ne (toString $v) "" }}
{{ $k }}: {{ toString $v | quote }}
{{- end }}
{{- end }}
{{- range $k, $v := .Values.extraEnv }}
{{ $k }}: {{ toString $v | quote }}
{{- end }}
{{- end -}}
