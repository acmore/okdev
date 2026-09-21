# Copy throughput and statistics

Use `okdev cp --stats <src> <dst>` to retain transfer measurements. The existing
interactive progress remains transient. Without `--stats`, noninteractive output
is unchanged. With it, one final JSON record with `event: "cp_stats"` goes to
stderr after a transfer succeeds or fails; source validation and target-selection
failures before transfer setup do not produce a record. Stdout retains the
normal completion/failure lines, without statistics mixed into it.

The record contains:

- `direction`: upload or download; `targets`: attempted target count.
- `streamBytes`: bytes counted through the application copy stream. Retries and
  archive framing can increase this beyond unique file sizes. It is not a TLS,
  network-interface or compressed wire-byte measurement.
- `reusedBytes`: accepted local bytes reused by a resumed or already-complete
  single-file download. These bytes are excluded from the transfer rate.
- `elapsedSeconds`: transfer-stage wall time, including file probes, retries,
  verification and fanout queueing; it excludes earlier CLI/config/target setup.
- `averageBytesPerSecond`: `streamBytes / elapsedSeconds`, aggregated across
  targets, not a per-pod link speed.
- `success`: whether the copy operation returned success. Size-acknowledged
  upload is not a checksum guarantee; use independent hashes when required.

An already-complete download without `--verify` retains the existing size-based
reuse semantics. Use `--verify` when the same-sized remote file could have changed;
that path verifies SHA-256 before reporting complete-file reuse.

```bash
okdev cp --stats ./model.bin :/tmp/model.bin 2>copy-diagnostics.log
okdev cp --stats :/tmp/model.bin ./new-download.bin 2>download-diagnostics.log
```

The stderr files may also contain errors or diagnostics; select JSON lines whose
`event` is `cp_stats`. For comparison, record the payload size/content, both CLI
wall time and transfer-stage time, direction, fanout, cluster location and hashes.
Use fresh download destinations so a reused file does not look like a fast network.

## Reproduce the local Kind investigation

The persistent Kind regression checks default output, aggregate upload counts,
hashes, new downloads, complete/verified reuse, partial resume and failed-upload
statistics. Set the following options to additionally run throughput experiments:

```bash
CP_BENCH_MIB=128 CP_BENCH_REPEATS=3 \
  bash scripts/e2e_kind_regressions.sh cp_stats >cp-benchmark.log 2>&1
# Capture this command's actual exit status before inspecting its log.
CP_BENCH_MIB=1024 CP_BENCH_REPEATS=1 CP_BENCH_CONTENTS=random \
  bash scripts/e2e_kind_regressions.sh cp_stats >cp-gib-benchmark.log 2>&1
```

This uses the existing `okdev-e2e` Kind cluster and cached sidecar image, creates
isolated test namespaces/homes, verifies hashes, and removes its resources. It
requires Python 3.11+ for the optional benchmark's streaming file hash helper and
POSIX `wait4` for child CPU/peak RSS. Default CI runs only the small regression.
The final `CP_BENCH_RESULT=` JSON records application transfer statistics,
end-to-end wall time, and child-process CPU/peak RSS. SSH master, API-server,
container-runtime and VM CPU are outside those child-process measurements.

The comparison includes the existing copy transport, Kubernetes stdin streamed
to `/dev/null`, remote `cp` reached through exec, and OpenSSH SFTP over okdev's
managed SSH path. Stream-to-null omits destination storage and copy integrity
checks. Remote `cp` includes exec startup and may benefit from page cache or
filesystem copy optimizations; it is not an isolated disk benchmark. SFTP timing
does not establish equivalent atomic replacement, resume or verification behavior.
Neither SSH nor SFTP over Kubernetes port-forward bypasses the API server.

Compression is measured on bounded 1 MiB samples at gzip levels 1 and 6. These
ratios/CPU times evaluate content sensitivity; they are not compressed-transfer
benchmarks or a recommendation to compress already-compressed model archives.

## Measured result (2026-09-21)

Local Kind on macOS ARM64, Kubernetes 1.32.2, Go 1.26.0, Ubuntu 22.04 dev
containers and the cached `okdev-sidecar:v0.0.0-e2e` image were used. The tested
working implementation was based on `d0fc6c9`. Each stored copy passed SHA-256
comparison. These are end-to-end CLI rates, including command startup, for three
128 MiB runs per content type (median MiB/s):

| Operation | Incompressible sample | Repeated text |
| --- | ---: | ---: |
| okdev upload | 78.71 | 91.14 |
| okdev download | 61.74 | 77.10 |
| kubectl stdin to `/dev/null` | 75.86 | 92.60 |
| Remote `cp` via exec | 394.23 | 397.80 |
| SFTP upload | 61.42 | 56.60 |

[Raw 128 MiB measurements](benchmarks/copy-kind-20260921-128m.json) include
per-run wall time, application counters, CPU and peak RSS. The incompressible
payload repeats a random 1 MiB block, larger than gzip's history window. The
repeated-text compression sample is 960 KiB; the random sample is 1 MiB.

A [single 1 GiB incompressible run](benchmarks/copy-kind-20260921-1g.json)
measured 63.20 MiB/s upload and 66.17 MiB/s download, with CLI peak RSS of
33.48 and 33.78 MiB respectively (roughly the same as the 128 MiB runs).
Client CPU was 1.08 s for upload and 3.33 s for download. This supports bounded
client memory for the measured streaming path; one run is not a capacity or
performance guarantee. Background system activity was not controlled.

The reported approximately 0.6 MB/s remote-cluster behavior was **not reproduced**.
Local upload throughput was comparable to the stream-to-null baseline; timing
variation can make the baseline slower. Remote `cp` was faster, but it omits the
client-to-cluster path and can benefit from cached data. This does not identify
the original bottleneck. Repeat these measurements against the affected cluster,
with its latency, network and server resource observations, before attributing
that slowdown to a transport or storage component.

SFTP was slower in this environment, so this change does not add a second copy
transport. Gzip slightly expanded the random sample while strongly compressing
repeated text. Sample compression alone does not establish end-to-end savings,
so automatic compression is not enabled. Existing atomic replacement, size
acknowledgements and retry behavior remain in place. A failure-path regression
also exposed an upload that could hang when the remote command rejected its
output path before consuming stdin; the upload now cancels that blocked stream
when it receives the existing permanent-error marker.
