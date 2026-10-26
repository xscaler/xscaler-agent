#!/bin/sh
# Install, upgrade and remove the xscaler-collector deb or rpm inside a plain
# distro container (no systemd running):
#
#   docker run --rm -v "$PWD:/w" -w /w debian:12 \
#     scripts/package-install-test.sh <old-package> <new-package>
#
# <old-package> is the same build stamped with a lower version. The test
# edits both config files after the first install and fails unless the
# upgrade keeps the edits, replaces the binaries, and leaves the user, groups,
# permissions and paths the service relies on.
set -eu

old=$1
new=$2

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "--- $*"; }

case "$old" in
  *.deb) kind=deb ;;
  *.rpm) kind=rpm ;;
  *) fail "unknown package type: $old" ;;
esac

install_pkg() {
  if [ "$kind" = deb ]; then
    # Keep edited conffiles without prompting, as unattended upgrades do.
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q \
      -o Dpkg::Options::=--force-confold "$1" >/dev/null
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q "$1" >/dev/null
  else
    yum install -y -q "$1" >/dev/null
  fi
}

pkg_version() {
  if [ "$kind" = deb ]; then
    dpkg-query -W -f '${Version}' xscaler-collector
  else
    rpm -q --qf '%{VERSION}' xscaler-collector
  fi
}

if [ "$kind" = deb ]; then
  apt-get update -q >/dev/null
fi

step "install $old"
install_pkg "./$old"
old_version=$(pkg_version)
echo "installed $old_version"

step "files and modes"
for f in /usr/bin/xscaler-collector /usr/bin/xscaler-supervisor; do
  [ -x "$f" ] || fail "$f missing or not executable"
done
[ -f /usr/lib/systemd/system/xscaler-collector.service ] || fail "systemd unit missing"
[ -f /usr/share/xscaler-collector/ebpf.conf ] || fail "ebpf.conf drop-in missing"
[ ! -e /etc/systemd/system/xscaler-collector.service.d/ebpf.conf ] ||
  fail "ebpf.conf must not be enabled by default"
grep -q '^AmbientCapabilities=.*CAP_BPF' /usr/share/xscaler-collector/ebpf.conf ||
  fail "ebpf.conf does not add CAP_BPF"
mode_owner() { stat -c '%a %U:%G' "$1"; }
# cmp and diff are not in every minimal image; cksum is coreutils.
same() { [ "$(cksum <"$1")" = "$(cksum <"$2")" ]; }
[ "$(mode_owner /etc/xscaler-collector)" = "750 root:xscaler-collector" ] ||
  fail "/etc/xscaler-collector is $(mode_owner /etc/xscaler-collector)"
for f in supervisor.yaml xscaler-collector.env; do
  [ "$(mode_owner "/etc/xscaler-collector/$f")" = "640 root:xscaler-collector" ] ||
    fail "/etc/xscaler-collector/$f is $(mode_owner "/etc/xscaler-collector/$f")"
done
[ "$(mode_owner /var/lib/xscaler-collector)" = "750 xscaler-collector:xscaler-collector" ] ||
  fail "/var/lib/xscaler-collector is $(mode_owner /var/lib/xscaler-collector)"
if grep -Eq '^[[:space:]]*reports_remote_config:' /etc/xscaler-collector/supervisor.yaml; then
  fail "supervisor.yaml sets reports_remote_config; supervisor 0.163 refuses it"
fi

step "service user"
id xscaler-collector
shell=$(getent passwd xscaler-collector | cut -d: -f7)
case "$shell" in */nologin) ;; *) fail "service user shell is $shell" ;; esac
for group in adm systemd-journal; do
  if getent group "$group" >/dev/null 2>&1; then
    id -nG xscaler-collector | tr ' ' '\n' | grep -qx "$group" ||
      fail "xscaler-collector is not in $group"
  fi
done

