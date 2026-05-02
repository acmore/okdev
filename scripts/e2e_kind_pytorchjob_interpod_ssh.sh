#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-ptjob-interpod-ssh}"
NAMESPACE="${NAMESPACE:-default}"
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
    echo "--- inter-pod ssh e2e local okdev logs ---"
    ls -R "$HOME_DIR/.okdev" 2>/dev/null || true
  fi
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes >/dev/null 2>&1 || true
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

echo "Scaffolding focused PyTorchJob inter-pod SSH config"
cd "$WORKDIR"
"$OKDEV_BIN" init \
  --workload pytorchjob \
  --yes \
  --name "$SESSION_NAME" \
  --namespace "$NAMESPACE" \
  --sidecar-image "$SIDECAR_IMAGE" \
  --ssh-user root \
  --sync-local "$SYNC_DIR" \
  --sync-remote /workspace

MANIFEST_PATH="$WORKDIR/.okdev/pytorchjob.yaml"
replace_all_in_file "$MANIFEST_PATH" 'name: dev' 'name: pytorch'
replace_all_in_file "$MANIFEST_PATH" 'image: # TODO: replace with your image' 'image: ubuntu:22.04'
replace_all_in_file "$MANIFEST_PATH" 'command: ["sleep", "infinity"]' 'command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]'
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

python3 - <<'PY' "$MANIFEST_PATH"
import pathlib, sys
path = pathlib.Path(sys.argv[1])
text = path.read_text()
text = text.replace("        spec:\n          containers:", "        spec:\n          terminationGracePeriodSeconds: 5\n          containers:")
path.write_text(text)
PY

insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'
insert_after_line_once "$CFG_PATH" '  ssh:' '    interPod: true'

echo "Starting focused PyTorchJob inter-pod SSH session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m

echo "Waiting for 1 master pod and 1 worker pod"
PODS_READY=false
for i in $(seq 1 30); do
  refresh_pytorchjob_pods
  if [[ -n "${MASTER_POD:-}" ]] && [[ "$(echo "$WORKER_PODS" | wc -w | tr -d ' ')" -eq 1 ]]; then
    PODS_READY=true
    break
  fi
  sleep 2
done
if [[ "$PODS_READY" != "true" ]]; then
  echo "ERROR: expected 1 master pod and 1 worker pod" >&2
  exit 1
fi

FIRST_WORKER_POD=$(echo "$WORKER_PODS" | awk '{print $1}')
WORKER_CONTAINERS=$(kubectl -n "$NAMESPACE" get pod "$FIRST_WORKER_POD" -o jsonpath='{.spec.containers[*].name}')
if [[ "$WORKER_CONTAINERS" != *"okdev-sidecar"* ]]; then
  echo "ERROR: expected worker pod $FIRST_WORKER_POD to include okdev-sidecar when ssh.interPod=true" >&2
  echo "containers: $WORKER_CONTAINERS" >&2
  exit 1
fi
echo "worker sidecar override verified"

echo "Verifying inter-pod SSH from master to worker"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" ssh --setup-key --cmd 'echo pytorchjob-ssh-ok' >/dev/null
INTERPOD_SSH_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" ssh --cmd "ssh -o BatchMode=yes $FIRST_WORKER_POD 'echo interpod-ssh-ok'")
if [[ "$INTERPOD_SSH_OUTPUT" != *"interpod-ssh-ok"* ]]; then
  echo "ERROR: expected inter-pod SSH from master to worker to succeed" >&2
  echo "$INTERPOD_SSH_OUTPUT" >&2
  exit 1
fi
echo "inter-pod SSH verified"

echo "Cleaning up focused inter-pod SSH session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes
assert_no_local_sync_processes "$SESSION_NAME" "$HOME_DIR/.okdev/sessions/${SESSION_NAME}/syncthing"

for i in $(seq 1 15); do
  if [[ -z "$(session_attachable_pod_names "$NAMESPACE" "$SESSION_NAME")" ]]; then
    echo "focused PyTorchJob inter-pod SSH session deleted on attempt $i"
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "ERROR: pytorchjob session ${SESSION_NAME} still has managed pod(s) after down" >&2
    exit 1
  fi
  sleep 2
done

echo "Focused PyTorchJob inter-pod SSH test completed"
