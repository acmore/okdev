#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-job}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
MANIFEST_PATH="$WORKDIR/.okdev/job.yaml"
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
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes >/dev/null 2>&1 || true
  return "$status"
}
trap cleanup EXIT

echo "Scaffolding job config via okdev init"
cd "$WORKDIR"
"$OKDEV_BIN" init \
  --workload job \
  --yes \
  --name "$SESSION_NAME" \
  --namespace "$NAMESPACE" \
  --sidecar-image "$SIDECAR_IMAGE" \
  --ssh-user root \
  --sync-local "$SYNC_DIR" \
  --sync-remote /workspace

replace_all_in_file "$MANIFEST_PATH" 'image: # TODO: replace with your image' 'image: ubuntu:22.04'
replace_all_in_file "$MANIFEST_PATH" 'command: ["sleep", "infinity"]' 'command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]'
insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'

echo "hello from job e2e" >"$SYNC_DIR/hello.txt"

echo "Starting job session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m

STATUS_OUTPUT=""
for i in $(seq 1 15); do
  STATUS_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
  if [[ "$STATUS_OUTPUT" == *"health: active"* ]]; then
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "ERROR: expected status --details to report active sync" >&2
    echo "$STATUS_OUTPUT" >&2
    exit 1
  fi
  sleep 2
done

SYNC_OK=false
for i in $(seq 1 30); do
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty -- sh -lc 'if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi' || true)
  if [[ "$REMOTE_CONTENT" == "hello from job e2e" ]]; then
    SYNC_OK=true
    break
  fi
  sleep 2
done
if [[ "$SYNC_OK" != "true" ]]; then
  echo "ERROR: synced file content did not match after 30 attempts" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# File copy (okdev cp) verification — job workload
# ---------------------------------------------------------------------------

echo "Testing cp upload single file"
echo "cp-job-content" >"$SYNC_DIR/cp-job.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp "$SYNC_DIR/cp-job.txt" :/tmp/cp-job.txt
CP_VERIFY=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty -- sh -lc 'cat /tmp/cp-job.txt')
if [[ "$CP_VERIFY" != "cp-job-content" ]]; then
  echo "ERROR: cp upload single file failed, got '$CP_VERIFY'" >&2
  exit 1
fi
echo "cp upload single file verified"

echo "Testing cp upload directory"
CP_DIR="$WORKDIR/cp-job-dir"
mkdir -p "$CP_DIR/sub"
echo "ja" >"$CP_DIR/a.txt"
echo "jb" >"$CP_DIR/sub/b.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp "$CP_DIR" :/tmp/cp-job-dir
CP_A=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty -- sh -lc 'cat /tmp/cp-job-dir/a.txt')
CP_B=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty -- sh -lc 'cat /tmp/cp-job-dir/sub/b.txt')
if [[ "$CP_A" != "ja" || "$CP_B" != "jb" ]]; then
  echo "ERROR: cp upload directory failed, got a='$CP_A' b='$CP_B'" >&2
  exit 1
fi
echo "cp upload directory verified"

echo "Testing cp download single file"
CP_DL="$WORKDIR/cp-job-downloaded.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp :/tmp/cp-job.txt "$CP_DL"
if [[ "$(cat "$CP_DL")" != "cp-job-content" ]]; then
  echo "ERROR: cp download single file failed" >&2
  exit 1
fi
echo "cp download single file verified"

echo "File copy (okdev cp) tests completed"

ORIGINAL_JOB_UID=$(kubectl -n "$NAMESPACE" get job "$SESSION_NAME" -o jsonpath='{.metadata.uid}')
ORIGINAL_POD_UID=$(kubectl -n "$NAMESPACE" get pods -l "job-name=$SESSION_NAME" -o jsonpath='{.items[0].metadata.uid}')

echo "Changing job workload spec to trigger drift detection"
replace_first_in_file "$MANIFEST_PATH" 'image: ubuntu:22.04' 'image: ubuntu:24.04'

set +e
DRIFT_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m 2>&1)
DRIFT_STATUS=$?
set -e
if [[ "$DRIFT_STATUS" -eq 0 ]]; then
  echo "ERROR: expected drifted job workload to require --reconcile" >&2
  exit 1
fi
if [[ "$DRIFT_OUTPUT" != *"workload spec changed; re-run with --reconcile to recreate"* ]]; then
  echo "ERROR: unexpected job drift output" >&2
  echo "$DRIFT_OUTPUT" >&2
  exit 1
fi

echo "Reconciling job via --reconcile"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --reconcile --wait-timeout 5m

RECONCILE_OK=false
for i in $(seq 1 30); do
  JOB_UID=$(kubectl -n "$NAMESPACE" get job "$SESSION_NAME" -o jsonpath='{.metadata.uid}')
  POD_UID=$(kubectl -n "$NAMESPACE" get pods -l "job-name=$SESSION_NAME" -o jsonpath='{.items[0].metadata.uid}')
  POD_IMAGE=$(kubectl -n "$NAMESPACE" get pods -l "job-name=$SESSION_NAME" -o jsonpath='{.items[0].spec.containers[?(@.name=="dev")].image}')
  if [[ "$JOB_UID" != "$ORIGINAL_JOB_UID" && "$POD_UID" != "$ORIGINAL_POD_UID" && "$POD_IMAGE" == "ubuntu:24.04" ]]; then
    RECONCILE_OK=true
    break
  fi
  sleep 2
done
if [[ "$RECONCILE_OK" != "true" ]]; then
  echo "ERROR: expected job reconcile to recreate job and pod with ubuntu:24.04" >&2
  exit 1
fi

RECONCILE_STATUS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
if [[ "$RECONCILE_STATUS" != *"health: active"* ]]; then
  echo "ERROR: expected sync health to remain active after job reconcile" >&2
  echo "$RECONCILE_STATUS" >&2
  exit 1
fi

echo "Tearing down job session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes
assert_no_local_sync_processes "$SESSION_NAME" "$HOME_DIR/.okdev/sessions/${SESSION_NAME}/syncthing"

for i in $(seq 1 20); do
  if ! kubectl -n "$NAMESPACE" get job "$SESSION_NAME" >/dev/null 2>&1; then
    break
  fi
  if [[ "$i" -eq 20 ]]; then
    echo "ERROR: job ${SESSION_NAME} still exists after down" >&2
    exit 1
  fi
  sleep 2
done

echo "Job e2e completed"
