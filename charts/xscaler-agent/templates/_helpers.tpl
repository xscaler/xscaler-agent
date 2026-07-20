{{/* Common name helpers */}}
{{- define "xscaler-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "xscaler-agent.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "xscaler-agent.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "xscaler-agent.labels" -}}
app.kubernetes.io/name: {{ include "xscaler-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "xscaler-agent.serviceAccountName" -}}
{{- default (include "xscaler-agent.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}

{{/*
Image tag. Defaults to the chart's appVersion (the release version the image
was published under), so releases drive the tag — set image.tag only to pin a
one-off (e.g. a locally built dev tag).
*/}}
{{- define "xscaler-agent.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion -}}
{{- end -}}

{{/*
Node agent image. The OBI eBPF receiver ships only in the `-ebpf` image
variant, so the DaemonSet uses `<tag>-ebpf` when eBPF is on and the plain
`<tag>` otherwise. The cluster Deployment always uses the plain tag.
*/}}
{{- define "xscaler-agent.nodeImage" -}}
{{- $tag := include "xscaler-agent.imageTag" . -}}
{{- if .Values.nodeAgent.ebpf.enabled -}}
{{- $tag = printf "%s-ebpf" $tag -}}
{{- end -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/* Cluster agent image: always the plain (non-ebpf) tag. */}}
{{- define "xscaler-agent.clusterImage" -}}
{{- printf "%s:%s" .Values.image.repository (include "xscaler-agent.imageTag" .) -}}
{{- end -}}

{{/* Secret name holding the enrollment token (existing or chart-created) */}}
{{- define "xscaler-agent.secretName" -}}
{{- if .Values.existingSecret.name -}}
{{- .Values.existingSecret.name -}}
{{- else -}}
{{- printf "%s-enrollment" (include "xscaler-agent.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
supervisor.yaml body. Arg is a dict: { "ctx": $, "role": "node"|"cluster" }.
The bearer token is injected at runtime from a Secret via env expansion
(${XSCALER_ENROLLMENT_TOKEN}), which the supervisor resolves in headers.
*/}}
{{- define "xscaler-agent.supervisorConfig" -}}
{{- $ctx := .ctx -}}
{{- $role := .role -}}
server:
  endpoint: {{ $ctx.Values.opampEndpoint | quote }}
  headers:
    Authorization: "Bearer ${XSCALER_ENROLLMENT_TOKEN}"

capabilities:
  accepts_remote_config: true
  reports_effective_config: true
  reports_remote_config: true
  reports_health: true
  # Required since OTEL 0.151: heartbeat is opt-in. Without it the supervisor
  # connects once and goes silent, so agent-api's stale sweep marks it offline.
  reports_heartbeat: true

agent:
  executable: /usr/local/bin/otelcol-contrib
  description:
    identifying_attributes:
      service.name: io.opentelemetry.collector
    non_identifying_attributes:
      role: {{ $role | quote }}
      agent_name: {{ printf "%s-%s" (include "xscaler-agent.fullname" $ctx) $role | quote }}
      {{- range $k, $v := $ctx.Values.labels }}
      {{ $k }}: {{ $v | quote }}
      {{- end }}

storage:
  directory: {{ $ctx.Values.storageDir | quote }}
{{- end -}}
