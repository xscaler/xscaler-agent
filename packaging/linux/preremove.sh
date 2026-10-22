#!/bin/sh
# Stop and disable the service on removal; leave it alone on upgrade.
set -e

# deb: "remove" or "purge" on removal, "upgrade" on upgrade.
# rpm: 0 on removal, 1 on upgrade.
case "$1" in
  remove|purge|0) ;;
  *) exit 0 ;;
esac

if [ -d /run/systemd/system ]; then
  systemctl disable --now xscaler-collector.service >/dev/null 2>&1 || true
fi
