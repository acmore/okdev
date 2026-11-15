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
(
  cd "$WT_B"
  "$OKDEV_BIN" --session "$SESSION" worktree attach
  "$OKDEV_BIN" sync wait --timeout 3m
  test "$("$OKDEV_BIN" exec --require-sync --no-tty --no-prefix -- cat marker.txt)" = B
)
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

(
  cd "$WT_B"
  "$OKDEV_BIN" worktree detach
  if "$OKDEV_BIN" exec --no-tty -- true >/dev/null 2>&1; then
    echo "detached worktree unexpectedly executed" >&2
    exit 1
  fi
)
(
  cd "$WT_A"
  test "$("$OKDEV_BIN" exec --require-sync --no-tty --no-prefix -- cat marker.txt)" = A2
)
echo "Shared-session worktree e2e completed"
