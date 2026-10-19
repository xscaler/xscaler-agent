#!/usr/bin/env bash
# Check the service identity rules in examples/node-daemonset.yaml.
#
# Loads the example's own transform/xscaler_service and resource/xscaler_agent,
# feeds OTLP logs whose resources look like what k8s_attributes produces, and
# checks service.name / service.namespace / service.version on the way out.
# The rest of the node config is loaded and started too (its Kubernetes and
# exporter endpoints are unreachable and only log retries); k8s_attributes
# itself needs a real cluster.
#
# Usage: scripts/service-identity-test.sh <path-to-xscaler-collector>
set -euo pipefail

bin=${1:?usage: $0 <xscaler-collector binary>}
bin=$(cd "$(dirname "$bin")" && pwd)/$(basename "$bin")
src=$(cd "$(dirname "$0")/../examples" && pwd)
port=${IDENTITY_PORT:-14320}
health=${IDENTITY_HEALTH_PORT:-13135}

work=$(mktemp -d)
pid=""
cleanup() {
  [ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null
  [ "${status:-1}" -ne 0 ] && tail -60 "$work/out.log"
  [ -n "${KEEP_WORK:-}" ] || rm -rf "$work"
}
trap cleanup EXIT

# shellcheck disable=SC2016 # a literal ${...} placeholder, not an expansion
sed 's/\${xscaler:agent_id}/agent-under-test/g' "$src/node-daemonset.yaml" >"$work/node.yaml"
cat >"$work/overlay.yaml" <<EOF
extensions:
  health_check: { endpoint: "127.0.0.1:${health}" }
receivers:
  otlp/test:
    protocols:
      http: { endpoint: "127.0.0.1:${port}" }
exporters:
  debug: { verbosity: detailed }
service:
  extensions: [health_check, file_storage]
  telemetry: { metrics: { level: none } }
  pipelines:
    logs/test:
      receivers: [otlp/test]
      processors: [transform/xscaler_service, resource/xscaler_agent]
      exporters: [debug]
EOF

# shellcheck source=scripts/lib-fake-sa.sh
. "$(dirname "$0")/lib-fake-sa.sh"
fake_service_account
export K8S_NODE_NAME=node-a K8S_NODE_IP=127.0.0.1 XSCALER_CLUSTER_NAME=test-cluster
export XSCALER_QUEUE_DIR="$work/queue" XSCALER_TOKEN=t XSCALER_TENANT_ID=t
export XSCALER_METRICS_ENDPOINT=http://127.0.0.1:1/v1/metrics
export XSCALER_LOGS_ENDPOINT=http://127.0.0.1:1/v1/logs
export XSCALER_TRACES_ENDPOINT=http://127.0.0.1:1/v1/traces
"$bin" --config "$work/node.yaml" --config "$work/overlay.yaml" >"$work/out.log" 2>&1 &
pid=$!
for _ in $(seq 1 60); do curl -fsS "http://127.0.0.1:$health/" >/dev/null 2>&1 && break; sleep 0.5; done

# case|resource attributes (key=value,...)
cases=(
  "sdk-wins|service.name=checkout,xscaler.k8s.app.name=web,k8s.deployment.name=web-deploy,k8s.namespace.name=shop"
  "unknown-replaced|service.name=unknown_service:java,xscaler.k8s.app.name=web,k8s.namespace.name=shop"
  "label|xscaler.k8s.app.name=cart,xscaler.k8s.app.version=1.4.2,k8s.deployment.name=cart-v2,k8s.namespace.name=shop"
  "deployment|k8s.deployment.name=api,k8s.replicaset.name=api-7d9f8c6b5,k8s.namespace.name=core"
  "statefulset|k8s.statefulset.name=pg,k8s.namespace.name=data"
  "daemonset|k8s.daemonset.name=fluent,k8s.namespace.name=kube-system"
  "cronjob|k8s.cronjob.name=backup,k8s.job.name=backup-29340120,k8s.namespace.name=ops"
  "job|k8s.job.name=migrate,k8s.namespace.name=ops"
  "bare-pod|k8s.pod.name=debug-shell,k8s.namespace.name=default"
  "unknown-kept|service.name=unknown_service:python"
  "sdk-version|service.name=pay,service.version=9.9.9,xscaler.k8s.app.version=1.0.0,k8s.namespace.name=shop"
)

json_attrs() {
  local IFS=, out="" kv
  for kv in $1; do
    out+="{\"key\":\"${kv%%=*}\",\"value\":{\"stringValue\":\"${kv#*=}\"}},"
  done
  echo "${out%,}"
}

for c in "${cases[@]}"; do
  name=${c%%|*}
  attrs=$(json_attrs "test.case=$name,${c#*|}")
  curl -fsS -o /dev/null -X POST "http://127.0.0.1:${port}/v1/logs" -H 'Content-Type: application/json' \
    -d "{\"resourceLogs\":[{\"resource\":{\"attributes\":[$attrs]},\"scopeLogs\":[{\"logRecords\":[{\"body\":{\"stringValue\":\"$name\"}}]}]}]}"
done
sleep 2

# One line per resource: "case key=value key=value ..." from the debug output.
awk '
  /ResourceLog #/ { if (line != "") print line; line = ""; next }
  /^Resource attributes:/ { inres = 1; next }
  inres && /^     -> / {
    k = $2; sub(":", "", k)
    v = $0; sub(/^[^:]*: Str\(/, "", v); sub(/\)$/, "", v)
    if (k == "test.case") line = v " " line; else line = line " " k "=" v
    next
  }
  { inres = 0 }
  END { if (line != "") print line }
' "$work/out.log" >"$work/resources.txt"

failures=0
expect() { # case key want ("" = must be absent)
  local row got
  row=$(grep -E "^$1 " "$work/resources.txt" | head -1 || true)
  if [ -z "$row" ]; then echo "FAIL $1: resource not seen"; failures=$((failures + 1)); return; fi
  got=$(tr ' ' '\n' <<<"$row" | sed -n "s/^$2=//p" | head -1)
  if [ "$got" != "$3" ]; then
    echo "FAIL $1: $2 = '${got}', want '${3}'"; failures=$((failures + 1))
  else
    echo "ok   $1: $2 = '${got}'"
  fi
}

expect sdk-wins service.name checkout
expect sdk-wins service.namespace shop
expect unknown-replaced service.name web
expect label service.name cart
expect label service.version 1.4.2
expect deployment service.name api
expect deployment service.namespace core
expect statefulset service.name pg
expect daemonset service.name fluent
expect cronjob service.name backup
expect job service.name migrate
expect bare-pod service.name ""
expect bare-pod service.namespace default
expect unknown-kept service.name unknown_service:python
expect sdk-version service.version 9.9.9
# The scratch label attributes never leave the collector.
expect label xscaler.k8s.app.name ""
expect label xscaler.k8s.app.version ""
# Collector identity on every resource.
expect deployment xscaler.agent.id agent-under-test
expect deployment k8s.cluster.name test-cluster
expect deployment host.name node-a

if [ "$failures" -ne 0 ]; then
  echo "FAIL: $failures check(s)"; status=1; exit 1
fi
echo "PASS: service identity rules"
status=0
