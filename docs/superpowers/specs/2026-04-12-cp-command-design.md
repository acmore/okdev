# `okdev cp` Command Design

## Goal

Add an `okdev cp` command that copies files and directories between the local machine and session pod(s), with the same multi-pod targeting semantics as `okdev exec`.

## Syntax

```
okdev cp [session] <src> <dst> [flags]
```

Exactly one of `src` or `dst` must use a `:` prefix to indicate the remote (pod) side. The other argument is a local path.

```bash
# Upload
okdev cp ./checkpoints :/workspace/checkpoints
okdev cp --all ./config.yaml :/etc/app/config.yaml

# Download
okdev cp :/workspace/output ./output
okdev cp --role worker :/workspace/metrics ./metrics
```

If neither or both arguments have the `:` prefix, the command returns a validation error.

## Modes

### Single-pod mode

Default when no targeting flag is set. Copies to/from the pinned target pod, resolved the same way as `okdev exec` single-pod mode.

### Multi-pod upload

Activated by `--all`, `--pod`, `--role`, or `--label`. The same local source is fanned out to all matched pods in parallel, bounded by `--fanout`. Each pod receives an identical copy.

Per-pod progress is printed with the short pod name prefix:

```
worker-0: copied ./config.yaml -> :/etc/app/config.yaml
worker-1: copied ./config.yaml -> :/etc/app/config.yaml
```

### Multi-pod download

Downloads from each matched pod into per-pod subdirectories under the local destination, using `shortPodNames` for directory names:

```
worker-0: copied :/workspace/output -> ./output/worker-0/output
worker-1: copied :/workspace/output -> ./output/worker-1/output
```

The per-pod subdirectories are created automatically.

## Transfer Mechanism

### Files

Streamed via `cat` stdin/stdout pipes through the existing `kube.Client.CopyToPod` / `CopyFromPod` methods.

### Directories

Tar-piped via exec:

- **Upload**: Local directory is archived with `archive/tar` into a stream, piped to `tar xf - -C <remote-parent>` on the pod via `ExtractTarToPod` (already exists).
- **Download**: Remote directory is archived via `tar cf - -C <remote-parent> <name>` exec, stdout is piped to a local `archive/tar` reader that extracts to the local destination.

### Auto-detection

The command determines whether to use file or directory mode:

- **Upload**: `os.Stat` the local source. If it is a directory, use tar mode. If it is a file, use file mode.
- **Download**: Probe the remote path via `test -d <path>` exec. If the remote path is a directory, use tar mode. Otherwise, use file mode.

## Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--all` | Target all running session pods | false |
| `--pod` | Target specific pods by name (repeatable/comma-separated) | nil |
| `--role` | Target pods by `okdev.io/workload-role` label | "" |
| `--label` | Target pods by label key=value (repeatable, AND logic) | nil |
| `--exclude` | Exclude specific pods (repeatable/comma-separated) | nil |
| `--container` | Override target container | "" |
| `--fanout` | Maximum concurrent pod transfers | 16 |

Mutual exclusivity and validation rules are identical to `okdev exec`: `--all`, `--pod`, `--role`, and `--label` are mutually exclusive. `--exclude` requires `--all`, `--role`, or `--label`. `--exclude` cannot be combined with `--pod`.

Unlike `okdev exec`, there is no `--detach`, `--timeout`, `--log-dir`, `--no-prefix`, `--cmd`, `--shell`, or `--no-tty` flag.

## Error Handling

- If the local source does not exist (upload), fail immediately before contacting any pod.
- If the remote path does not exist (download file mode), report per-pod errors.
- Partial failures in multi-pod mode are reported per-pod with the short name: `FAILED: worker-1 (tar: remote-path: No such file or directory)`.
- The exit error summarizes: `2 of 4 pods failed`.
- The error reporting pattern matches `runMultiExec` / `runDetachExec` in `pdsh.go`.

## New Code

### `internal/cli/cp.go`

- `newCpCmd(opts *Options) *cobra.Command` -- command definition, flag binding, argument parsing.
- `parseCpArgs(src, dst string) (localPath, remotePath string, upload bool, err error)` -- validates exactly one `:` prefix.
- `runCp(...)` -- single-pod copy orchestration (resolve target, detect file vs dir, call kube client).
- `runMultiPodCp(...)` -- multi-pod orchestration reusing pod filtering helpers from `pdsh.go` and fanout semaphore pattern.

### `internal/kube/client.go`

- `CopyDirToPod(ctx, namespace, pod, container, localDir, remoteDir)` -- creates tar stream from local directory via `archive/tar`, pipes to `ExtractTarToPod`.
- `CopyDirFromPod(ctx, namespace, pod, container, remoteDir, localDir)` -- execs `tar cf - -C <parent> <basename>` on pod, extracts via `archive/tar` reader locally.
- `IsRemoteDir(ctx, namespace, pod, container, path)` -- probes via `test -d` exec, returns bool.

### Reused from `pdsh.go`

- `filterRunningPods`, `filterPodsByRole`, `filterPodsByName`, `filterPodsByLabels`, `excludePods`
- `shortPodNames` from `pdsh_writer.go`
- Pod resolution via `commandContext`, `resolveCommandContext`, `ensureExistingSessionOwnership`
- `resolveTargetRef` for single-pod mode
- `resolveTargetContainer` for default container
- `validateMultiPodFlags` (subset -- no `cmdStr`/`detach` params, but same mutual-exclusivity logic)

## Command Registration

Registered in the root command alongside `exec`, `ssh`, `logs`, etc. No aliases.

## Documentation

`docs/command-reference.md` updated with the `okdev cp` entry and detailed flag descriptions.

## Testing

### Unit tests (`internal/cli/cp_test.go`)

- `parseCpArgs` validation: both local, both remote, valid upload, valid download.
- Pod filtering integration: filter → copy → verify per-pod results.
- Multi-pod download directory layout verification.
- Error aggregation for partial failures.

### Unit tests (`internal/kube/client_test.go` or `client_copy_test.go`)

- `CopyDirToPod`: tar creation and extraction round-trip.
- `CopyDirFromPod`: tar stream extraction to local filesystem.
- `IsRemoteDir`: probe true/false cases.

### E2E Kind test

Add to `scripts/e2e_kind_pytorchjob.sh` (has 3 pods):
- Upload a file to all pods, verify via exec.
- Upload a directory to a role, verify contents.
- Download a file from all pods, verify per-pod subdirectories.
- Download a directory from a single pod.
