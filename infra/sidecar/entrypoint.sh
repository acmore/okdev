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

# Write helper to mirror current group IDs into the sidecar and dev container.
cat > /usr/local/bin/sync-gids.sh << 'SCRIPT'
#!/bin/sh
set -eu

DEV_PID="$1"
ALL_GIDS="$(id -G 2>/dev/null || true)"
if [ -z "$ALL_GIDS" ]; then
  exit 0
fi

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
SCRIPT
chmod +x /usr/local/bin/sync-gids.sh

# Find the dev container process by explicit role marker in its environment.
cat > /usr/local/bin/find-dev-pid.sh << 'SCRIPT'
#!/bin/sh
set -eu

for pid in $(ls /proc 2>/dev/null | grep -E '^[0-9]+$' | sort -n); do
  [ "$pid" = "1" ] && continue
  [ "$pid" = "$$" ] && continue
  [ -r "/proc/$pid/environ" ] 2>/dev/null || continue
  if tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep -qx 'OKDEV_CONTAINER_ROLE=dev'; then
    printf '%s\n' "$pid"
    exit 0
  fi
done

exit 1
SCRIPT
chmod +x /usr/local/bin/find-dev-pid.sh

# Write lightweight shell wrapper for tmux windows/panes.
# Reads the cached dev PID and nsenter's directly — no SSH/tmux logic.
cat > /usr/local/bin/dev-shell.sh << 'SCRIPT'
#!/bin/sh
OKDEV_WORKSPACE_PATH="__OKDEV_WORKSPACE_PATH__"

DEV_PID="$(cat /var/run/okdev-dev.pid 2>/dev/null || true)"
if [ -n "$DEV_PID" ] && ! [ -d "/proc/$DEV_PID" ]; then
  DEV_PID=""
fi
if [ -z "$DEV_PID" ]; then
  DEV_PID="$(/usr/local/bin/find-dev-pid.sh 2>/dev/null || true)"
  if [ -n "$DEV_PID" ]; then
    echo "$DEV_PID" > /var/run/okdev-dev.pid
  fi
fi

if [ -z "$DEV_PID" ]; then
  echo "ERROR: could not find dev container PID" >&2
  exit 1
fi

if nsenter --target "$DEV_PID" --mount -- test -x /bin/bash 2>/dev/null; then
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid --cgroup -- /bin/bash -lc "if [ -d \"$OKDEV_WORKSPACE_PATH\" ]; then cd \"$OKDEV_WORKSPACE_PATH\"; fi; exec /bin/bash -l"
else
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid --cgroup -- /bin/sh -lc "if [ -d \"$OKDEV_WORKSPACE_PATH\" ]; then cd \"$OKDEV_WORKSPACE_PATH\"; fi; exec /bin/sh -l"
fi
SCRIPT

# Write nsenter wrapper script for SSH sessions.
cat > /usr/local/bin/nsenter-dev.sh << 'SCRIPT'
#!/bin/sh
OKDEV_TMUX_FLAG="__OKDEV_TMUX_FLAG__"
OKDEV_WORKSPACE_PATH="__OKDEV_WORKSPACE_PATH__"

DEV_PID="$(cat /var/run/okdev-dev.pid 2>/dev/null || true)"
if [ -n "$DEV_PID" ] && ! [ -d "/proc/$DEV_PID" ]; then
  DEV_PID=""
fi
if [ -z "$DEV_PID" ]; then
  DEV_PID="$(/usr/local/bin/find-dev-pid.sh 2>/dev/null || true)"
fi

if [ -z "$DEV_PID" ]; then
  echo "ERROR: could not find dev container PID" >&2
  exit 1
fi

echo "$DEV_PID" > /var/run/okdev-dev.pid

# Ensure all current GIDs exist in /etc/group inside BOTH the sidecar and the
# target to suppress "groups: cannot find name for group ID ..." warnings.
/usr/local/bin/sync-gids.sh "$DEV_PID"

# If a remote command was requested, execute it inside the dev container.
if [ -n "${SSH_ORIGINAL_COMMAND:-}" ]; then
  if nsenter --target "$DEV_PID" --mount -- test -x /bin/bash 2>/dev/null; then
    exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid --cgroup -- /bin/bash -lc "$SSH_ORIGINAL_COMMAND"
  else
    exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid --cgroup -- /bin/sh -lc "$SSH_ORIGINAL_COMMAND"
  fi
fi

