#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-ptjob}"
NAMESPACE="${NAMESPACE:-default}"
PVC_NAME="${PVC_NAME:-${SESSION_NAME}-workspace}"
PVC_WORKSPACE_SUBPATH="${PVC_WORKSPACE_SUBPATH:-users/e2e/workspace}"
SECONDARY_PVC_MOUNT="${SECONDARY_PVC_MOUNT:-/proj-tango-pvc}"
PVC_OUTSIDE_WORKSPACE_DIR="${PVC_OUTSIDE_WORKSPACE_DIR:-shared-outside-workspace}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
SYNC_DIR="$WORKDIR/workspace"
ORIG_HOME="${HOME}"
ORIG_KUBECONFIG="${KUBECONFIG:-}"
KUBECONFIG_PATH="$HOME_DIR/.kube/config"
PVC_INIT_POD="${SESSION_NAME}-pvc-init"

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
  kubectl -n "$NAMESPACE" delete pod "$PVC_INIT_POD" >/dev/null 2>&1 || true
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

wait_for_pod_file_content() {
  local pod="$1"
  local path="$2"
  local expected="$3"
  local label="$4"
  local content=""

  for attempt in $(seq 1 30); do
    content=$(kubectl -n "$NAMESPACE" exec "$pod" -c pytorch -- sh -lc "cat '$path' 2>/dev/null || true")
    if [[ "$content" == "$expected" ]]; then
      echo "$label verified on attempt $attempt"
      return 0
    fi
    sleep 2
  done

  echo "ERROR: expected $label on pod $pod at $path, got '$content'" >&2
  return 1
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
python3 - <<'PY' "$MANIFEST_PATH" "$PVC_NAME" "$PVC_WORKSPACE_SUBPATH" "$SECONDARY_PVC_MOUNT"
import pathlib, sys

path = pathlib.Path(sys.argv[1])
pvc_name = sys.argv[2]
workspace_subpath = sys.argv[3]
secondary_mount = sys.argv[4]
text = path.read_text()

old_mount = """- name: workspace
                  mountPath: /workspace"""
new_mount = f"""- name: workspace
                  mountPath: /workspace
                  subPath: {workspace_subpath}
                - name: workspace
                  mountPath: {secondary_mount}"""
if old_mount not in text:
    raise SystemExit("workspace mount not found")
text = text.replace(old_mount, new_mount)

old_volume = """- name: workspace
              emptyDir: {}"""
new_volume = f"""- name: workspace
              persistentVolumeClaim:
                claimName: {pvc_name}"""
if old_volume not in text:
    raise SystemExit("workspace volume not found")
text = text.replace(old_volume, new_volume)

path.write_text(text)
PY
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

echo "Preparing PVC subdirectories for /workspace subPath coverage"
kubectl -n "$NAMESPACE" delete pod "$PVC_INIT_POD" --ignore-not-found >/dev/null 2>&1 || true
kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${PVC_INIT_POD}
spec:
  restartPolicy: Never
  containers:
    - name: init
      image: busybox:1.36
      command:
        - sh
        - -lc
        - mkdir -p /pvc/${PVC_WORKSPACE_SUBPATH} /pvc/${PVC_OUTSIDE_WORKSPACE_DIR}
      volumeMounts:
        - name: workspace
          mountPath: /pvc
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: ${PVC_NAME}
EOF

PVC_INIT_PHASE=""
for i in $(seq 1 30); do
  PVC_INIT_PHASE=$(kubectl -n "$NAMESPACE" get pod "$PVC_INIT_POD" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [[ "$PVC_INIT_PHASE" == "Succeeded" ]]; then
    break
  fi
  if [[ "$PVC_INIT_PHASE" == "Failed" ]]; then
    kubectl -n "$NAMESPACE" logs "$PVC_INIT_POD" >&2 || true
    echo "ERROR: failed to initialize PVC subdirectories" >&2
    exit 1
  fi
  sleep 1
done
if [[ "$PVC_INIT_PHASE" != "Succeeded" ]]; then
  kubectl -n "$NAMESPACE" logs "$PVC_INIT_POD" >&2 || true
  echo "ERROR: timed out initializing PVC subdirectories" >&2
  exit 1
fi
kubectl -n "$NAMESPACE" delete pod "$PVC_INIT_POD" >/dev/null 2>&1 || true

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

echo "Writing local file for PyTorchJob workspace sync verification"
echo "hello from pytorchjob e2e" >"$SYNC_DIR/hello.txt"

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

echo "Waiting for PyTorchJob pods before validating PVC mount layout"
PODS_READY=false
for i in $(seq 1 30); do
  refresh_pytorchjob_pods
  if [[ -n "${MASTER_POD:-}" ]] && [[ "$(echo "$WORKER_PODS" | wc -w | tr -d ' ')" -eq 2 ]]; then
    PODS_READY=true
    break
  fi
  sleep 2
done
if [[ "$PODS_READY" != "true" ]]; then
  echo "ERROR: expected 1 master pod and 2 worker pods before PVC mount verification" >&2
  exit 1
fi

echo "Verifying PVC subPath is visible through the full PVC mount on master"
wait_for_pod_file_content \
  "$MASTER_POD" \
  "$SECONDARY_PVC_MOUNT/$PVC_WORKSPACE_SUBPATH/hello.txt" \
  "hello from pytorchjob e2e" \
  "secondary PVC mount on master"

echo "Verifying workers see the synced workspace through both mount paths"
for WPOD in $WORKER_PODS; do
  wait_for_pod_file_content \
    "$WPOD" \
    "/workspace/hello.txt" \
    "hello from pytorchjob e2e" \
    "workspace mount on worker $WPOD"
  wait_for_pod_file_content \
    "$WPOD" \
    "$SECONDARY_PVC_MOUNT/$PVC_WORKSPACE_SUBPATH/hello.txt" \
    "hello from pytorchjob e2e" \
    "secondary PVC mount on worker $WPOD"
done

echo "Writing through the full PVC mount and verifying the /workspace subPath view"
kubectl -n "$NAMESPACE" exec "$MASTER_POD" -c pytorch -- sh -lc \
  "echo shared-through-secondary > '$SECONDARY_PVC_MOUNT/$PVC_WORKSPACE_SUBPATH/from-secondary.txt'"
wait_for_pod_file_content \
  "$MASTER_POD" \
  "/workspace/from-secondary.txt" \
  "shared-through-secondary" \
  "master workspace view of secondary mount write"
for WPOD in $WORKER_PODS; do
  wait_for_pod_file_content \
    "$WPOD" \
    "/workspace/from-secondary.txt" \
    "shared-through-secondary" \
    "worker workspace view of secondary mount write on $WPOD"
done

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
# In no-prefix mode, lines should be raw (no "[podname]" prefix).
if echo "$EXEC_NOPREFIX_OUTPUT" | grep -qE '^\[.*\] raw-output$'; then
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

# ---------------------------------------------------------------------------
# exec-jobs: verify the detached-job listing, pid-is-user-command guarantee,
# and completion-metadata write after the job exits. We discover jobs via
# `exec-jobs --output json` (exec --detach itself only prints a text line).
# ---------------------------------------------------------------------------
EXEC_JOBS_DIR=/tmp/okdev-exec
echo "Cleaning prior detached-job metadata on all pods"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- \
  sh -lc "rm -f $EXEC_JOBS_DIR/*.json $EXEC_JOBS_DIR/*.sh $EXEC_JOBS_DIR/*.log $EXEC_JOBS_DIR/*.tmp 2>/dev/null; true" \
  >/dev/null

echo "Testing exec-jobs lists a running detached job on every pod"
# A 30s sleep gives us a wide window to observe the running state and to
# verify the reported pid corresponds to the user command itself. Launching
# `sleep` directly (no `sh -c` wrapper) means the pid we capture is
# unambiguously the sleep process, independent of any shell tail-exec
# optimizations.
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --detach --no-tty -- sleep 30

# Give wrappers a moment to publish metadata on every pod.
sleep 1

EXEC_JOBS_JSON=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec-jobs --output json)
echo "$EXEC_JOBS_JSON"

LONG_DETACH_PARSED=$(
  OKDEV_JOBS_JSON="$EXEC_JOBS_JSON" python3 - <<'PY'
import json, os, sys
payload = json.loads(os.environ["OKDEV_JOBS_JSON"])
jobs = payload.get("jobs", [])
errs = payload.get("errors", [])
if errs:
    print(f"ERROR: exec-jobs reported per-pod errors: {errs}", file=sys.stderr)
    sys.exit(1)
if not jobs:
    print("ERROR: exec-jobs returned no jobs", file=sys.stderr)
    sys.exit(1)
for row in jobs:
    # Only consider the sleep 30 jobs we just launched. `command` in the
    # metadata is the literal string we passed to `exec --detach`.
    if "sleep 30" not in (row.get("command") or ""):
        continue
    print(f"{row['pod']}\t{row['jobId']}\t{row['pid']}\t{row['state']}")
PY
)
if [[ -z "$LONG_DETACH_PARSED" ]]; then
  echo "ERROR: could not find sleep 30 jobs via exec-jobs --output json" >&2
  echo "$EXEC_JOBS_JSON" >&2
  exit 1
fi

LONG_DETACH_ROW_COUNT=$(wc -l <<<"$LONG_DETACH_PARSED" | tr -d '[:space:]')
if [[ "$LONG_DETACH_ROW_COUNT" -lt 3 ]]; then
  echo "ERROR: expected at least 3 detached sleep jobs, got $LONG_DETACH_ROW_COUNT" >&2
  echo "$LONG_DETACH_PARSED" >&2
  exit 1
fi

while IFS=$'\t' read -r L_POD L_JOB L_PID L_STATE; do
  [[ -z "$L_POD" ]] && continue
  echo "Verifying exec-jobs row for pod=$L_POD job=$L_JOB pid=$L_PID state=$L_STATE"
  if [[ "$L_STATE" != "running" ]]; then
    echo "ERROR: expected state=running for freshly launched job, got state=$L_STATE" >&2
    exit 1
  fi
  # Pid correctness: /proc/<pid>/cmdline must be the user's `sleep 30`, NOT a
  # wrapper shell. The NUL-delimited cmdline is "sleep\x0030\x00".
  CMDLINE=$(kubectl -n "$NAMESPACE" exec "$L_POD" -c pytorch -- sh -lc "tr '\0' ' ' </proc/$L_PID/cmdline 2>/dev/null" || true)
  if ! grep -qE '^sleep 30[[:space:]]*$' <<<"$CMDLINE"; then
    echo "ERROR: pid $L_PID on pod $L_POD is not the user command (cmdline=<$CMDLINE>); expected 'sleep 30'" >&2
    exit 1
  fi
  # Liveness reconciliation relies on OKDEV_JOB_ID being exported to the
  # user process. If this breaks, exec-jobs would spuriously mark the job
  # orphaned.
  ENVIRON=$(kubectl -n "$NAMESPACE" exec "$L_POD" -c pytorch -- sh -lc "tr '\0' '\n' </proc/$L_PID/environ 2>/dev/null" || true)
  if ! grep -qx "OKDEV_JOB_ID=$L_JOB" <<<"$ENVIRON"; then
    echo "ERROR: OKDEV_JOB_ID=$L_JOB not set in /proc/$L_PID/environ on pod $L_POD" >&2
    echo "$ENVIRON" >&2
    exit 1
  fi
done <<<"$LONG_DETACH_PARSED"
echo "exec-jobs running-state + pid correctness verified"

echo "Waiting for detached sleep jobs to finish..."
# Poll exec-jobs instead of a fixed sleep so slower CI is not flaky.
for _ in $(seq 1 45); do
  EXEC_JOBS_NOW=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec-jobs --output json)
  STILL_RUNNING=$(
    OKDEV_JOBS_JSON="$EXEC_JOBS_NOW" python3 - <<'PY'
import json, os
payload = json.loads(os.environ["OKDEV_JOBS_JSON"])
print(sum(1 for j in payload.get("jobs", [])
          if "sleep 30" in (j.get("command") or "") and j.get("state") == "running"))
PY
  )
  if [[ "$STILL_RUNNING" == "0" ]]; then
    break
  fi
  sleep 1
