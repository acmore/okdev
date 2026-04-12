#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-smoke}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH=""
FOLDER_CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
LEGACY_CFG_PATH="$WORKDIR/.okdev.yaml"
SYNC_DIR="$WORKDIR/workspace"
ORIG_HOME="${HOME}"
ORIG_KUBECONFIG="${KUBECONFIG:-}"
KUBECONFIG_PATH="$HOME_DIR/.kube/config"
SSH_AGENT_PID=""

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
  if [[ -n "$SSH_AGENT_PID" ]]; then
    ssh-agent -k >/dev/null 2>&1 || true
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
replace_all_in_file "$CFG_PATH" 'persistentSession: true' 'persistentSession: false'
insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'
insert_after_line_once "$CFG_PATH" '  ssh:' '    forwardAgent: true'
python3 - "$CFG_PATH" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
text = path.read_text()
block = '    remoteIgnore:\n      - profiles/\n      - "*.prof"\n'
if "remoteIgnore:" not in text:
    marker = "\n  ports:\n"
    if marker not in text:
        raise SystemExit("ports block not found")
    text = text.replace(marker, "\n" + block + "  ports:\n", 1)
path.write_text(text)
PY

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

echo "Starting temporary local ssh-agent"
eval "$(ssh-agent -s)" >/dev/null
SSH_AGENT_KEY="$WORKDIR/agent_id_ed25519"
ssh-keygen -q -t ed25519 -N '' -f "$SSH_AGENT_KEY" >/dev/null
ssh-add "$SSH_AGENT_KEY" >/dev/null
ssh-add -l >/dev/null

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
SSH_OUTSIDE=$("$OKDEV_BIN" ssh "$SESSION_NAME" --setup-key --cmd 'echo ssh-ok')
if [[ "$SSH_OUTSIDE" != *"ssh-ok"* ]]; then
  echo "ERROR: expected config-free ssh to succeed outside repo" >&2
  echo "$SSH_OUTSIDE" >&2
  exit 1
fi
SSH_AGENT_OUTSIDE=$("$OKDEV_BIN" ssh "$SESSION_NAME" --setup-key --cmd 'if [ -S "${SSH_AUTH_SOCK:-}" ]; then echo ssh-agent-ok; fi')
if [[ "$SSH_AGENT_OUTSIDE" != *"ssh-agent-ok"* ]]; then
  echo "ERROR: expected config-derived ssh agent forwarding to succeed outside repo" >&2
  echo "$SSH_AGENT_OUTSIDE" >&2
  exit 1
fi
SSH_NO_AGENT_OUTSIDE=$("$OKDEV_BIN" ssh "$SESSION_NAME" --setup-key --no-forward-agent --cmd 'if [ -z "${SSH_AUTH_SOCK:-}" ]; then echo no-agent-ok; fi')
if [[ "$SSH_NO_AGENT_OUTSIDE" != *"no-agent-ok"* ]]; then
  echo "ERROR: expected --no-forward-agent to disable forwarding outside repo" >&2
  echo "$SSH_NO_AGENT_OUTSIDE" >&2
  exit 1
fi
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

echo "Verifying remoteIgnore writes remote .stignore and keeps profiling data local-only"
REMOTE_STIGNORE=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --cmd 'cat /workspace/.stignore 2>/dev/null || true')
if [[ "$REMOTE_STIGNORE" != *"profiles/"* || "$REMOTE_STIGNORE" != *"*.prof"* ]]; then
  echo "ERROR: expected remote .stignore to contain remoteIgnore patterns" >&2
  echo "$REMOTE_STIGNORE" >&2
  exit 1
fi
mkdir -p "$SYNC_DIR/profiles"
echo "local profile payload" >"$SYNC_DIR/profiles/run.prof"
echo "remote ignore control" >"$SYNC_DIR/remote-ignore-control.txt"
CONTROL_SYNCED=false
for i in $(seq 1 30); do
  REMOTE_CONTROL=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --cmd 'if [ -f /workspace/remote-ignore-control.txt ]; then cat /workspace/remote-ignore-control.txt; fi' || true)
  if [[ "$REMOTE_CONTROL" == "remote ignore control" ]]; then
    CONTROL_SYNCED=true
    break
  fi
  sleep 2
done
if [[ "$CONTROL_SYNCED" != "true" ]]; then
  echo "ERROR: expected non-ignored control file to sync while testing remoteIgnore" >&2
  exit 1
fi
REMOTE_PROFILE_STATE=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --cmd 'if [ -e /workspace/profiles/run.prof ]; then echo present; else echo absent; fi' || true)
if [[ "$REMOTE_PROFILE_STATE" != "absent" ]]; then
  echo "ERROR: expected remoteIgnore to keep profiling data out of remote workspace, got $REMOTE_PROFILE_STATE" >&2
  exit 1
fi
echo "remoteIgnore behavior verified"

echo "Verifying repeated okdev up reuses active sync"
SYNC_PID_BEFORE=""
for i in $(seq 1 5); do
  STATUS_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
  SYNC_PID_BEFORE=$(printf '%s' "$STATUS_OUTPUT" | sync_pid_from_status || true)
  if [[ -n "$SYNC_PID_BEFORE" ]]; then
    break
  fi
  echo "Sync pid not found in status output yet, retrying ($i/5)"
  sleep 1
