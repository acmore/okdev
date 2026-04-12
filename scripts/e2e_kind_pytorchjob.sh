#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-ptjob}"
NAMESPACE="${NAMESPACE:-default}"
PVC_NAME="${PVC_NAME:-${SESSION_NAME}-workspace}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
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
  kubectl -n "$NAMESPACE" delete pvc "$PVC_NAME" >/dev/null 2>&1 || true
  return "$status"
}
trap cleanup EXIT

refresh_pytorchjob_pods() {
  local workload_name
  workload_name=$(session_workload_name "$NAMESPACE" "$SESSION_NAME")
  MASTER_POD=$(kubectl -n "$NAMESPACE" get pods \
    -l "training.kubeflow.org/job-name=$workload_name,training.kubeflow.org/replica-type=master" \
    -o jsonpath='{.items[0].metadata.name}')
  WORKER_PODS=$(kubectl -n "$NAMESPACE" get pods \
    -l "training.kubeflow.org/job-name=$workload_name,training.kubeflow.org/replica-type=worker" \
    -o jsonpath='{.items[*].metadata.name}')
}

echo "Scaffolding PyTorchJob config via okdev init"
cd "$WORKDIR"
"$OKDEV_BIN" init \
  --workload pytorchjob \
  --yes \
  --name e2e-ptjob \
  --namespace "$NAMESPACE" \
  --sidecar-image "$SIDECAR_IMAGE" \
  --ssh-user root \
  --sync-local "$SYNC_DIR" \
  --sync-remote /workspace

# The training-operator webhook requires a container named "pytorch".
# The scaffold uses "dev", so patch the manifest and config.
MANIFEST_PATH="$WORKDIR/.okdev/pytorchjob.yaml"
replace_all_in_file "$MANIFEST_PATH" 'name: dev' 'name: pytorch'
replace_all_in_file "$MANIFEST_PATH" 'image: # TODO: replace with your image' 'image: ubuntu:22.04'
replace_all_in_file "$MANIFEST_PATH" 'command: ["sleep", "infinity"]' 'command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]'
perl -0pi -e 's/- name: workspace\n              emptyDir: \{\}/- name: workspace\n              persistentVolumeClaim:\n                claimName: '"$PVC_NAME"'/g' "$MANIFEST_PATH"
replace_all_in_file "$CFG_PATH" 'container: dev' 'container: pytorch'
python3 - <<'PY' "$CFG_PATH"
import pathlib, sys
path = pathlib.Path(sys.argv[1])
text = path.read_text()
old = '      - path: "spec.pytorchReplicaSpecs.Worker.template"\n'
new = '      - path: "spec.pytorchReplicaSpecs.Worker.template"\n        sidecar: false\n'
if old not in text:
    raise SystemExit("worker inject path not found")
path.write_text(text.replace(old, new, 1))
PY

# Set 2 worker replicas for multi-pod testing (1 master + 2 workers = 3 pods).
# Also set a short terminationGracePeriodSeconds so pod cleanup after
# okdev down completes quickly (the container traps SIGTERM as a no-op,
# so the default 30 s grace period causes the teardown check to time out).
python3 - <<'PY' "$MANIFEST_PATH"
import pathlib, sys
path = pathlib.Path(sys.argv[1])
text = path.read_text()
old = "Worker:\n      replicas: 1"
new = "Worker:\n      replicas: 2"
if old in text:
    text = text.replace(old, new, 1)
text = text.replace("        spec:\n          containers:", "        spec:\n          terminationGracePeriodSeconds: 5\n          containers:")
path.write_text(text)
PY

# Disable persistent SSH sessions for CI
insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'

# Add lifecycle hooks for verification.
# postCreate runs only on target pod; postSync runs on all pods after sync;
# preStop is injected as a Kubernetes lifecycle handler.
cat >>"$CFG_PATH" <<'LIFECYCLE'
  lifecycle:
    postCreate: "touch /tmp/post-create-marker"
    postSync: "touch /tmp/post-sync-marker"
    preStop: "touch /tmp/pre-stop-marker"
LIFECYCLE

echo "Generated config:"
cat "$CFG_PATH"
echo "Generated manifest:"
cat "$MANIFEST_PATH"

