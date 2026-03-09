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

OKDEV_TMUX_FLAG="${OKDEV_TMUX:-0}"
OKDEV_WORKSPACE_PATH="${OKDEV_WORKSPACE:-/workspace}"

# Write nsenter wrapper script for SSH sessions.
# Finds PID 1 of the dev container (first process whose root is not our root).
cat > /usr/local/bin/nsenter-dev.sh << 'SCRIPT'
#!/bin/sh
OKDEV_TMUX_FLAG="__OKDEV_TMUX_FLAG__"
OKDEV_WORKSPACE_PATH="__OKDEV_WORKSPACE_PATH__"

# Find the dev container's PID 1 by looking for a process with a different root filesystem.
# In a shared PID namespace, our PID 1 is the pause/sandbox container.
# We look for the first long-lived process in the dev container.
DEV_PID=""
for pid in $(ls /proc 2>/dev/null | grep -E '^[0-9]+$' | sort -n); do
  [ "$pid" = "1" ] && continue
  [ "$pid" = "$$" ] && continue
  # Use 2>/dev/null to suppress error if process disappears while we're checking.
  [ -r "/proc/$pid/root" ] 2>/dev/null || continue
  if ! [ "/proc/$pid/root" -ef "/proc/self/root" ] 2>/dev/null; then
    # Found a process in a different mount namespace — likely the dev container
    # Verify it still exists before we commit to it.
    if [ -d "/proc/$pid" ]; then
      DEV_PID="$pid"
      break
    fi
  fi
done

if [ -z "$DEV_PID" ]; then
  echo "ERROR: could not find dev container PID" >&2
  exit 1
fi

# Ensure all current GIDs exist in /etc/group inside BOTH the sidecar and the
# target to suppress "groups: cannot find name for group ID ..." warnings.
ALL_GIDS="$(id -G 2>/dev/null || true)"
if [ -n "$ALL_GIDS" ]; then
  for gid in $ALL_GIDS; do
    case "$gid" in
      ''|*[!0-9]*)
        continue
        ;;
    esac
    grep -q ":${gid}:" /etc/group 2>/dev/null || \
      echo "okdev${gid}:x:${gid}:" >> /etc/group 2>/dev/null || true
    nsenter --target "$DEV_PID" --mount -- sh -c "
      grep -q \":${gid}:\" /etc/group 2>/dev/null || \
      echo \"okdev${gid}:x:${gid}:\" >> /etc/group 2>/dev/null || true
    " 2>/dev/null || true
  done
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

# Run postAttach script for interactive sessions when present.
if [ -n "${OKDEV_WORKSPACE_PATH:-}" ]; then
  POST_ATTACH="${OKDEV_WORKSPACE_PATH}/.okdev/post-attach.sh"
  if nsenter --target "$DEV_PID" --mount -- test -x "$POST_ATTACH" 2>/dev/null; then
    nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- "$POST_ATTACH" 2>&1 || \
      echo "warning: postAttach script failed" >&2
  fi
fi

# Interactive login shell in dev container.
# Wrap in tmux if enabled (OKDEV_TMUX=1) and not opted out (OKDEV_NO_TMUX!=1).
if [ "${OKDEV_TMUX_FLAG:-}" = "1" ] && [ "${OKDEV_NO_TMUX:-}" != "1" ] && command -v tmux >/dev/null 2>&1; then
  # Some modern terminals (for example Ghostty) may not exist in the sidecar
  # terminfo database. Use a widely available fallback for tmux startup.
  if [ "${TERM:-}" = "xterm-ghostty" ]; then
    export TERM="xterm-256color"
  fi
  # Create the session in detached mode first so the tmux server fully
  # daemonises before we attach a client. This ensures the server (and
  # any running commands) survive SSH disconnects — only the client
  # process is killed by sshd, while the server keeps the session alive.
  if ! tmux has-session -t okdev 2>/dev/null; then
    if nsenter --target "$DEV_PID" --mount -- test -x /bin/bash 2>/dev/null; then
      tmux new-session -d -s okdev "nsenter --target $DEV_PID --mount --uts --ipc --pid -- /bin/bash -l"
    else
      tmux new-session -d -s okdev "nsenter --target $DEV_PID --mount --uts --ipc --pid -- /bin/sh -l"
    fi
  fi
  exec tmux attach-session -t okdev
fi
if nsenter --target "$DEV_PID" --mount -- test -x /bin/bash 2>/dev/null; then
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- /bin/bash -l
else
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- /bin/sh -l
fi
SCRIPT
safe_tmux_flag=$(printf '%s' "$OKDEV_TMUX_FLAG" | sed 's/[\/&]/\\&/g')
safe_workspace_path=$(printf '%s' "$OKDEV_WORKSPACE_PATH" | sed 's/[\/&]/\\&/g')
sed -i \
  -e "s|__OKDEV_TMUX_FLAG__|${safe_tmux_flag}|g" \
  -e "s|__OKDEV_WORKSPACE_PATH__|${safe_workspace_path}|g" \
  /usr/local/bin/nsenter-dev.sh
chmod +x /usr/local/bin/nsenter-dev.sh

# Harden sshd_config for long-lived idle sessions.
# Server-side keepalive: probe every 10s, tolerate 30 misses (~5min of dead connection).
# This complements the client-side ServerAliveInterval and keeps intermediate
# connections (kubectl port-forward, load balancers) alive with bidirectional traffic.
if ! grep -q "ClientAliveInterval" /etc/ssh/sshd_config; then
  cat >> /etc/ssh/sshd_config << 'SSHD_KEEPALIVE'
ClientAliveInterval 10
ClientAliveCountMax 30
TCPKeepAlive yes
SSHD_KEEPALIVE
fi

# Add ForceCommand to sshd_config dynamically
if ! grep -q "ForceCommand" /etc/ssh/sshd_config; then
  echo "ForceCommand /usr/local/bin/nsenter-dev.sh" >> /etc/ssh/sshd_config
fi

# Start syncthing in background (run as root for workspace access)
syncthing serve --home /var/syncthing --no-browser \
  --gui-address=http://0.0.0.0:8384 --no-restart --skip-port-probing &

# Start sshd in foreground
exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
