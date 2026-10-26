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
"true" when image.repository is the xscaler-collector image (every build has
OBI, supervisor 0.163+), empty for the legacy xscaler-agent images.
*/}}
{{- define "xscaler-agent.collectorImage" -}}
{{- if hasSuffix "/xscaler-collector" .Values.image.repository -}}true{{- end -}}
{{- end -}}

{{/*
Node agent image. On the legacy xscaler-agent images the OBI eBPF receiver
ships only in the `-ebpf` variant, so the DaemonSet uses `<tag>-ebpf` when eBPF
is on. xscaler-collector has OBI in every build and one tag. The cluster
Deployment always uses the plain tag.
*/}}
{{- define "xscaler-agent.nodeImage" -}}
{{- $tag := include "xscaler-agent.imageTag" . -}}
{{- if and .Values.nodeAgent.ebpf.enabled (not (include "xscaler-agent.collectorImage" .)) -}}
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
kube-state-metrics subchart values, or an empty dict when the dependency is
absent. `kube-state-metrics` is hyphenated, so it needs `index`, not `.foo`.
*/}}
{{- define "xscaler-agent.ksmValues" -}}
{{- default dict (index .Values "kube-state-metrics") | toYaml -}}
{{- end -}}

{{/*
Service name of the kube-state-metrics subchart. Mirrors that chart's own
fullname helper — the value has to be computed here rather than read from it,
since a parent template can't call a subchart's defines.
*/}}
{{- define "xscaler-agent.ksmFullname" -}}
{{- $ksm := include "xscaler-agent.ksmValues" . | fromYaml -}}
{{- if $ksm.fullnameOverride -}}
{{- $ksm.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "kube-state-metrics" $ksm.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* host:port a pushed prometheus receiver should scrape kube-state-metrics at */}}
{{- define "xscaler-agent.ksmEndpoint" -}}
{{- $ksm := include "xscaler-agent.ksmValues" . | fromYaml -}}
{{- $port := 8080 -}}
{{- if $ksm.service -}}
{{- $port = default 8080 $ksm.service.port -}}
{{- end -}}
{{- printf "%s.%s.svc.cluster.local:%v" (include "xscaler-agent.ksmFullname" .) .Release.Namespace $port -}}
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
  {{- if not (include "xscaler-agent.collectorImage" $ctx) }}
  # The 0.151 supervisor in the legacy images needs this to send config status;
  # 0.163 sends it whenever accepts_remote_config is on and refuses to start
  # when it is set.
  reports_remote_config: true
  {{- end }}
  reports_health: true
  # Required since OTEL 0.151: heartbeat is opt-in. Without it the supervisor
  # connects once and goes silent, so agent-api's stale sweep marks it offline.
  reports_heartbeat: true
  # The collector's component list, recorded by agent-api's inventory.
  reports_available_components: true

agent:
  executable: /usr/local/bin/otelcol-contrib
  description:
    identifying_attributes:
      service.name: io.opentelemetry.collector
    non_identifying_attributes:
      role: {{ $role | quote }}
      # The cluster this agent runs in, for agent-api's inventory and so pushed
      # configs can be matched per cluster.
      k8s.cluster.name: {{ default "kubernetes" $ctx.Values.clusterName | quote }}
      agent_name: {{ printf "%s-%s" (include "xscaler-agent.fullname" $ctx) $role | quote }}
      {{- range $k, $v := $ctx.Values.labels }}
      {{ $k }}: {{ $v | quote }}
      {{- end }}
      {{- if include "xscaler-agent.collectorImage" $ctx }}
      # agent-api reads this to offer the xscaler_policy processor. Only the
      # xscaler-collector image has it (legacy 0.151 images do not), and it
      # sits after the user labels so values.yaml cannot override it.
      xscaler.distribution: "xscaler-collector"
      {{- end }}
      {{- if eq $role $ctx.Values.nodeAgent.name }}
      # Capability flag: this pod spec sets K8S_NODE_IP, so a pushed config may
      # dial the node by IP. Assignments select it with `Exists`. Node role only
      # (the cluster Deployment has no such var), and placed after the user
      # labels so values.yaml cannot contradict the pod spec.
      node_ip: "true"
      {{- end }}
      {{- if and (eq $role $ctx.Values.clusterAgent.name) (dig "enabled" false (default dict (index $ctx.Values "kube-state-metrics"))) }}
      # Capability flag: this release installs the kube-state-metrics subchart,
      # so a pushed prometheus receiver can scrape it at the endpoint below.
      # Cluster role only — one exporter wants one scraper, not one per node.
      # Chart-emitted like node_ip, and placed after the user labels so
      # values.yaml cannot contradict what is actually deployed.
      kube_state_metrics: "true"
      kube_state_metrics_endpoint: {{ include "xscaler-agent.ksmEndpoint" $ctx | quote }}
      {{- end }}

storage:
  directory: {{ $ctx.Values.storageDir | quote }}
{{- end -}}

{{- define "xscaler-agent.deriveUidScript" -}}
set -eu
: "${XSCALER_AGENT_IDENTITY:?identity is empty}"
uid=$(printf '%s' "$XSCALER_AGENT_IDENTITY" | sha256sum | sed -E 's/(.{8})(.{4})(.{4})(.{4})(.{12}).*/\1-\2-\3-\4-\5/')
printf 'instance_id: %s\n' "$uid" > "$STORAGE_DIR/persistent_state.yaml"
echo "derived OpAMP instance_uid=$uid identity=$XSCALER_AGENT_IDENTITY"
{{- end -}}

{{- define "xscaler-agent.identityInit" -}}
{{- $ctx := .ctx -}}
{{- $role := .role -}}
{{- $cluster := default "kubernetes" $ctx.Values.clusterName -}}
- name: derive-instance-uid
  image: {{ $ctx.Values.instanceUid.image | quote }}
  imagePullPolicy: {{ $ctx.Values.image.pullPolicy }}
  securityContext:
    runAsUser: 10001
    runAsGroup: 10001
  command: ["/bin/sh", "-c"]
  args:
    - |
      {{- include "xscaler-agent.deriveUidScript" $ctx | nindent 6 }}
  env:
    - name: STORAGE_DIR
      value: {{ $ctx.Values.storageDir | quote }}
    {{- if .nodeScoped }}
    - name: K8S_NODE_NAME
      valueFrom:
        fieldRef:
          fieldPath: spec.nodeName
    - name: XSCALER_AGENT_IDENTITY
      value: {{ printf "%s/$(K8S_NODE_NAME)" $cluster | quote }}
    {{- else }}
    - name: XSCALER_AGENT_IDENTITY
      value: {{ printf "%s/%s" $cluster $role | quote }}
    {{- end }}
  volumeMounts:
    - { name: storage, mountPath: {{ $ctx.Values.storageDir | quote }} }
{{- end -}}
