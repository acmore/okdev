#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-smoke}"
NAMESPACE="${NAMESPACE:-default}"
WORKDIR="$(make_workdir)"
HOME_DIR="${HOME_DIR:-$WORKDIR/home}"
CFG_PATH=""
FOLDER_CFG_PATH="$WORKDIR/.okdev/okdev.yaml"
LEGACY_CFG_PATH="$WORKDIR/.okdev.yaml"
SYNC_DIR="$WORKDIR/workspace"
ORIG_HOME="${HOME}"
ORIG_KUBECONFIG="${KUBECONFIG:-}"
KUBECONFIG_PATH="$HOME_DIR/.kube/config"
SSH_AGENT_PID=""

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
  if [[ -n "$CFG_PATH" && -f "$CFG_PATH" ]]; then
    "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes >/dev/null 2>&1 || true
  fi
  if [[ -n "$SSH_AGENT_PID" ]]; then
    ssh-agent -k >/dev/null 2>&1 || true
  fi
  return "$status"
}
trap cleanup EXIT

echo "Scaffolding pod config via okdev init"
cd "$WORKDIR"
"$OKDEV_BIN" init \
  --workload pod \
  --yes \
  --name e2e-smoke \
  --namespace "$NAMESPACE" \
  --dev-image ubuntu:22.04 \
  --sidecar-image "$SIDECAR_IMAGE" \
  --ssh-user root \
  --shell /bin/zsh \
  --sync-local "$SYNC_DIR" \
  --sync-remote /workspace

if [[ -f "$FOLDER_CFG_PATH" ]]; then
  CFG_PATH="$FOLDER_CFG_PATH"
elif [[ -f "$LEGACY_CFG_PATH" ]]; then
  CFG_PATH="$LEGACY_CFG_PATH"
else
  echo "ERROR: okdev init did not write a config file" >&2
  exit 1
fi

# Verify `okdev init --shell /bin/zsh` scaffolded the zsh starter files
# alongside the config.
ZSH_DIR="$WORKDIR/.okdev"
for path in "$ZSH_DIR/zshrc" "$ZSH_DIR/zsh-setup.example.sh"; do
  if [[ ! -f "$path" ]]; then
    echo "ERROR: expected okdev init --shell /bin/zsh to scaffold $path" >&2
    ls -la "$ZSH_DIR" >&2 || true
    exit 1
  fi
done
if ! grep -q 'shell: /bin/zsh' "$CFG_PATH"; then
  echo "ERROR: expected scaffolded config to contain spec.ssh.shell: /bin/zsh" >&2
  cat "$CFG_PATH" >&2
  exit 1
fi
echo "okdev init --shell scaffolding verified"

# Disable persistent SSH sessions for CI
replace_all_in_file "$CFG_PATH" 'persistentSession: true' 'persistentSession: false'
insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'
insert_after_line_once "$CFG_PATH" '  ssh:' '    forwardAgent: true'

echo "Generated config:"
cat "$CFG_PATH"

echo "hello from okdev e2e" >"$SYNC_DIR/hello.txt"

echo "Starting kind smoke session"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m

echo "Checking status"
STATUS_OUTPUT=""
for i in $(seq 1 15); do
  STATUS_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
  echo "$STATUS_OUTPUT"
  if [[ "$STATUS_OUTPUT" == *"health: active"* ]]; then
    break
  fi
  if [[ "$i" -eq 15 ]]; then
    echo "ERROR: expected status --details to report active sync" >&2
    exit 1
  fi
  echo "Sync health not active yet, retrying status check ($i/15)"
  sleep 2
done
echo "Sync health status verified"

echo "Checking local session state layout"
SESSION_DIR="$HOME_DIR/.okdev/sessions/${SESSION_NAME}"
SESSION_JSON="$SESSION_DIR/session.json"
TARGET_JSON="$SESSION_DIR/target.json"
SYNC_JSON="$SESSION_DIR/sync.json"
SYNC_HOME="$SESSION_DIR/syncthing"
for path in "$SESSION_DIR" "$SESSION_JSON" "$TARGET_JSON" "$SYNC_JSON" "$SYNC_HOME"; do
  if [[ ! -e "$path" ]]; then
    echo "ERROR: expected session state path to exist: $path" >&2
    exit 1
  fi
done
if ! grep -Eq "\"configPath\"[[:space:]]*:[[:space:]]*\"$CFG_PATH\"" "$SESSION_JSON"; then
  echo "ERROR: expected session.json to contain configPath=$CFG_PATH" >&2
  cat "$SESSION_JSON" >&2
  exit 1
fi
if ! grep -Eq "\"namespace\"[[:space:]]*:[[:space:]]*\"$NAMESPACE\"" "$SESSION_JSON"; then
  echo "ERROR: expected session.json to contain namespace=$NAMESPACE" >&2
  cat "$SESSION_JSON" >&2
  exit 1
fi
if ! grep -Eq "\"repoRoot\"[[:space:]]*:[[:space:]]*\"$WORKDIR\"" "$SESSION_JSON"; then
  echo "ERROR: expected session.json to contain repoRoot=$WORKDIR" >&2
  cat "$SESSION_JSON" >&2
  exit 1
fi
if [[ -e "$HOME_DIR/Sync/.stfolder" ]]; then
  echo "ERROR: unexpected Syncthing default folder marker created at $HOME_DIR/Sync/.stfolder" >&2
  find "$HOME_DIR/Sync" -maxdepth 3 -ls >&2 || true
  exit 1
fi
echo "Session state layout verified"

echo "Starting temporary local ssh-agent"
eval "$(ssh-agent -s)" >/dev/null
SSH_AGENT_KEY="$WORKDIR/agent_id_ed25519"
ssh-keygen -q -t ed25519 -N '' -f "$SSH_AGENT_KEY" >/dev/null
ssh-add "$SSH_AGENT_KEY" >/dev/null
ssh-add -l >/dev/null

