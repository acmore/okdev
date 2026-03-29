#!/usr/bin/env bash
set -euo pipefail

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-ptjob}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(mktemp -d)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH="$WORKDIR/.okdev.yaml"
SYNC_DIR="$WORKDIR/workspace"
MANIFEST_DIR="$WORKDIR/.okdev"
MANIFEST_PATH="$MANIFEST_DIR/pytorchjob.yaml"
ORIG_HOME="${HOME}"
ORIG_KUBECONFIG="${KUBECONFIG:-}"
KUBECONFIG_PATH="$HOME_DIR/.kube/config"

mkdir -p "$HOME_DIR" "$SYNC_DIR" "$MANIFEST_DIR" "$HOME_DIR/.kube"
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

# Write the PyTorchJob manifest
cat >"$MANIFEST_PATH" <<EOF
apiVersion: kubeflow.org/v1
kind: PyTorchJob
metadata:
  name: ${SESSION_NAME}
spec:
  pytorchReplicaSpecs:
    Master:
      replicas: 1
      restartPolicy: Never
      template:
        spec:
          containers:
            - name: dev
              image: ubuntu:22.04
              command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]
    Worker:
      replicas: 1
      restartPolicy: Never
      template:
        spec:
          containers:
            - name: dev
              image: ubuntu:22.04
              command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]
EOF

# Write the okdev config
cat >"$CFG_PATH" <<EOF
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: e2e-ptjob
spec:
  namespace: ${NAMESPACE}
  session:
    defaultNameTemplate: ${SESSION_NAME}
  workload:
    type: pytorchjob
    manifestPath: ${MANIFEST_PATH}
    inject:
      - path: "spec.pytorchReplicaSpecs.Master.template"
    attach:
      container: dev
  sync:
    engine: syncthing
    paths:
      - ${SYNC_DIR}:/workspace
  ssh:
    user: root
    persistentSession: false
  sidecar:
    image: ${SIDECAR_IMAGE}
EOF

echo "hello from pytorchjob e2e" >"$SYNC_DIR/hello.txt"

echo "Starting PyTorchJob smoke session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m

echo "Checking status"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details

echo "Waiting for synced file to appear remotely with correct content"
SYNC_OK=false
for i in $(seq 1 30); do
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" ssh --setup-key --cmd 'cat /workspace/hello.txt 2>/dev/null' || true)
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

echo "PyTorchJob smoke test completed"
