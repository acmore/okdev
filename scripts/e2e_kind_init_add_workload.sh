#!/usr/bin/env bash
set -euo pipefail

# `okdev init` on a project that already has a config declares an additional
# workload instead of refusing, and the result is immediately usable.

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-init-add}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
MANIFEST_PATH="$WORKDIR/.okdev/batch.yaml"
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
    echo "--- config ---"
    cat "$CFG_PATH" 2>&1 || true
    echo "--- .okdev contents ---"
    ls -la "$WORKDIR/.okdev" 2>&1 || true
  fi
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes >/dev/null 2>&1 || true
  return "$status"
}
trap cleanup EXIT

okdev() {
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" "$@"
}

echo "Scaffolding a pod config via okdev init"
cd "$WORKDIR"
"$OKDEV_BIN" init \
  --yes \
  --name "$SESSION_NAME" \
  --namespace "$NAMESPACE" \
  --sidecar-image "$SIDECAR_IMAGE" \
  --dev-image ubuntu:22.04 \
  --ssh-user root \
  --sync-local "$SYNC_DIR" \
  --sync-remote /workspace

# A pod init must produce the folder config, so manifests added later have a
# home instead of landing in the project root.
if [[ ! -f "$CFG_PATH" ]]; then
  echo "ERROR: expected okdev init to write $CFG_PATH" >&2
  ls -la "$WORKDIR" >&2
  exit 1
fi
echo "pod init wrote the folder config"

replace_all_in_file "$CFG_PATH" 'persistentSession: true' 'persistentSession: false'
insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'

echo "Declaring a second workload on the existing config"
"$OKDEV_BIN" --config "$CFG_PATH" init --yes --template job --workload-name batch

if [[ ! -f "$MANIFEST_PATH" ]]; then
  echo "ERROR: expected the manifest at $MANIFEST_PATH" >&2
  ls -la "$WORKDIR/.okdev" >&2
  exit 1
fi
if [[ -f "$WORKDIR/batch.yaml" ]]; then
  echo "ERROR: the manifest must not land in the project root" >&2
  exit 1
fi
okdev validate >/dev/null
echo "second workload declared, manifest in .okdev/, config still valid"

echo "Refusing project-level flags on an existing config"
if "$OKDEV_BIN" --config "$CFG_PATH" init --yes --template job --workload-name other \
  --namespace someother >/dev/null 2>&1; then
  echo "ERROR: a project-level flag on an existing config must be refused" >&2
  exit 1
fi
okdev validate >/dev/null
echo "project-level flag refused, config untouched"

replace_all_in_file "$MANIFEST_PATH" 'image: # TODO: replace with your image' 'image: ubuntu:22.04'
replace_all_in_file "$MANIFEST_PATH" 'command: ["sleep", "infinity"]' 'command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]'

echo "hello from init-add e2e" >"$SYNC_DIR/hello.txt"

echo "Starting the session on the default pod workload"
okdev up --wait-timeout 5m --yes

echo "Switching to the declared job workload"
okdev workload use batch
okdev up --wait-timeout 5m --yes

LIVE_TYPE="$(kubectl -n "$NAMESPACE" get pods -l "okdev.io/session=$SESSION_NAME" \
  --field-selector=status.phase=Running \
  -o jsonpath='{.items[0].metadata.labels.okdev\.io/workload-type}' 2>/dev/null || true)"
if [[ "$LIVE_TYPE" != "job" ]]; then
  echo "ERROR: expected a live job workload, got '$LIVE_TYPE'" >&2
  exit 1
fi

SYNC_OK=false
for _ in $(seq 1 30); do
  REMOTE_CONTENT=$(okdev exec --no-tty --no-prefix -- sh -lc 'if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi' || true)
  if [[ "$REMOTE_CONTENT" == "hello from init-add e2e" ]]; then
    SYNC_OK=true
    break
  fi
  sleep 2
done
if [[ "$SYNC_OK" != "true" ]]; then
  echo "ERROR: sync did not converge in the declared workload" >&2
  exit 1
fi
echo "exec lands in the declared workload and sync converged"

echo "Tearing down"
okdev down --yes >/dev/null
echo "init add-workload e2e completed"
