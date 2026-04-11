#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
SESSION_NAME="${SESSION_NAME:-e2e-sync-health}"
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
export OKDEV_SYNC_HEALTH_CHECK_INTERVAL="${OKDEV_SYNC_HEALTH_CHECK_INTERVAL:-2s}"
export OKDEV_SYNC_HEALTH_CHECK_MAX_INTERVAL="${OKDEV_SYNC_HEALTH_CHECK_MAX_INTERVAL:-2s}"

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

echo "Scaffolding sync health config via okdev init"
cd "$WORKDIR"
"$OKDEV_BIN" init \
  --workload pod \
  --yes \
  --name e2e-sync-health \
  --namespace "$NAMESPACE" \
  --dev-image ubuntu:22.04 \
  --sidecar-image "$SIDECAR_IMAGE" \
  --ssh-user root \
  --sync-local "$SYNC_DIR" \
  --sync-remote /workspace

if [[ ! -f "$CFG_PATH" ]]; then
  CFG_PATH="$WORKDIR/.okdev.yaml"
fi
if [[ ! -f "$CFG_PATH" ]]; then
  echo "ERROR: okdev init did not write a config file" >&2
  exit 1
fi

replace_all_in_file "$CFG_PATH" 'persistentSession: true' 'persistentSession: false'
insert_after_line_once "$CFG_PATH" '  ssh:' '    persistentSession: false'

echo "Starting sync health e2e session"
echo "before stale injection" >"$SYNC_DIR/health.txt"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" up --wait-timeout 5m

SYNC_HOME="$HOME_DIR/.okdev/sessions/${SESSION_NAME}/syncthing"
LOG_PATH="$SYNC_HOME/local.log"

echo "Waiting for sync health to become active"
for i in $(seq 1 20); do
  STATUS_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
  if [[ "$STATUS_OUTPUT" == *"health: active"* ]]; then
    break
  fi
  if [[ "$i" -eq 20 ]]; then
    echo "ERROR: expected status --details to report active sync" >&2
    echo "$STATUS_OUTPUT" >&2
    exit 1
  fi
  sleep 2
done

echo "Injecting stale local Syncthing peer address"
python3 - "$SYNC_HOME" <<'PY'
import http.client
import json
import pathlib
import sys
import time
import urllib.parse
import xml.etree.ElementTree as ET

home = pathlib.Path(sys.argv[1])
endpoint = home.joinpath("endpoint").read_text().strip()
parsed = urllib.parse.urlparse(endpoint)
cfg_path = home / "config.xml"
root = ET.parse(cfg_path).getroot()
api_key = root.findtext("./gui/apikey")
if not api_key:
    raise SystemExit("local Syncthing API key not found")


def request(method, path, body=None):
    conn = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=5)
    headers = {"X-API-Key": api_key}
    if body is not None:
        body = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    conn.request(method, path, body=body, headers=headers)
    resp = conn.getresponse()
    data = resp.read()
    conn.close()
    if resp.status >= 300:
        raise SystemExit(f"{method} {path} failed: {resp.status} {data.decode(errors='replace')}")
    return data


config = json.loads(request("GET", "/rest/config"))
devices = config.get("devices", [])
if len(devices) != 1:
    raise SystemExit(f"expected one remote device, got {len(devices)}")
devices[0]["addresses"] = ["tcp://127.0.0.1:1"]
request("PUT", "/rest/config", config)
request("POST", "/rest/system/restart")

deadline = time.time() + 30
while time.time() < deadline:
    try:
        request("GET", "/rest/system/status")
        break
    except Exception:
        time.sleep(0.5)
else:
    raise SystemExit("local Syncthing API did not recover after restart")
PY

echo "Waiting for sync health loop to restore peer connection"
for i in $(seq 1 40); do
  if grep -q "sync: reconnected to peer" "$LOG_PATH"; then
    echo "Sync health restored the peer connection on attempt $i"
    break
  fi
  if [[ "$i" -eq 40 ]]; then
    echo "ERROR: expected sync health loop to restore stale peer connection" >&2
    exit 1
  fi
  sleep 1
done

echo "Verifying sync remains active after health restoration"
for i in $(seq 1 20); do
  STATUS_OUTPUT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" status --details)
  if [[ "$STATUS_OUTPUT" == *"health: active"* ]]; then
    break
  fi
  if [[ "$i" -eq 20 ]]; then
    echo "ERROR: expected sync health active after restoration" >&2
    echo "$STATUS_OUTPUT" >&2
    exit 1
  fi
  sleep 2
done

echo "Verifying post-restoration file sync"
echo "after health restore" >"$SYNC_DIR/health-after.txt"
SYNC_OK=false
for i in $(seq 1 30); do
  REMOTE_CONTENT=$("$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" exec --no-tty --cmd 'if [ -f /workspace/health-after.txt ]; then cat /workspace/health-after.txt; fi' || true)
  if [[ "$REMOTE_CONTENT" == "after health restore" ]]; then
    SYNC_OK=true
    break
  fi
  sleep 2
done
if [[ "$SYNC_OK" != "true" ]]; then
  echo "ERROR: expected post-restoration file to sync to remote workspace" >&2
  exit 1
fi

echo "Testing explicit okdev down"
"$OKDEV_BIN" --config "$CFG_PATH" --session "$SESSION_NAME" down --yes
assert_no_local_sync_processes "$SESSION_NAME" "$SYNC_HOME"

echo "Sync health e2e completed"