echo "hello from pytorchjob e2e" >"$SYNC_DIR/hello.txt"

echo "Creating shared workspace PVC"
kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_NAME}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
EOF

echo "Starting PyTorchJob smoke session (1 master + 2 workers)"
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

echo "Waiting for synced file to appear remotely with correct content"
SYNC_OK=false
for i in $(seq 1 30); do
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --role master --no-tty --no-prefix -- sh -lc 'if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi' || true)
  if [[ "$REMOTE_CONTENT" == "hello from pytorchjob e2e" ]]; then
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

echo "Verifying SSH into master pod"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" ssh --setup-key --cmd 'echo pytorchjob-ssh-ok'

# ---------------------------------------------------------------------------
# Multi-pod exec (pdsh) verification
# ---------------------------------------------------------------------------

echo "Testing exec -- across all session pods"
EXEC_ALL_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty -- sh -lc 'echo hello-from-$(hostname)')
echo "$EXEC_ALL_OUTPUT"
# Expect prefixed output from 3 pods (1 master + 2 workers).
EXEC_ALL_LINES=$(echo "$EXEC_ALL_OUTPUT" | grep -c 'hello-from-')
if [[ "$EXEC_ALL_LINES" -lt 3 ]]; then
  echo "ERROR: expected exec output from 3 pods, got $EXEC_ALL_LINES lines" >&2
  echo "$EXEC_ALL_OUTPUT" >&2
  exit 1
fi
echo "exec verified ($EXEC_ALL_LINES pods responded)"

echo "Testing exec --role master -- targets only master"
EXEC_MASTER_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --role master --no-tty -- sh -lc 'echo master-only')
echo "$EXEC_MASTER_OUTPUT"
EXEC_MASTER_LINES=$(echo "$EXEC_MASTER_OUTPUT" | grep -c 'master-only')
if [[ "$EXEC_MASTER_LINES" -ne 1 ]]; then
  echo "ERROR: expected exec --role master to target 1 pod, got $EXEC_MASTER_LINES" >&2
  echo "$EXEC_MASTER_OUTPUT" >&2
  exit 1
fi
echo "exec --role master verified"

echo "Testing exec --role worker -- targets only workers"
EXEC_WORKER_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --role worker --no-tty -- sh -lc 'echo worker-only')
echo "$EXEC_WORKER_OUTPUT"
EXEC_WORKER_LINES=$(echo "$EXEC_WORKER_OUTPUT" | grep -c 'worker-only')
if [[ "$EXEC_WORKER_LINES" -ne 2 ]]; then
  echo "ERROR: expected exec --role worker to target 2 pods, got $EXEC_WORKER_LINES" >&2
  echo "$EXEC_WORKER_OUTPUT" >&2
  exit 1
fi
echo "exec --role worker verified"

echo "Testing exec --exclude targets subset"
# Get master pod name to exclude it.
EXCLUDE_MASTER_POD=$(kubectl -n "$NAMESPACE" get pods \
  -l "training.kubeflow.org/job-name=$(session_workload_name "$NAMESPACE" "$SESSION_NAME"),training.kubeflow.org/replica-type=master" \
  -o jsonpath='{.items[0].metadata.name}')
EXEC_EXCLUDE_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --exclude "$EXCLUDE_MASTER_POD" --no-tty -- sh -lc 'echo after-exclude')
echo "$EXEC_EXCLUDE_OUTPUT"
EXEC_EXCLUDE_LINES=$(echo "$EXEC_EXCLUDE_OUTPUT" | grep -c 'after-exclude')
if [[ "$EXEC_EXCLUDE_LINES" -ne 2 ]]; then
  echo "ERROR: expected exec --exclude to target 2 pods, got $EXEC_EXCLUDE_LINES" >&2
  echo "$EXEC_EXCLUDE_OUTPUT" >&2
  exit 1
fi
echo "exec --exclude verified"