step "binaries run"
/usr/bin/xscaler-collector --version
/usr/bin/xscaler-collector components >/dev/null
/usr/bin/xscaler-supervisor --help >/dev/null 2>&1 || true

step "supervisor accepts the packaged config"
# Minimal images (amazonlinux) ship without setpriv and timeout.
if ! command -v setpriv >/dev/null 2>&1 || ! command -v timeout >/dev/null 2>&1; then
  dnf install -y -q util-linux coreutils-single >/dev/null 2>&1 ||
    dnf install -y -q util-linux >/dev/null
fi
# Load the packaged config as the service would and run it for a few seconds
# against an endpoint nothing listens on. The supervisor must get past config
# validation (which rejects reports_remote_config on 0.163) and keep running.
(
  set -a
  # shellcheck disable=SC1091,SC2034 # set -a exports what is assigned here
  . /etc/xscaler-collector/xscaler-collector.env
  # shellcheck disable=SC2034
  XSCALER_ENROLLMENT_TOKEN=xse_test
  # shellcheck disable=SC2034
  XSCALER_ENDPOINT=ws://127.0.0.1:1/v1/opamp
  set +a
  cd /var/lib/xscaler-collector
  timeout 8 setpriv --reuid xscaler-collector --regid xscaler-collector --init-groups \
    /usr/bin/xscaler-supervisor --config /etc/xscaler-collector/supervisor.yaml \
    >/tmp/supervisor.log 2>&1
) && rc=0 || rc=$?
# timeout exits 124 when it had to stop a supervisor that was still running.
[ "$rc" = 124 ] || { cat /tmp/supervisor.log; fail "supervisor exited early ($rc)"; }
grep -q 'reports_remote_config' /tmp/supervisor.log &&
  { cat /tmp/supervisor.log; fail "supervisor rejected the config"; }
tail -3 /tmp/supervisor.log | cut -c1-200

step "edit config"
sed -i 's/^XSCALER_ENROLLMENT_TOKEN=.*/XSCALER_ENROLLMENT_TOKEN=xse_kept_across_upgrade/' \
  /etc/xscaler-collector/xscaler-collector.env
sed -i 's/^      # environment: prod$/      environment: upgrade-test/' \
  /etc/xscaler-collector/supervisor.yaml
grep -q '^      environment: upgrade-test$' /etc/xscaler-collector/supervisor.yaml ||
  fail "could not edit supervisor.yaml"
cp /etc/xscaler-collector/supervisor.yaml /tmp/supervisor.yaml.edited
cp /etc/xscaler-collector/xscaler-collector.env /tmp/env.edited

step "upgrade to $new"
install_pkg "./$new"
new_version=$(pkg_version)
echo "upgraded $old_version -> $new_version"
[ "$new_version" != "$old_version" ] || fail "version did not change"
same /tmp/supervisor.yaml.edited /etc/xscaler-collector/supervisor.yaml ||
  fail "upgrade replaced the edited supervisor.yaml"
same /tmp/env.edited /etc/xscaler-collector/xscaler-collector.env ||
  fail "upgrade replaced the edited xscaler-collector.env"
[ "$(mode_owner /etc/xscaler-collector/xscaler-collector.env)" = "640 root:xscaler-collector" ] ||
  fail "env file mode changed on upgrade"
step "remove"
if [ "$kind" = deb ]; then
  DEBIAN_FRONTEND=noninteractive apt-get purge -y -q xscaler-collector >/dev/null
  [ ! -e /etc/xscaler-collector ] || fail "purge left /etc/xscaler-collector"
  ! getent passwd xscaler-collector >/dev/null || fail "purge left the service user"
else
  rpm -e xscaler-collector
fi
[ ! -e /usr/bin/xscaler-collector ] || fail "remove left /usr/bin/xscaler-collector"
[ ! -e /usr/lib/systemd/system/xscaler-collector.service ] || fail "remove left the unit"

echo "PASS: $kind install, upgrade ($old_version -> $new_version) and remove"
