# Short-command latency and SSH connection reuse

The #289 investigation found a useful latency reduction from reusing an SSH
connection, but not an equivalent replacement for `okdev exec`. No
`exec --transport=ssh` option is introduced by this investigation. Existing exec
selectors, container targeting, authorization checks and command semantics remain
unchanged. Use `exec` when those contracts matter; do not transparently substitute
`ssh okdev-<session>` in an automation workflow.

## Measured result

On 2026-09-21, the CLI at `1239990` was tested against local Kind on macOS 15.7.4
ARM64, Kubernetes 1.32.2, with Ubuntu 22.04 and cached
`okdev-sidecar:v0.0.0-e2e`. Twelve `true` commands were run per transport at each
concurrency level. Rates include client process startup; reused SSH excludes
master establishment. These small local samples are descriptive, not latency
budgets or evidence of performance on the originally reported remote cluster.

| Transport | Sequential median (ms) | Sequential commands/s | Concurrency 4 median (ms) | Concurrency 4 commands/s |
| --- | ---: | ---: | ---: | ---: |
| `okdev exec --json --pod … --container dev` | 61.55 | 15.71 | 97.54 | 39.27 |
| `kubectl exec` | 69.97 | 14.07 | 112.79 | 34.28 |
| Cold SSH over the managed proxy | 274.46 | 3.29 | 301.20 | 13.56 |
| Reused OpenSSH master over the managed proxy | 9.52 | 103.87 | 12.34 | 293.85 |

[Raw durations and batch wall times](benchmarks/exec-kind-20260921.json) are retained.
The reported 1–3 second fixed overhead was not reproduced locally. These are
single-pod measurements, not multi-pod gateway benchmarks. The existing gateway
can already fan out through one API exec; do not assume one new API exec per pod.
Order and background load were not randomized or controlled. The reused master
ran through a local TCP fault-injection relay; the other paths did not.

## What the experiment establishes

The persistent Kind regression checks an explicit exec pod/container selection,
remote exit 7 and separate stdout/stderr, plus SSH's matching exit/stream behavior,
binary stdin and connected pod identity. Terminating a local multiplexed command
leaves the master usable for another command. It does **not** establish that all
remote descendants are killed; use tracked jobs and explicit stop operations when
remote process lifetime matters.

After observing a remote side effect, the test forcibly disconnects the TCP
connection carrying Kubernetes port-forward. The SSH command and master fail;
the side effect occurs exactly once. A later command explicitly configured to
require that master fails instead of silently reconnecting. This tests loss of an
established API connection, not every form of API-server outage. An outage that
only prevents new requests can have different effects on already-open streams.
Never automatically replay a possibly delivered mutating command.

## Decision and requirements for a future exec transport

Connection reuse is promising for repeated calls to a fixed dev container. The
experiment does not justify treating a bare SSH alias as an exec backend:

- `exec` resolves selectors and checks session access on each invocation. Reusing
  an established SSH connection does not repeat those Kubernetes checks; removing
  them accounts for part of the measured difference.
- A master remains connected to its original pod even if the session target is
  repinned or replaced. A session alias alone is insufficient cache identity.
- The managed SSH service runs in the configured dev container. It cannot honor
  an arbitrary `exec --container` by connecting to that same service.
- OpenSSH accepts a remote shell command string. An adapter must preserve argv
  quoting, stdin, separate output streams, remote status and cancellation, not
  reuse the combined-output helper behind `okdev ssh --cmd`.
- Persistent sockets require ownership, lifetime and invalidation rules. A
  transport error after delivery must stay an ambiguous failure, never trigger
  command replay or silent fallback to Kubernetes exec.

A future opt-in implementation should retain existing per-call authorization and
selection, bind a connection to cluster/namespace/pod UID/container/SSH identity,
reject unsupported targets explicitly, and measure the complete checked CLI path.
It needs regressions for repinning, pod replacement, owner changes, non-dev
containers, selectors/fanout, quoting, cancellation and ambiguous delivery before
being presented as interchangeable with exec. The current result supports further
implementation work; it does not claim these requirements are already implemented.

## Reproduce

The default regression runs without timing assertions in the existing local and
CI Kind suites. The optional benchmark uses OpenSSH and Python's standard library:

```bash
EXEC_BENCH_RUNS=12 bash scripts/e2e_kind_regressions.sh exec_transport >exec-benchmark.log 2>&1
result=$?
cat exec-benchmark.log
exit "$result"
```

Build `bin/okdev` first or set `OKDEV_BIN` to an existing binary. The fixture uses
the existing `okdev-e2e` cluster and sidecar image, an isolated home and kubeconfig,
and its own master socket. It tears down the session, namespace, SSH processes,
relay and temporary files. `EXEC_BENCH_RESULT=` records all individual durations.
Re-run against a representative environment before setting production latency
expectations; do not infer API-outage independence from low local SSH latency.