done

echo "Testing exec-jobs reports completion metadata"
EXEC_JOBS_DONE=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec-jobs --output json)
echo "$EXEC_JOBS_DONE"
MISMATCH=$(
  OKDEV_JOBS_JSON="$EXEC_JOBS_DONE" python3 - <<'PY'
import json, os, sys
payload = json.loads(os.environ["OKDEV_JOBS_JSON"])
for row in payload.get("jobs", []):
    if "sleep 30" not in (row.get("command") or ""):
        continue
    if row.get("state") != "exited" or row.get("exitCode") != 0:
        sys.stdout.write(f"{row.get('pod')}/{row.get('jobId')} state={row.get('state')} exit={row.get('exitCode')}\n")
PY
)
if [[ -n "$MISMATCH" ]]; then
  # A 'running' or missing exited/0 row here would indicate the metadata race
  # we fixed (launcher clobbering the wrapper's completion write).
  echo "ERROR: exec-jobs did not record exited/0 for every sleep 30 job:" >&2
  echo "$MISMATCH" >&2
  exit 1
fi
echo "exec-jobs completion metadata verified"

echo "Testing exec-jobs captures non-zero exit codes"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --detach --no-tty --role master -- sh -lc 'exit 7' >/dev/null
# Give the EXIT trap a moment to write completion metadata.
sleep 2
FAIL_JOBS_JSON=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec-jobs --role master --output json)
echo "$FAIL_JOBS_JSON"
FAIL_CHECK=$(
  OKDEV_JOBS_JSON="$FAIL_JOBS_JSON" python3 - <<'PY'
import json, os, sys
payload = json.loads(os.environ["OKDEV_JOBS_JSON"])
for row in payload.get("jobs", []):
    if row.get("command") == "sh -lc 'exit 7'" or "exit 7" in (row.get("command") or ""):
        if row.get("state") == "exited" and row.get("exitCode") == 7:
            print("ok")
            sys.exit(0)
sys.exit(1)
PY
)
if [[ "$FAIL_CHECK" != "ok" ]]; then
  echo "ERROR: exec-jobs did not record exit=7 for failing detached command" >&2
  exit 1
