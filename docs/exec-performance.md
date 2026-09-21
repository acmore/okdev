# Short-command latency and SSH connection reuse

`okdev exec --transport=ssh -- <command>` opts into a reusable OpenSSH
connection to a managed dev container. The default remains
`--transport=kubernetes`; existing invocations keep their transport.

```bash
okdev exec --transport=ssh -- python -c 'print("ready")'
okdev exec --transport=ssh --all --json --require-all -- hostname
printf 'input' | okdev exec --transport=ssh --stdin -- cat
```

## Requirements and behavior

- Local OpenSSH, a Linux managed dev container with `readlink` and a POSIX-compatible SSH shell, a running
  `okdev-sshd` on port 2222, and the configured SSH private key are required.
  `okdev up` prepares the current target. For another pod, run
  `okdev target set --pod <name>` followed by
  `okdev ssh --setup-key --cmd true` before selecting it for SSH exec.
  The exec transport itself never installs keys or starts sshd.
- Selectors, groups, target pins, foreground commands, `--script`, `--detach`,
  `--stdin`, timeouts, and JSON results retain their exec behavior. JSON uses
  direct per-pod SSH channels even if `spec.exec.fanoutMode` selects a gateway.
  Explicit `--gateway`, interactive shells, attach-only mode, and containers
  other than the configured dev container are rejected. Use the default
  Kubernetes transport for those targets.
- Every invocation resolves current pods, checks each selected pod's owner, and
  uses Kubernetes SelfSubjectAccessReview to verify `get` and `create` on both `pods/exec`
  and `pods/portforward`. Denied, incomplete, or unavailable authorization fails
  closed, including with a warm SSH connection. This still requires API access.
  SelfSubjectAccessReview checks authorization, not command admission. Commands
  sent over SSH do not pass through Kubernetes exec admission or produce
  per-command Kubernetes exec audit records. Use the Kubernetes transport when
  those mechanisms are required.
- Connections are bound to the effective kubeconfig (including file-backed
  credentials), namespace, pod UID, container ID/start time, SSH key/user, session,
  and owner. Repinning or replacing a pod cannot reuse the former target's
  connection. Initial setup compares mount namespaces through Kubernetes exec
  and SSH, proving the shared pod SSH port belongs to the configured container.
- Arguments are shell-quoted, stdin remains a byte stream, and stdout/stderr stay
  separate. A completion record distinguishes remote exit 255 from SSH transport
  failure. JSON reports completed remote exits as `status: responded`. Delivery
  failures use `status: error`, `exit: -1`; execution deadlines use `status: timeout`,
  `exit: -1`. Failures before fanout (such as preflight rejection) can exit without
  a JSON document. `--require-all` fails on incomplete results, but a responded
  pod's nonzero exit is data: callers must check each pod's exit code too.
- A failed command is never automatically replayed or sent through Kubernetes
  as a fallback. A later, separately invoked command can establish a new master.
  Cancellation closes the client channel; it does not prove that every remote
  descendant stopped. Use tracked jobs and `jobs stop` for process lifetime.
- Masters expire after 60 idle seconds. Private sockets and identity metadata
  live under `~/.okdev/exec-ssh` (mode 0700); `okdev down` closes the session's
  masters. Expired metadata is pruned on later connection creation or down.
  Very long home paths are rejected because Unix socket paths have a size limit.
- SSH commands run through the configured SSH login shell and its environment;
  this may differ from Kubernetes exec's container environment/working directory.
  Pass an explicit shell and working directory in the command when needed, e.g.
  `okdev exec --transport=ssh -- sh -c 'cd /workspace && exec python train.py'`.
  The `--shell` flag selects interactive mode and is unsupported by this transport.

## Complete CLI measurement

The `exec_ssh` Kind regression optionally compares the complete checked CLI paths
with `EXEC_BENCH_RUNS=12`. It includes process startup, target/owner checks,
SelfSubjectAccessReview and SSH channel creation. Warm samples exclude initial
master creation and its container identity probe. It is separate from the earlier
bare-SSH experiment below; the bare-SSH numbers are not this feature's latency.

On 2026-09-21, local Kind (Kubernetes 1.32.2, macOS ARM64, Ubuntu 22.04,
cached `okdev-sidecar:v0.0.0-e2e`) produced:

| Transport | Sequential median (ms) | Sequential commands/s | Concurrency 4 median (ms) | Concurrency 4 commands/s |
| --- | ---: | ---: | ---: | ---: |
| `--transport=kubernetes` | 61.80 | 16.13 | 67.45 | 53.02 |
| `--transport=ssh` | 42.05 | 23.49 | 53.62 | 74.22 |

[Raw complete-CLI samples and source fingerprints](benchmarks/exec-ssh-kind-20260921.json)
are retained. This small local sample shows lower warm latency and higher
concurrent throughput; it does not reproduce the original remote-cluster report
or set a production latency budget. Order and background load were not controlled.

## Earlier bare-SSH measurement

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

## Regression coverage

The `exec_ssh` scenario exercises the integrated option's stream/exit contracts,
quoting, binary stdin, warm reuse, concurrent calls, selectors/groups, script and
detached execution, target changes, owner changes, real RBAC revocation on a warm
master, same-name pod replacement, wrong-container rejection, cancellation, and
API-stream interruption after a side effect. It verifies that the side effect is
not replayed and that `down` removes the session's cache connections.
The earlier `exec_transport` scenario remains as an independent bare-SSH baseline.

## Reproduce

The default regression runs without timing assertions in the existing local and
CI Kind suites. The optional benchmark uses OpenSSH and Python's standard library:

```bash
EXEC_BENCH_RUNS=12 bash scripts/e2e_kind_regressions.sh exec_ssh >exec-benchmark.log 2>&1
result=$?
cat exec-benchmark.log
exit "$result"
```

Build `bin/okdev` first or set `OKDEV_BIN` to an existing binary. The fixture uses
the existing `okdev-e2e` cluster and sidecar image, an isolated home and kubeconfig,
and its own master socket. It tears down the session, namespace, SSH processes,
relay and temporary files. `EXEC_SSH_BENCH_RESULT=` records all individual durations.
Re-run against a representative environment before setting production latency
expectations; do not infer API-outage independence from low local SSH latency.
