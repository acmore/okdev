#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-deployment}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
MANIFEST_PATH="$WORKDIR/.okdev/deployment.yaml"
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
    echo "--- session sync log ---"
    cat "$HOME_DIR/.okdev/sessions/${SESSION_NAME}/syncthing/local.log" 2>/dev/null || true
  fi
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes >/dev/null 2>&1 || true
  return "$status"
}
trap cleanup EXIT

echo "Scaffolding deployment config via okdev init"
cd "$WORKDIR"
"$OKDEV_BIN" init \
  --template deployment \
  --yes \
  --name "$SESSION_NAME" \
  --namespace "$NAMESPACE" \
  --sidecar-image "$SIDECAR_IMAGE" \
  --ssh-user root \
  --sync-local "$SYNC_DIR" \
  --sync-remote /workspace

replace_all_in_file "$MANIFEST_PATH" 'image: # TODO: replace with your image' 'image: ubuntu:22.04'
replace_all_in_file "$MANIFEST_PATH" 'command: ["sleep", "infinity"]' 'command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]'
insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'

echo "hello from deployment e2e" >"$SYNC_DIR/hello.txt"

echo "Starting deployment session"
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
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi' || true)
  if [[ "$REMOTE_CONTENT" == "hello from deployment e2e" ]]; then
    SYNC_OK=true
    break
  fi
  sleep 2
done
if [[ "$SYNC_OK" != "true" ]]; then
  echo "ERROR: synced file content did not match after 30 attempts" >&2
  exit 1
fi

WORKLOAD_NAME=$(session_workload_name "$NAMESPACE" "$SESSION_NAME")
ORIGINAL_DEPLOY_UID=$(kubectl -n "$NAMESPACE" get deployment "$WORKLOAD_NAME" -o jsonpath='{.metadata.uid}')
DEPLOYMENT_POD_NAME=$(session_attachable_pod_name "$NAMESPACE" "$SESSION_NAME")
ORIGINAL_POD_UID=$(kubectl -n "$NAMESPACE" get pod "$DEPLOYMENT_POD_NAME" -o jsonpath='{.metadata.uid}')

echo "Changing deployment workload spec to trigger drift detection"
replace_first_in_file "$MANIFEST_PATH" 'image: ubuntu:22.04' 'image: ubuntu:24.04'

set +e
DRIFT_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m 2>&1)
DRIFT_STATUS=$?
set -e
if [[ "$DRIFT_STATUS" -eq 0 ]]; then
  echo "ERROR: expected drifted deployment workload to require --reconcile" >&2
  exit 1
fi
if [[ "$DRIFT_OUTPUT" != *"workload spec changed; re-run with --reconcile to apply"* ]]; then
  echo "ERROR: unexpected deployment drift output" >&2
  echo "$DRIFT_OUTPUT" >&2
  exit 1
fi

echo "Reconciling deployment via --reconcile"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --reconcile --wait-timeout 5m

RECONCILE_OK=false
for i in $(seq 1 30); do
  WORKLOAD_NAME=$(session_workload_name "$NAMESPACE" "$SESSION_NAME")
  DEPLOY_IMAGE=$(kubectl -n "$NAMESPACE" get deployment "$WORKLOAD_NAME" -o jsonpath='{.spec.template.spec.containers[?(@.name=="dev")].image}')
  DEPLOYMENT_POD_NAME=$(session_attachable_pod_name "$NAMESPACE" "$SESSION_NAME")
  POD_UID=$(kubectl -n "$NAMESPACE" get pod "$DEPLOYMENT_POD_NAME" -o jsonpath='{.metadata.uid}')
  POD_IMAGE=$(kubectl -n "$NAMESPACE" get pod "$DEPLOYMENT_POD_NAME" -o jsonpath='{.spec.containers[?(@.name=="dev")].image}')
  if [[ "$DEPLOY_IMAGE" == "ubuntu:24.04" && "$POD_IMAGE" == "ubuntu:24.04" && "$POD_UID" != "$ORIGINAL_POD_UID" ]]; then
    RECONCILE_OK=true
    break
  fi
  sleep 2
done
if [[ "$RECONCILE_OK" != "true" ]]; then
  echo "ERROR: expected deployment rollout to update live pod image to ubuntu:24.04" >&2
  exit 1
