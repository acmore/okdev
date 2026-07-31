#!/usr/bin/env bash
set -euo pipefail

# Switching a session's workload in place: a pod session becomes a job session
# without a new session name, sync channel, or SSH alias.

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-wl-switch}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
# Added workloads are named after the workload, not its type, so two of the
# same type never collide.
MANIFEST_PATH="$WORKDIR/.okdev/batch.yaml"
SYNC_DIR="$WORKDIR/workspace"
ORIG_HOME="${HOME}"
ORIG_KUBECONFIG="${KUBECONFIG:-}"
KUBECONFIG_PATH="$HOME_DIR/.kube/config"

mkdir -p "$HOME_DIR" "$SYNC_DIR" "$HOME_DIR/.kube"
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
  if [[ "$status" -ne 0 ]]; then
    echo "--- workload list ---"
    "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" workload list 2>&1 || true
    echo "--- session pods ---"
    kubectl -n "$NAMESPACE" get pods -l "okdev.io/session=$SESSION_NAME" -o wide 2>&1 || true
  fi
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes >/dev/null 2>&1 || true
  return "$status"
}
trap cleanup EXIT

okdev() {
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" "$@"
}

session_pod_label() {
  kubectl -n "$NAMESPACE" get pods -l "okdev.io/session=$SESSION_NAME" \
    --field-selector=status.phase=Running \
    -o jsonpath="{.items[0].metadata.labels.$1}" 2>/dev/null || true
}

echo "Scaffolding a pod config via okdev init"
cd "$WORKDIR"
"$OKDEV_BIN" init \
  --yes \
  --name "$SESSION_NAME" \
  --namespace "$NAMESPACE" \
  --sidecar-image "$SIDECAR_IMAGE" \
  --dev-image ubuntu:22.04 \
  --ssh-user root \
  --sync-local "$SYNC_DIR" \
  --sync-remote /workspace

# The pod scenarios disable tmux-backed persistent sessions; a bare ubuntu dev
# image has no tmux, and sshd never comes ready with it enabled.
replace_all_in_file "$CFG_PATH" 'persistentSession: true' 'persistentSession: false'
insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'

echo "Declaring a second workload"
"$OKDEV_BIN" --config "$CFG_PATH" init --yes --workload job --workload-name batch
replace_all_in_file "$MANIFEST_PATH" 'image: # TODO: replace with your image' 'image: ubuntu:22.04'
replace_all_in_file "$MANIFEST_PATH" 'command: ["sleep", "infinity"]' 'command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]'

LIST_OUT="$(okdev workload list)"
if [[ "$LIST_OUT" != *"default"* || "$LIST_OUT" != *"batch"* ]]; then
  echo "ERROR: workload list must show both profiles" >&2
  echo "$LIST_OUT" >&2
  exit 1
fi
echo "workload list shows both profiles"

echo "hello from switch e2e" >"$SYNC_DIR/hello.txt"

echo "Starting the session on the default (pod) workload"
okdev up --wait-timeout 5m --yes

POD_PROFILE="$(session_pod_label 'okdev\.io/workload-profile')"
if [[ "$POD_PROFILE" != "default" ]]; then
  echo "ERROR: expected the live pod to carry workload-profile=default, got '$POD_PROFILE'" >&2
  exit 1
fi
OLD_POD="$(kubectl -n "$NAMESPACE" get pods -l "okdev.io/session=$SESSION_NAME" -o jsonpath='{.items[0].metadata.name}')"
echo "pod workload is live as profile 'default' ($OLD_POD)"

echo "Pinning the batch workload"
okdev workload use batch
LIST_OUT="$(okdev workload list)"
# PINNED moves immediately; LIVE must not, until okdev up applies the switch.
if ! grep -Eq '^batch[[:space:]]+job[[:space:]]+\S+[[:space:]]+\*[[:space:]]+-' <<<"$LIST_OUT"; then
  echo "ERROR: after 'workload use', batch must be PINNED but not yet LIVE" >&2
  echo "$LIST_OUT" >&2
  exit 1
fi
echo "pin moved to batch while the pod workload is still live"

echo "Applying the switch"
okdev up --wait-timeout 5m --yes

if kubectl -n "$NAMESPACE" get pod "$OLD_POD" >/dev/null 2>&1; then
  PHASE="$(kubectl -n "$NAMESPACE" get pod "$OLD_POD" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  if [[ "$PHASE" == "Running" ]]; then
    echo "ERROR: the previous pod workload ($OLD_POD) is still running after the switch" >&2
    exit 1
  fi
fi
echo "previous pod workload is gone"

NEW_PROFILE="$(session_pod_label 'okdev\.io/workload-profile')"
NEW_TYPE="$(session_pod_label 'okdev\.io/workload-type')"
if [[ "$NEW_PROFILE" != "batch" || "$NEW_TYPE" != "job" ]]; then
  echo "ERROR: expected a live job pod labelled batch, got profile='$NEW_PROFILE' type='$NEW_TYPE'" >&2
  exit 1
fi
echo "job workload is live as profile 'batch'"

SYNC_OK=false
for _ in $(seq 1 30); do
  REMOTE_CONTENT=$(okdev exec --no-tty --no-prefix -- sh -lc 'if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi' || true)
  if [[ "$REMOTE_CONTENT" == "hello from switch e2e" ]]; then
    SYNC_OK=true
    break
  fi
  sleep 2
done
if [[ "$SYNC_OK" != "true" ]]; then
  echo "ERROR: sync did not converge in the switched-to workload" >&2
  exit 1
fi
echo "exec lands in the new workload and sync converged"

LIST_OUT="$(okdev workload list)"
if ! grep -Eq '^batch[[:space:]]+job[[:space:]]+\S+[[:space:]]+\*[[:space:]]+\*' <<<"$LIST_OUT"; then
  echo "ERROR: after the switch, batch must be both PINNED and LIVE" >&2
  echo "$LIST_OUT" >&2
  exit 1
fi
echo "workload list reports batch as pinned and live"

echo "Tearing down"
okdev down --yes >/dev/null
echo "Workload switch e2e completed"
