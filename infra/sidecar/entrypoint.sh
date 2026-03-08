#!/bin/sh
set -eu

# Setup SSH
mkdir -p /run/sshd /root/.ssh
chmod 700 /root/.ssh
touch /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys

if [ ! -f /etc/ssh/ssh_host_ed25519_key ]; then
  ssh-keygen -A
fi

# Write nsenter wrapper script for SSH sessions.
# Finds PID 1 of the dev container (first process whose root is not our root).
cat > /usr/local/bin/nsenter-dev.sh << 'SCRIPT'
#!/bin/sh
# Find the dev container's PID 1 by looking for a process with a different root filesystem.
# In a shared PID namespace, our PID 1 is the pause/sandbox container.
# We look for the first "sleep infinity" or the init process of the dev container.
DEV_PID=""
for pid in $(ls /proc | grep -E '^[0-9]+$' | sort -n); do
  [ "$pid" = "1" ] && continue
  [ "$pid" = "$$" ] && continue
  # Skip if we can't read the process
  [ -r "/proc/$pid/root" ] || continue
  # Check if this process has a different root than us (different container)
  if ! [ "/proc/$pid/root" -ef "/proc/self/root" ]; then
    # Found a process in a different mount namespace — likely the dev container
    DEV_PID="$pid"
    break
  fi
done

if [ -z "$DEV_PID" ]; then
  echo "ERROR: could not find dev container PID" >&2
  exit 1
fi

# Ensure current GID exists in /etc/group inside the target to suppress
# "groups: cannot find name for group ID ..." warnings from login shells.
CURRENT_GID="$(id -g 2>/dev/null || true)"
if [ -n "$CURRENT_GID" ]; then
  nsenter --target "$DEV_PID" --mount -- sh -c "
    grep -q \":${CURRENT_GID}:\" /etc/group 2>/dev/null || \
    echo \"okdev:x:${CURRENT_GID}:\" >> /etc/group 2>/dev/null || true
  "
fi

# If a remote command was requested, execute it inside the dev container.
if [ -n "${SSH_ORIGINAL_COMMAND:-}" ]; then
  if nsenter --target "$DEV_PID" --mount -- test -x /bin/bash 2>/dev/null; then
    exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- /bin/bash -lc "$SSH_ORIGINAL_COMMAND"
  else
    exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- /bin/sh -lc "$SSH_ORIGINAL_COMMAND"
  fi
fi

# If there is no TTY and no remote command (e.g. SSH master/control session),
# keep the connection open so forwarding channels stay alive.
if [ ! -t 0 ]; then
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- /bin/sh -lc "while :; do sleep 3600; done"
fi

# Interactive login shell in dev container.
if nsenter --target "$DEV_PID" --mount -- test -x /bin/bash 2>/dev/null; then
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- /bin/bash -l
else
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- /bin/sh -l
fi
SCRIPT
chmod +x /usr/local/bin/nsenter-dev.sh

# Add ForceCommand to sshd_config dynamically
if ! grep -q "ForceCommand" /etc/ssh/sshd_config; then
  echo "ForceCommand /usr/local/bin/nsenter-dev.sh" >> /etc/ssh/sshd_config
fi

# Start syncthing in background (run as root for workspace access)
syncthing serve --home /var/syncthing --no-browser \
  --gui-address=http://0.0.0.0:8384 --no-restart --skip-port-probing &

# Start sshd in foreground
exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