echo "Verifying session commands work outside repo with positional session arg"
cd "$HOME_DIR"
STATUS_OUTSIDE=$("$OKDEV_BIN" status "$SESSION_NAME" --details)
if [[ "$STATUS_OUTSIDE" != *"Session: ${SESSION_NAME}"* && "$STATUS_OUTSIDE" != *"Session:"* ]]; then
  echo "ERROR: expected config-free status to succeed outside repo" >&2
  echo "$STATUS_OUTSIDE" >&2
  exit 1
fi
LOGS_OUTSIDE=$("$OKDEV_BIN" logs "$SESSION_NAME" --all --follow=false --tail 5)
if [[ "$LOGS_OUTSIDE" != *"[okdev-sidecar]"* ]]; then
  echo "ERROR: expected config-free logs to include sidecar output outside repo" >&2
  echo "$LOGS_OUTSIDE" >&2
  exit 1
fi
SSH_OUTSIDE=$("$OKDEV_BIN" ssh "$SESSION_NAME" --setup-key --cmd 'echo ssh-ok')
if [[ "$SSH_OUTSIDE" != *"ssh-ok"* ]]; then
  echo "ERROR: expected config-free ssh to succeed outside repo" >&2
  echo "$SSH_OUTSIDE" >&2
  exit 1
fi
SSH_AGENT_OUTSIDE=$("$OKDEV_BIN" ssh "$SESSION_NAME" --setup-key --cmd 'if [ -S "${SSH_AUTH_SOCK:-}" ]; then echo ssh-agent-ok; fi')
if [[ "$SSH_AGENT_OUTSIDE" != *"ssh-agent-ok"* ]]; then
  echo "ERROR: expected config-derived ssh agent forwarding to succeed outside repo" >&2
  echo "$SSH_AGENT_OUTSIDE" >&2
  exit 1
fi
# Verify the configured spec.ssh.shell was injected onto the dev container
# as OKDEV_SHELL. ubuntu:22.04 does not ship zsh, so the interactive
# resolver would fall back, but the env var must still be exported for the
# drift-warning path to fire on subsequent interactive sessions.
SHELL_ENV_OUTSIDE=$("$OKDEV_BIN" ssh "$SESSION_NAME" --setup-key --cmd 'printf "OKDEV_SHELL=%s\n" "${OKDEV_SHELL:-<unset>}"')
if [[ "$SHELL_ENV_OUTSIDE" != *"OKDEV_SHELL=/bin/zsh"* ]]; then
  echo "ERROR: expected dev container to have OKDEV_SHELL=/bin/zsh injected" >&2
  echo "$SHELL_ENV_OUTSIDE" >&2
  exit 1
fi
echo "spec.ssh.shell -> OKDEV_SHELL injection verified"

SSH_NO_AGENT_OUTSIDE=$("$OKDEV_BIN" ssh "$SESSION_NAME" --setup-key --no-forward-agent --cmd 'if [ -z "${SSH_AUTH_SOCK:-}" ]; then echo no-agent-ok; fi')
if [[ "$SSH_NO_AGENT_OUTSIDE" != *"no-agent-ok"* ]]; then
  echo "ERROR: expected --no-forward-agent to disable forwarding outside repo" >&2
  echo "$SSH_NO_AGENT_OUTSIDE" >&2
  exit 1
fi
cd "$WORKDIR"
echo "Config-free positional session commands verified"

echo "Waiting for synced file to appear remotely with correct content"
SYNC_OK=false
for i in $(seq 1 30); do
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'if [ -f /workspace/hello.txt ]; then cat /workspace/hello.txt; fi' || true)
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

# okdev sync wait: the edit-run guarantee. Write a fresh local file, wait for
# convergence, then read it remotely in one chain — no polling loop allowed.
echo "Testing okdev sync wait edit-run guarantee"
echo "sync-wait-payload" > "$SYNC_DIR/sync-wait-probe.txt"
SYNC_WAIT_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" sync wait --timeout 3m)
if [[ "$SYNC_WAIT_OUTPUT" != *"Sync converged"* ]]; then
  echo "ERROR: expected sync wait to report convergence" >&2
  echo "Got: $SYNC_WAIT_OUTPUT" >&2
  exit 1
fi
PROBE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- cat /workspace/sync-wait-probe.txt)
if [[ "$PROBE_CONTENT" != *"sync-wait-payload"* ]]; then
  echo "ERROR: file written before sync wait was not visible remotely after it returned" >&2
  echo "Got: $PROBE_CONTENT" >&2
  exit 1
fi
echo "sync wait convergence guarantee verified"

echo "Verifying repeated okdev up reuses active sync"
SYNC_PID_BEFORE=""
for i in $(seq 1 5); do
  STATUS_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
  SYNC_PID_BEFORE=$(printf '%s' "$STATUS_OUTPUT" | sync_pid_from_status || true)
  if [[ -n "$SYNC_PID_BEFORE" ]]; then
    break
  fi
  echo "Sync pid not found in status output yet, retrying ($i/5)"
  sleep 1
done
if [[ -z "$SYNC_PID_BEFORE" ]]; then
  echo "ERROR: expected status to include running sync pid before repeated up" >&2
  echo "$STATUS_OUTPUT" >&2
  exit 1
fi
SYNC_LOG_LINES_BEFORE=$(wc -l <"$SYNC_HOME/local.log")
REPEAT_UP_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m)
echo "$REPEAT_UP_OUTPUT"
if [[ "$REPEAT_UP_OUTPUT" != *"reused existing workload"* ]]; then
  echo "ERROR: expected repeated up to reuse existing pod workload" >&2
  exit 1
fi
SYNC_REUSED=false
if [[ "$REPEAT_UP_OUTPUT" == *"sync: already active"* ]]; then
  SYNC_REUSED=true
elif [[ "$REPEAT_UP_OUTPUT" != *"sync: active"* ]]; then
  echo "ERROR: expected repeated up to report active sync" >&2
  exit 1
fi
REPEAT_STATUS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
echo "$REPEAT_STATUS"
SYNC_PID_AFTER=$(printf '%s' "$REPEAT_STATUS" | sync_pid_from_status || true)
if [[ -z "$SYNC_PID_AFTER" ]]; then
  echo "ERROR: expected status to include running sync pid after repeated up" >&2
  exit 1