echo "Testing exec --no-prefix suppresses pod name prefix"
EXEC_NOPREFIX_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-prefix --no-tty -- sh -lc 'echo raw-output')
echo "$EXEC_NOPREFIX_OUTPUT"
# In no-prefix mode, lines should be raw (no "podname: " prefix).
if echo "$EXEC_NOPREFIX_OUTPUT" | grep -qE '^[a-z0-9-]+: raw-output$'; then
  echo "ERROR: expected no-prefix mode but found prefixed output" >&2
  echo "$EXEC_NOPREFIX_OUTPUT" >&2
  exit 1
fi
EXEC_NOPREFIX_LINES=$(echo "$EXEC_NOPREFIX_OUTPUT" | grep -c 'raw-output')
if [[ "$EXEC_NOPREFIX_LINES" -lt 3 ]]; then
  echo "ERROR: expected 3 raw-output lines in no-prefix mode, got $EXEC_NOPREFIX_LINES" >&2
  exit 1
fi
echo "exec --no-prefix verified"

echo "Testing exec --log-dir writes per-pod log files"
EXEC_LOG_DIR=$(mktemp -d)
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --log-dir "$EXEC_LOG_DIR" -- sh -lc 'echo log-test'
EXEC_LOG_COUNT=$(find "$EXEC_LOG_DIR" -name '*.log' | wc -l | tr -d ' ')
if [[ "$EXEC_LOG_COUNT" -lt 3 ]]; then
  echo "ERROR: expected at least 3 log files in $EXEC_LOG_DIR, found $EXEC_LOG_COUNT" >&2
  ls -la "$EXEC_LOG_DIR" >&2
  exit 1
