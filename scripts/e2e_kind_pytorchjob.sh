#!/usr/bin/env bash
set -euo pipefail

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-ptjob}"
NAMESPACE="${NAMESPACE:-default}"
PVC_NAME="${PVC_NAME:-${SESSION_NAME}-workspace}"
WORKDIR="$(mktemp -d)"
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
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes >/dev/null 2>&1 || true
  kubectl -n "$NAMESPACE" delete pvc "$PVC_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

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
sed -i 's/name: dev/name: pytorch/g' "$MANIFEST_PATH"
sed -i 's/image: # TODO:.*/image: ubuntu:22.04/g' "$MANIFEST_PATH"
sed -i 's/command: \["sleep", "infinity"\]/command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]/g' "$MANIFEST_PATH"
perl -0pi -e 's/- name: workspace\n              emptyDir: \{\}/- name: workspace\n              persistentVolumeClaim:\n                claimName: '"$PVC_NAME"'/g' "$MANIFEST_PATH"
sed -i 's/container: dev/container: pytorch/' "$CFG_PATH"

# Set 2 worker replicas for multi-pod testing (1 master + 2 workers = 3 pods).
sed -i '/Worker:/,/replicas:/ s/replicas: 1/replicas: 2/' "$MANIFEST_PATH"

# Disable persistent SSH sessions for CI
sed -i '/ssh:/a\    persistentSession: false' "$CFG_PATH"

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
STATUS_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
echo "$STATUS_OUTPUT"
if [[ "$STATUS_OUTPUT" != *"health: active"* ]]; then
  echo "ERROR: expected status --details to report active sync" >&2
  exit 1
fi
echo "Sync health status verified"

echo "Waiting for synced file to appear remotely with correct content"
SYNC_OK=false
for i in $(seq 1 30); do
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" ssh --setup-key --cmd 'sh -lc "if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi"' || true)
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
# Lifecycle hook verification
# ---------------------------------------------------------------------------
MASTER_POD=$(kubectl -n "$NAMESPACE" get pods \
  -l "training.kubeflow.org/job-name=$SESSION_NAME,training.kubeflow.org/replica-type=master" \
  -o jsonpath='{.items[0].metadata.name}')
echo "Master pod: $MASTER_POD"

WORKER_PODS=$(kubectl -n "$NAMESPACE" get pods \
  -l "training.kubeflow.org/job-name=$SESSION_NAME,training.kubeflow.org/replica-type=worker" \
  -o jsonpath='{.items[*].metadata.name}')
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

echo "Changing controller workload spec to trigger drift detection"
sed -i 's/image: ubuntu:22.04/image: ubuntu:24.04/g' "$MANIFEST_PATH"

echo "Verifying non-interactive up fails with reconcile guidance"
set +e
DRIFT_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m 2>&1)
DRIFT_STATUS=$?
set -e
if [[ "$DRIFT_STATUS" -eq 0 ]]; then
  echo "ERROR: expected drifted controller workload to require --reconcile" >&2
  exit 1
fi
if [[ "$DRIFT_OUTPUT" != *"workload spec changed; re-run with --reconcile to apply"* ]]; then
  echo "ERROR: unexpected controller drift output" >&2
  echo "$DRIFT_OUTPUT" >&2
  exit 1
fi
echo "Controller drift guidance verified"

echo "Reapplying PyTorchJob via --reconcile"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --reconcile --wait-timeout 5m

echo "Verifying PyTorchJob spec was updated in place"
MASTER_IMAGE=$(kubectl -n "$NAMESPACE" get pytorchjob "$SESSION_NAME" \
  -o jsonpath='{.spec.pytorchReplicaSpecs.Master.template.spec.containers[0].image}')
WORKER_IMAGE=$(kubectl -n "$NAMESPACE" get pytorchjob "$SESSION_NAME" \
  -o jsonpath='{.spec.pytorchReplicaSpecs.Worker.template.spec.containers[0].image}')
if [[ "$MASTER_IMAGE" != "ubuntu:24.04" || "$WORKER_IMAGE" != "ubuntu:24.04" ]]; then
  echo "ERROR: expected PyTorchJob images to update to ubuntu:24.04, got master='$MASTER_IMAGE' worker='$WORKER_IMAGE'" >&2
  exit 1
fi
echo "Controller reconcile verified"

echo "Testing explicit okdev down"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes

echo "Verifying PyTorchJob is deleted"
for i in $(seq 1 15); do
  if ! kubectl -n "$NAMESPACE" get pytorchjob "$SESSION_NAME" >/dev/null 2>&1; then
    echo "PyTorchJob deleted on attempt $i"
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "ERROR: pytorchjob ${SESSION_NAME} still exists after down" >&2
    exit 1
  fi
  sleep 2
done

echo "PyTorchJob smoke test completed (1 master + 2 workers, all lifecycle hooks verified)"
