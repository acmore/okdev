#!/usr/bin/env bash
set -euo pipefail

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-smoke}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(mktemp -d)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH="$WORKDIR/.okdev.yaml"
SYNC_DIR="$WORKDIR/workspace"

mkdir -p "$HOME_DIR" "$SYNC_DIR"
export HOME="$HOME_DIR"

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
          image: alpine:3.21
          command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]
EOF

echo "hello from okdev e2e" >"$SYNC_DIR/hello.txt"

echo "Starting kind smoke session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m

echo "Checking status"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details

echo "Waiting for synced file to appear remotely"
for _ in $(seq 1 30); do
  if "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" ssh --setup-key --cmd 'test -f /workspace/hello.txt'; then
    break
  fi
  sleep 2
done

echo "Verifying remote shell and logs"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" ssh --setup-key --cmd 'test -f /workspace/hello.txt'
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" logs --follow=false --tail 10 >/dev/null

echo "Smoke test completed"
