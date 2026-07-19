#!/usr/bin/env bash
# e2e: gateway fanout for `okdev exec --json` and `okdev jobs list`.
#
# Verifies that with ssh.interPod=true and spec.exec.fanoutMode=gateway, a
# multi-pod exec goes through the in-cluster fanout driver (one apiserver
# exec, SSH fanout over the pod network) and returns a per-pod status
# envelope; that --require-all fails when a pod is unreachable; and that the
# dev container needs NO ssh client (the driver ships its own).
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-ptjob-gw-fanout}"
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
    echo "--- gateway fanout e2e local okdev logs ---"
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

echo "Scaffolding PyTorchJob gateway fanout config"
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
# Deliberately NO openssh-client install: gateway fanout must not need any
# ssh client in the user image (the driver carries its own SSH client).
replace_all_in_file "$CFG_PATH" 'container: dev' 'container: pytorch'

python3 - <<'PY' "$MANIFEST_PATH"
import pathlib, sys
path = pathlib.Path(sys.argv[1])
text = path.read_text()
text = text.replace("        spec:\n          containers:", "        spec:\n          terminationGracePeriodSeconds: 5\n          containers:")
path.write_text(text)
PY

insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'
insert_after_line_once "$CFG_PATH" '  ssh:' '    interPod: true'

python3 - <<'PY' "$CFG_PATH"
import pathlib, sys
path = pathlib.Path(sys.argv[1])
text = path.read_text()
if "\nspec:\n" not in text:
    raise SystemExit("spec block not found")
text = text.replace("\nspec:\n", "\nspec:\n  exec:\n    fanoutMode: gateway\n", 1)
path.write_text(text)
PY

echo "Starting PyTorchJob gateway fanout session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m

echo "Waiting for 1 master pod and 1 worker pod"
PODS_READY=false
for i in $(seq 1 30); do
  refresh_pytorchjob_pods
  if [[ -n "${MASTER_POD:-}" ]] && [[ "$(echo "$WORKER_PODS" | wc -w | tr -d ' ')" -ge 1 ]]; then
    PODS_READY=true
    break
  fi
  sleep 2
done
if [[ "$PODS_READY" != "true" ]]; then
  echo "ERROR: expected 1 master pod and at least 1 worker pod" >&2
  exit 1
fi
FIRST_WORKER_POD=$(echo "$WORKER_PODS" | awk '{print $1}')
POD_COUNT=$(( 1 + $(echo "$WORKER_PODS" | wc -w | tr -d ' ') ))

echo "Waiting for interpod sshd to accept gateway fanout on all pods"
GW_READY=false
for i in $(seq 1 45); do
  set +e
  OUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --all --json -- echo gw-ready 2>"$WORKDIR/gw-stderr.log")
  rc=$?
  set -e
  if [[ "$rc" -eq 0 ]] && OKDEV_EXEC_JSON="$OUT" python3 - "$POD_COUNT" <<'PY'
import json, os, sys
results = json.loads(os.environ["OKDEV_EXEC_JSON"])
count = int(sys.argv[1])
ok = (isinstance(results, list) and len(results) == count
      and all(r.get("status") == "responded" and r.get("exit") == 0
              and r.get("stdout").strip() == "gw-ready" for r in results))
raise SystemExit(0 if ok else 1)
PY
  then
    GW_READY=true
    break
  fi
  sleep 2
done
if [[ "$GW_READY" != "true" ]]; then
  echo "ERROR: gateway fanout exec did not become ready" >&2
  cat "$WORKDIR/gw-stderr.log" >&2 || true
  echo "last output: $OUT" >&2
  exit 1
fi
if grep -q "falling back to direct exec" "$WORKDIR/gw-stderr.log"; then
  echo "ERROR: exec fell back to direct path; gateway driver was not used" >&2
  cat "$WORKDIR/gw-stderr.log" >&2
  exit 1
fi
echo "gateway fanout exec verified across $POD_COUNT pods (no ssh client in user image)"

echo "Verifying --require-all passes on a healthy fleet"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --all --json --require-all -- true >/dev/null