fi
if [[ "$SYNC_REUSED" == "true" && "$SYNC_PID_AFTER" != "$SYNC_PID_BEFORE" ]]; then
  echo "ERROR: expected sync pid to remain $SYNC_PID_BEFORE after repeated up, got ${SYNC_PID_AFTER:-missing}" >&2
  exit 1
fi
if [[ "$REPEAT_STATUS" != *"health: active"* ]]; then
  echo "ERROR: expected sync health to remain active after repeated up" >&2
  exit 1
fi
SYNC_LOG_LINES_AFTER=$(wc -l <"$SYNC_HOME/local.log")
if [[ "$SYNC_REUSED" == "true" && $((SYNC_LOG_LINES_AFTER - SYNC_LOG_LINES_BEFORE)) -gt 5 ]]; then
  echo "ERROR: expected repeated up not to restart or churn sync log" >&2
  tail -20 "$SYNC_HOME/local.log" >&2
  exit 1
fi
STATUS_OUTPUT="$REPEAT_STATUS"
if [[ "$SYNC_REUSED" == "true" ]]; then
  echo "Repeated okdev up sync reuse verified"
else
  echo "Repeated okdev up sync recovery verified"
fi

echo "Verifying remote shell and logs"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" ssh --setup-key --cmd 'test -f /workspace/hello.txt'
LOG_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" logs --all --follow=false --tail 10)
if [[ "$LOG_OUTPUT" != *"[okdev-sidecar]"* ]]; then
  echo "ERROR: expected okdev logs --all to include sidecar output" >&2
  echo "Got: $LOG_OUTPUT" >&2
  exit 1
fi
echo "logs output verified"

echo "Testing exec --"
EXEC_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'echo exec-ok')
if [[ "$EXEC_OUTPUT" != *"exec-ok"* ]]; then
  echo "ERROR: exec -- did not return expected output" >&2
  echo "Got: $EXEC_OUTPUT" >&2
  exit 1
fi
echo "exec -- verified"

# Exit semantics: a command that runs but exits non-zero is a command result
# (COMMAND EXITED NON-ZERO framing, remote status preserved on single pod),
# never the FAILED delivery framing or the infra exit code 69.
echo "Testing exec exit semantics for a non-zero remote command"
set +e
NONZERO_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'exit 3' 2>&1)
NONZERO_STATUS=$?
set -e
if [[ "$NONZERO_STATUS" -ne 3 ]]; then
  echo "ERROR: expected single-pod exec to preserve remote exit 3, got $NONZERO_STATUS" >&2
  echo "$NONZERO_OUTPUT" >&2
  exit 1
fi
if [[ "$NONZERO_OUTPUT" != *"COMMAND EXITED NON-ZERO"* || "$NONZERO_OUTPUT" == *"FAILED:"* ]]; then
  echo "ERROR: expected command-result framing without FAILED header" >&2
  echo "$NONZERO_OUTPUT" >&2
  exit 1
fi
echo "exec non-zero command framing and exit status verified"

# Issue #170 follow-up: `exec --pkill` kills processes matching a pattern
# without ever matching okdev's own exec machinery (the raw `pkill -f`
# self-match footgun). The decoy subshell keeps the marker in its cmdline
# (trailing `:` defeats shell tail-call exec) and survives the launching exec.
echo "Testing exec --pkill"
PKILL_MARKER="okdev-pkill-e2e-$$"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- \
  sh -c "( : $PKILL_MARKER; sleep 300; : ) >/dev/null 2>&1 &"
sleep 1
PKILL_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix --pkill "$PKILL_MARKER")
if [[ "$PKILL_OUTPUT" != *"killed "* ]]; then
  echo "ERROR: expected --pkill to report a killed process" >&2
  echo "Got: $PKILL_OUTPUT" >&2
  exit 1
fi
set +e
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix --pkill "$PKILL_MARKER" >/dev/null 2>&1
PKILL_NOMATCH_STATUS=$?
set -e
if [[ "$PKILL_NOMATCH_STATUS" -eq 0 ]]; then
  echo "ERROR: expected --pkill to exit non-zero when nothing matches" >&2
  exit 1
fi
echo "exec --pkill verified (kill reported, pkill exit convention held)"

# exec --json: the remote command's own non-zero exit must surface as data
# (exit field) without making okdev itself exit non-zero, and stdout must be
# captured into the envelope. Single-pod sessions still emit a JSON array.
echo "Testing exec --json captures remote exit as data"
set +e
JSON_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --json -- sh -lc 'printf "json-ok\n"; exit 3')
JSON_STATUS=$?
set -e
if [[ "$JSON_STATUS" -ne 0 ]]; then
  echo "ERROR: expected okdev exec --json to exit 0 (remote exit is data), got $JSON_STATUS" >&2
  echo "$JSON_OUTPUT" >&2
  exit 1
fi
OKDEV_EXEC_JSON="$JSON_OUTPUT" python3 - <<'PY'
import json, os, sys

doc = json.loads(os.environ["OKDEV_EXEC_JSON"])
if not isinstance(doc, list):
    print(f"ERROR: expected a JSON array for exec --json, got {type(doc).__name__}", file=sys.stderr)
    sys.exit(1)
if len(doc) != 1:
    print(f"ERROR: expected a single per-pod envelope, got {len(doc)}", file=sys.stderr)
    sys.exit(1)

env = doc[0]
for key in ("pod", "exit", "stdout", "stderr", "status"):
    if key not in env:
        print(f"ERROR: envelope missing key {key!r}: {env}", file=sys.stderr)
        sys.exit(1)
if env["exit"] != 3:
    print(f"ERROR: expected remote exit 3 in JSON envelope, got {env['exit']}", file=sys.stderr)
    sys.exit(1)
if env["stdout"] != "json-ok\n":
    print(f"ERROR: expected captured stdout 'json-ok\\n', got {env['stdout']!r}", file=sys.stderr)
    sys.exit(1)
if env["stderr"] != "":
    print(f"ERROR: expected empty captured stderr, got {env['stderr']!r}", file=sys.stderr)
    sys.exit(1)