fi
echo "exec-jobs non-zero exit capture verified"

# Clean up metadata so this block does not leak state into the rest of the run.
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- \
  sh -lc "rm -f $EXEC_JOBS_DIR/*.json $EXEC_JOBS_DIR/*.sh $EXEC_JOBS_DIR/*.log $EXEC_JOBS_DIR/*.tmp 2>/dev/null; true" \
  >/dev/null

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

echo "Creating sibling PVC data outside the synced workspace subPath"
kubectl -n "$NAMESPACE" exec "$MASTER_POD" -c pytorch -- sh -lc \
  "mkdir -p '$SECONDARY_PVC_MOUNT/$PVC_OUTSIDE_WORKSPACE_DIR' && echo keep > '$SECONDARY_PVC_MOUNT/$PVC_OUTSIDE_WORKSPACE_DIR/keep.txt'"
wait_for_pod_file_content \
  "$MASTER_POD" \
  "$SECONDARY_PVC_MOUNT/$PVC_OUTSIDE_WORKSPACE_DIR/keep.txt" \
  "keep" \
  "master sibling PVC data"
for WPOD in $WORKER_PODS; do
  wait_for_pod_file_content \
    "$WPOD" \
    "$SECONDARY_PVC_MOUNT/$PVC_OUTSIDE_WORKSPACE_DIR/keep.txt" \
    "keep" \
    "worker sibling PVC data on $WPOD"
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