fi
for logfile in "$EXEC_LOG_DIR"/*.log; do
  if [[ ! -s "$logfile" ]]; then
    echo "ERROR: log file $logfile is empty" >&2
    exit 1
  fi
done
rm -rf "$EXEC_LOG_DIR"
echo "exec --log-dir verified"

echo "Testing exec --fanout 1 (serial execution)"
EXEC_FANOUT_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --fanout 1 --no-tty -- sh -lc 'echo fanout-test')
echo "$EXEC_FANOUT_OUTPUT"
EXEC_FANOUT_LINES=$(echo "$EXEC_FANOUT_OUTPUT" | grep -c 'fanout-test')
if [[ "$EXEC_FANOUT_LINES" -lt 3 ]]; then
  echo "ERROR: expected 3 pods with fanout=1, got $EXEC_FANOUT_LINES" >&2
  exit 1
fi
echo "exec --fanout 1 verified"

echo "Testing exec --detach launches background command"
DETACH_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --detach --no-tty -- sh -lc 'touch /tmp/detach-marker')
echo "$DETACH_OUTPUT"
DETACH_LINES=$(echo "$DETACH_OUTPUT" | grep -c 'detached')
if [[ "$DETACH_LINES" -lt 3 ]]; then
  echo "ERROR: expected 3 detached confirmations, got $DETACH_LINES" >&2
  echo "$DETACH_OUTPUT" >&2
  exit 1
fi
# Give detached commands a moment to run.
sleep 2
# Verify the marker file was created on at least the master.
DETACH_MARKER_MASTER=$(kubectl -n "$NAMESPACE" get pods \
  -l "training.kubeflow.org/job-name=$(session_workload_name "$NAMESPACE" "$SESSION_NAME"),training.kubeflow.org/replica-type=master" \
  -o jsonpath='{.items[0].metadata.name}')
if ! kubectl -n "$NAMESPACE" exec "$DETACH_MARKER_MASTER" -c pytorch -- test -f /tmp/detach-marker; then
  echo "ERROR: detached command did not create marker on master pod" >&2
  exit 1
fi
echo "exec --detach verified"

echo "Multi-pod exec (pdsh) tests completed"

# ---------------------------------------------------------------------------
# File copy (okdev cp) verification
# ---------------------------------------------------------------------------

echo "Testing cp upload single file to target pod"
echo "cp-test-content" >"$SYNC_DIR/cp-test.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp "$SYNC_DIR/cp-test.txt" :/tmp/cp-test.txt
CP_VERIFY=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --role master --no-tty --no-prefix -- sh -lc 'cat /tmp/cp-test.txt')
if [[ "$CP_VERIFY" != "cp-test-content" ]]; then
  echo "ERROR: cp upload single file failed, got '$CP_VERIFY'" >&2
  exit 1
fi
echo "cp upload single file verified"

echo "Testing cp download single file from explicit container"
CP_CONTAINER_DL="$WORKDIR/cp-container-download.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp --container pytorch :/tmp/cp-test.txt "$CP_CONTAINER_DL"
CP_CONTAINER_CONTENT=$(cat "$CP_CONTAINER_DL")
if [[ "$CP_CONTAINER_CONTENT" != "cp-test-content" ]]; then
  echo "ERROR: cp --container download single file failed, got '$CP_CONTAINER_CONTENT'" >&2
  exit 1
fi
echo "cp download single file from explicit container verified"

echo "Testing cp upload directory to all pods"
CP_DIR="$WORKDIR/cp-upload-dir"
mkdir -p "$CP_DIR/sub"
echo "file-a" >"$CP_DIR/a.txt"
echo "file-b" >"$CP_DIR/sub/b.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp --all "$CP_DIR" :/tmp/cp-upload-dir
CP_ALL_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty -- sh -lc 'cat /tmp/cp-upload-dir/a.txt && cat /tmp/cp-upload-dir/sub/b.txt')
echo "$CP_ALL_OUTPUT"
CP_ALL_LINES=$(echo "$CP_ALL_OUTPUT" | grep -c 'file-a')
if [[ "$CP_ALL_LINES" -lt 3 ]]; then
  echo "ERROR: expected cp --all to reach 3 pods, got $CP_ALL_LINES" >&2
  exit 1
fi
echo "cp upload directory to all pods verified"

echo "Testing cp download from all pods"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty -- sh -lc 'echo download-$(hostname) > /tmp/cp-download.txt'
CP_DL_DIR="$WORKDIR/cp-download"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp --all :/tmp/cp-download.txt "$CP_DL_DIR"
CP_DL_COUNT=$(find "$CP_DL_DIR" -name 'cp-download.txt' | wc -l | tr -d ' ')
if [[ "$CP_DL_COUNT" -lt 3 ]]; then
  echo "ERROR: expected 3 per-pod download files, found $CP_DL_COUNT" >&2
  find "$CP_DL_DIR" -type f >&2
  exit 1
fi
echo "cp download from all pods verified"

echo "Testing cp download directory from all pods"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty -- sh -lc 'mkdir -p /tmp/cp-download-dir/nested && echo dir-$(hostname) > /tmp/cp-download-dir/root.txt && echo nested-$(hostname) > /tmp/cp-download-dir/nested/value.txt'
CP_DIR_DL="$WORKDIR/cp-dir-download"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp --all :/tmp/cp-download-dir "$CP_DIR_DL"
CP_DIR_ROOT_COUNT=$(find "$CP_DIR_DL" -path '*/cp-download-dir/root.txt' | wc -l | tr -d ' ')
CP_DIR_NESTED_COUNT=$(find "$CP_DIR_DL" -path '*/cp-download-dir/nested/value.txt' | wc -l | tr -d ' ')
CP_DIR_DUP_COUNT=$(find "$CP_DIR_DL" -path '*/cp-download-dir/cp-download-dir/root.txt' | wc -l | tr -d ' ')
if [[ "$CP_DIR_ROOT_COUNT" -lt 3 || "$CP_DIR_NESTED_COUNT" -lt 3 || "$CP_DIR_DUP_COUNT" -ne 0 ]]; then
  echo "ERROR: expected per-pod directory downloads without duplicated basename" >&2
  echo "root files: $CP_DIR_ROOT_COUNT, nested files: $CP_DIR_NESTED_COUNT, duplicated basename files: $CP_DIR_DUP_COUNT" >&2
  find "$CP_DIR_DL" -type f >&2
  exit 1
fi
echo "cp download directory from all pods verified"

echo "Testing cp download --role worker"
CP_ROLE_DIR="$WORKDIR/cp-role-download"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp --role worker :/tmp/cp-download.txt "$CP_ROLE_DIR"
CP_ROLE_COUNT=$(find "$CP_ROLE_DIR" -name 'cp-download.txt' | wc -l | tr -d ' ')
if [[ "$CP_ROLE_COUNT" -ne 2 ]]; then
  echo "ERROR: expected 2 per-pod download files for --role worker, found $CP_ROLE_COUNT" >&2
  find "$CP_ROLE_DIR" -type f >&2
  exit 1