if env["status"] != "responded":
    print(f"ERROR: expected status 'responded', got {env['status']!r}", file=sys.stderr)
    sys.exit(1)
if env.get("error"):
    print(f"ERROR: remote non-zero exit must not set error field, got {env['error']!r}", file=sys.stderr)
    sys.exit(1)
PY
echo "exec --json envelope verified"

# Exit-code differentiation: a command against a session that does not exist
# must surface the dedicated "session not found" code (74), not a generic 1.
# The cluster is reachable here, so this isolates the not-found path from the
# transient-contact path (78, exercised by unit tests).
echo "Testing exec against missing session exits 74"
set +e
MISSING_SESSION_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "${SESSION_NAME}-does-not-exist" exec --no-tty --no-prefix -- sh -lc 'echo should-not-run' 2>&1)
MISSING_SESSION_STATUS=$?
set -e
if [[ "$MISSING_SESSION_STATUS" -ne 74 ]]; then
  echo "ERROR: expected exec against missing session to exit 74, got $MISSING_SESSION_STATUS" >&2
  echo "$MISSING_SESSION_OUTPUT" >&2
  exit 1
fi
if [[ "$MISSING_SESSION_OUTPUT" != *"does not exist"* ]]; then
  echo "ERROR: expected missing-session error message, got: $MISSING_SESSION_OUTPUT" >&2
  exit 1
fi
echo "exec missing-session exit code 74 verified"

# ---------------------------------------------------------------------------
# File copy (okdev cp) single-pod verification
# ---------------------------------------------------------------------------

echo "Testing cp upload single file"
echo "cp-smoke-content" >"$SYNC_DIR/cp-smoke.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp "$SYNC_DIR/cp-smoke.txt" :/tmp/cp-smoke.txt
CP_VERIFY=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'cat /tmp/cp-smoke.txt')
if [[ "$CP_VERIFY" != "cp-smoke-content" ]]; then
  echo "ERROR: cp upload single file failed, got '$CP_VERIFY'" >&2
  exit 1
fi
echo "cp upload single file verified"

echo "Testing cp upload directory"
CP_DIR="$WORKDIR/cp-smoke-dir"
mkdir -p "$CP_DIR/nested"
echo "dir-a" >"$CP_DIR/a.txt"
echo "dir-b" >"$CP_DIR/nested/b.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp "$CP_DIR" :/tmp/cp-smoke-dir
CP_DIR_A=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'cat /tmp/cp-smoke-dir/a.txt')
CP_DIR_B=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'cat /tmp/cp-smoke-dir/nested/b.txt')
if [[ "$CP_DIR_A" != "dir-a" || "$CP_DIR_B" != "dir-b" ]]; then
  echo "ERROR: cp upload directory failed, got a='$CP_DIR_A' b='$CP_DIR_B'" >&2
  exit 1
fi
echo "cp upload directory verified"

echo "Testing cp download single file"
CP_DL_FILE="$WORKDIR/cp-downloaded.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp :/tmp/cp-smoke.txt "$CP_DL_FILE"
CP_DL_CONTENT=$(cat "$CP_DL_FILE")
if [[ "$CP_DL_CONTENT" != "cp-smoke-content" ]]; then
  echo "ERROR: cp download single file failed, got '$CP_DL_CONTENT'" >&2
  exit 1
fi
echo "cp download single file verified"

# Perf sanity: a ~32 MiB single-file download must complete quickly. With
# the byte-by-byte `dd skip=N bs=1` shape of bug, even this tiny file
# would take several minutes; with block-sized streaming it lands in a
# second or two on kind.
echo "Testing cp download large single file completes promptly"
LARGE_REMOTE=/tmp/cp-large.bin
LARGE_LOCAL="$WORKDIR/cp-large-downloaded.bin"
LARGE_BYTES=$((32 * 1024 * 1024))
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- \
  sh -lc "head -c $LARGE_BYTES /dev/urandom > $LARGE_REMOTE"
START_TS=$(date +%s)
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp ":$LARGE_REMOTE" "$LARGE_LOCAL"
END_TS=$(date +%s)
ELAPSED=$((END_TS - START_TS))
if [[ ! -f "$LARGE_LOCAL" ]] || [[ "$(stat -c %s "$LARGE_LOCAL" 2>/dev/null || stat -f %z "$LARGE_LOCAL")" != "$LARGE_BYTES" ]]; then
  echo "ERROR: cp large single-file download did not produce a $LARGE_BYTES-byte file" >&2
  exit 1
fi
# 30 s is generous for kind on CI hardware. A byte-by-byte regression would
# take well over a minute even for 32 MiB.
if (( ELAPSED > 30 )); then
  echo "ERROR: cp large single-file download took ${ELAPSED}s, expected < 30s (suggests byte-by-byte streaming regression)" >&2
  exit 1
fi
# Resume sanity: a rerun against the already-complete local file must
# succeed quickly without redownloading.
RESUME_START=$(date +%s)
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp ":$LARGE_REMOTE" "$LARGE_LOCAL"
RESUME_END=$(date +%s)
RESUME_ELAPSED=$((RESUME_END - RESUME_START))
if (( RESUME_ELAPSED > 10 )); then
  echo "ERROR: cp rerun against complete file took ${RESUME_ELAPSED}s, expected < 10s (resume not short-circuiting)" >&2
  exit 1
fi
echo "cp large single-file download verified (${ELAPSED}s initial, ${RESUME_ELAPSED}s rerun)"

echo "Testing cp download directory"
CP_DL_DIR="$WORKDIR/cp-downloaded-dir"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" cp :/tmp/cp-smoke-dir "$CP_DL_DIR"
CP_DL_A=$(cat "$CP_DL_DIR/cp-smoke-dir/a.txt")
CP_DL_B=$(cat "$CP_DL_DIR/cp-smoke-dir/nested/b.txt")
if [[ "$CP_DL_A" != "dir-a" || "$CP_DL_B" != "dir-b" ]]; then
  echo "ERROR: cp download directory failed, got a='$CP_DL_A' b='$CP_DL_B'" >&2
  exit 1