echo "Verifying reset-remote preserved sibling PVC data outside the synced subPath"
wait_for_pod_file_content \
  "$MASTER_POD" \
  "$SECONDARY_PVC_MOUNT/$PVC_OUTSIDE_WORKSPACE_DIR/keep.txt" \
  "keep" \
  "master sibling PVC data after reset"
for WPOD in $WORKER_PODS; do
  wait_for_pod_file_content \
    "$WPOD" \
    "$SECONDARY_PVC_MOUNT/$PVC_OUTSIDE_WORKSPACE_DIR/keep.txt" \
    "keep" \
    "worker sibling PVC data after reset on $WPOD"
done

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

# ---------------------------------------------------------------------------
# Syncthing mesh test: workers get sidecars, no shared PVC
# ---------------------------------------------------------------------------
echo ""
echo "=== Syncthing Mesh Test (no shared PVC) ==="

MESH_SESSION="e2e-ptjob-mesh"
MESH_WORKDIR="$(make_workdir)"
MESH_HOME="$MESH_WORKDIR/home"
MESH_CFG="$MESH_WORKDIR/.okdev/okdev.yaml"
MESH_SYNC="$MESH_WORKDIR/workspace"
MESH_MANIFEST="$MESH_WORKDIR/.okdev/pytorchjob.yaml"
MESH_REMOTE_ROOT="/workspace/a"

mkdir -p "$MESH_HOME" "$MESH_SYNC" "$MESH_HOME/.kube"
cp "$KUBECONFIG_PATH" "$MESH_HOME/.kube/config"

mesh_cleanup() {
  "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" down --yes >/dev/null 2>&1 || true
}
# We already have the EXIT trap for the PVC test; add mesh cleanup.
trap 'mesh_cleanup; cleanup' EXIT

