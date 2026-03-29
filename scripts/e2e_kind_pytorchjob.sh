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
sed -i 's/name: dev/name: pytorch/' "$MANIFEST_PATH"
sed -i 's/image: # TODO:.*/image: ubuntu:22.04/' "$MANIFEST_PATH"
sed -i 's/command: \["sleep", "infinity"\]/command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]/' "$MANIFEST_PATH"
sed -i 's/container: dev/container: pytorch/' "$CFG_PATH"

# Disable persistent SSH sessions for CI
sed -i '/ssh:/a\    persistentSession: false' "$CFG_PATH"

echo "Generated config:"
cat "$CFG_PATH"
echo "Generated manifest:"
cat "$MANIFEST_PATH"

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
