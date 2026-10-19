#!/usr/bin/env bash
# Prove the persistent sending queue survives a restart.
#
#  1. Start the collector with an otlp_http exporter whose endpoint is down and
#     whose sending_queue lives on file_storage.
#  2. Send N log records over OTLP/HTTP. They cannot be delivered, so they sit
#     in the on-disk queue.
#  3. Stop the collector.
#  4. Start a sink (a second instance: otlp receiver -> debug exporter).
#  5. Start the collector again on the same queue directory, with no new input.
#  6. Pass when the sink has logged every record from step 2.
#
# Usage: scripts/smoke-test.sh <path-to-xscaler-collector>
set -euo pipefail

bin=${1:?usage: $0 <xscaler-collector binary>}
bin=$(cd "$(dirname "$bin")" && pwd)/$(basename "$bin")
records=${SMOKE_RECORDS:-5}
in_port=${SMOKE_IN_PORT:-14318}
sink_port=${SMOKE_SINK_PORT:-14319}
health_port=${SMOKE_HEALTH_PORT:-13133}
sink_health_port=${SMOKE_SINK_HEALTH_PORT:-13134}

work=$(mktemp -d)
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  if [ "${status:-1}" -ne 0 ]; then
    for f in "$work"/*.log; do echo "--- $f"; tail -40 "$f"; done
  fi
  rm -rf "$work"
}
trap cleanup EXIT

mkdir -p "$work/queue"
cat >"$work/agent.yaml" <<EOF
extensions:
  health_check: { endpoint: "127.0.0.1:${health_port}" }
  file_storage: { directory: "$work/queue" }
receivers:
  otlp:
    protocols:
      http: { endpoint: "127.0.0.1:${in_port}" }
exporters:
  otlp_http:
    endpoint: "http://127.0.0.1:${sink_port}"
    sending_queue:
      enabled: true
      storage: file_storage
    retry_on_failure:
      enabled: true
      initial_interval: 1s
      max_interval: 2s
      # Never drop: keep retrying until the sink comes up.
      max_elapsed_time: 0
service:
  extensions: [health_check, file_storage]
  telemetry: { metrics: { level: none } }
  pipelines:
    logs: { receivers: [otlp], exporters: [otlp_http] }
EOF

cat >"$work/sink.yaml" <<EOF
extensions:
  health_check: { endpoint: "127.0.0.1:${sink_health_port}" }
receivers:
  otlp:
    protocols:
      http: { endpoint: "127.0.0.1:${sink_port}" }
exporters:
  debug: { verbosity: detailed }
service:
  extensions: [health_check]
  telemetry: { metrics: { level: none } }
  pipelines:
    logs: { receivers: [otlp], exporters: [debug] }
EOF

wait_healthy() { # port name
  for _ in $(seq 1 60); do
    curl -fsS "http://127.0.0.1:$1/" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  echo "FAIL: $2 never became healthy"; return 1
}

start() { # name config
  "$bin" --config "$2" >"$work/$1.log" 2>&1 &
  pids+=("$!")
  last_pid=$!
}

echo "== validate configs"
"$bin" validate --config "$work/agent.yaml"
"$bin" validate --config "$work/sink.yaml"

echo "== start collector with the sink down"
start agent1 "$work/agent.yaml"
agent_pid=$last_pid
wait_healthy "$health_port" collector

echo "== send $records records"
now=$(date +%s)000000000
for i in $(seq 1 "$records"); do
  curl -fsS -o /dev/null -X POST "http://127.0.0.1:${in_port}/v1/logs" \
    -H 'Content-Type: application/json' \
    -d "{\"resourceLogs\":[{\"resource\":{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"smoke\"}}]},\"scopeLogs\":[{\"logRecords\":[{\"timeUnixNano\":\"$now\",\"body\":{\"stringValue\":\"smoke-record-$i\"}}]}]}]}"
done

# Let the exporter fail at least once so the records are in the queue, not in
# flight, when we stop.
sleep 3
grep -qiE 'connection refused|Exporting failed' "$work/agent1.log" \
  || { echo "FAIL: exporter never tried the dead sink"; status=1; exit 1; }
queue_files=$(find "$work/queue" -type f | wc -l | tr -d ' ')
[ "$queue_files" -gt 0 ] || { echo "FAIL: no file_storage files in $work/queue"; status=1; exit 1; }
echo "queue on disk: $(du -sk "$work/queue" | cut -f1) KiB in $queue_files file(s)"

echo "== stop collector"
kill -TERM "$agent_pid"
wait "$agent_pid" 2>/dev/null || true

echo "== start sink, then restart collector on the same queue"
start sink "$work/sink.yaml"
wait_healthy "$sink_health_port" sink
start agent2 "$work/agent.yaml"
wait_healthy "$health_port" "restarted collector"

for _ in $(seq 1 60); do
  got=$( { grep -oE 'smoke-record-[0-9]+' "$work/sink.log" || true; } | sort -u | wc -l | tr -d ' ')
  [ "$got" -ge "$records" ] && break
  sleep 0.5
done
if [ "$got" -lt "$records" ]; then
  echo "FAIL: sink received $got of $records queued records after restart"
  status=1; exit 1
fi
echo "PASS: all $records records queued before the restart reached the sink"
status=0
