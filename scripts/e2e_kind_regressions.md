# Kind CLI regression tests

These tests run the real CLI against an existing Kind cluster. They are included
in `bash scripts/e2e_local_kind.sh` and `.github/workflows/e2e-kind.yml`, after the CLI and sidecar image are built and
loaded. Each test uses a unique namespace and isolated HOME, then calls `okdev
down`, deletes the namespace, and stops its test processes. Cleanup failures fail
the test and identify retained state. They never use the developer's session state.

To reuse a prepared cluster and cached sidecar image:

```sh
go build -o bin/okdev ./cmd/okdev
bash scripts/e2e_kind_regressions.sh
# Run only one group:
bash scripts/e2e_kind_regressions.sh config_jobs
```

Available groups:

| Group | Real Kind coverage |
| --- | --- |
| `sync_diagnostics` | Sync child death before Ready, exit evidence, repeated warnings, strict execution gate, repair and survival after parent exit |
| `cp_stats` | Clean opt-in statistics, fanout counts, hashes, full/verified reuse, partial resume and failed uploads; optional large-file benchmark |
| `jobs_ready` | Old healthy instance rejection, all-member readiness, job exit, deadlines/cancellation, scoped stop and unchanged completion wait |
| `exec_retry` | Preflight budget exhaustion/recovery/cancellation, clean JSON, no replay after a delivered command loses its stream or exits nonzero |
| `replicas` | Read-only Deployment scale/manifest differences, text/JSON and all-session isolation; PyTorchJob roles run after training-operator installation |
| `podgroup` | Controlled PodGroup conditions/events through the real API, text/JSON, UID filtering, RBAC denial and absent CRD; no Volcano scheduler required |
| `hook_evidence` | Successful no-op hooks, per-pod stdout/stderr, prerequisite failure, recorded exit states and retry after repair |
| `forward_recovery` | Initial API/DNS failure, visible retries, live forwarding stream loss, replacement Pod selection, cancellation and listener cleanup |
| `snapshot` | Sync-independent snapshot delivery, explicit targets, remote archive hashes, target-only defaults, partial failure |
| `automation` | Executable documentation: JSON/hash checks, job-specific waits, remote quoting, explicit port-forward bind, preflight failure |
| `config_jobs` | Named config discovery, config isolation, collision rejection, single/multi-pod job log bytes |
| `readiness` | Live API connection loss, readiness retry, timeout continuation on the same Pod, one-time setup |
| `stdin` | Binary streams, output before EOF, remote exit codes, cancellation, timeout, invalid modes |
| `sync_revision` | Same-size content changes, empty entries, deletion, ignores, nested independent sync paths |
| `status_identity` | Two-session selection, config scope changes, historical scope isolation, API failure |
| `mesh` | All receivers and hooks, disconnected worker, sync/exec convergence gates, recovery |

`CLUSTER_NAME` defaults to `okdev-e2e`; `OKDEV_BIN` defaults to `bin/okdev`;
`SIDECAR_IMAGE` defaults to `okdev-sidecar:v0.0.0-e2e`. The sidecar and
`ubuntu:22.04` must be available to the cluster. The fixture reuses a locally
cached Syncthing binary if available; otherwise normal CLI installation applies.
Python 3 (standard library only), kubectl, git, and Kind are required.

Redirect the whole run when saving logs and inspect its actual exit status;
do not pipe the run into `tail`.

The `podgroup` group temporarily installs a minimal Volcano PodGroup CRD when
none exists, then removes only that test-created CRD. It reuses an existing CRD
without modifying or deleting it. Conditions and events are injected fixtures;
this checks diagnostic reads and RBAC, not Volcano scheduling or quota enforcement.

- `exec_transport`: real SSH master reuse, explicit exec pod/container, exit/stream/stdin contracts, client cancellation and API-stream interruption without command replay. `EXEC_BENCH_RUNS=12` enables the optional short-command latency/concurrency experiment.

- `attach`: external StatefulSet exec/jobs/cp/setup without workload adoption, scope/container selection, owner and RBAC rejection, and lifecycle refusal with unchanged workload metadata/spec.
