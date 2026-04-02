#!/bin/sh
set -eu

mkdir -p /var/okdev
cp /usr/local/bin/okdev-sshd /var/okdev/okdev-sshd
chmod +x /var/okdev/okdev-sshd
cp /etc/okdev-dev.tmux.conf /var/okdev/dev.tmux.conf
chmod 644 /var/okdev/dev.tmux.conf

# Start syncthing in foreground (run as root for workspace access)
export STNOUPGRADE=1

exec syncthing serve --home /var/syncthing --no-browser \
  --gui-address=http://0.0.0.0:8384 --no-restart --no-upgrade --skip-port-probing
