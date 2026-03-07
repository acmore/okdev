#!/bin/sh
set -eu

mkdir -p /run/sshd /root/.ssh
chmod 700 /root/.ssh
touch /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys

if [ ! -f /etc/ssh/ssh_host_ed25519_key ]; then
  ssh-keygen -A
fi

exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
