{{/*
Throwaway dependencies for a test cluster (values.dev). Each is a Helm hook
at weight -10 so it exists before the migrate hook (weight 0) runs; the
regular resources come after every hook. before-hook-creation recreates
them on upgrade: a fresh database and a fresh chain, together.
*/}}
{{- define "exchange.devHookAnnotations" -}}
helm.sh/hook: pre-install,pre-upgrade
helm.sh/hook-weight: "-10"
helm.sh/hook-delete-policy: before-hook-creation
{{- end -}}

{{- define "exchange.devLabels" -}}
{{ include "exchange.labels" .root }}
app.kubernetes.io/component: dev-{{ .name }}
{{- end -}}

{{- define "exchange.devSelector" -}}
{{ include "exchange.selectorLabels" .root }}
app.kubernetes.io/component: dev-{{ .name }}
{{- end -}}