fi
echo "cp download --role worker verified"

echo "File copy (okdev cp) tests completed"

# ---------------------------------------------------------------------------
# Lifecycle hook verification
# ---------------------------------------------------------------------------
refresh_pytorchjob_pods
echo "Master pod: $MASTER_POD"

echo "Worker pods: $WORKER_PODS"

WORKER_COUNT=$(echo "$WORKER_PODS" | wc -w | tr -d ' ')
if [[ "$WORKER_COUNT" -ne 2 ]]; then
  echo "ERROR: expected 2 worker pods, found $WORKER_COUNT" >&2
  exit 1
fi
echo "Worker pod count verified: $WORKER_COUNT"

# -- postCreate: runs only on the target (master) pod --
echo "Verifying postCreate ran on master pod"
kubectl -n "$NAMESPACE" exec "$MASTER_POD" -c pytorch -- test -f /tmp/post-create-marker
echo "postCreate marker verified on master"

echo "Verifying postCreate annotation on master pod"
POST_CREATE_ANN=$(kubectl -n "$NAMESPACE" get pod "$MASTER_POD" \
  -o jsonpath='{.metadata.annotations.okdev\.io/post-create-done}')
if [[ "$POST_CREATE_ANN" != "true" ]]; then
  echo "ERROR: okdev.io/post-create-done annotation missing or not true on master (got: '$POST_CREATE_ANN')" >&2
  exit 1
fi
echo "postCreate annotation verified on master"

echo "Verifying postCreate did NOT run on worker pods"
for WPOD in $WORKER_PODS; do
  if kubectl -n "$NAMESPACE" exec "$WPOD" -c pytorch -- test -f /tmp/post-create-marker 2>/dev/null; then
    echo "ERROR: postCreate marker unexpectedly found on worker pod $WPOD" >&2
    exit 1
  fi
  echo "postCreate correctly absent on $WPOD"
done

# -- postSync: runs on ALL pods after initial sync --
echo "Verifying postSync ran on master pod"
kubectl -n "$NAMESPACE" exec "$MASTER_POD" -c pytorch -- test -f /tmp/post-sync-marker
echo "postSync marker verified on master"

echo "Verifying postSync annotation on master pod"
POST_SYNC_ANN=$(kubectl -n "$NAMESPACE" get pod "$MASTER_POD" \
  -o jsonpath='{.metadata.annotations.okdev\.io/post-sync-done}')
if [[ "$POST_SYNC_ANN" != "true" ]]; then
  echo "ERROR: okdev.io/post-sync-done annotation missing or not true on master (got: '$POST_SYNC_ANN')" >&2
  exit 1
fi
echo "postSync annotation verified on master"

echo "Verifying postSync ran on all worker pods"
for WPOD in $WORKER_PODS; do
  kubectl -n "$NAMESPACE" exec "$WPOD" -c pytorch -- test -f /tmp/post-sync-marker
  echo "postSync marker verified on $WPOD"

  WANN=$(kubectl -n "$NAMESPACE" get pod "$WPOD" \
    -o jsonpath='{.metadata.annotations.okdev\.io/post-sync-done}')
  if [[ "$WANN" != "true" ]]; then
    echo "ERROR: okdev.io/post-sync-done annotation missing or not true on $WPOD (got: '$WANN')" >&2
    exit 1
  fi
  echo "postSync annotation verified on $WPOD"
done

# -- preStop: Kubernetes lifecycle handler injected on all pods --
echo "Verifying preStop lifecycle handler on master pod"
PRESTOP_CMD=$(kubectl -n "$NAMESPACE" get pod "$MASTER_POD" \
  -o jsonpath='{.spec.containers[?(@.name=="pytorch")].lifecycle.preStop.exec.command}')
if [[ "$PRESTOP_CMD" != *"pre-stop-marker"* ]]; then
  echo "ERROR: preStop lifecycle handler not found on master (got: '$PRESTOP_CMD')" >&2
  exit 1
fi
echo "preStop handler verified on master"

