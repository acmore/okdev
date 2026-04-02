#!/usr/bin/env bash
set -euo pipefail

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-job}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(mktemp -d)"
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
    echo "--- background sync log ---"
    cat "$HOME_DIR/.okdev/logs/syncthing-${SESSION_NAME}.log" 2>/dev/null || true
    echo "--- local syncthing log ---"
    cat "$HOME_DIR/.okdev/syncthing/${SESSION_NAME}/local.log" 2>/dev/null || true
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

sed -i 's/image: # TODO:.*/image: ubuntu:22.04/g' "$MANIFEST_PATH"
sed -i 's/command: \["sleep", "infinity"\]/command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]/g' "$MANIFEST_PATH"
sed -i '/ssh:/a\    persistentSession: false' "$CFG_PATH"

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
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --cmd 'if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi' || true)
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

ORIGINAL_JOB_UID=$(kubectl -n "$NAMESPACE" get job "$SESSION_NAME" -o jsonpath='{.metadata.uid}')
ORIGINAL_POD_UID=$(kubectl -n "$NAMESPACE" get pods -l "job-name=$SESSION_NAME" -o jsonpath='{.items[0].metadata.uid}')

echo "Changing job workload spec to trigger drift detection"
sed -i '0,/image: ubuntu:22.04/s//image: ubuntu:24.04/' "$MANIFEST_PATH"

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