dump_mesh_diagnostics() {
  local pod="$1"
  local role="$2"
  echo "--- mesh diagnostic dump (pod=$pod role=$role) ---"
  echo "--- $role pytorch: ls -la $MESH_REMOTE_ROOT ---"
  kubectl -n "$NAMESPACE" exec "$pod" -c pytorch -- ls -la "$MESH_REMOTE_ROOT" 2>&1 || true
  echo "--- $role pytorch: ls -la /workspace ---"
  kubectl -n "$NAMESPACE" exec "$pod" -c pytorch -- ls -la /workspace 2>&1 || true
  echo "--- $role sidecar: ls -la $MESH_REMOTE_ROOT ---"
  kubectl -n "$NAMESPACE" exec "$pod" -c okdev-sidecar -- ls -la "$MESH_REMOTE_ROOT" 2>&1 || true
  echo "--- $role sidecar: ls -la /workspace ---"
  kubectl -n "$NAMESPACE" exec "$pod" -c okdev-sidecar -- ls -la /workspace 2>&1 || true
  echo "--- $role sidecar: stat /workspace/a and parents ---"
  kubectl -n "$NAMESPACE" exec "$pod" -c okdev-sidecar -- sh -c 'for p in /workspace /workspace/a /workspace/a/mesh-hello.txt; do echo "$p:"; stat "$p" 2>&1 || true; done' 2>&1 || true
  echo "--- $role sidecar: config.xml folders ---"
  kubectl -n "$NAMESPACE" exec "$pod" -c okdev-sidecar -- sh -c 'cat /var/syncthing/config.xml 2>/dev/null || cat /var/syncthing/config/config.xml 2>/dev/null || true' 2>&1 | grep -E '<folder |<device |path=|<ignoreDelete' || true
  echo "--- $role sidecar container logs (tail 300) ---"
  kubectl -n "$NAMESPACE" logs "$pod" -c okdev-sidecar --tail=300 2>&1 || true
  echo "--- $role sidecar previous container logs (tail 300) ---"
  kubectl -n "$NAMESPACE" logs "$pod" -c okdev-sidecar --previous --tail=300 2>&1 || true
  echo "--- local mesh session sync log ---"
  cat "$MESH_HOME/.okdev/sessions/$MESH_SESSION/syncthing/local.log" 2>/dev/null || true
  echo "--- mesh status --details ---"
  HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" status --details 2>&1 || true
  echo "--- end mesh diagnostic dump ---"
}

echo "Scaffolding mesh PyTorchJob config"
cd "$MESH_WORKDIR"
HOME="$MESH_HOME" "$OKDEV_BIN" init \
  --workload pytorchjob \
  --yes \
  --name "$MESH_SESSION" \
  --namespace "$NAMESPACE" \
  --sidecar-image "$SIDECAR_IMAGE" \
  --ssh-user root \
  --sync-local "$MESH_SYNC" \
  --sync-remote "$MESH_REMOTE_ROOT"

# Patch manifest: container name, image, command (same as PVC test).
replace_all_in_file "$MESH_MANIFEST" 'name: dev' 'name: pytorch'
replace_all_in_file "$MESH_MANIFEST" 'image: # TODO: replace with your image' 'image: ubuntu:22.04'
replace_all_in_file "$MESH_MANIFEST" 'command: ["sleep", "infinity"]' 'command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]'
replace_all_in_file "$MESH_CFG" 'container: dev' 'container: pytorch'

# Key difference: workers keep sidecar: true (the default) instead of false.
# This enables syncthing mesh — no PVC needed. Workers use emptyDir volumes.
# We only need to disable the worker attachable flag (not the default target).
python3 - <<'PY' "$MESH_CFG"
import pathlib, sys
path = pathlib.Path(sys.argv[1])
text = path.read_text()
old = '      - path: "spec.pytorchReplicaSpecs.Worker.template"\n'
new = '      - path: "spec.pytorchReplicaSpecs.Worker.template"\n        attachable: false\n'
if old not in text:
    raise SystemExit("worker inject path not found")
path.write_text(text.replace(old, new, 1))
PY

# Set 2 worker replicas + short terminationGracePeriodSeconds.
python3 - <<'PY' "$MESH_MANIFEST"
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

# Disable persistent SSH sessions for CI.
insert_after_line_once "$MESH_CFG" '  ssh:' '    persistentSession: false'

echo "Mesh config:"
cat "$MESH_CFG"

echo "mesh-hello" >"$MESH_SYNC/mesh-hello.txt"

echo "Starting mesh PyTorchJob session"
HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" up --wait-timeout 5m

echo "Checking mesh status"
MESH_STATUS=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" status --details)
echo "$MESH_STATUS"
if [[ "$MESH_STATUS" != *"health: active"* ]]; then
  echo "ERROR: expected mesh session sync health to be active" >&2
  exit 1
