#!/usr/bin/env bash
set -euo pipefail

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-ptjob}"
NAMESPACE="${NAMESPACE:-default}"
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

echo "Starting PyTorchJob smoke session (1 master + 2 workers)"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m

echo "Checking status"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details

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