fi
echo "cp download directory verified"

echo "File copy (okdev cp) tests completed"

echo "Capturing current pod identity before spec drift"
ATTACHABLE_POD_NAME=$(session_attachable_pod_name "$NAMESPACE" "$SESSION_NAME")
ORIGINAL_POD_UID=$(kubectl -n "$NAMESPACE" get pod "$ATTACHABLE_POD_NAME" -o jsonpath='{.metadata.uid}')
echo "Original pod UID: $ORIGINAL_POD_UID"

echo "Changing pod workload spec to trigger drift detection"
replace_first_in_file "$CFG_PATH" 'image: ubuntu:22.04' 'image: ubuntu:24.04'

echo "Verifying non-interactive up fails with reconcile guidance"
set +e
DRIFT_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m 2>&1)
DRIFT_STATUS=$?
set -e
if [[ "$DRIFT_STATUS" -eq 0 ]]; then
  echo "ERROR: expected drifted pod workload to require --reconcile" >&2
  exit 1
fi
if [[ "$DRIFT_OUTPUT" != *"workload spec changed; re-run with --reconcile to recreate"* ]]; then
  echo "ERROR: unexpected pod drift output" >&2
  echo "$DRIFT_OUTPUT" >&2
  exit 1
fi
echo "Pod drift guidance verified"

echo "Recreating pod workload via --reconcile"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --reconcile --wait-timeout 5m

echo "Verifying pod was recreated with updated image"
ATTACHABLE_POD_NAME=$(session_attachable_pod_name "$NAMESPACE" "$SESSION_NAME")
RECONCILED_POD_UID=$(kubectl -n "$NAMESPACE" get pod "$ATTACHABLE_POD_NAME" -o jsonpath='{.metadata.uid}')
if [[ "$RECONCILED_POD_UID" == "$ORIGINAL_POD_UID" ]]; then
  echo "ERROR: expected pod UID to change after --reconcile recreate" >&2
  exit 1
fi
RECONCILED_IMAGE=$(kubectl -n "$NAMESPACE" get pod "$ATTACHABLE_POD_NAME" -o jsonpath='{.spec.containers[?(@.name=="dev")].image}')
if [[ "$RECONCILED_IMAGE" != "ubuntu:24.04" ]]; then
  echo "ERROR: expected reconciled pod image ubuntu:24.04, got '$RECONCILED_IMAGE'" >&2
  exit 1
fi
echo "Pod reconcile verified"

echo "Testing okdev restart recreates the workload in one command"
RESTART_POD_BEFORE=$(session_attachable_pod_name "$NAMESPACE" "$SESSION_NAME")
RESTART_UID_BEFORE=$(kubectl -n "$NAMESPACE" get pod "$RESTART_POD_BEFORE" -o jsonpath='{.metadata.uid}')
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" restart --yes --wait-timeout 5m
RESTART_POD_AFTER=$(session_attachable_pod_name "$NAMESPACE" "$SESSION_NAME")
RESTART_UID_AFTER=$(kubectl -n "$NAMESPACE" get pod "$RESTART_POD_AFTER" -o jsonpath='{.metadata.uid}')
if [[ "$RESTART_UID_AFTER" == "$RESTART_UID_BEFORE" ]]; then
  echo "ERROR: expected restart to recreate the pod (uid unchanged)" >&2
  exit 1
fi
RESTART_EXEC=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'echo restart-ok')
if [[ "$RESTART_EXEC" != *"restart-ok"* ]]; then
  echo "ERROR: exec after restart failed, got '$RESTART_EXEC'" >&2
  exit 1
fi
echo "okdev restart verified"

# --- Sync direction: per-path direction semantics on a live session --------
# Covers the direction chain end-to-end: editing spec.sync.paths[].direction
# restarts sync with new folder types on the next `okdev up` (config-hash
# driven), `up` enforces one-way local->pod, `down` makes the pod the
# authority (a stray local write cannot clobber pod results — the NCCL
# data-loss reproduction), and reset-workspace is rejected under `down`.

echo "Switching sync direction to up"
DIRECTION_UP_ENTRY="- local: \"$SYNC_DIR\"
        remote: /workspace
        direction: up"
replace_first_in_file "$CFG_PATH" "- \"$SYNC_DIR:/workspace\"" "$DIRECTION_UP_ENTRY"
if ! grep -q "direction: up" "$CFG_PATH"; then
  echo "ERROR: failed to rewrite sync path entry to structured form; config is:" >&2
  cat "$CFG_PATH" >&2
  exit 1
fi
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m
DETAILS_UP=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
if [[ "$DETAILS_UP" != *"direction: up (->)"* ]]; then
  echo "ERROR: expected status --details to show direction up, got:" >&2
  echo "$DETAILS_UP" >&2
  exit 1
fi

echo "up-direction: local edits still reach the pod"
echo "up-probe-content" > "$SYNC_DIR/up-probe.txt"
UP_SYNC_OK=false
for i in $(seq 1 30); do
  REMOTE_UP=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'cat /workspace/up-probe.txt 2>/dev/null' || true)
  if [[ "$REMOTE_UP" == "up-probe-content" ]]; then
    UP_SYNC_OK=true
    break
  fi
  sleep 2
done
if [[ "$UP_SYNC_OK" != true ]]; then
  echo "ERROR: local file did not reach pod in up direction" >&2
  exit 1
fi

echo "up-direction: pod-side writes must not flow back to local"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'echo intruder > /workspace/pod-write.txt'
sleep 15
if [[ -f "$SYNC_DIR/pod-write.txt" ]]; then
  echo "ERROR: pod-side write leaked back to local in up direction" >&2
  exit 1
fi
echo "sync direction up verified"

echo "Switching sync direction to down"
replace_first_in_file "$CFG_PATH" "direction: up" "direction: down"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m
DETAILS_DOWN=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
if [[ "$DETAILS_DOWN" != *"direction: down (<-)"* ]]; then
  echo "ERROR: expected status --details to show direction down, got:" >&2
  echo "$DETAILS_DOWN" >&2
  exit 1
fi