done
if [[ -z "$SYNC_PID_BEFORE" ]]; then
  echo "ERROR: expected status to include running sync pid before repeated up" >&2
  echo "$STATUS_OUTPUT" >&2
  exit 1
fi
SYNC_LOG_LINES_BEFORE=$(wc -l <"$SYNC_HOME/local.log")
REPEAT_UP_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m)
echo "$REPEAT_UP_OUTPUT"
if [[ "$REPEAT_UP_OUTPUT" != *"reused existing workload"* ]]; then
  echo "ERROR: expected repeated up to reuse existing pod workload" >&2
  exit 1
fi
if [[ "$REPEAT_UP_OUTPUT" != *"sync: already active"* ]]; then
  echo "ERROR: expected repeated up to report already-active sync" >&2
  exit 1
fi
REPEAT_STATUS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
echo "$REPEAT_STATUS"
SYNC_PID_AFTER=$(printf '%s' "$REPEAT_STATUS" | sync_pid_from_status || true)
if [[ "$SYNC_PID_AFTER" != "$SYNC_PID_BEFORE" ]]; then
  echo "ERROR: expected sync pid to remain $SYNC_PID_BEFORE after repeated up, got ${SYNC_PID_AFTER:-missing}" >&2
  exit 1
fi
if [[ "$REPEAT_STATUS" != *"health: active"* ]]; then
  echo "ERROR: expected sync health to remain active after repeated up" >&2
  exit 1
fi
SYNC_LOG_LINES_AFTER=$(wc -l <"$SYNC_HOME/local.log")
if [[ $((SYNC_LOG_LINES_AFTER - SYNC_LOG_LINES_BEFORE)) -gt 5 ]]; then
  echo "ERROR: expected repeated up not to restart or churn sync log" >&2
  tail -20 "$SYNC_HOME/local.log" >&2
  exit 1
fi
STATUS_OUTPUT="$REPEAT_STATUS"
echo "Repeated okdev up sync reuse verified"

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

# ---------------------------------------------------------------------------
# File copy (okdev cp) single-pod verification
# ---------------------------------------------------------------------------

echo "Testing cp upload single file"
echo "cp-smoke-content" >"$SYNC_DIR/cp-smoke.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp "$SYNC_DIR/cp-smoke.txt" :/tmp/cp-smoke.txt
CP_VERIFY=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --cmd 'cat /tmp/cp-smoke.txt')
if [[ "$CP_VERIFY" != "cp-smoke-content" ]]; then
  echo "ERROR: cp upload single file failed, got '$CP_VERIFY'" >&2
  exit 1
fi
echo "cp upload single file verified"

echo "Testing cp upload directory"
CP_DIR="$WORKDIR/cp-smoke-dir"
mkdir -p "$CP_DIR/nested"
echo "dir-a" >"$CP_DIR/a.txt"
echo "dir-b" >"$CP_DIR/nested/b.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp "$CP_DIR" :/tmp/cp-smoke-dir
CP_DIR_A=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --cmd 'cat /tmp/cp-smoke-dir/a.txt')
CP_DIR_B=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --cmd 'cat /tmp/cp-smoke-dir/nested/b.txt')
if [[ "$CP_DIR_A" != "dir-a" || "$CP_DIR_B" != "dir-b" ]]; then
  echo "ERROR: cp upload directory failed, got a='$CP_DIR_A' b='$CP_DIR_B'" >&2
  exit 1
fi
echo "cp upload directory verified"

echo "Testing cp download single file"
CP_DL_FILE="$WORKDIR/cp-downloaded.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp :/tmp/cp-smoke.txt "$CP_DL_FILE"
CP_DL_CONTENT=$(cat "$CP_DL_FILE")
if [[ "$CP_DL_CONTENT" != "cp-smoke-content" ]]; then
  echo "ERROR: cp download single file failed, got '$CP_DL_CONTENT'" >&2
  exit 1
fi
echo "cp download single file verified"

echo "Testing cp download directory"
CP_DL_DIR="$WORKDIR/cp-downloaded-dir"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp :/tmp/cp-smoke-dir "$CP_DL_DIR"
CP_DL_A=$(cat "$CP_DL_DIR/cp-smoke-dir/a.txt")
CP_DL_B=$(cat "$CP_DL_DIR/cp-smoke-dir/nested/b.txt")
if [[ "$CP_DL_A" != "dir-a" || "$CP_DL_B" != "dir-b" ]]; then
  echo "ERROR: cp download directory failed, got a='$CP_DL_A' b='$CP_DL_B'" >&2
  exit 1
fi
echo "cp download directory verified"

echo "File copy (okdev cp) tests completed"

echo "Capturing current pod identity before spec drift"
ORIGINAL_POD_UID=$(kubectl -n "$NAMESPACE" get pod "okdev-${SESSION_NAME}" -o jsonpath='{.metadata.uid}')
echo "Original pod UID: $ORIGINAL_POD_UID"

echo "Changing pod workload spec to trigger drift detection"
replace_first_in_file "$CFG_PATH" 'image: ubuntu:22.04' 'image: ubuntu:24.04'

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
assert_no_local_sync_processes "$SESSION_NAME" "$SYNC_HOME"

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
