#!/usr/bin/env bash
set -euo pipefail

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-smoke}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(mktemp -d)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH=""
FOLDER_CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
LEGACY_CFG_PATH="$WORKDIR/.okdev.yaml"
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
    echo "--- local okdev logs ---"
    ls -R "$HOME_DIR/.okdev" 2>/dev/null || true
    echo "--- session sync log ---"
    cat "$HOME_DIR/.okdev/sessions/${SESSION_NAME}/syncthing/local.log" 2>/dev/null || true
  fi
  if [[ -n "$CFG_PATH" && -f "$CFG_PATH" ]]; then
    "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes >/dev/null 2>&1 || true
  fi
  return "$status"
}
trap cleanup EXIT

echo "Scaffolding pod config via okdev init"
cd "$WORKDIR"
"$OKDEV_BIN" init \
  --workload pod \
  --yes \
  --name e2e-smoke \
  --namespace "$NAMESPACE" \
  --dev-image ubuntu:22.04 \
  --sidecar-image "$SIDECAR_IMAGE" \
  --ssh-user root \
  --sync-local "$SYNC_DIR" \
  --sync-remote /workspace

if [[ -f "$FOLDER_CFG_PATH" ]]; then
  CFG_PATH="$FOLDER_CFG_PATH"
elif [[ -f "$LEGACY_CFG_PATH" ]]; then
  CFG_PATH="$LEGACY_CFG_PATH"
else
  echo "ERROR: okdev init did not write a config file" >&2
  exit 1
fi

# Disable persistent SSH sessions for CI
sed -i 's/persistentSession: true/persistentSession: false/' "$CFG_PATH" 2>/dev/null || true
grep -q 'persistentSession' "$CFG_PATH" || sed -i '/ssh:/a\    persistentSession: false' "$CFG_PATH"

echo "Generated config:"
cat "$CFG_PATH"

echo "hello from okdev e2e" >"$SYNC_DIR/hello.txt"

echo "Starting kind smoke session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m

echo "Checking status"
STATUS_OUTPUT=""
for i in $(seq 1 15); do
  STATUS_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
  echo "$STATUS_OUTPUT"
  if [[ "$STATUS_OUTPUT" == *"health: active"* ]]; then
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "ERROR: expected status --details to report active sync" >&2
    exit 1
  fi
  echo "Sync health not active yet, retrying status check ($i/15)"
  sleep 2
done
echo "Sync health status verified"

echo "Checking local session state layout"
SESSION_DIR="$HOME_DIR/.okdev/sessions/${SESSION_NAME}"
SESSION_JSON="$SESSION_DIR/session.json"
TARGET_JSON="$SESSION_DIR/target.json"
SYNC_JSON="$SESSION_DIR/sync.json"
SYNC_HOME="$SESSION_DIR/syncthing"
for path in "$SESSION_DIR" "$SESSION_JSON" "$TARGET_JSON" "$SYNC_JSON" "$SYNC_HOME"; do
  if [[ ! -e "$path" ]]; then
    echo "ERROR: expected session state path to exist: $path" >&2
    exit 1
  fi
done
if ! grep -Eq "\"configPath\"[[:space:]]*:[[:space:]]*\"$CFG_PATH\"" "$SESSION_JSON"; then
  echo "ERROR: expected session.json to contain configPath=$CFG_PATH" >&2
  cat "$SESSION_JSON" >&2
  exit 1
fi
if ! grep -Eq "\"namespace\"[[:space:]]*:[[:space:]]*\"$NAMESPACE\"" "$SESSION_JSON"; then
  echo "ERROR: expected session.json to contain namespace=$NAMESPACE" >&2
  cat "$SESSION_JSON" >&2
  exit 1
fi
if ! grep -Eq "\"repoRoot\"[[:space:]]*:[[:space:]]*\"$WORKDIR\"" "$SESSION_JSON"; then
  echo "ERROR: expected session.json to contain repoRoot=$WORKDIR" >&2
  cat "$SESSION_JSON" >&2
  exit 1
fi
if [[ -e "$HOME_DIR/Sync/.stfolder" ]]; then
  echo "ERROR: unexpected Syncthing default folder marker created at $HOME_DIR/Sync/.stfolder" >&2
  find "$HOME_DIR/Sync" -maxdepth 3 -ls >&2 || true
  exit 1
fi
echo "Session state layout verified"

echo "Verifying session commands work outside repo with positional session arg"
cd "$HOME_DIR"
STATUS_OUTSIDE=$("$OKDEV_BIN" status "$SESSION_NAME" --details)
if [[ "$STATUS_OUTSIDE" != *"Session: ${SESSION_NAME}"* && "$STATUS_OUTSIDE" != *"Session:"* ]]; then
  echo "ERROR: expected config-free status to succeed outside repo" >&2
  echo "$STATUS_OUTSIDE" >&2
  exit 1
