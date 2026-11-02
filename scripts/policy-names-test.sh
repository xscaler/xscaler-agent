#!/usr/bin/env bash
# Check that xscaler_policy rules match metrics by their stored name.
#
# People write policy from what they see in Metrics, where k8s.pod.cpu.usage
# is k8s_pod_cpu_usage. Runs the collector with drop rules in both forms,
# posts OTLP metrics with dotted names and checks which ones come out.
#
# Usage: scripts/policy-names-test.sh <path-to-xscaler-collector>
set -euo pipefail

bin=${1:?usage: $0 <xscaler-collector binary>}
bin=$(cd "$(dirname "$bin")" && pwd)/$(basename "$bin")
port=${POLICY_PORT:-14330}
health=${POLICY_HEALTH_PORT:-13136}

work=$(mktemp -d)
pid=""
cleanup() {
  [ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null
  [ "${status:-1}" -ne 0 ] && tail -60 "$work/out.log"
  [ -n "${KEEP_WORK:-}" ] || rm -rf "$work"
}
trap cleanup EXIT

cat >"$work/config.yaml" <<EOF
extensions:
  health_check: { endpoint: "127.0.0.1:${health}" }
receivers:
  otlp:
    protocols:
      http: { endpoint: "127.0.0.1:${port}" }
processors:
  xscaler_policy:
    drops:
      - metric: "k8s_pod_filesystem_*"
      - metric: "k8s.node.network.*"
      - metric: "regex:system_paging_.*"
exporters:
  debug: { verbosity: detailed }
service:
  extensions: [health_check]
  telemetry: { metrics: { level: none } }
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [xscaler_policy]
      exporters: [debug]
EOF

"$bin" --config "$work/config.yaml" >"$work/out.log" 2>&1 &
pid=$!
for _ in $(seq 1 60); do curl -fsS "http://127.0.0.1:$health/" >/dev/null 2>&1 && break; sleep 0.5; done

now=$(date +%s)000000000
metrics=""
for name in k8s.pod.filesystem.usage k8s.node.network.io system.paging.faults k8s.pod.cpu.usage; do
  metrics+="{\"name\":\"$name\",\"gauge\":{\"dataPoints\":[{\"timeUnixNano\":\"$now\",\"asDouble\":1}]}},"
done
curl -fsS -o /dev/null -X POST "http://127.0.0.1:${port}/v1/metrics" -H 'Content-Type: application/json' \
  -d "{\"resourceMetrics\":[{\"resource\":{},\"scopeMetrics\":[{\"metrics\":[${metrics%,}]}]}]}"
sleep 2

failures=0
expect() { # metric want(present|absent)
  local got=absent
  grep -qE -- "-> Name: $1\$" "$work/out.log" && got=present
  if [ "$got" != "$2" ]; then
    echo "FAIL $1: $got, want $2"; failures=$((failures + 1))
  else
    echo "ok   $1: $got"
  fi
}

expect k8s.pod.cpu.usage present
expect k8s.pod.filesystem.usage absent
expect k8s.node.network.io absent
expect system.paging.faults absent

if [ "$failures" -ne 0 ]; then
  echo "FAIL: $failures check(s)"; status=1; exit 1
fi
echo "PASS: policy rules match stored metric names"
status=0