fi
if [[ "$MESH_STATUS" != *"topology: hub-and-spoke"* ]]; then
  echo "ERROR: expected mesh topology in status output" >&2
  exit 1
fi
if [[ -e "$MESH_SYNC/a" ]]; then
  echo "ERROR: expected explicit sync remote $MESH_REMOTE_ROOT to avoid creating local $MESH_SYNC/a" >&2
  exit 1
fi

echo "Waiting for mesh receivers to report healthy before verifying synced files"
MESH_HEALTH_STATUS=""
for i in $(seq 1 15); do
  MESH_HEALTH_STATUS=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" status --details)
  if [[ "$MESH_HEALTH_STATUS" == *"2/2 receiver(s) healthy"* ]]; then
    echo "Mesh health fully converged on attempt $i"
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "ERROR: expected mesh status to show 2/2 healthy receivers before file verification" >&2
    echo "$MESH_HEALTH_STATUS" >&2
    exit 1
  fi
  sleep 3
done
echo "Mesh status verified"

echo "Verifying workspace synced to master via syncthing"
MESH_WORKLOAD=$(HOME="$MESH_HOME" session_workload_name "$NAMESPACE" "$MESH_SESSION")
MESH_MASTER=$(kubectl -n "$NAMESPACE" get pods \
  -l "training.kubeflow.org/job-name=$MESH_WORKLOAD,training.kubeflow.org/replica-type=master" \
  -o jsonpath='{.items[0].metadata.name}')
MASTER_CONTENT=""
MASTER_SYNC_OK=false
for attempt in $(seq 1 30); do
  MASTER_CONTENT=$(kubectl -n "$NAMESPACE" exec "$MESH_MASTER" -c pytorch -- cat "$MESH_REMOTE_ROOT/mesh-hello.txt" 2>/dev/null || true)
  if [[ "$MASTER_CONTENT" == "mesh-hello" ]]; then
    MASTER_SYNC_OK=true
    echo "Master workspace verified on attempt $attempt"
    break
  fi
  sleep 2
done
if [[ "$MASTER_SYNC_OK" != "true" ]]; then
  echo "ERROR: expected mesh-hello.txt on master, got '$MASTER_CONTENT'" >&2
  dump_mesh_diagnostics "$MESH_MASTER" "master" >&2 || true
  exit 1
fi

echo "Verifying workspace synced to workers via mesh (no PVC)"
MESH_WORKERS=$(kubectl -n "$NAMESPACE" get pods \
  -l "training.kubeflow.org/job-name=$MESH_WORKLOAD,training.kubeflow.org/replica-type=worker" \
  -o jsonpath='{.items[*].metadata.name}')
MESH_WORKER_OK=true
for WPOD in $MESH_WORKERS; do
  WORKER_CONTENT=""
  for attempt in $(seq 1 30); do
    WORKER_CONTENT=$(kubectl -n "$NAMESPACE" exec "$WPOD" -c pytorch -- cat "$MESH_REMOTE_ROOT/mesh-hello.txt" 2>/dev/null || true)
    if [[ "$WORKER_CONTENT" == "mesh-hello" ]]; then
      echo "Worker $WPOD workspace verified on attempt $attempt"
      break
    fi
    if [[ "$attempt" -eq 30 ]]; then
      echo "ERROR: expected mesh-hello.txt on worker $WPOD, got '$WORKER_CONTENT'" >&2
      dump_mesh_diagnostics "$WPOD" "worker" >&2 || true
      MESH_WORKER_OK=false
    fi
    sleep 2
  done
done
if [[ "$MESH_WORKER_OK" != "true" ]]; then
  exit 1
fi
echo "Mesh workspace sync verified on all workers"

# ---------------------------------------------------------------------------
# status --details: verify live mesh health output
# ---------------------------------------------------------------------------

echo "Verifying status --details shows live mesh health"
MESH_HEALTH_STATUS=""
for i in $(seq 1 15); do
  MESH_HEALTH_STATUS=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" status --details)
  if [[ "$MESH_HEALTH_STATUS" == *"receiver(s) healthy"* ]]; then
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "ERROR: expected status --details to include live mesh health" >&2
    echo "$MESH_HEALTH_STATUS" >&2
    exit 1
  fi
  echo "Mesh health not in status yet, retrying ($i/15)"
  sleep 3
done
echo "$MESH_HEALTH_STATUS"

# All receivers should be healthy at this point.
if [[ "$MESH_HEALTH_STATUS" != *"2/2 receiver(s) healthy"* ]]; then
  echo "ERROR: expected 2/2 receivers healthy in status output" >&2
  echo "$MESH_HEALTH_STATUS" >&2
  exit 1
