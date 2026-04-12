#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

OKDEV_BIN="${OKDEV_BIN:-$ROOT_DIR/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-large-repo}"
NAMESPACE="${NAMESPACE:-default}"
LARGE_REPO_PATH="${LARGE_REPO_PATH:-}"
LARGE_REPO_URL="${LARGE_REPO_URL:-}"
LARGE_REPO_REF="${LARGE_REPO_REF:-}"
LARGE_REPO_FILE="${LARGE_REPO_FILE:-}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
SYNC_REPO="$WORKDIR/repo"
SOURCE_REPO=""
CFG_PATH=""
FOLDER_CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
LEGACY_CFG_PATH="$WORKDIR/.okdev.yaml"
ORIG_HOME="${HOME}"
ORIG_KUBECONFIG="${KUBECONFIG:-}"
KUBECONFIG_PATH="$HOME_DIR/.kube/config"
HEAD_SHA=""
PREV_SHA=""
CHANGED_FILE=""

if [[ -z "$LARGE_REPO_PATH" && -z "$LARGE_REPO_URL" ]]; then
  echo "set either LARGE_REPO_PATH or LARGE_REPO_URL" >&2
  exit 1
fi

if [[ -z "$LARGE_REPO_PATH" ]]; then
  SOURCE_REPO="$WORKDIR/source-repo"
  echo "Cloning large repo from $LARGE_REPO_URL"
  if [[ -n "$LARGE_REPO_REF" ]]; then
    git clone --depth=2 --branch "$LARGE_REPO_REF" "$LARGE_REPO_URL" "$SOURCE_REPO" >/dev/null
  else
    git clone --depth=2 "$LARGE_REPO_URL" "$SOURCE_REPO" >/dev/null
  fi
  LARGE_REPO_PATH="$SOURCE_REPO"
fi

if [[ ! -d "$LARGE_REPO_PATH/.git" && ! -f "$LARGE_REPO_PATH/.git" ]]; then
  echo "LARGE_REPO_PATH must point to a git repository" >&2
  exit 1
fi

mkdir -p "$HOME_DIR/.kube"
if [[ -n "$ORIG_KUBECONFIG" ]]; then
  cp "$ORIG_KUBECONFIG" "$KUBECONFIG_PATH"
elif [[ -f "$ORIG_HOME/.kube/config" ]]; then
  cp "$ORIG_HOME/.kube/config" "$KUBECONFIG_PATH"
else
  echo "kubeconfig not found" >&2
  exit 1
fi
export HOME="$HOME_DIR"
export KUBECONFIG="$KUBECONFIG_PATH"

cleanup() {
  status=$?
  if [[ -n "$CFG_PATH" && -f "$CFG_PATH" ]]; then
    "$OKDEV_BIN" --config "$CFG_PATH" down "$SESSION_NAME" --yes >/dev/null 2>&1 || true
  fi
  if [[ -d "$SYNC_REPO" ]]; then
    git -C "$LARGE_REPO_PATH" worktree remove --force "$SYNC_REPO" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR"
  exit "$status"
}
trap cleanup EXIT

HEAD_SHA="$(git -C "$LARGE_REPO_PATH" rev-parse HEAD)"
PREV_SHA="$(git -C "$LARGE_REPO_PATH" rev-parse HEAD~1)"
if [[ -n "$LARGE_REPO_FILE" ]]; then
  CHANGED_FILE="$LARGE_REPO_FILE"
else
  CHANGED_FILE="$(git -C "$LARGE_REPO_PATH" diff --name-only --diff-filter=M "$PREV_SHA" "$HEAD_SHA" \
    | grep -E '\.(py|md|rst|txt|ya?ml|toml|json)$' \
    | head -n 1)"
fi
if [[ -z "$CHANGED_FILE" ]]; then
  echo "could not find a suitable modified tracked file between HEAD and HEAD~1; set LARGE_REPO_FILE explicitly" >&2
  exit 1
fi

echo "Preparing temporary worktree from $LARGE_REPO_PATH"
git -C "$LARGE_REPO_PATH" worktree add --detach "$SYNC_REPO" "$HEAD_SHA" >/dev/null

