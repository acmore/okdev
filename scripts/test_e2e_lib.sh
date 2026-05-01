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

echo "ok"
