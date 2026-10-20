#!/usr/bin/env bash
# Install the xscaler-collector deb on a host running systemd and check the
# service end to end. Run as root on a throwaway machine (the Ubuntu CI
# runner):
#
#   sudo scripts/package-systemd-test.sh <old.deb> <new.deb>
#
# Covers: retiring a running otelcol-contrib / opampsupervisor service,
# enable-without-start when there is no token, the service user and groups,
# the supervisor launching the collector, an upgrade that keeps edited config
# and restarts onto the new package, the eBPF drop-in, and purge.
set -Eeuo pipefail

old=$(realpath "$1")
new=$(realpath "$2")

fail() { echo "FAIL: $*" >&2; journalctl -u xscaler-collector --no-pager -n 50 >&2 || true; exit 1; }
# A command that fails outside a check still says where.
trap 'fail "line $LINENO: $BASH_COMMAND exited $?"' ERR
step() { echo "--- $*"; }

apt_install() {
  DEBIAN_FRONTEND=noninteractive apt-get install -y -q \
    -o Dpkg::Options::=--force-confold "$1" >/dev/null
}

wait_for() {
  local what=$1; shift
  for _ in $(seq 1 60); do
    if "$@" >/dev/null; then return 0; fi
    sleep 1
  done
  fail "timed out waiting for $what"
}

collector_pid() { pgrep -u xscaler-collector -f '^/usr/bin/xscaler-collector' | head -1; }

step "legacy agents running"
for unit in otelcol-contrib opampsupervisor; do
  cat >"/etc/systemd/system/$unit.service" <<EOF
[Service]
ExecStart=/bin/sleep infinity
[Install]
WantedBy=multi-user.target
EOF
done
systemctl daemon-reload
systemctl enable --now otelcol-contrib.service opampsupervisor.service
systemctl is-active --quiet otelcol-contrib.service || fail "fake otelcol-contrib did not start"

step "install $old"
apt_install "$old"
for unit in otelcol-contrib opampsupervisor; do
  ! systemctl is-active --quiet "$unit.service" || fail "$unit is still running"
  ! systemctl is-enabled --quiet "$unit.service" || fail "$unit is still enabled"
done
systemctl is-enabled --quiet xscaler-collector.service || fail "service not enabled"
! systemctl is-active --quiet xscaler-collector.service ||
  fail "service started without an enrollment token"

step "configure and start"
env=/etc/xscaler-collector/xscaler-collector.env
sed -i 's/^XSCALER_ENROLLMENT_TOKEN=.*/XSCALER_ENROLLMENT_TOKEN=xse_systemd_test/' "$env"
# Nothing listens here: the supervisor retries and runs the collector anyway.
sed -i 's#^XSCALER_ENDPOINT=.*#XSCALER_ENDPOINT=ws://127.0.0.1:4320/v1/opamp#' "$env"
sed -i 's/^      # environment: prod$/      environment: systemd-test/' /etc/xscaler-collector/supervisor.yaml
grep -q 'environment: systemd-test' /etc/xscaler-collector/supervisor.yaml || fail "could not edit supervisor.yaml"
cp /etc/xscaler-collector/supervisor.yaml /tmp/supervisor.yaml.edited
cp "$env" /tmp/env.edited
systemctl start xscaler-collector.service
wait_for "service active" systemctl is-active --quiet xscaler-collector.service

main_pid=$(systemctl show -p MainPID --value xscaler-collector.service)
[ "$(ps -o user= -p "$main_pid")" = xscaler-collector ] ||
  fail "supervisor runs as $(ps -o user= -p "$main_pid")"
# The supervisor first runs the collector briefly to read its description,
# then starts it for good, so retry until the running one passes.
collector_ok() {
  local pid gid
  pid=$(collector_pid) || return 1
  [ -n "$pid" ] || return 1
  [ "$(ps -o ppid= -p "$pid" | tr -d ' ')" = "$main_pid" ] || return 1
  for gid in "$adm_gid" "$journal_gid"; do
    grep '^Groups:' "/proc/$pid/status" 2>/dev/null | tr -s '\t ' '\n' | grep -qx "$gid" || return 1
  done
}
adm_gid=$(getent group adm | cut -d: -f3)
journal_gid=$(getent group systemd-journal | cut -d: -f3)
wait_for "collector running as the supervisor's child in adm and systemd-journal" collector_ok
echo "supervisor pid $main_pid, collector pid $(collector_pid)"
sleep 5
systemctl is-active --quiet xscaler-collector.service || fail "service died"
pgrep -u xscaler-collector -f '^/usr/bin/xscaler-collector' >/dev/null || fail "collector died"

step "upgrade to $new"
apt_install "$new"
cmp /tmp/supervisor.yaml.edited /etc/xscaler-collector/supervisor.yaml || fail "upgrade replaced supervisor.yaml"
cmp /tmp/env.edited "$env" || fail "upgrade replaced xscaler-collector.env"
wait_for "service active after upgrade" systemctl is-active --quiet xscaler-collector.service
new_pid=$(systemctl show -p MainPID --value xscaler-collector.service)
[ "$new_pid" != "$main_pid" ] || fail "upgrade did not restart the service"
main_pid=$new_pid
wait_for "collector after upgrade" collector_ok

step "eBPF drop-in"
mkdir -p /etc/systemd/system/xscaler-collector.service.d
ln -sf /usr/share/xscaler-collector/ebpf.conf /etc/systemd/system/xscaler-collector.service.d/ebpf.conf
systemctl daemon-reload
systemctl restart xscaler-collector.service
wait_for "service active with eBPF" systemctl is-active --quiet xscaler-collector.service
systemctl show -p AmbientCapabilities --value xscaler-collector.service | grep -q cap_bpf ||
  fail "AmbientCapabilities lacks cap_bpf"
main_pid=$(systemctl show -p MainPID --value xscaler-collector.service)
collector_has_caps() {
  local pid
  collector_ok || return 1
  pid=$(collector_pid)
  [ "$(awk '/^CapAmb:/{print $2}' "/proc/$pid/status" 2>/dev/null)" != 0000000000000000 ]
}
wait_for "collector with the ambient eBPF capabilities" collector_has_caps
echo "collector CapAmb=$(awk '/^CapAmb:/{print $2}' "/proc/$(collector_pid)/status")"
rm /etc/systemd/system/xscaler-collector.service.d/ebpf.conf
systemctl daemon-reload

step "purge"
DEBIAN_FRONTEND=noninteractive apt-get purge -y -q xscaler-collector >/dev/null
! systemctl is-active --quiet xscaler-collector.service || fail "service still running after purge"
[ ! -e /usr/lib/systemd/system/xscaler-collector.service ] || fail "unit left behind"
! getent passwd xscaler-collector >/dev/null || fail "service user left behind"
! pgrep -u xscaler-collector >/dev/null 2>&1 || fail "processes left behind"

echo "PASS: systemd install, upgrade, eBPF drop-in and purge"