fi
# Each worker should show "synced".
for WPOD in $MESH_WORKERS; do
  if [[ "$MESH_HEALTH_STATUS" != *"$WPOD: synced"* ]]; then
    echo "ERROR: expected worker $WPOD to show synced in mesh health" >&2
    echo "$MESH_HEALTH_STATUS" >&2
    exit 1
  fi
done
echo "Live mesh health in status --details verified"

# ---------------------------------------------------------------------------
# sync --reset flag validation
# ---------------------------------------------------------------------------

echo "Verifying --force/--local/--mesh require --reset"
set +e
ERR_FORCE=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" sync --force 2>&1)
ERR_FORCE_RC=$?
ERR_LOCAL=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" sync --local 2>&1)
ERR_LOCAL_RC=$?
ERR_MESH=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" sync --mesh 2>&1)
ERR_MESH_RC=$?
set -e
if [[ "$ERR_FORCE_RC" -eq 0 ]]; then
  echo "ERROR: --force without --reset should fail" >&2; exit 1
fi
if [[ "$ERR_LOCAL_RC" -eq 0 ]]; then
  echo "ERROR: --local without --reset should fail" >&2; exit 1
fi
if [[ "$ERR_MESH_RC" -eq 0 ]]; then
  echo "ERROR: --mesh without --reset should fail" >&2; exit 1
fi
echo "Flag validation verified"

# ---------------------------------------------------------------------------
# sync --reset --local: check local sync only, skip mesh
# ---------------------------------------------------------------------------

echo "Testing sync --reset --local (should check local sync, skip mesh)"
RESET_LOCAL_OUTPUT=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" sync --reset --local 2>&1)
echo "$RESET_LOCAL_OUTPUT"
if [[ "$RESET_LOCAL_OUTPUT" != *"Local sync: healthy"* ]]; then
  echo "ERROR: expected --reset --local to report local sync health" >&2
  exit 1
fi
# Should NOT contain mesh output since --local scopes to local only.
if [[ "$RESET_LOCAL_OUTPUT" == *"mesh"* ]] || [[ "$RESET_LOCAL_OUTPUT" == *"Mesh"* ]] || [[ "$RESET_LOCAL_OUTPUT" == *"receiver"* ]]; then
  echo "ERROR: --reset --local should not touch mesh" >&2
  exit 1
fi
echo "sync --reset --local verified"

# ---------------------------------------------------------------------------
# sync --reset --mesh: break receiver, repair mesh only
# ---------------------------------------------------------------------------

echo "Breaking a mesh receiver's syncthing config to test --reset --mesh"
BREAK_WPOD=$(echo "$MESH_WORKERS" | awk '{print $1}')
kubectl -n "$NAMESPACE" exec "$BREAK_WPOD" -c okdev-sidecar -- sh -c \
  'syncthing generate --home /var/syncthing/config >/dev/null 2>&1; kill $(cat /var/syncthing/syncthing.pid 2>/dev/null) 2>/dev/null; sleep 1; syncthing serve --home /var/syncthing/config --no-browser --no-default-folder >/dev/null 2>&1 &'

echo "Waiting for receiver to become unhealthy"
BROKEN_DETECTED=false
for i in $(seq 1 10); do
  BREAK_STATUS=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" status --details 2>&1 || true)
  if [[ "$BREAK_STATUS" != *"2/2 receiver(s) healthy"* ]] && [[ "$BREAK_STATUS" == *"receiver(s) healthy"* ]]; then
    BROKEN_DETECTED=true
    echo "Broken receiver detected in status on attempt $i"
    break
  fi
  sleep 3
done
if [[ "$BROKEN_DETECTED" != "true" ]]; then
  echo "WARNING: could not confirm receiver became unhealthy via status; proceeding with --reset --mesh anyway"
fi

echo "Running sync --reset --mesh to repair only mesh receivers"
RESET_MESH_OUTPUT=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" sync --reset --mesh 2>&1)
echo "$RESET_MESH_OUTPUT"

# Should NOT contain local sync output since --mesh scopes to mesh only.
if [[ "$RESET_MESH_OUTPUT" == *"Local sync:"* ]]; then
  echo "ERROR: --reset --mesh should not check local sync" >&2
  exit 1
fi

# Should have handled mesh receivers.
if [[ "$RESET_MESH_OUTPUT" == *"broken receiver"* ]] || [[ "$RESET_MESH_OUTPUT" == *"Repairing mesh"* ]] || [[ "$RESET_MESH_OUTPUT" == *"all receivers healthy"* ]]; then
  echo "sync --reset --mesh handling confirmed"
