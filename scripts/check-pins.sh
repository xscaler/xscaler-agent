#!/usr/bin/env bash
# Fail when builder.yaml or the Dockerfile defaults drift from versions.env.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck disable=SC1091 # versions.env is plain KEY=value
. "$root/versions.env"

status=0
fail() { echo "pin drift: $*"; status=1; }

# Every gomod line: contrib and core beta modules at v$OTEL_VERSION, core
# stable modules (confmap providers) at v$OTEL_STABLE_VERSION, contrib
# stable-base modules at v$CONTRIB_STABLE_VERSION, OBI at v$OBI_VERSION.
contrib=github.com/open-telemetry/opentelemetry-collector-contrib
while read -r mod ver; do
  stable=""
  for m in $CONTRIB_STABLE_MODULES; do [ "$mod" = "$contrib/$m" ] && stable=1; done
  if [ -n "$stable" ]; then
    [ "$ver" = "v$CONTRIB_STABLE_VERSION" ] || fail "$mod is $ver, versions.env says v$CONTRIB_STABLE_VERSION"
    continue
  fi
  case "$mod" in
    github.com/open-telemetry/opentelemetry-collector-contrib/*) want="v$OTEL_VERSION" ;;
    go.opentelemetry.io/collector/confmap/*) want="v$OTEL_STABLE_VERSION" ;;
    go.opentelemetry.io/collector/*) want="v$OTEL_VERSION" ;;
    go.opentelemetry.io/obi/*) want="v$OBI_VERSION" ;;
    *) fail "unexpected module $mod (upstream modules only)"; continue ;;
  esac
  [ "$ver" = "$want" ] || fail "$mod is $ver, versions.env says $want"
done < <(grep -E '^\s*- gomod:' "$root/builder.yaml" | awk '{print $3, $4}')

# Only the OBI generate replace is allowed without a matching fork entry.
while read -r line; do
  case "$line" in
    "go.opentelemetry.io/obi => /src/obi") ;;
    *" => github.com/xscaler/opentelemetry-collector-contrib/"*) ;;
    *) fail "replace not allowed: $line" ;;
  esac
done < <(sed -n '/^replaces:/,/^[a-z]/p' "$root/builder.yaml" | grep -E '^[[:space:]]*- ' | sed -E 's/^[[:space:]]*- //')

for arg in GO_VERSION OBI_GENERATOR_VERSION OTEL_VERSION OBI_VERSION; do
  want=${!arg}
  while read -r got; do
    [ "$got" = "$want" ] || fail "Dockerfile ARG $arg=$got, versions.env says $want"
  done < <(grep -E "^ARG $arg=" "$root/Dockerfile" | cut -d= -f2)
done

[ "$status" -eq 0 ] && echo "pins OK: otel $OTEL_VERSION / $OTEL_STABLE_VERSION, contrib stable $CONTRIB_STABLE_VERSION, obi $OBI_VERSION"
exit "$status"