echo "Verifying preStop lifecycle handler on worker pods"
for WPOD in $WORKER_PODS; do
  WPRESTOP=$(kubectl -n "$NAMESPACE" get pod "$WPOD" \
    -o jsonpath='{.spec.containers[?(@.name=="pytorch")].lifecycle.preStop.exec.command}')
  if [[ "$WPRESTOP" != *"pre-stop-marker"* ]]; then
    echo "ERROR: preStop lifecycle handler not found on worker $WPOD (got: '$WPRESTOP')" >&2
    exit 1
  fi
  echo "preStop handler verified on $WPOD"
done

echo "Creating remote-only stale file before workspace reset"
kubectl -n "$NAMESPACE" exec "$MASTER_POD" -c pytorch -- sh -lc 'mkdir -p /workspace/generated && echo stale > /workspace/generated/stale.txt'
for WPOD in $WORKER_PODS; do
  kubectl -n "$NAMESPACE" exec "$WPOD" -c pytorch -- test -f /workspace/generated/stale.txt
done

echo "Updating local workspace before reset"
echo "hello after reset" >"$SYNC_DIR/hello.txt"
echo "fresh from local after reset" >"$SYNC_DIR/fresh.txt"

echo "Resetting remote workspace from local"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" sync reset-remote

echo "Waiting for local-authoritative workspace to converge after reset"
RESET_OK=false
for i in $(seq 1 45); do
  REMOTE_HELLO=$(kubectl -n "$NAMESPACE" exec "$MASTER_POD" -c pytorch -- sh -lc 'cat /workspace/hello.txt 2>/dev/null || true')
  REMOTE_FRESH=$(kubectl -n "$NAMESPACE" exec "$MASTER_POD" -c pytorch -- sh -lc 'cat /workspace/fresh.txt 2>/dev/null || true')
  if [[ "$REMOTE_HELLO" == "hello after reset" && "$REMOTE_FRESH" == "fresh from local after reset" ]]; then
    RESET_OK=true
    echo "Workspace reset converged on attempt $i"
    break
  fi
  echo "Reset convergence poll attempt $i/45: workspace not ready yet"
  sleep 2
done

if [[ "$RESET_OK" != "true" ]]; then
  echo "ERROR: workspace did not converge after reset-remote" >&2
  exit 1
fi

echo "Verifying stale remote-only file was removed from shared workspace"
if kubectl -n "$NAMESPACE" exec "$MASTER_POD" -c pytorch -- test -e /workspace/generated/stale.txt; then
  echo "ERROR: stale remote-only file still exists on master after reset-remote" >&2
  exit 1
fi
for WPOD in $WORKER_PODS; do
  if kubectl -n "$NAMESPACE" exec "$WPOD" -c pytorch -- test -e /workspace/generated/stale.txt; then
    echo "ERROR: stale remote-only file still exists on worker $WPOD after reset-remote" >&2
    exit 1
  fi
done
echo "Workspace reset verified across all pods"

echo "Verifying sync remains active after reset"
POST_RESET_STATUS=""
for i in $(seq 1 15); do
  POST_RESET_STATUS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
  if [[ "$POST_RESET_STATUS" == *"health: active"* ]]; then
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "ERROR: expected sync to remain active after reset-remote" >&2
    echo "$POST_RESET_STATUS" >&2
    exit 1
  fi
  sleep 2
done

echo "Verifying steady-state sync still propagates after reset"
echo "steady state after reset" >"$SYNC_DIR/post-reset.txt"
POST_RESET_SYNC_OK=false
for i in $(seq 1 30); do
  POST_RESET_CONTENT=$(kubectl -n "$NAMESPACE" exec "$MASTER_POD" -c pytorch -- sh -lc 'cat /workspace/post-reset.txt 2>/dev/null || true')
  if [[ "$POST_RESET_CONTENT" == "steady state after reset" ]]; then
    POST_RESET_SYNC_OK=true
    echo "Post-reset steady-state sync verified on attempt $i"
    break
  fi
  echo "Post-reset sync poll attempt $i/30: file not ready yet"
  sleep 2
done
if [[ "$POST_RESET_SYNC_OK" != "true" ]]; then
  echo "ERROR: expected post-reset local change to sync to remote workspace" >&2
  exit 1