fi

WORKLOAD_NAME=$(session_workload_name "$NAMESPACE" "$SESSION_NAME")
RECONCILED_DEPLOY_UID=$(kubectl -n "$NAMESPACE" get deployment "$WORKLOAD_NAME" -o jsonpath='{.metadata.uid}')
if [[ "$RECONCILED_DEPLOY_UID" != "$ORIGINAL_DEPLOY_UID" ]]; then
  echo "ERROR: expected deployment object to be updated in place" >&2
  exit 1
fi

RECONCILE_STATUS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
if [[ "$RECONCILE_STATUS" != *"health: active"* ]]; then
  echo "ERROR: expected sync health to remain active after deployment reconcile" >&2
  echo "$RECONCILE_STATUS" >&2
  exit 1
fi
# The rollout retired the old ReplicaSet's pod by design.
if [[ "$RECONCILE_STATUS" == *"Replaced Pods:"* ]]; then
  echo "ERROR: rollout-retired pod reported as an unexplained replacement" >&2
  echo "$RECONCILE_STATUS" >&2
  exit 1
fi

# #325: a pod the ReplicaSet replaces within the same run must be explained,
# first by exec (where it is usually noticed) and then by status, even though
# the snapshot that proves it is refreshed on the way.
echo "Deleting the deployment pod outside okdev"
# Let the rollout's terminating pod finish so exactly one pod is live.
for i in $(seq 1 60); do
  [[ "$(session_attachable_pod_names "$NAMESPACE" "$SESSION_NAME")" != *" "* ]] && break
  sleep 2
done
REPLACED_POD=$(session_attachable_pod_name "$NAMESPACE" "$SESSION_NAME")
kubectl -n "$NAMESPACE" delete pod "$REPLACED_POD" --wait=true >/dev/null
NEW_POD=""
for i in $(seq 1 60); do
  NEW_POD=$(session_attachable_pod_name "$NAMESPACE" "$SESSION_NAME" || true)
  if [[ -n "$NEW_POD" && "$NEW_POD" != "$REPLACED_POD" ]]; then
    break
  fi
  sleep 2
done
kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/$NEW_POD" --timeout=180s >/dev/null
EXEC_NOTICE=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- true 2>&1 >/dev/null || true)
if [[ "$EXEC_NOTICE" != *"notice: pod $REPLACED_POD replaced by $NEW_POD"* ]]; then
  echo "ERROR: expected exec to report the replaced pod, got:" >&2
  echo "$EXEC_NOTICE" >&2
  exit 1
fi
REPLACED_STATUS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status)
if [[ "$REPLACED_STATUS" != *"pod $REPLACED_POD replaced by $NEW_POD"* ]]; then
  echo "ERROR: expected status to keep explaining the replaced pod, got:" >&2
  echo "$REPLACED_STATUS" >&2
  exit 1
fi
REPLACED_JSON=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --output json)
if [[ "$REPLACED_JSON" != *'"previousPodEnd"'* || "$REPLACED_JSON" != *"\"name\": \"$REPLACED_POD\""* ]]; then
  echo "ERROR: expected previousPodEnd in status json, got:" >&2
  echo "$REPLACED_JSON" >&2
  exit 1
fi
echo "An intentional up supersedes the replacement report"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m >/dev/null
AFTER_UP_STATUS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status)
if [[ "$AFTER_UP_STATUS" == *"replaced by"* ]]; then
  echo "ERROR: replacement report must be cleared by an intentional up, got:" >&2
  echo "$AFTER_UP_STATUS" >&2
  exit 1
fi
echo "controller pod replacement report verified"

echo "Tearing down deployment session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes
assert_no_local_sync_processes "$SESSION_NAME" "$HOME_DIR/.okdev/sessions/${SESSION_NAME}/syncthing"

for i in $(seq 1 20); do
  if [[ -z "$(session_attachable_pod_names "$NAMESPACE" "$SESSION_NAME")" ]]; then
    break
  fi
  if [[ "$i" -eq 20 ]]; then
    echo "ERROR: deployment session ${SESSION_NAME} still has managed pod(s) after down" >&2
    exit 1
  fi
  sleep 2
done

echo "Deployment e2e completed"
