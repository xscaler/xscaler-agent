#!/usr/bin/env bash
# Run `xscaler-collector validate` on every example config with placeholder env.
# Usage: scripts/validate-examples.sh <path-to-xscaler-collector>
# On Windows (Git Bash) it validates windows-host.yaml only.
set -euo pipefail

bin=${1:?usage: $0 <xscaler-collector binary>}
src=$(cd "$(dirname "$0")/../examples" && pwd)

# agent-api renders ${xscaler:agent_id} per agent before it pushes a config;
# the collector has no `xscaler` provider. Render a stand-in id the same way.
ex=$(mktemp -d)
trap 'rm -rf "$ex"' EXIT
for f in "$src"/*.yaml; do
  # shellcheck disable=SC2016 # a literal ${...} placeholder, not an expansion
  sed 's/\${xscaler:agent_id}/00000000-0000-0000-0000-000000000000/g' "$f" >"$ex/$(basename "$f")"
done

export K8S_NODE_NAME=node-1 K8S_NODE_IP=10.0.0.1 POD_NAMESPACE=xscaler
export XSCALER_CLUSTER_NAME=ci XSCALER_TENANT_ID=ci XSCALER_TOKEN=ci
export XSCALER_METRICS_ENDPOINT=https://ingest.example.invalid/otlp/v1/metrics
export XSCALER_LOGS_ENDPOINT=https://ingest.example.invalid/otlp/v1/logs
export XSCALER_TRACES_ENDPOINT=https://ingest.example.invalid/otlp/v1/traces
export XSCALER_QUEUE_DIR=${TMPDIR:-/tmp}/xscaler-validate-queue
export XSCALER_PG_PASSWORD=ci XSCALER_MYSQL_PASSWORD=ci XSCALER_MONGO_PASSWORD=ci
export XSCALER_MSSQL_PASSWORD=ci XSCALER_ORACLE_PASSWORD=ci

# windows_event_log only builds on Windows; everything else only on Linux
# (Kubernetes service account paths, journald).
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*)
    "$bin" validate --config "$ex/windows-host.yaml"
    echo "examples OK (windows)"
    exit 0 ;;
esac

# shellcheck source=scripts/lib-fake-sa.sh
. "$(dirname "$0")/lib-fake-sa.sh"
fake_service_account

run() { echo "== $*"; "$bin" validate "$@"; }
run --config "$ex/node-daemonset.yaml"
run --config "$ex/node-daemonset.yaml" --config "$ex/node-daemonset-ebpf.yaml"
run --config "$ex/cluster-deployment.yaml"
run --config "$ex/linux-host.yaml"
run --config "$ex/linux-host.yaml" --config "$ex/databases.yaml"
echo "examples OK (linux; windows-host.yaml is validated on Windows)"