fi

echo "Changing controller workload spec to trigger drift detection"
replace_all_in_file "$MANIFEST_PATH" 'image: ubuntu:22.04' 'image: ubuntu:24.04'

echo "Verifying non-interactive up fails with reconcile guidance"
set +e
DRIFT_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m 2>&1)
DRIFT_STATUS=$?
set -e
if [[ "$DRIFT_STATUS" -eq 0 ]]; then
  echo "ERROR: expected drifted controller workload to require --reconcile" >&2
  exit 1
fi
if [[ "$DRIFT_OUTPUT" != *"workload spec changed; re-run with --reconcile to recreate"* ]]; then
  echo "ERROR: unexpected controller drift output" >&2
  echo "$DRIFT_OUTPUT" >&2
  exit 1
fi
echo "Controller drift guidance verified"

echo "Recreating PyTorchJob via --reconcile"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --reconcile --wait-timeout 5m

echo "Verifying PyTorchJob spec and live pods were recreated with the new image"
RECONCILE_OK=false
for i in $(seq 1 30); do
  refresh_pytorchjob_pods
  WORKLOAD_NAME=$(session_workload_name "$NAMESPACE" "$SESSION_NAME")
  MASTER_IMAGE=$(kubectl -n "$NAMESPACE" get pytorchjob "$WORKLOAD_NAME" \
    -o jsonpath='{.spec.pytorchReplicaSpecs.Master.template.spec.containers[0].image}')
  WORKER_IMAGE=$(kubectl -n "$NAMESPACE" get pytorchjob "$WORKLOAD_NAME" \
    -o jsonpath='{.spec.pytorchReplicaSpecs.Worker.template.spec.containers[0].image}')
  MASTER_POD_IMAGE=$(kubectl -n "$NAMESPACE" get pod "$MASTER_POD" \
    -o jsonpath='{.spec.containers[?(@.name=="pytorch")].image}')
  LIVE_WORKER_OK=true
  for WPOD in $WORKER_PODS; do
    WPOD_IMAGE=$(kubectl -n "$NAMESPACE" get pod "$WPOD" \
      -o jsonpath='{.spec.containers[?(@.name=="pytorch")].image}')
    if [[ "$WPOD_IMAGE" != "ubuntu:24.04" ]]; then
      LIVE_WORKER_OK=false
      break
    fi
  done
  if [[ "$MASTER_IMAGE" == "ubuntu:24.04" && "$WORKER_IMAGE" == "ubuntu:24.04" && "$MASTER_POD_IMAGE" == "ubuntu:24.04" && "$LIVE_WORKER_OK" == "true" ]]; then
    RECONCILE_OK=true
    echo "Controller reconcile verified on attempt $i"
    break
  fi
  echo "Reconcile poll attempt $i/30: live workload not updated yet"
  sleep 2
done
if [[ "$RECONCILE_OK" != "true" ]]; then
  echo "ERROR: expected PyTorchJob spec and live pods to update to ubuntu:24.04, got master='$MASTER_IMAGE' worker='$WORKER_IMAGE' masterPod='$MASTER_POD_IMAGE'" >&2
  exit 1
fi

echo "Verifying session remains healthy after reconcile"
RECONCILE_STATUS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
if [[ "$RECONCILE_STATUS" != *"health: active"* ]]; then
  echo "ERROR: expected sync health to remain active after controller reconcile" >&2
  echo "$RECONCILE_STATUS" >&2
  exit 1
fi

echo "Testing explicit okdev down"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes
assert_no_local_sync_processes "$SESSION_NAME" "$HOME_DIR/.okdev/sessions/${SESSION_NAME}/syncthing"

echo "Verifying PyTorchJob is deleted"
for i in $(seq 1 15); do
  if [[ -z "$(session_attachable_pod_names "$NAMESPACE" "$SESSION_NAME")" ]]; then
    echo "PyTorchJob deleted on attempt $i"
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "ERROR: pytorchjob session ${SESSION_NAME} still has managed pod(s) after down" >&2
    exit 1
  fi
  sleep 2
done

echo "PyTorchJob smoke test completed (1 master + 2 workers, all lifecycle hooks verified)"