echo "Scaffolding large-repo pod config"
cd "$WORKDIR"
"$OKDEV_BIN" init \
  --workload pod \
  --yes \
  --name e2e-large-repo \
  --namespace "$NAMESPACE" \
  --dev-image ubuntu:22.04 \
  --sidecar-image "$SIDECAR_IMAGE" \
  --ssh-user root \
  --sync-local "$SYNC_REPO" \
  --sync-remote /workspace

if [[ -f "$FOLDER_CFG_PATH" ]]; then
  CFG_PATH="$FOLDER_CFG_PATH"
elif [[ -f "$LEGACY_CFG_PATH" ]]; then
  CFG_PATH="$LEGACY_CFG_PATH"
else
  echo "ERROR: okdev init did not write a config file" >&2
  exit 1
fi

replace_all_in_file "$CFG_PATH" 'persistentSession: true' 'persistentSession: false'
insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'

echo "Starting large-repo session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 10m

echo "Waiting for sync to become active"
for i in $(seq 1 20); do
  STATUS_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" status "$SESSION_NAME" --details)
  if [[ "$STATUS_OUTPUT" == *"health: active"* ]]; then
    break
  fi
  if [[ "$i" -eq 20 ]]; then
    echo "ERROR: expected large-repo status --details to report active sync" >&2
    echo "$STATUS_OUTPUT" >&2
    exit 1
  fi
  sleep 3
done

echo "Verifying config-free session commands outside repo"
cd "$HOME_DIR"
"$OKDEV_BIN" status "$SESSION_NAME" --details >/dev/null
"$OKDEV_BIN" ssh "$SESSION_NAME" --setup-key --cmd 'echo large-repo-ok' | grep -q "large-repo-ok"
cd "$WORKDIR"

LOCAL_CONTENT="$(cat "$SYNC_REPO/$CHANGED_FILE")"
REMOTE_CONTENT=""
echo "Waiting for initial tracked file sync: $CHANGED_FILE"
for i in $(seq 1 40); do
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc "if [ -f '/workspace/$CHANGED_FILE' ]; then cat '/workspace/$CHANGED_FILE'; fi" || true)
  if [[ "$REMOTE_CONTENT" == "$LOCAL_CONTENT" ]]; then
    break
  fi
  if [[ "$i" -eq 40 ]]; then
    echo "ERROR: initial large-repo tracked file did not converge: $CHANGED_FILE" >&2
    exit 1
  fi
  sleep 3
done

echo "Switching worktree to previous revision"
git -C "$SYNC_REPO" checkout --detach "$PREV_SHA" >/dev/null
LOCAL_CONTENT="$(cat "$SYNC_REPO/$CHANGED_FILE")"

echo "Waiting for branch-switch sync convergence"
for i in $(seq 1 60); do
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc "if [ -f '/workspace/$CHANGED_FILE' ]; then cat '/workspace/$CHANGED_FILE'; fi" || true)
  if [[ "$REMOTE_CONTENT" == "$LOCAL_CONTENT" ]]; then
    echo "Large-repo sync converged on attempt $i"
    break
  fi
  if [[ "$i" -eq 60 ]]; then
    echo "ERROR: large-repo branch-switch sync did not converge for $CHANGED_FILE" >&2
    exit 1
  fi
  sleep 3
done

echo "Verifying local reset wins without Syncthing conflict files"
REMOTE_CONFLICTS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc "find /workspace -name '*.sync-conflict-*' -print | head -n 20" || true)
if [[ -n "$REMOTE_CONFLICTS" ]]; then
  echo "ERROR: found unexpected Syncthing conflict files after large-repo reset" >&2
  echo "$REMOTE_CONFLICTS" >&2
  exit 1
fi
echo "No Syncthing conflict files found after reset"

echo "Tearing down large-repo session"
"$OKDEV_BIN" --config "$CFG_PATH" down "$SESSION_NAME" --yes >/dev/null
echo "Large-repo local e2e completed"
