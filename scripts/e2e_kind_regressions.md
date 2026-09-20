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
