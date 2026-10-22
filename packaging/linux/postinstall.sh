#!/bin/sh
# Runs after install and upgrade, for both deb (configure) and rpm.
set -e

user=xscaler-collector

# The config holds the enrollment token: readable by the service, not others.
chown root:"$user" /etc/xscaler-collector /etc/xscaler-collector/supervisor.yaml \
  /etc/xscaler-collector/xscaler-collector.env
chmod 0750 /etc/xscaler-collector
chmod 0640 /etc/xscaler-collector/supervisor.yaml /etc/xscaler-collector/xscaler-collector.env
install -d -o "$user" -g "$user" -m 0750 /var/lib/xscaler-collector

if [ ! -d /run/systemd/system ]; then
  exit 0
fi

# The supervisor runs the collector itself. Agents from earlier installs (the
# otelcol-contrib package's own service, and the opampsupervisor unit that
# get-xscaler's install.sh wrote) would run a second collector on the same
# ports and files, so stop and disable them.
for unit in otelcol-contrib.service opampsupervisor.service; do
  if systemctl list-unit-files "$unit" 2>/dev/null | grep -q "^$unit"; then
    systemctl disable --now "$unit" >/dev/null 2>&1 || true
  fi
done

systemctl daemon-reload

# deb passes "configure <old-version>" on upgrade (empty on first install);
# rpm passes the number of installed instances (2 on upgrade).
upgrade=false
case "$1" in
  configure) [ -n "$2" ] && upgrade=true ;;
  [0-9]*) [ "$1" -ge 2 ] && upgrade=true ;;
esac

if [ "$upgrade" = true ]; then
  # Pick up the new binaries; leave a stopped service stopped.
  systemctl try-restart xscaler-collector.service || true
else
  systemctl enable xscaler-collector.service >/dev/null 2>&1 || true
  # Start once there is a token to enroll with; otherwise the first start
  # waits for the operator to fill in xscaler-collector.env.
  if grep -Eq '^XSCALER_ENROLLMENT_TOKEN=.+' /etc/xscaler-collector/xscaler-collector.env; then
    systemctl start xscaler-collector.service || true
  fi
fi
