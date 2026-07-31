#!/usr/bin/env bash
set -euo pipefail

# okdev does not touch what the manifest declares. Its one exception is the sync
# workspace, injected only when the manifest declares none. Both directions are
# asserted against the live pod, because a unit test cannot show that the
# injected volume is the one sync actually uses.

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
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

SESSIONS=()
cleanup() {
  status=$?
  for s in "${SESSIONS[@]:-}"; do
    [[ -n "$s" ]] || continue
    "$OKDEV_BIN" --config "$WORKDIR/$s/.okdev/okdev.yaml" --session "$s" down --yes >/dev/null 2>&1 || true
  done
  return "$status"
}
trap cleanup EXIT

# write_project <dir> <session> <volumes-block-or-empty>
write_project() {
  local dir="$1" session="$2" volumes="$3"
  mkdir -p "$dir/.okdev"
  cat >"$dir/.okdev/okdev.yaml" <<EOF
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: $session
spec:
  namespace: $NAMESPACE
  session:
    defaultNameTemplate: '$session'
  sync:
    engine: syncthing
    syncthing:
      autoInstall: true
    paths:
      - "$SYNC_DIR:/workspace"
  ssh:
    user: root
    persistentSession: false
  sidecar:
    image: $SIDECAR_IMAGE
  workload:
    type: pod
    manifestPath: pod.yaml
    attach:
      container: dev
EOF
  cat >"$dir/.okdev/pod.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: ubuntu:22.04
      command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]
      volumeMounts:
        - name: workspace
          mountPath: /workspace
$volumes
EOF
}

live_pod() {
  kubectl -n "$NAMESPACE" get pods -l "okdev.io/session=$1" \
    --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# 1. The manifest declares no workspace volume -> okdev injects one, and it is
#    the volume sync actually writes into.
# ---------------------------------------------------------------------------
SESSION_A="e2e-vol-inject"
SESSIONS+=("$SESSION_A")
DIR_A="$WORKDIR/$SESSION_A"
write_project "$DIR_A" "$SESSION_A" ""

echo "not-declared: starting a session whose manifest has no volumes at all"
"$OKDEV_BIN" --config "$DIR_A/.okdev/okdev.yaml" --session "$SESSION_A" up --wait-timeout 5m --yes

POD_A="$(live_pod "$SESSION_A")"
if [[ -z "$POD_A" ]]; then
  echo "ERROR: no running pod for $SESSION_A" >&2
  exit 1
fi
KIND_A="$(kubectl -n "$NAMESPACE" get pod "$POD_A" \
  -o jsonpath='{.spec.volumes[?(@.name=="workspace")].emptyDir}' 2>/dev/null || true)"
if [[ -z "$KIND_A" ]]; then
  echo "ERROR: expected an injected workspace emptyDir on $POD_A" >&2
  kubectl -n "$NAMESPACE" get pod "$POD_A" -o jsonpath='{.spec.volumes}' >&2
  exit 1
fi
echo "not-declared: okdev injected the workspace emptyDir"

# The volume is only useful if it is the one sync uses.
echo "hello from volumes e2e" >"$SYNC_DIR/hello.txt"
SYNC_OK=false
for _ in $(seq 1 30); do
  GOT=$("$OKDEV_BIN" --config "$DIR_A/.okdev/okdev.yaml" --session "$SESSION_A" \
    exec --no-tty --no-prefix -- sh -lc 'if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi' || true)
  if [[ "$GOT" == "hello from volumes e2e" ]]; then
    SYNC_OK=true
    break
  fi
  sleep 2
done
if [[ "$SYNC_OK" != "true" ]]; then
  echo "ERROR: sync did not reach the injected workspace volume" >&2
  exit 1
fi
echo "not-declared: sync converged into the injected volume"

"$OKDEV_BIN" --config "$DIR_A/.okdev/okdev.yaml" --session "$SESSION_A" down --yes >/dev/null

# ---------------------------------------------------------------------------
# 2. The manifest declares workspace itself -> okdev leaves it exactly as
#    written. sizeLimit is the marker: kind can satisfy it without a PV, and
#    okdev's own emptyDir has none, so its survival proves nothing substituted.
# ---------------------------------------------------------------------------
SESSION_B="e2e-vol-declared"
SESSIONS+=("$SESSION_B")
DIR_B="$WORKDIR/$SESSION_B"
write_project "$DIR_B" "$SESSION_B" '  volumes:
    - name: workspace
      emptyDir:
        sizeLimit: 123Mi'

echo "declared: starting a session whose manifest declares its own workspace"
"$OKDEV_BIN" --config "$DIR_B/.okdev/okdev.yaml" --session "$SESSION_B" up --wait-timeout 5m --yes

POD_B="$(live_pod "$SESSION_B")"
if [[ -z "$POD_B" ]]; then
  echo "ERROR: no running pod for $SESSION_B" >&2
  exit 1
fi
SIZE_B="$(kubectl -n "$NAMESPACE" get pod "$POD_B" \
  -o jsonpath='{.spec.volumes[?(@.name=="workspace")].emptyDir.sizeLimit}' 2>/dev/null || true)"
if [[ "$SIZE_B" != "123Mi" ]]; then
  echo "ERROR: okdev replaced the manifest's workspace volume; sizeLimit='$SIZE_B', want 123Mi" >&2
  kubectl -n "$NAMESPACE" get pod "$POD_B" -o jsonpath='{.spec.volumes}' >&2
  exit 1
fi
echo "declared: the manifest's workspace volume survived untouched"

"$OKDEV_BIN" --config "$DIR_B/.okdev/okdev.yaml" --session "$SESSION_B" down --yes >/dev/null

echo "Volumes e2e completed"