echo "down-direction: pod-generated results flow back to local"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'echo result-data > /workspace/result.txt'
DOWN_SYNC_OK=false
for i in $(seq 1 30); do
  if [[ "$(cat "$SYNC_DIR/result.txt" 2>/dev/null)" == "result-data" ]]; then
    DOWN_SYNC_OK=true
    break
  fi
  sleep 2
done
if [[ "$DOWN_SYNC_OK" != true ]]; then
  echo "ERROR: pod result did not flow back to local in down direction" >&2
  exit 1
fi

echo "down-direction: a stray local write cannot clobber the pod's result"
# Reproduces the reported incident: a failed local redirect truncates the
# synced copy. With the pod as authority the empty file must never
# propagate; the pod-side result stays intact.
: > "$SYNC_DIR/result.txt"
sleep 15
REMOTE_RESULT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'cat /workspace/result.txt')
if [[ "$REMOTE_RESULT" != "result-data" ]]; then
  echo "ERROR: pod-side result was clobbered by a local write in down direction, got '$REMOTE_RESULT'" >&2
  exit 1
fi

echo "down-direction: --reset-workspace is rejected"
set +e
RESET_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --reset-workspace --wait-timeout 5m 2>&1)
RESET_STATUS=$?
set -e
if [[ "$RESET_STATUS" -eq 0 ]] || [[ "$RESET_OUTPUT" != *"conflicts with sync direction"* ]]; then
  echo "ERROR: expected --reset-workspace to be rejected under direction down (status=$RESET_STATUS)" >&2
  echo "$RESET_OUTPUT" >&2
  exit 1
fi
REMOTE_RESULT_AFTER=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'cat /workspace/result.txt')
if [[ "$REMOTE_RESULT_AFTER" != "result-data" ]]; then
  echo "ERROR: remote result damaged by rejected reset-workspace, got '$REMOTE_RESULT_AFTER'" >&2
  exit 1
fi
echo "sync direction down verified"

# --- Multiple sync mappings: per-path directions, nested local roots ------
# The session currently has one mapping (direction: down). Verify remote
# overlap is rejected, then add a second mapping whose LOCAL root nests
# inside the primary root (managed .stignore exclusion) while the remote
# stays disjoint on an auto-provisioned volume.

echo "Testing overlapping remote roots are rejected"
cp "$CFG_PATH" "$CFG_PATH.bak"
OVERLAP_ENTRY="- local: \"$SYNC_DIR\"
        remote: /workspace
        direction: down
      - local: \"$WORKDIR/elsewhere\"
        remote: /workspace/nested"
replace_first_in_file "$CFG_PATH" "- local: \"$SYNC_DIR\"
        remote: /workspace
        direction: down" "$OVERLAP_ENTRY"
set +e
VALIDATE_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" validate 2>&1)
VALIDATE_STATUS=$?
set -e
if [[ "$VALIDATE_STATUS" -eq 0 ]] || [[ "$VALIDATE_OUTPUT" != *"overlap"* ]]; then
  echo "ERROR: expected okdev validate to reject overlapping mappings (status=$VALIDATE_STATUS)" >&2
  echo "$VALIDATE_OUTPUT" >&2
  exit 1
fi
cp "$CFG_PATH.bak" "$CFG_PATH"
echo "overlap rejection verified"

echo "Adding a second mapping nested inside the primary local root (code up, results down)"
COLLECT_DIR="$SYNC_DIR/collected"
mkdir -p "$COLLECT_DIR"
# The local root nests inside the repo — okdev excludes it from the primary
# folder via a managed .stignore block. No volume YAML needed: okdev
# auto-provisions an emptyDir at the extra mapping's remote root and mirrors
# it into the sidecar. Adding the mapping changes the pod spec, so plain
# `up` must first surface drift guidance.
MULTI_ENTRY="- local: \"$SYNC_DIR\"
        remote: /workspace
        direction: up
      - local: \"$COLLECT_DIR\"
        remote: /data/results
        direction: down"
replace_first_in_file "$CFG_PATH" "- local: \"$SYNC_DIR\"
        remote: /workspace
        direction: down" "$MULTI_ENTRY"
if ! grep -q "/data/results" "$CFG_PATH"; then
  echo "ERROR: failed to add second sync mapping; config is:" >&2
  cat "$CFG_PATH" >&2
  exit 1
fi

echo "multi-mapping: plain up surfaces reconcile guidance for the new volume"
set +e
MULTI_DRIFT_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m 2>&1)
MULTI_DRIFT_STATUS=$?
set -e
if [[ "$MULTI_DRIFT_STATUS" -eq 0 ]] || [[ "$MULTI_DRIFT_OUTPUT" != *"--reconcile"* ]]; then
  echo "ERROR: expected drift guidance when adding a sync mapping (status=$MULTI_DRIFT_STATUS)" >&2
  echo "$MULTI_DRIFT_OUTPUT" >&2
  exit 1
fi

"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --reconcile --wait-timeout 5m

echo "multi-mapping: results folder flows pod -> local"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'mkdir -p /data/results && echo multi-result > /data/results/multi.txt'
MULTI_DOWN_OK=false
for i in $(seq 1 30); do
  if [[ "$(cat "$COLLECT_DIR/multi.txt" 2>/dev/null)" == "multi-result" ]]; then
    MULTI_DOWN_OK=true
    break
  fi
  sleep 2
done
if [[ "$MULTI_DOWN_OK" != true ]]; then
  echo "ERROR: second mapping did not sync pod result to local" >&2
  exit 1
fi

echo "multi-mapping: primary code folder still flows local -> pod"
echo "multi-code-probe" > "$SYNC_DIR/multi-code.txt"
MULTI_UP_OK=false
for i in $(seq 1 30); do
  MULTI_REMOTE=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'cat /workspace/multi-code.txt 2>/dev/null' || true)
  if [[ "$MULTI_REMOTE" == "multi-code-probe" ]]; then
    MULTI_UP_OK=true
    break
  fi
  sleep 2
done
if [[ "$MULTI_UP_OK" != true ]]; then
  echo "ERROR: primary mapping stopped syncing local code to pod" >&2
  exit 1