echo "Verifying script mode with args over the gateway"
cat >"$WORKDIR/gw-script.sh" <<'EOF'
#!/bin/sh
printf 'script-ok arg=%s host=%s' "$1" "$(hostname)"
EOF
SCRIPT_OUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --all --json --script "$WORKDIR/gw-script.sh" -- alpha)
OKDEV_SCRIPT_JSON="$SCRIPT_OUT" python3 - "$POD_COUNT" <<'PY'
import json, os, sys
results = json.loads(os.environ["OKDEV_SCRIPT_JSON"])
count = int(sys.argv[1])
assert len(results) == count, results
hosts = set()
for r in results:
    assert r["status"] == "responded" and r["exit"] == 0, r
    assert r["stdout"].startswith("script-ok arg=alpha host="), r
    hosts.add(r["stdout"])
assert len(hosts) == count, f"expected distinct per-pod output, got {hosts}"
PY
echo "script mode verified"

# Issue #170: the sshd exec wrapper's cmdline used to contain the full command
# string, so `pkill -f <pattern>` inside a command matched — and killed — its
# own wrapper. The probe walks its ancestor chain via /proc and asserts no
# okdev-owned process above the command exposes a marker embedded in the
# command text (the payload now travels via the environment, which pkill/pgrep
# -f never match).
echo "Verifying no okdev wrapper exposes the command text in its cmdline (issue #170 pkill -f self-match)"
SELFKILL_MARKER="okdev-selfkill-$$-$RANDOM"
SELFKILL_PROBE='pid=$PPID; bad=0; while [ "$pid" -gt 1 ] 2>/dev/null; do if tr "\0" " " </proc/"$pid"/cmdline 2>/dev/null | grep -q '"$SELFKILL_MARKER"'; then bad=1; break; fi; pid=$(grep ^PPid: /proc/"$pid"/status 2>/dev/null | cut -f2); [ -n "$pid" ] || break; done; echo matchable=$bad'
SELFKILL_OUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --all --json -- sh -c "$SELFKILL_PROBE")
OKDEV_SELFKILL_JSON="$SELFKILL_OUT" python3 - "$POD_COUNT" <<'PY'
import json, os, sys
results = json.loads(os.environ["OKDEV_SELFKILL_JSON"])
count = int(sys.argv[1])
assert len(results) == count, results
for r in results:
    assert r["status"] == "responded" and r["exit"] == 0, r
    assert r["stdout"].strip() == "matchable=0", r
PY
echo "wrapper cmdline is unmatchable by command patterns"

echo "Verifying jobs list through the gateway route"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --all --detach -- sleep 60 >/dev/null
JOBS_OUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" jobs list --output json)
OKDEV_JOBS_JSON="$JOBS_OUT" python3 - "$POD_COUNT" <<'PY'
import json, os, sys
doc = json.loads(os.environ["OKDEV_JOBS_JSON"])
count = int(sys.argv[1])
assert doc.get("errors") in (None, []), doc
jobs = doc.get("jobs") or []
assert len(jobs) >= 1, doc
assert any(j.get("pods") == count for j in jobs), doc
PY
echo "jobs list verified"

echo "Breaking sshd on $FIRST_WORKER_POD and verifying --require-all fails"
# Streaming (non-JSON) exec uses the direct apiserver path, so it still works
# with the worker's sshd down.
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --pod "$FIRST_WORKER_POD" -- sh -c 'pkill -f "[o]kdev-sshd" || true'
sleep 1
set +e
REQ_OUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --all --json --require-all -- true 2>/dev/null)
REQ_RC=$?
set -e
if [[ "$REQ_RC" -eq 0 ]]; then
  echo "ERROR: --require-all should fail with an unreachable pod" >&2
  echo "$REQ_OUT" >&2
  exit 1
fi
OKDEV_REQUIRE_JSON="$REQ_OUT" python3 - "$FIRST_WORKER_POD" <<'PY'
import json, os, sys
results = json.loads(os.environ["OKDEV_REQUIRE_JSON"])
worker = sys.argv[1]
by_pod = {r["pod"]: r for r in results}
assert by_pod[worker]["status"] in ("unreachable", "error"), by_pod[worker]
responded = [p for p, r in by_pod.items() if r["status"] == "responded"]
assert len(responded) == len(results) - 1, by_pod
PY
echo "unreachable pod correctly reported; --require-all exited $REQ_RC"

echo "PyTorchJob gateway fanout e2e passed"