# If there is no TTY and no remote command (e.g. SSH master/control session),
# keep the connection open so forwarding channels stay alive.
if [ ! -t 0 ]; then
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid --cgroup -- /bin/sh -lc "while :; do sleep 3600; done"
fi

# Run postAttach script for interactive sessions when present.
if [ -n "${OKDEV_WORKSPACE_PATH:-}" ]; then
  POST_ATTACH="${OKDEV_WORKSPACE_PATH}/.okdev/post-attach.sh"
  if nsenter --target "$DEV_PID" --mount -- test -x "$POST_ATTACH" 2>/dev/null; then
    nsenter --target "$DEV_PID" --mount --uts --ipc --pid --cgroup -- "$POST_ATTACH" 2>&1 || \
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
    tmux new-session -d -s okdev "/usr/local/bin/dev-shell.sh"
    tmux set-option -t okdev default-command "/usr/local/bin/dev-shell.sh"
  fi
  exec tmux attach-session -t okdev
fi
if nsenter --target "$DEV_PID" --mount -- test -x /bin/bash 2>/dev/null; then
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid --cgroup -- /bin/bash -lc "if [ -d \"$OKDEV_WORKSPACE_PATH\" ]; then cd \"$OKDEV_WORKSPACE_PATH\"; fi; exec /bin/bash -l"
else
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid --cgroup -- /bin/sh -lc "if [ -d \"$OKDEV_WORKSPACE_PATH\" ]; then cd \"$OKDEV_WORKSPACE_PATH\"; fi; exec /bin/sh -l"
fi
SCRIPT
safe_tmux_flag=$(printf '%s' "$OKDEV_TMUX_FLAG" | sed 's/[\/&]/\\&/g')
safe_workspace_path=$(printf '%s' "$OKDEV_WORKSPACE_PATH" | sed 's/[\/&]/\\&/g')
sed -i \
  -e "s|__OKDEV_TMUX_FLAG__|${safe_tmux_flag}|g" \
  -e "s|__OKDEV_WORKSPACE_PATH__|${safe_workspace_path}|g" \
  /usr/local/bin/nsenter-dev.sh
chmod +x /usr/local/bin/nsenter-dev.sh
sed -i \
  -e "s|__OKDEV_WORKSPACE_PATH__|${safe_workspace_path}|g" \
  /usr/local/bin/dev-shell.sh
chmod +x /usr/local/bin/dev-shell.sh

# Add ForceCommand to sshd_config dynamically
if ! grep -q "ForceCommand" /etc/ssh/sshd_config; then
  echo "ForceCommand /usr/local/bin/nsenter-dev.sh" >> /etc/ssh/sshd_config
fi

# When embedded SSH mode is enabled, copy okdev-sshd into the dev container
# and start it there. The SSH server runs natively in the dev container's cgroup.
OKDEV_SSH_MODE="${OKDEV_SSH_MODE:-sidecar}"
if [ "$OKDEV_SSH_MODE" = "embedded" ]; then
  DEV_PID=""
  tries=0
  while [ -z "$DEV_PID" ] && [ "$tries" -lt 60 ]; do
    DEV_PID="$(/usr/local/bin/find-dev-pid.sh 2>/dev/null || true)"
    if [ -z "$DEV_PID" ]; then
      sleep 0.5
      tries=$((tries + 1))
    fi
  done

  if [ -n "$DEV_PID" ]; then
    /usr/local/bin/sync-gids.sh "$DEV_PID"
    nsenter --target "$DEV_PID" --mount -- mkdir -p /var/okdev
    cat /usr/local/bin/okdev-sshd | nsenter --target "$DEV_PID" --mount -- sh -c "cat > /var/okdev/okdev-sshd && chmod +x /var/okdev/okdev-sshd"
    cat /root/.ssh/authorized_keys | nsenter --target "$DEV_PID" --mount -- sh -c "cat > /var/okdev/authorized_keys && chmod 600 /var/okdev/authorized_keys"

    nsenter --target "$DEV_PID" --mount --uts --ipc --pid --cgroup -- \
      /var/okdev/okdev-sshd --port 2222 --authorized-keys /var/okdev/authorized_keys &
    echo "okdev-sshd started in dev container (PID $DEV_PID) on port 2222"
  else
    echo "WARNING: could not find dev container PID, embedded SSH not started" >&2
  fi
fi

# Start syncthing in background (run as root for workspace access)
syncthing serve --home /var/syncthing --no-browser \
  --gui-address=http://0.0.0.0:8384 --no-restart --skip-port-probing &

# Start sshd in foreground
exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
