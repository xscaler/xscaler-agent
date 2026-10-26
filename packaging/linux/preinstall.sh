#!/bin/sh
# Create the service user before the package's files are unpacked.
set -e

user=xscaler-collector
home=/var/lib/xscaler-collector

if ! getent group "$user" >/dev/null 2>&1; then
  groupadd --system "$user"
fi
if ! getent passwd "$user" >/dev/null 2>&1; then
  nologin=/usr/sbin/nologin
  [ -x "$nologin" ] || nologin=/sbin/nologin
  useradd --system --gid "$user" --home-dir "$home" --no-create-home \
    --shell "$nologin" --comment "xScaler Collector" "$user"
fi

# adm reads /var/log, systemd-journal reads the journal. Only the groups the
# host has; a container image may have neither.
for group in adm systemd-journal; do
  if getent group "$group" >/dev/null 2>&1; then
    usermod -a -G "$group" "$user"
  fi
done
