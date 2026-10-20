#!/bin/sh
# deb purge removes state and the service user; rpm never purges, so the
# user and /var/lib/xscaler-collector stay, as they do for other rpm services.
set -e

if [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi

if [ "$1" = "purge" ]; then
  rm -rf /var/lib/xscaler-collector /etc/xscaler-collector
  if getent passwd xscaler-collector >/dev/null 2>&1; then
    userdel xscaler-collector >/dev/null 2>&1 || true
  fi
  if getent group xscaler-collector >/dev/null 2>&1; then
    groupdel xscaler-collector >/dev/null 2>&1 || true
  fi
fi
