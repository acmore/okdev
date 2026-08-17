#!/bin/sh
set -eu

mkdir -p /var/okdev
cp /usr/local/bin/okdev-sshd /var/okdev/okdev-sshd
chmod +x /var/okdev/okdev-sshd
cp /etc/okdev-dev.tmux.conf /var/okdev/dev.tmux.conf
chmod 644 /var/okdev/dev.tmux.conf

# Start syncthing in foreground (run as root for workspace access)
export STNOUPGRADE=1

# The GUI address is also the REST API, and it is effectively unauthenticated:
# a plain GET / returns a CSRF token that authorizes the whole API without the
# API key, so anything that can reach this port can read the config (which
# contains that key), browse the filesystem and point a folder anywhere. This
# process runs as root, so that is arbitrary root read/write in the pod.
#
# Pod networks are flat and okdev ships no NetworkPolicy, so 0.0.0.0 published
# it to every pod in the cluster. okdev only reaches it through a port-forward,
# which enters this pod's own network namespace, so loopback is sufficient.
#
# The sync protocol port (22000) is deliberately left alone: mesh sync dials it
# pod-to-pod, and it authenticates peers by device ID over TLS.
exec syncthing serve --home /var/syncthing --no-browser \
  --gui-address=http://127.0.0.1:8384 --no-restart --no-upgrade --skip-port-probing
