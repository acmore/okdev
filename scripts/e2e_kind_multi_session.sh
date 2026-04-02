#!/usr/bin/env bash
set -euo pipefail

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
NAMESPACE="${NAMESPACE:-default}"
SESSION_A="${SESSION_A:-e2e-multi-a}"
SESSION_B="${SESSION_B:-e2e-multi-b}"
WORKDIR="$(mktemp -d)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
ORIG_HOME="${HOME}"
ORIG_KUBECONFIG="${KUBECONFIG:-}"
KUBECONFIG_PATH="$HOME_DIR/.kube/config"

DIR_A="$WORKDIR/a"
DIR_B="$WORKDIR/b"
CFG_A=""
CFG_B=""
SYNC_A="$DIR_A/workspace"
SYNC_B="$DIR_B/workspace"

mkdir -p "$HOME_DIR/.kube" "$SYNC_A" "$SYNC_B"
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
  "$OKDEV_BIN" --config "$CFG_A" --session "$SESSION_A" down --yes >/dev/null 2>&1 || true
  "$OKDEV_BIN" --config "$CFG_B" --session "$SESSION_B" down --yes >/dev/null 2>&1 || true
  return "$status"
}
trap cleanup EXIT

init_pod_config() {
  local dir="$1"
  local name="$2"
  local sync_dir="$3"
  mkdir -p "$dir"
  local init_output
  init_output=$(
    cd "$dir"
    "$OKDEV_BIN" init \
      --workload pod \
      --yes \
      --name "$name" \
      --namespace "$NAMESPACE" \
      --dev-image ubuntu:22.04 \
      --sidecar-image "$SIDECAR_IMAGE" \
      --ssh-user root \
      --sync-local "$sync_dir" \
      --sync-remote /workspace
  )
  printf '%s\n' "$init_output" >&2
  local cfg_path=""
  if [[ -f "$dir/.okdev/okdev.yaml" ]]; then
    cfg_path="$dir/.okdev/okdev.yaml"
  elif [[ -f "$dir/.okdev.yaml" ]]; then
    cfg_path="$dir/.okdev.yaml"
  else
    echo "ERROR: okdev init did not write a config file for $name" >&2
    exit 1
  fi
  sed -i 's/persistentSession: true/persistentSession: false/' "$cfg_path" 2>/dev/null || true
  grep -q 'persistentSession' "$cfg_path" || sed -i '/ssh:/a\    persistentSession: false' "$cfg_path"
  printf '%s\n' "$cfg_path"
}

wait_status_active() {
  local cfg="$1"
  local session="$2"
  local out=""
  for i in $(seq 1 15); do
    out=$("$OKDEV_BIN" --config "$cfg" --session "$session" status --details)
    if [[ "$out" == *"health: active"* ]]; then
      return 0
    fi
    sleep 2
  done
  echo "ERROR: expected session $session to report active sync" >&2
  echo "$out" >&2
  return 1
}

wait_remote_file() {
  local cfg="$1"
  local session="$2"
  local expected="$3"
  for i in $(seq 1 30); do
    local content
    content=$("$OKDEV_BIN" --config "$cfg" --session "$session" exec --no-tty --cmd 'if [ -f /workspace/session.txt ]; then cat /workspace/session.txt; fi' || true)
    if [[ "$content" == "$expected" ]]; then
      return 0
    fi
    sleep 2
  done
  echo "ERROR: expected session $session to sync content '$expected'" >&2
  return 1
}

echo "Scaffolding multi-session pod configs"
CFG_A=$(init_pod_config "$DIR_A" "$SESSION_A" "$SYNC_A")
CFG_B=$(init_pod_config "$DIR_B" "$SESSION_B" "$SYNC_B")

echo "hello from session a" >"$SYNC_A/session.txt"
echo "hello from session b" >"$SYNC_B/session.txt"

echo "Starting session A"
"$OKDEV_BIN" --config "$CFG_A" --session "$SESSION_A" up --wait-timeout 5m

echo "Starting session B"
"$OKDEV_BIN" --config "$CFG_B" --session "$SESSION_B" up --wait-timeout 5m

wait_status_active "$CFG_A" "$SESSION_A"
wait_status_active "$CFG_B" "$SESSION_B"

wait_remote_file "$CFG_A" "$SESSION_A" "hello from session a"
wait_remote_file "$CFG_B" "$SESSION_B" "hello from session b"

LIST_OUTPUT=$("$OKDEV_BIN" list)
if [[ "$LIST_OUTPUT" != *"$SESSION_A"* || "$LIST_OUTPUT" != *"$SESSION_B"* ]]; then
  echo "ERROR: expected okdev list to include both sessions" >&2
  echo "$LIST_OUTPUT" >&2
  exit 1
fi

POD_A=$(kubectl -n "$NAMESPACE" get pod "okdev-$SESSION_A" -o jsonpath='{.metadata.name}')
POD_B=$(kubectl -n "$NAMESPACE" get pod "okdev-$SESSION_B" -o jsonpath='{.metadata.name}')
if [[ "$POD_A" == "$POD_B" ]]; then
  echo "ERROR: expected distinct pods for concurrent sessions" >&2
  exit 1
fi

echo "Updating both sessions independently"
echo "session a updated" >"$SYNC_A/session.txt"
echo "session b updated" >"$SYNC_B/session.txt"

wait_remote_file "$CFG_A" "$SESSION_A" "session a updated"
wait_remote_file "$CFG_B" "$SESSION_B" "session b updated"

echo "Tearing down both sessions"
"$OKDEV_BIN" --config "$CFG_A" --session "$SESSION_A" down --yes
"$OKDEV_BIN" --config "$CFG_B" --session "$SESSION_B" down --yes

for i in $(seq 1 20); do
  A_EXISTS=0
  B_EXISTS=0
  kubectl -n "$NAMESPACE" get pod "okdev-$SESSION_A" >/dev/null 2>&1 && A_EXISTS=1 || true
  kubectl -n "$NAMESPACE" get pod "okdev-$SESSION_B" >/dev/null 2>&1 && B_EXISTS=1 || true
  if [[ "$A_EXISTS" -eq 0 && "$B_EXISTS" -eq 0 ]]; then
    break
  fi
  if [[ "$i" -eq 20 ]]; then
    echo "ERROR: expected both multi-session pods to be deleted" >&2
    exit 1
  fi
  sleep 2
done

echo "Multi-session e2e completed"
