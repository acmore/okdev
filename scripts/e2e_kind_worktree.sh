#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
WORKDIR="$(mktemp -d /tmp/owt.XXXXXX)"
SESSION="e2e-wt-$(basename "$WORKDIR" | tr '[:upper:]' '[:lower:]' | tr '.' '-')"
MAIN="$WORKDIR/main"
WT_A="$WORKDIR/a"
WT_B="$MAIN/.worktrees/b"
CFG="$MAIN/.okdev/okdev.yaml"

cleanup() {
  status=$?
  "$OKDEV_BIN" --config "$CFG" --session "$SESSION" down --yes >/dev/null 2>&1 || true
  if [[ "$status" -ne 0 ]]; then
    echo "--- worktree sync log ---" >&2
    cat "$HOME/.okdev/sessions/$SESSION/syncthing/local.log" >&2 2>/dev/null || true
  fi
  if [[ -d "$HOME/.okdev/workspaces" ]]; then
    while IFS= read -r -d '' active_file; do
      if [[ "$(cat "$active_file")" == "$SESSION" ]]; then
        rm -f "$active_file"
      fi
    done < <(find "$HOME/.okdev/workspaces" -name active_session -type f -print0)
  fi
  rm -rf "$HOME/.okdev/sessions/$SESSION"
  rm -rf "$WORKDIR"
  exit "$status"
}
trap cleanup EXIT

mkdir -p "$MAIN"
git -C "$MAIN" init -b main >/dev/null
echo primary >"$MAIN/source.txt"
(
  cd "$MAIN"
  "$OKDEV_BIN" init --template pod --yes --name e2e-worktree \
    --dev-image ubuntu:22.04 --sidecar-image "$SIDECAR_IMAGE" \
    --ssh-user root --sync-local "$MAIN" --sync-remote /workspace >/dev/null
)
git -C "$MAIN" add .
git -C "$MAIN" -c user.name=E2E -c user.email=e2e@example.com commit -m initial >/dev/null
git -C "$MAIN" worktree add --detach "$WT_A" >/dev/null
git -C "$MAIN" worktree add --detach "$WT_B" >/dev/null
echo B >"$WT_B/marker.txt"

echo "Starting one shared session"
(
  cd "$MAIN"
  "$OKDEV_BIN" --config "$CFG" --session "$SESSION" up --no-tmux --wait-timeout 5m
)

echo A >"$WT_A/marker.txt"
(cd "$MAIN" && "$OKDEV_BIN" --session "$SESSION" exec --require-sync --no-tty --no-prefix -- sh -c 'test ! -e /workspace/.worktrees/b/marker.txt')
if (cd "$WT_B" && "$OKDEV_BIN" --session "$SESSION" exec --no-tty -- true >/dev/null 2>&1); then
  echo "unattached worktree unexpectedly executed" >&2
  exit 1
fi

(
  cd "$WT_A"
  "$OKDEV_BIN" --session "$SESSION" worktree attach
  "$OKDEV_BIN" sync wait --timeout 3m
  test "$("$OKDEV_BIN" exec --require-sync --no-tty --no-prefix -- cat marker.txt)" = A
  "$OKDEV_BIN" exec --require-sync --no-tty --no-prefix -- sh -c 'test ! -e .git'
  for lifecycle in up down restart; do
    if "$OKDEV_BIN" "$lifecycle" --yes >/dev/null 2>&1; then
      echo "attached worktree unexpectedly ran $lifecycle" >&2
      exit 1
    fi
  done
)
SYNC_PID_FILE="$HOME/.okdev/sessions/$SESSION/syncthing/sync.pid"
sync_pid_before="$(cat "$SYNC_PID_FILE")"
(
  cd "$WT_B"
  "$OKDEV_BIN" --session "$SESSION" worktree attach
  "$OKDEV_BIN" sync wait --timeout 3m
  test "$("$OKDEV_BIN" exec --require-sync --no-tty --no-prefix -- cat marker.txt)" = B
)
# Attaching to a running sync adds a folder in place: the sync process, and
# with it every other folder's connection, is not restarted.
if [[ "$(cat "$SYNC_PID_FILE")" != "$sync_pid_before" ]]; then
  echo "attaching a second worktree restarted the shared sync" >&2
  exit 1
fi
(
  cd "$WT_A"
  test "$("$OKDEV_BIN" exec --require-sync --no-tty --no-prefix -- cat marker.txt)" = A
  echo A2 >marker.txt
  "$OKDEV_BIN" sync wait --timeout 3m
  test "$("$OKDEV_BIN" exec --require-sync --no-tty --no-prefix -- cat marker.txt)" = A2
)
(
  cd "$WT_B"
  test "$("$OKDEV_BIN" exec --require-sync --no-tty --no-prefix -- cat marker.txt)" = B
)

pod_count="$(kubectl get pods -l "okdev.io/session=$SESSION" --no-headers | wc -l | tr -d ' ')"
test "$pod_count" = 1

echo "Restarting the shared session from the primary checkout"
(
  cd "$MAIN"
  "$OKDEV_BIN" --config "$CFG" --session "$SESSION" restart --yes --wait-timeout 5m
)
(
  # The runtime volume is new with the Pod: the worktree root must come back
  # with its content and stay writable for the dev container's user.
  cd "$WT_A"
  "$OKDEV_BIN" sync wait --timeout 3m
  test "$("$OKDEV_BIN" exec --require-sync --no-tty --no-prefix -- cat marker.txt)" = A2
  # Owned by the dev container's user, not opened to everyone.
  test "$("$OKDEV_BIN" exec --no-tty --no-prefix -- sh -c 'echo "$(stat -c %a:%u .):$(id -u)"')" = 755:0:0
)

sync_pid_before="$(cat "$SYNC_PID_FILE")"
(
  cd "$WT_B"
  remote_b="$("$OKDEV_BIN" exec --no-tty --no-prefix -- pwd)"
  case "$remote_b" in /var/okdev/worktrees/*) ;; *) echo "unexpected worktree directory: $remote_b" >&2; exit 1 ;; esac
  "$OKDEV_BIN" worktree detach --delete-remote
  cd "$MAIN"
  "$OKDEV_BIN" --session "$SESSION" exec --no-tty --no-prefix -- test ! -e "$remote_b"
)
if [[ "$(cat "$SYNC_PID_FILE")" != "$sync_pid_before" ]]; then
  echo "detaching a worktree restarted the shared sync" >&2
  exit 1
fi
(
  cd "$WT_B"
  if "$OKDEV_BIN" exec --no-tty -- true >/dev/null 2>&1; then
    echo "detached worktree unexpectedly executed" >&2
    exit 1
  fi
)
(
  cd "$WT_A"
  test "$("$OKDEV_BIN" exec --require-sync --no-tty --no-prefix -- cat marker.txt)" = A2
)

echo "Tearing down releases the remaining attachment"
(
  cd "$MAIN"
  "$OKDEV_BIN" --config "$CFG" --session "$SESSION" down --yes
)
if [[ -e "$HOME/.okdev/sessions/$SESSION/worktrees" ]]; then
  echo "worktree attachments survived down" >&2
  exit 1
fi
if grep -rlx --include=active_session "$SESSION" "$HOME/.okdev/workspaces" 2>/dev/null | grep -q .; then
  echo "a worktree still selects the torn-down session" >&2
  exit 1
fi
echo "Shared-session worktree e2e completed"