fi
LOGS_OUTSIDE=$("$OKDEV_BIN" logs "$SESSION_NAME" --all --follow=false --tail 5)
if [[ "$LOGS_OUTSIDE" != *"[okdev-sidecar]"* ]]; then
  echo "ERROR: expected config-free logs to include sidecar output outside repo" >&2
  echo "$LOGS_OUTSIDE" >&2
  exit 1
fi
"$OKDEV_BIN" ssh "$SESSION_NAME" --setup-key --cmd 'test -f /workspace/hello.txt'
cd "$WORKDIR"
echo "Config-free positional session commands verified"

echo "Waiting for synced file to appear remotely with correct content"
SYNC_OK=false
for i in $(seq 1 30); do
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --cmd 'if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi' || true)
  if [[ "$REMOTE_CONTENT" == "hello from okdev e2e" ]]; then
    SYNC_OK=true
    echo "Sync verified on attempt $i"
    break
  fi
  echo "Sync poll attempt $i/30: file not ready yet"
  sleep 2
done

if [[ "$SYNC_OK" != "true" ]]; then
  echo "ERROR: synced file content did not match after 30 attempts" >&2
  exit 1
fi

echo "Verifying remote shell and logs"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" ssh --setup-key --cmd 'test -f /workspace/hello.txt'
LOG_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" logs --all --follow=false --tail 10)
if [[ "$LOG_OUTPUT" != *"[okdev-sidecar]"* ]]; then
  echo "ERROR: expected okdev logs --all to include sidecar output" >&2
  echo "Got: $LOG_OUTPUT" >&2
  exit 1
fi
echo "logs output verified"

echo "Testing exec --cmd"
EXEC_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --cmd 'echo exec-ok')
if [[ "$EXEC_OUTPUT" != *"exec-ok"* ]]; then
  echo "ERROR: exec --cmd did not return expected output" >&2
  echo "Got: $EXEC_OUTPUT" >&2
  exit 1
fi
echo "exec --cmd verified"

echo "Capturing current pod identity before spec drift"
ORIGINAL_POD_UID=$(kubectl -n "$NAMESPACE" get pod "okdev-${SESSION_NAME}" -o jsonpath='{.metadata.uid}')
echo "Original pod UID: $ORIGINAL_POD_UID"

echo "Changing pod workload spec to trigger drift detection"
sed -i '0,/image: ubuntu:22.04/s//image: ubuntu:24.04/' "$CFG_PATH"

echo "Verifying non-interactive up fails with reconcile guidance"
set +e
DRIFT_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m 2>&1)
DRIFT_STATUS=$?
set -e
if [[ "$DRIFT_STATUS" -eq 0 ]]; then
  echo "ERROR: expected drifted pod workload to require --reconcile" >&2
  exit 1
fi
if [[ "$DRIFT_OUTPUT" != *"workload spec changed; re-run with --reconcile to recreate"* ]]; then
  echo "ERROR: unexpected pod drift output" >&2
  echo "$DRIFT_OUTPUT" >&2
  exit 1
fi
echo "Pod drift guidance verified"

echo "Recreating pod workload via --reconcile"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --reconcile --wait-timeout 5m

echo "Verifying pod was recreated with updated image"
RECONCILED_POD_UID=$(kubectl -n "$NAMESPACE" get pod "okdev-${SESSION_NAME}" -o jsonpath='{.metadata.uid}')
if [[ "$RECONCILED_POD_UID" == "$ORIGINAL_POD_UID" ]]; then
  echo "ERROR: expected pod UID to change after --reconcile recreate" >&2
  exit 1
fi
RECONCILED_IMAGE=$(kubectl -n "$NAMESPACE" get pod "okdev-${SESSION_NAME}" -o jsonpath='{.spec.containers[?(@.name=="dev")].image}')
if [[ "$RECONCILED_IMAGE" != "ubuntu:24.04" ]]; then
  echo "ERROR: expected reconciled pod image ubuntu:24.04, got '$RECONCILED_IMAGE'" >&2
  exit 1
fi
echo "Pod reconcile verified"

echo "Testing explicit okdev down"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes

echo "Verifying pod is deleted"
for i in $(seq 1 30); do
  if ! kubectl -n "$NAMESPACE" get pod "okdev-${SESSION_NAME}" >/dev/null 2>&1; then
    echo "Pod deleted on attempt $i"
    break
  fi
  if [[ "$i" -eq 30 ]]; then
    echo "ERROR: pod okdev-${SESSION_NAME} still exists after down" >&2
    exit 1
  fi
  sleep 2
done

echo "Smoke test completed"