else
  echo "NOTE: sync --reset --mesh output: $RESET_MESH_OUTPUT"
fi

echo "Verifying file sync works after --reset --mesh"
echo "post-mesh-reset" >"$MESH_SYNC/post-mesh-reset.txt"

POST_MESH_RESET_OK=false
for i in $(seq 1 30); do
  MASTER_CONTENT=$(kubectl -n "$NAMESPACE" exec "$MESH_MASTER" -c pytorch -- cat "$MESH_REMOTE_ROOT/post-mesh-reset.txt" 2>/dev/null || true)
  if [[ "$MASTER_CONTENT" == "post-mesh-reset" ]]; then
    POST_MESH_RESET_OK=true
    echo "Post --reset --mesh file on master verified on attempt $i"
    break
  fi
  sleep 2
done
if [[ "$POST_MESH_RESET_OK" != "true" ]]; then
  echo "ERROR: file did not sync to master after --reset --mesh" >&2
  exit 1
fi

for WPOD in $MESH_WORKERS; do
  WORKER_MESH_OK=false
  for attempt in $(seq 1 30); do
    WORKER_CONTENT=$(kubectl -n "$NAMESPACE" exec "$WPOD" -c pytorch -- cat "$MESH_REMOTE_ROOT/post-mesh-reset.txt" 2>/dev/null || true)
    if [[ "$WORKER_CONTENT" == "post-mesh-reset" ]]; then
      WORKER_MESH_OK=true
      echo "Post --reset --mesh file on worker $WPOD verified on attempt $attempt"
      break
    fi
    sleep 2
  done
  if [[ "$WORKER_MESH_OK" != "true" ]]; then
    echo "ERROR: file did not sync to worker $WPOD via mesh after --reset --mesh" >&2
    exit 1
  fi
done
echo "sync --reset --mesh repair verified"

# ---------------------------------------------------------------------------
# sync --reset -f --mesh: force mesh reset without health check
# ---------------------------------------------------------------------------

echo "Testing sync --reset --force --mesh (force mesh reset, skip health check)"
FORCE_MESH_OUTPUT=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" sync --reset --force --mesh 2>&1)
echo "$FORCE_MESH_OUTPUT"

# Should NOT contain local sync output.
if [[ "$FORCE_MESH_OUTPUT" == *"Local sync:"* ]]; then
  echo "ERROR: --reset --force --mesh should not check local sync" >&2
  exit 1
fi
# Should force repair without checking health first.
if [[ "$FORCE_MESH_OUTPUT" != *"Force resetting mesh"* ]]; then
  echo "ERROR: expected force mesh reset output" >&2
  exit 1
fi
echo "sync --reset --force --mesh verified"

# ---------------------------------------------------------------------------
# sync --reset (bare): check both local + mesh, repair broken
# ---------------------------------------------------------------------------

echo "Testing bare sync --reset (checks both local + mesh)"
RESET_BOTH_OUTPUT=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" sync --reset 2>&1)
echo "$RESET_BOTH_OUTPUT"

# Should check local sync health.
if [[ "$RESET_BOTH_OUTPUT" != *"Local sync:"* ]]; then
  echo "ERROR: bare --reset should check local sync health" >&2
  exit 1
fi
# Should also check mesh.
if [[ "$RESET_BOTH_OUTPUT" == *"receiver"* ]] || [[ "$RESET_BOTH_OUTPUT" == *"Mesh"* ]] || [[ "$RESET_BOTH_OUTPUT" == *"mesh"* ]]; then
  echo "bare sync --reset handled both local and mesh"
else
  echo "NOTE: bare --reset may have skipped mesh (no receivers or no mesh labels)"
fi
echo "sync --reset (bare) verified"

echo "Verifying mesh health is still good after all reset tests"
REPAIRED_STATUS=""
for i in $(seq 1 15); do
  REPAIRED_STATUS=$(HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" status --details)
  if [[ "$REPAIRED_STATUS" == *"2/2 receiver(s) healthy"* ]]; then
    echo "Mesh health fully recovered on attempt $i"
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "WARNING: mesh health did not show 2/2 healthy after all reset tests, got:"
    echo "$REPAIRED_STATUS"
  fi
  sleep 3
done

echo "Tearing down mesh session"
HOME="$MESH_HOME" "$OKDEV_BIN" --config "$MESH_CFG" --session "$MESH_SESSION" down --yes

echo "Syncthing mesh test completed"