fi

echo "multi-mapping: nested local root is excluded from the primary folder"
# The primary folder (direction up) must NOT carry the nested subtree to
# the pod: the managed .stignore block excludes it, so /workspace/collected
# never appears even though local collected/ has content.
sleep 15
NESTED_LEAK=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'test -e /workspace/collected && echo leaked || echo clean')
if [[ "$NESTED_LEAK" != *"clean"* ]]; then
  echo "ERROR: nested local root leaked through the primary folder" >&2
  exit 1
fi
if ! grep -q "okdev:begin managed sync excludes" "$SYNC_DIR/.stignore" || ! grep -q "^/collected$" "$SYNC_DIR/.stignore"; then
  echo "ERROR: expected managed exclude for /collected in primary .stignore, got:" >&2
  cat "$SYNC_DIR/.stignore" >&2
  exit 1
fi

echo "multi-mapping: status shows both mappings with their directions"
MULTI_DETAILS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
if [[ "$MULTI_DETAILS" != *"-> /workspace"* ]] || [[ "$MULTI_DETAILS" != *"<- /data/results"* ]]; then
  echo "ERROR: expected per-path direction arrows in status --details, got:" >&2
  echo "$MULTI_DETAILS" >&2
  exit 1
fi
if [[ "$MULTI_DETAILS" != *"excluded from primary folder (own mappings): /collected"* ]]; then
  echo "ERROR: expected managed exclude in status --details, got:" >&2
  echo "$MULTI_DETAILS" >&2
  exit 1
fi
echo "multiple sync mappings verified"

echo "Removing the nested mapping leaves a tombstone (no silent widening)"
SINGLE_ENTRY="- local: \"$SYNC_DIR\"
        remote: /workspace
        direction: up"
replace_first_in_file "$CFG_PATH" "- local: \"$SYNC_DIR\"
        remote: /workspace
        direction: up
      - local: \"$COLLECT_DIR\"
        remote: /data/results
        direction: down" "$SINGLE_ENTRY"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --reconcile --wait-timeout 5m
if ! grep -q "retained after mapping removal" "$SYNC_DIR/.stignore"; then
  echo "ERROR: expected tombstone comment in primary .stignore, got:" >&2
  cat "$SYNC_DIR/.stignore" >&2
  exit 1
fi
if [[ "$(cat "$COLLECT_DIR/multi.txt" 2>/dev/null)" != "multi-result" ]]; then
  echo "ERROR: local files of the removed mapping must be kept" >&2
  exit 1
fi
echo "tombstone-probe" > "$COLLECT_DIR/tombstone-probe.txt"
sleep 15
TOMB_LEAK=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'test -e /workspace/collected && echo leaked || echo clean')
if [[ "$TOMB_LEAK" != *"clean"* ]]; then
  echo "ERROR: removed mapping's subtree leaked into the primary folder" >&2
  exit 1
fi
TOMB_DETAILS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
if [[ "$TOMB_DETAILS" != *"retained after mapping removal): /collected"* ]]; then
  echo "ERROR: expected retained exclude in status --details, got:" >&2
  echo "$TOMB_DETAILS" >&2
  exit 1
fi
echo "nested mapping removal verified"

echo "Sync direction tests completed"

# --- Detached jobs: runtime-volume logs, process-group stop, sidecar -------
# fallback. Covers the detach chain end-to-end on a real pod: logs land on
# the shared /var/okdev volume, `jobs stop` kills the whole process tree
# (not just the leader), and logs stay readable through the sidecar after
# the dev container dies. The container-crash step is deliberately last
# before teardown: it leaves the dev container in CrashLoopBackOff.

echo "Testing exec --detach places logs on the runtime volume"
DETACH_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --detach -- sh -c 'sleep 7301 & sleep 7302 & wait')
echo "$DETACH_OUTPUT"
if [[ "$DETACH_OUTPUT" != *"log=/var/okdev/exec/"* ]]; then
  echo "ERROR: expected detach log under /var/okdev/exec, got: $DETACH_OUTPUT" >&2
  exit 1
fi
TREE_JOB_ID=$(printf '%s\n' "$DETACH_OUTPUT" | sed -n 's/.*job_id=\([^ ]*\).*/\1/p' | head -n1)
if [[ -z "$TREE_JOB_ID" ]]; then
  echo "ERROR: could not extract detach job id from: $DETACH_OUTPUT" >&2
  exit 1
fi
echo "detach on runtime volume verified (job $TREE_JOB_ID)"

echo "Testing jobs list reports the live process group"
JOBS_JSON=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" --output json jobs list)
OKDEV_JOBS_JSON="$JOBS_JSON" OKDEV_TREE_JOB_ID="$TREE_JOB_ID" python3 - <<'PY'
import json, os, sys

doc = json.loads(os.environ["OKDEV_JOBS_JSON"])
job_id = os.environ["OKDEV_TREE_JOB_ID"]
jobs = [j for j in doc.get("jobs", []) if j.get("jobId") == job_id]
if not jobs:
    print(f"ERROR: job {job_id} missing from jobs list: {doc}", file=sys.stderr)
    sys.exit(1)
job = jobs[0]
if not job.get("state", "").startswith("running"):
    print(f"ERROR: expected running state, got {job}", file=sys.stderr)
    sys.exit(1)
row = job["podStates"][0]
if row.get("pgid", 0) <= 0:
    print(f"ERROR: expected pgid > 0 (setsid group), got {row}", file=sys.stderr)
    sys.exit(1)
# Leader sh plus the two background sleeps.
if row.get("groupLive", 0) < 3:
    print(f"ERROR: expected >=3 live group members, got {row}", file=sys.stderr)
    sys.exit(1)
PY
echo "jobs list process group verified"

