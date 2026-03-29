#!/usr/bin/env bash
set -euo pipefail

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-smoke}"
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

cat >"$CFG_PATH" <<EOF
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: e2e-smoke
spec:
  namespace: ${NAMESPACE}
  session:
    defaultNameTemplate: ${SESSION_NAME}
  sync:
    engine: syncthing
    paths:
      - ${SYNC_DIR}:/workspace
  ssh:
    user: root
    persistentSession: false
  sidecar:
    image: ${SIDECAR_IMAGE}
  podTemplate:
    spec:
      containers:
        - name: dev
          image: ubuntu:22.04
          command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]
EOF

echo "hello from okdev e2e" >"$SYNC_DIR/hello.txt"

echo "Starting kind smoke session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m

echo "Checking status"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details

echo "Waiting for synced file to appear remotely with correct content"
SYNC_OK=false
for i in $(seq 1 30); do
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" ssh --setup-key --cmd 'cat /workspace/hello.txt 2>/dev/null' || true)
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
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" logs --follow=false --tail 10 >/dev/null

echo "Testing connect --cmd"
CONNECT_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" connect --no-tty --cmd 'echo connect-ok')
if [[ "$CONNECT_OUTPUT" != *"connect-ok"* ]]; then
  echo "ERROR: connect --cmd did not return expected output" >&2
  echo "Got: $CONNECT_OUTPUT" >&2
  exit 1
fi
echo "connect --cmd verified"

echo "Testing explicit okdev down"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes

echo "Verifying pod is deleted"
for i in $(seq 1 15); do
  if ! kubectl -n "$NAMESPACE" get pod "okdev-${SESSION_NAME}" >/dev/null 2>&1; then
    echo "Pod deleted on attempt $i"
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "ERROR: pod okdev-${SESSION_NAME} still exists after down" >&2
    exit 1
  fi
  sleep 2
done

echo "Smoke test completed"
