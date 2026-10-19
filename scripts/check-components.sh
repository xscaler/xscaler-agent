#!/usr/bin/env bash
# Compare `xscaler-collector components` with scripts/expected-components.txt.
# Usage: scripts/check-components.sh <path-to-xscaler-collector>
set -euo pipefail

bin=${1:?usage: $0 <xscaler-collector binary>}
here=$(cd "$(dirname "$0")" && pwd)

expected=$(grep -vE '^\s*(#|$)' "$here/expected-components.txt" | sort -u)
# The output is YAML with top-level kinds (receivers:, processors:, ...) and
# `- name: <type>` entries under each. Take the name lines per kind.
actual=$("$bin" components | awk '
  /^[a-z]+:/ { kind = $1; sub(":", "", kind); next }
  kind ~ /^(receivers|processors|exporters|extensions|connectors|providers)$/ &&
    $1 == "-" && $2 == "name:" { print kind, $3 }
  kind ~ /^(receivers|processors|exporters|extensions|connectors|providers)$/ &&
    $1 == "-" && $2 == "scheme:" { print kind, $3 }
' | sort -u)

missing=$(comm -23 <(echo "$expected") <(echo "$actual"))
extra=$(comm -13 <(echo "$expected") <(echo "$actual"))

status=0
if [ -n "$missing" ]; then
  echo "MISSING components (in expected-components.txt, not in the binary):"
  awk '{print "  " $0}' <<<"$missing"
  status=1
fi
if [ -n "$extra" ]; then
  echo "UNEXPECTED components (in the binary, not in expected-components.txt):"
  awk '{print "  " $0}' <<<"$extra"
  status=1
fi
[ "$status" -eq 0 ] && echo "components OK: $(echo "$actual" | wc -l | tr -d ' ') match"
exit "$status"