# Issue #167: jobs logs must be reliable (size-verified read, no silent empty
# output) and cheap to poll (--tail limits lines; --since skips pods whose log
# file has not changed since the cutoff).
echo "Testing jobs logs full read, --tail, and --since"
LOGS_JOB_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --detach -- sh -c 'for i in 1 2 3 4 5; do echo "logline-$i"; done')
LOGS_JOB_ID=$(printf '%s\n' "$LOGS_JOB_OUTPUT" | sed -n 's/.*job_id=\([^ ]*\).*/\1/p' | head -n1)
if [[ -z "$LOGS_JOB_ID" ]]; then
  echo "ERROR: could not extract logs job id from: $LOGS_JOB_OUTPUT" >&2
  exit 1
fi
sleep 3
FULL_LOGS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" jobs logs "$LOGS_JOB_ID")
for i in 1 5; do
  if [[ "$FULL_LOGS" != *"logline-$i"* ]]; then
    echo "ERROR: full jobs logs read missing logline-$i: $FULL_LOGS" >&2
    exit 1
  fi
done
TAIL_LOGS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" jobs logs "$LOGS_JOB_ID" --tail 2)
if [[ "$TAIL_LOGS" != *"logline-5"* || "$TAIL_LOGS" == *"logline-1"* ]]; then
  echo "ERROR: jobs logs --tail 2 should contain logline-5 but not logline-1: $TAIL_LOGS" >&2
  exit 1
fi
sleep 3
SINCE_LOGS=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" jobs logs "$LOGS_JOB_ID" --since 2s)
if [[ "$SINCE_LOGS" == *"logline-"* ]]; then
  echo "ERROR: jobs logs --since 2s should skip an idle log file: $SINCE_LOGS" >&2
  exit 1
fi
SINCE_ACTIVE=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" jobs logs "$LOGS_JOB_ID" --since 24h)
if [[ "$SINCE_ACTIVE" != *"logline-5"* ]]; then
  echo "ERROR: jobs logs --since 24h should include recent content: $SINCE_ACTIVE" >&2
  exit 1
fi
echo "jobs logs full/--tail/--since verified"

echo "Testing jobs stop kills the whole process tree"
STOP_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" jobs stop "$TREE_JOB_ID")
echo "$STOP_OUTPUT"
if [[ "$STOP_OUTPUT" != *"pgid="* ]]; then
  echo "ERROR: expected group signal (pgid=...) in stop output, got: $STOP_OUTPUT" >&2
  exit 1
fi
LEFTOVER=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'n=0; for d in /proc/[0-9]*/cmdline; do c=$(tr "\0" " " < "$d" 2>/dev/null || true); case "$c" in "sleep 7301 "|"sleep 7302 ") n=$((n+1)) ;; esac; done; echo "leftover=$n"')
if [[ "$LEFTOVER" != *"leftover=0"* ]]; then
  echo "ERROR: expected no surviving group processes after stop, got: $LEFTOVER" >&2
  exit 1
fi
echo "jobs group stop verified"

echo "Testing detached job logs survive dev-container death via sidecar"
MARKER_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --detach -- sh -c 'echo sidecar-fallback-marker; exec sleep 7303')
MARKER_JOB_ID=$(printf '%s\n' "$MARKER_OUTPUT" | sed -n 's/.*job_id=\([^ ]*\).*/\1/p' | head -n1)
if [[ -z "$MARKER_JOB_ID" ]]; then
  echo "ERROR: could not extract marker job id from: $MARKER_OUTPUT" >&2
  exit 1
fi
LOGS_HEALTHY=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" jobs logs "$MARKER_JOB_ID")
if [[ "$LOGS_HEALTHY" != *"sidecar-fallback-marker"* ]]; then
  echo "ERROR: expected marker in healthy-container job logs, got: $LOGS_HEALTHY" >&2
  exit 1
fi

echo "Crashing dev container into CrashLoopBackOff"
CRASH_POD_NAME=$(session_attachable_pod_name "$NAMESPACE" "$SESSION_NAME")
DEV_WAITING_JSONPATH='{.status.containerStatuses[?(@.name=="dev")].state.waiting.reason}'
CRASHED=0
for attempt in $(seq 1 30); do
  REASON=$(kubectl -n "$NAMESPACE" get pod "$CRASH_POD_NAME" -o jsonpath="$DEV_WAITING_JSONPATH" 2>/dev/null || true)
  if [[ "$REASON" == "CrashLoopBackOff" ]]; then
    CRASHED=1
    break
  fi
  # Kill the dev container's main process (sleep infinity, from the config
  # template). The exec session dies with the container, so tolerate
  # errors; kubelet escalates to CrashLoopBackOff after repeated crashes.
  "$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --no-prefix -- sh -lc 'for d in /proc/[0-9]*/cmdline; do if [ "$(tr "\0" " " < "$d" 2>/dev/null)" = "sleep infinity " ]; then p=${d%/cmdline}; kill -9 "${p#/proc/}" 2>/dev/null || true; fi; done' >/dev/null 2>&1 || true
  sleep 2
done
if [[ "$CRASHED" -ne 1 ]]; then
  echo "ERROR: dev container did not enter CrashLoopBackOff" >&2
  kubectl -n "$NAMESPACE" get pod "$CRASH_POD_NAME" -o jsonpath='{.status.containerStatuses}' >&2 || true
  exit 1
fi
echo "dev container crashed (CrashLoopBackOff)"

LOGS_FALLBACK=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" jobs logs "$MARKER_JOB_ID")
if [[ "$LOGS_FALLBACK" != *"sidecar-fallback-marker"* ]]; then
  echo "ERROR: expected marker via sidecar log fallback, got: $LOGS_FALLBACK" >&2
  exit 1
fi
echo "sidecar log fallback verified"

echo "Detached job tests completed"

echo "Testing explicit okdev down"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes
assert_no_local_sync_processes "$SESSION_NAME" "$SYNC_HOME"

echo "Verifying pod is deleted"
for i in $(seq 1 30); do
  if [[ -z "$(session_attachable_pod_names "$NAMESPACE" "$SESSION_NAME")" ]]; then
    echo "Pod deleted on attempt $i"
    break
  fi
  if [[ "$i" -eq 30 ]]; then
    echo "ERROR: session $SESSION_NAME still has attachable pod(s) after down" >&2
    exit 1
  fi
  sleep 2
done

echo "Smoke test completed"
