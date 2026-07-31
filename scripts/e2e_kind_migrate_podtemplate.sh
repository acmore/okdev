#!/usr/bin/env bash
set -euo pipefail

# A config written before spec.podTemplate was removed is refused with a pointer
# to `okdev migrate`, and the migrated project comes up and syncs unchanged.

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-migrate-pt}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
MANIFEST_PATH="$WORKDIR/.okdev/pod.yaml"
SYNC_DIR="$WORKDIR/workspace"
ORIG_HOME="${HOME}"
ORIG_KUBECONFIG="${KUBECONFIG:-}"
KUBECONFIG_PATH="$HOME_DIR/.kube/config"

mkdir -p "$HOME_DIR" "$SYNC_DIR" "$HOME_DIR/.kube" "$WORKDIR/.okdev"
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
    echo "--- pod manifest ---"
    cat "$MANIFEST_PATH" 2>&1 || true
  fi
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes >/dev/null 2>&1 || true
  return "$status"
}
trap cleanup EXIT

okdev() {
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" "$@"
}

# Hand-written, because `okdev init` no longer produces this shape. The dev
# container carries a literal {{ }} so the migration's brace escaping is
# exercised on a real apply rather than only in unit tests.
cat >"$CFG_PATH" <<EOF
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: $SESSION_NAME
spec:
  namespace: $NAMESPACE
  # this comment must survive the migration
  session:
    defaultNameTemplate: '{{ .Repo }}-{{ .User }}'
  sync:
    engine: syncthing
    paths:
      - "$SYNC_DIR:/workspace"
  ssh:
    user: root
    persistentSession: false
  sidecar:
    image: $SIDECAR_IMAGE
  podTemplate:
    metadata:
      labels:
        e2e: migrate-podtemplate
    spec:
      containers:
        - name: dev
          image: ubuntu:22.04
          command: ["sh", "-lc", "trap : TERM INT; while true; do sleep 3600; done"]
          env:
            - name: OKDEV_E2E_BRACES
              value: "{{ .NotATemplate }}"
EOF

echo "A config with spec.podTemplate is refused, naming the fix"
UP_OUT="$(okdev up --wait-timeout 1m --yes 2>&1 || true)"
if ! grep -q "okdev migrate" <<<"$UP_OUT"; then
  echo "ERROR: okdev up must point at 'okdev migrate', got:" >&2
  echo "$UP_OUT" >&2
  exit 1
fi
echo "up refused a legacy config and named okdev migrate"

echo "Migrating"
okdev migrate

if [[ ! -f "$MANIFEST_PATH" ]]; then
  echo "ERROR: expected the extracted manifest at $MANIFEST_PATH" >&2
  ls -la "$WORKDIR/.okdev" >&2
  exit 1
fi
if grep -q "podTemplate" "$CFG_PATH"; then
  echo "ERROR: spec.podTemplate must be gone from the config" >&2
  exit 1
fi
if ! grep -q "this comment must survive" "$CFG_PATH"; then
  echo "ERROR: the migration stripped comments" >&2
  exit 1
fi
if ! grep -q "e2e: migrate-podtemplate" "$MANIFEST_PATH"; then
  echo "ERROR: podTemplate metadata.labels must travel to the manifest" >&2
  exit 1
fi
okdev validate >/dev/null
echo "migrated: manifest written, comments kept, config valid"

echo "hello from migrate e2e" >"$SYNC_DIR/hello.txt"

echo "Starting the session on the migrated config"
okdev up --wait-timeout 5m --yes

LIVE_TYPE="$(kubectl -n "$NAMESPACE" get pods -l "okdev.io/session=$SESSION_NAME" \
  --field-selector=status.phase=Running \
  -o jsonpath='{.items[0].metadata.labels.okdev\.io/workload-type}' 2>/dev/null || true)"
if [[ "$LIVE_TYPE" != "pod" ]]; then
  echo "ERROR: expected a live pod workload, got '$LIVE_TYPE'" >&2
  exit 1
fi

# The label from the old podTemplate has to be on the running pod, and the
# escaped braces have to reach the container verbatim.
LIVE_LABEL="$(kubectl -n "$NAMESPACE" get pods -l "okdev.io/session=$SESSION_NAME" \
  --field-selector=status.phase=Running \
  -o jsonpath='{.items[0].metadata.labels.e2e}' 2>/dev/null || true)"
if [[ "$LIVE_LABEL" != "migrate-podtemplate" ]]; then
  echo "ERROR: the migrated pod lost its label, got '$LIVE_LABEL'" >&2
  exit 1
fi

BRACES="$(okdev exec --no-tty --no-prefix -- sh -lc 'printf %s "$OKDEV_E2E_BRACES"' || true)"
if [[ "$BRACES" != "{{ .NotATemplate }}" ]]; then
  echo "ERROR: literal braces must survive the migration, got '$BRACES'" >&2
  exit 1
fi
echo "the migrated pod kept its labels and its literal braces"

SYNC_OK=false
for _ in $(seq 1 30); do
  REMOTE_CONTENT=$(okdev exec --no-tty --no-prefix -- sh -lc 'if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi' || true)
  if [[ "$REMOTE_CONTENT" == "hello from migrate e2e" ]]; then
    SYNC_OK=true
    break
  fi
  sleep 2
done
if [[ "$SYNC_OK" != "true" ]]; then
  echo "ERROR: sync did not converge on the migrated config" >&2
  exit 1
fi
echo "sync converged"

echo "Refusing to overwrite an existing manifest"
BEFORE="$(cat "$MANIFEST_PATH")"
if "$OKDEV_BIN" --config "$CFG_PATH" migrate >/dev/null 2>&1; then
  : # already migrated: a no-op run is fine
fi
if [[ "$(cat "$MANIFEST_PATH")" != "$BEFORE" ]]; then
  echo "ERROR: re-running migrate rewrote the manifest" >&2
  exit 1
fi
echo "re-running migrate left the manifest alone"

okdev down --yes
echo "PASS: e2e_kind_migrate_podtemplate"
