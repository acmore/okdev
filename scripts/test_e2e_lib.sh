#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

assert_eq() {
  local got="$1"
  local want="$2"
  local msg="$3"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL: $msg" >&2
    echo "  want: $want" >&2
    echo "  got:  $got" >&2
    exit 1
  fi
}

kubectl() {
  printf '%s\n' "$KUBECTL_JSON"
}

KUBECTL_JSON='{
  "items": [
    {
      "metadata": {
        "name": "old-pod",
        "creationTimestamp": "2026-05-01T09:03:10Z",
        "deletionTimestamp": "2026-05-01T09:03:40Z",
        "labels": {
          "okdev.io/attachable": "true",
          "okdev.io/workload-name": "workload-old"
        },
        "annotations": {}
      },
      "status": {
        "phase": "Running",
        "containerStatuses": [
          {"ready": true},
          {"ready": true}
        ]
      }
    },
    {
      "metadata": {
        "name": "new-pod",
        "creationTimestamp": "2026-05-01T09:03:21Z",
        "labels": {
          "okdev.io/attachable": "true",
          "okdev.io/workload-name": "workload-new"
        },
        "annotations": {}
      },
      "status": {
        "phase": "Running",
        "containerStatuses": [
          {"ready": true},
          {"ready": true}
        ]
      }
    }
  ]
}'

assert_eq "$(session_attachable_pod_name default test-session)" "new-pod" \
  "session_attachable_pod_name should prefer the non-deleting rollout pod"
assert_eq "$(session_attachable_pod_names default test-session)" "new-pod old-pod" \
  "session_attachable_pod_names should return pods in priority order"
assert_eq "$(session_workload_name default test-session)" "workload-new" \
  "session_workload_name should use the highest-priority pod"

PS_SNAPSHOT="$(mktemp)"
cat >"$PS_SNAPSHOT" <<'PS'
  101 /repo/bin/okdev --config /tmp/a/.okdev/okdev.yaml --session e2e-ptjob --namespace default sync --mode two-phase
  102 /repo/bin/okdev --config /tmp/b/.okdev/okdev.yaml --session e2e-ptjob-interpod-ssh --namespace default sync --mode two-phase
  103 /repo/bin/okdev --session=e2e-ptjob sync
  104 /repo/bin/okdev --session e2e-ptjob status
PS
assert_eq "$(filter_session_sync_commands e2e-ptjob "$PS_SNAPSHOT" | awk '{print $1}' | tr '\n' ' ')" "101 103 " \
  "filter_session_sync_commands should match the exact --session value, not prefixes, and only sync commands"
assert_eq "$(filter_session_sync_commands e2e-ptjob-interpod-ssh "$PS_SNAPSHOT" | awk '{print $1}')" "102" \
  "filter_session_sync_commands should still match the longer session name"
rm -f "$PS_SNAPSHOT"

echo "ok"
